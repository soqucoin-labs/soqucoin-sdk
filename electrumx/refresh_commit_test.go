package electrumx

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// twoUTXOs is the set the later listunspent reports; oneUTXO is the older one.
const twoUTXOs = `[{"tx_hash":"` + txA + `","tx_pos":0,"value":100,"height":10},` +
	`{"tx_hash":"` + txB + `","tx_pos":0,"value":900,"height":11}]`

// connectOnly builds a client against stub and tracks addrs without starting
// the refresher, so every call in the test is one the test made.
func connectOnly(t *testing.T, addr string, addrs ...string) *Client {
	t.Helper()
	c := NewClient(addr, time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses(addrs); err != nil {
		t.Fatal(err)
	}
	return c
}

// Two refreshes of one address can be in flight at once: the refresher's pass
// and a caller's RefreshAll both reach refreshAddress, and nothing serialises
// them. Their replies can arrive in either order, and the merge drops every
// output its own reply does not list. Without an order on the commits, the
// older reply lands last and takes a deposit the newer one found back out of
// the cache, while dating the set current.
func TestALateListunspentReplyDoesNotRestoreAnOlderSet(t *testing.T) {
	a := craftAddr(t, 0x11)
	var mu sync.Mutex
	seen := 0
	held := make(chan int64, 1)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		switch req.Method {
		case "blockchain.scripthash.subscribe":
			return []string{reply(req.ID, `null`)}
		case "blockchain.scripthash.listunspent":
			mu.Lock()
			seen++
			first := seen == 1
			mu.Unlock()
			if first {
				held <- req.ID
				return nil // held: this reply is pushed after the second one
			}
			return []string{reply(req.ID, twoUTXOs)}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connectOnly(t, stub.addr(), a)

	done := make(chan error, 1)
	go func() { done <- c.refreshAddress(context.Background(), a) }()
	id := <-held

	if err := c.refreshAddress(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if n := len(c.GetUTXOs(a)); n != 2 {
		t.Fatalf("the second refresh left %d outputs cached, want 2", n)
	}

	stub.push(reply(id, oneUTXO))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(c.GetUTXOs(a)); n != 2 {
		t.Errorf("the older reply committed over the newer set: %d outputs cached, want 2", n)
	}
}

// OnRefresh runs outside mu so a slow handler cannot hold the reader. The
// unlock and re-lock that arranged that were inside a function holding a
// deferred Unlock, so a handler that panicked returned through a defer that
// unlocked an already unlocked mutex. That is a fatal runtime error: no
// recover sees it, and the process the exchange runs ends.
func TestAPanicInOnRefreshLeavesTheClientUsable(t *testing.T) {
	stub := newPushStub(t)
	a := craftAddr(t, 0x11)
	stub.set(scripthashOf(t, a), "", oneUTXO)
	c := connectOnly(t, stub.addr(), a)
	c.OnRefresh = func(string, int) { panic("a handler of the integrator's") }

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_ = c.refreshAddress(context.Background(), a)
	}()

	got := make(chan int, 1)
	go func() { got <- len(c.GetUTXOs(a)) }()
	select {
	case n := <-got:
		if n != 1 {
			t.Errorf("the cache holds %d outputs, want the 1 the commit wrote", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read blocked after the panic: the commit left mu locked")
	}
}

// A subscribe reply that came over a connection since replaced must commit
// nothing. Its status was true when the server wrote it, the record would
// date it now, and the address is not subscribed on the live connection.
func TestASubscribeReplyFromAReplacedConnectionCommitsNothing(t *testing.T) {
	a := craftAddr(t, 0x11)
	c := NewClient("127.0.0.1:1", time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	c.liveGen.Store(2)

	c.commitSubscribe(a, 1, "the status the old connection reported", time.Now())
	c.mu.Lock()
	gen, subscribed := c.subscribed[a]
	_, statusWritten := c.status[a]
	rec := c.refreshed[a]
	c.mu.Unlock()
	if subscribed {
		t.Errorf("a reply from generation 1 recorded a subscription on generation %d", gen)
	}
	if statusWritten {
		t.Error("a reply from a replaced connection wrote the address's status")
	}
	if rec.dirty || rec.seq != 0 {
		t.Error("a reply from a replaced connection moved the freshness record")
	}

	c.commitSubscribe(a, 2, "the live connection's status", time.Now())
	c.mu.Lock()
	gen, subscribed = c.subscribed[a]
	st := c.status[a]
	c.mu.Unlock()
	if !subscribed || gen != 2 {
		t.Errorf("the live connection's reply recorded generation %d, subscribed=%v", gen, subscribed)
	}
	if st != "the live connection's status" {
		t.Errorf("the status recorded is %q", st)
	}
}

// A listunspent reply from a connection that has been replaced commits
// nothing, as a subscribe reply from one commits nothing: the set was true
// when the server wrote it and the record would date it now against a
// generation that is gone. The stale reply is recorded as a lost connection so
// the pass ends, and the error on the record makes the next pass on the live
// connection refresh the address whatever status its subscribe reply carries.
func TestAListunspentReplyFromAReplacedConnectionCommitsNothing(t *testing.T) {
	a := craftAddr(t, 0x11)
	c := NewClient("127.0.0.1:1", time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	c.liveGen.Store(2)
	fresh := []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 10}}

	n, committed, err := c.commitRefresh(a, 1, 0, 1, fresh)
	if committed || n != 0 {
		t.Fatalf("a reply from generation 1 committed %d outputs while generation 2 is live", n)
	}
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a stale reply reported %v, want an error wrapping ErrNotConnected so the pass ends", err)
	}
	if got := c.GetUTXOs(a); len(got) != 0 {
		t.Fatalf("a reply from a replaced connection reached the cache: %+v", got)
	}
	at, rerr := c.LastRefreshOf(a)
	if rerr == nil || !at.IsZero() {
		t.Fatalf("after a stale reply the record reads at=%v err=%v, want the zero time and the error", at, rerr)
	}
	c.mu.Lock()
	pending := c.changed[a]
	c.mu.Unlock()
	if !pending {
		t.Fatal("a stale reply left no refresh pending, so an address whose live subscribe had already committed clean would carry the error until the reconcile")
	}

	n, committed, err = c.commitRefresh(a, 2, 0, 2, fresh)
	if err != nil || !committed || n != 1 {
		t.Fatalf("the live connection's reply: n=%d committed=%v err=%v", n, committed, err)
	}
	if got := c.GetUTXOs(a); len(got) != 1 {
		t.Fatalf("the live connection's reply did not reach the cache: %+v", got)
	}
	at, rerr = c.LastRefreshOf(a)
	if rerr != nil || at.IsZero() {
		t.Fatalf("after the live reply the record reads at=%v err=%v", at, rerr)
	}
}

// The live generation is written under the cache lock as well as the
// connection semaphore, so a check of it under the lock cannot be overtaken by
// a replacement before the write that follows. Here the lock is held while a
// Connect and then a Stop try to move it: neither completes until the lock is
// released. Without this a reply from the old connection could pass the
// stale check and commit after Connect had moved the generation.
func TestTheLiveGenerationDoesNotMoveWhileTheCacheLockIsHeld(t *testing.T) {
	stub := newStub(t, nil)
	c := NewClient(stub.addr(), time.Hour, nil)

	c.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()
	select {
	case err := <-done:
		c.mu.Unlock()
		t.Fatalf("Connect completed while the cache lock was held (err=%v): the generation moved outside the lock", err)
	case <-time.After(150 * time.Millisecond):
	}
	if g := c.liveGen.Load(); g != 0 {
		c.mu.Unlock()
		t.Fatalf("the live generation moved to %d while the cache lock was held", g)
	}
	c.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("Connect after the lock was released: %v", err)
	}
	if g := c.liveGen.Load(); g != 1 {
		t.Fatalf("live generation after Connect: %d, want 1", g)
	}

	c.mu.Lock()
	stopped := make(chan struct{})
	go func() { c.Stop(); close(stopped) }()
	select {
	case <-stopped:
		c.mu.Unlock()
		t.Fatal("Stop completed while the cache lock was held: the generation moved outside the lock")
	case <-time.After(150 * time.Millisecond):
	}
	if g := c.liveGen.Load(); g != 1 {
		c.mu.Unlock()
		t.Fatalf("the live generation moved to %d during Stop while the cache lock was held", g)
	}
	c.mu.Unlock()
	<-stopped
	if g := c.liveGen.Load(); g != 0 {
		t.Fatalf("live generation after Stop: %d, want 0", g)
	}
}

// A reply that could not be parsed, from a connection that has since been
// replaced, is recorded as the replacement and not as a fault of the address:
// the pass ends as it does for any lost connection, and the Monitor is not
// alarmed for a parse error the replacement explains. From the live
// connection the parse error itself is recorded.
func TestAMalformedReplyFromAReplacedConnectionIsRecordedAsTheReplacement(t *testing.T) {
	a := craftAddr(t, 0x11)
	c := NewClient("127.0.0.1:1", time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	c.liveGen.Store(2)
	parse := errors.New("parse utxos: entry 0: value -5 is negative")

	err := c.recordRefreshFailure(a, 1, 0, 1, parse)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a malformed reply from generation 1 while 2 is live was recorded as %v, want ErrNotConnected", err)
	}
	if _, rerr := c.LastRefreshOf(a); !errors.Is(rerr, ErrNotConnected) {
		t.Fatalf("the record reads %v, want ErrNotConnected", rerr)
	}

	err = c.recordRefreshFailure(a, 2, 0, 2, parse)
	if !errors.Is(err, parse) {
		t.Fatalf("a malformed reply from the live connection was recorded as %v, want the parse error", err)
	}
	if _, rerr := c.LastRefreshOf(a); !errors.Is(rerr, parse) {
		t.Fatalf("the record reads %v, want the parse error", rerr)
	}

	// A stale malformed reply that a later refresh has already superseded
	// (ticket 1 after ticket 2 committed) records nothing and leaves no
	// refresh pending, as commitRefresh drops a superseded reply.
	c.mu.Lock()
	c.changed = make(map[string]bool)
	c.mu.Unlock()
	_ = c.recordRefreshFailure(a, 1, 0, 1, parse)
	c.mu.Lock()
	pending := c.changed[a]
	c.mu.Unlock()
	if pending {
		t.Fatal("a superseded stale reply left a refresh pending, which costs a listunspent the later refresh already made")
	}
}
