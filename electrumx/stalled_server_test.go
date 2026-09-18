package electrumx

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A server that completes the handshake, answers pings, and then holds every
// reply. This is what an overloaded or throttling indexer looks like from the
// client, and it is the case the reconnect rule in Start's comment was
// written for.

// callCounter counts requests by method and scripthash for a scripted stub.
type callCounter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (k *callCounter) add(req request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.calls == nil {
		k.calls = map[string]int{}
	}
	k.calls[req.Method+" "+firstParam(req)]++
}

func (k *callCounter) count(method, sh string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls[method+" "+sh]
}

func connections(s *scriptedStub) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// The subscribe path. The pass ends at the first call whose reply does not
// arrive, so the second address is never asked on that connection; the
// retry's pass makes the same call first, its timeout is the second in a row,
// and the connection is rebuilt. Before this the pass walked every address at
// one call deadline each and the policy saw nothing until it returned: with
// 60,000 addresses the first rebuild was weeks away.
func TestAReplyTimeoutEndsThePassAndTwoInARowRebuildTheConnection(t *testing.T) {
	var k callCounter
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		k.add(req)
		if req.Method == "blockchain.scripthash.subscribe" {
			return nil // held for the life of the connection
		}
		return []string{reply(req.ID, `null`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	c.callDeadline = 150 * time.Millisecond
	setHRP(t, c, types.Stagenet.HRP)
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)

	// Pass one times out on a1 at 150 ms; the retry fires a second later and
	// times out on a1 again; that is two in a row.
	waitFor(t, "the connection to be rebuilt after two timeouts in a row", func() bool {
		return connections(stub) >= 2
	})
	if n := k.count("blockchain.scripthash.subscribe", sh2); n != 0 {
		t.Fatalf("the second address was asked %d times while the first address's reply was outstanding: the pass did not end at the timeout", n)
	}
	if n := k.count("blockchain.scripthash.subscribe", sh1); n < 2 {
		t.Fatalf("the first address was asked %d times before the rebuild, want one per pass over two passes", n)
	}
}

// The listunspent path. Subscribes are answered, so the pass reaches the
// refresh; the first address's listunspent is held. The pass ends there and
// the second address is not asked during the backoff that follows. The
// addresses not reached keep their change pending, which the retry after a
// failed refresh already pins.
func TestAReplyTimeoutEndsTheRefreshWhereItIs(t *testing.T) {
	var k callCounter
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		k.add(req)
		switch req.Method {
		case "blockchain.scripthash.subscribe":
			return []string{reply(req.ID, `"s1"`)}
		case "blockchain.scripthash.listunspent":
			return nil // held
		}
		return []string{reply(req.ID, `null`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	c.callDeadline = 150 * time.Millisecond
	setHRP(t, c, types.Stagenet.HRP)
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)

	// The opening pass is full, so the refresh walks the addresses in tracked
	// order and a1 is asked first.
	waitFor(t, "the first listunspent", func() bool {
		return k.count("blockchain.scripthash.listunspent", sh1) == 1
	})
	// Past the call deadline and well inside the one-second backoff that
	// follows the timeout: the pass has ended and no retry has run yet.
	time.Sleep(500 * time.Millisecond)
	if n := k.count("blockchain.scripthash.listunspent", sh2); n != 0 {
		t.Fatalf("the second address was asked %d times after the first address's listunspent timed out: the refresh did not end there", n)
	}
	if n := k.count("blockchain.scripthash.listunspent", sh1); n != 1 {
		t.Fatalf("the first address was asked %d times inside the backoff, want 1", n)
	}
	if _, err := c.LastRefreshOf(a1); !errIsNoReply(err) {
		t.Fatalf("the timed-out address records %v, want the no-reply error", err)
	}
}
