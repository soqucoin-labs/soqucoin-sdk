package electrumx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// pushStub is a scripted server that answers subscribe and listunspent from
// per-scripthash state a test sets, and counts the calls by method and
// scripthash. Statuses are what the test says they are; the client never
// interprets them beyond equality.
type pushStub struct {
	*scriptedStub
	mu       sync.Mutex
	status   map[string]string // scripthash -> status the subscribe reply carries
	utxos    map[string]string // scripthash -> listunspent JSON
	failList map[string]bool   // scripthash -> listunspent answers an error
	calls    map[string]int    // method+" "+scripthash -> count
}

func newPushStub(t *testing.T) *pushStub {
	t.Helper()
	p := &pushStub{status: map[string]string{}, utxos: map[string]string{}, failList: map[string]bool{}, calls: map[string]int{}}
	p.scriptedStub = newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		p.mu.Lock()
		defer p.mu.Unlock()
		sh := firstParam(req)
		p.calls[req.Method+" "+sh]++
		switch req.Method {
		case "blockchain.scripthash.subscribe":
			st, ok := p.status[sh]
			if !ok || st == "" {
				return []string{reply(req.ID, `null`)}
			}
			return []string{reply(req.ID, fmt.Sprintf("%q", st))}
		case "blockchain.scripthash.listunspent":
			if p.failList[sh] {
				return []string{fmt.Sprintf(`{"id":%d,"error":{"code":1,"message":"boom"}}`, req.ID)}
			}
			u, ok := p.utxos[sh]
			if !ok {
				u = `[]`
			}
			return []string{reply(req.ID, u)}
		}
		return []string{reply(req.ID, `null`)}
	})
	return p
}

func (p *pushStub) count(method, sh string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[method+" "+sh]
}

func (p *pushStub) set(sh, status, utxos string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status[sh] = status
	p.utxos[sh] = utxos
}

// notify pushes a scripthash notification and updates what listunspent
// answers, as a real server would after processing a block.
func (p *pushStub) notify(sh, status, utxos string) {
	p.set(sh, status, utxos)
	p.push(scripthashNotification(sh, status))
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startClient connects, tracks addrs and starts the refresher with a long
// reconcile interval, so everything observed comes from subscriptions.
func startClient(t *testing.T, stub *pushStub, addrs ...string) (*Client, context.CancelFunc) {
	t.Helper()
	c := NewClient(stub.addr(), time.Hour, nil)
	c.HRP = types.Stagenet.HRP
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses(addrs); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)
	return c, cancel
}

const oneUTXO = `[{"tx_hash":"` + txA + `","tx_pos":0,"value":100,"height":10}]`

// The attack first: a notification writes nothing. The server pushes a change
// for an address; the cache holds what the last listunspent said until the
// listunspent the notification asked for has landed, and then holds what that
// reply said. The notification's own content never reaches the cache.
func TestNotificationWritesNothingUntilListunspentAnswers(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	stub.set(sh1, "s0", `[]`)
	c, _ := startClient(t, stub, a1)
	waitFor(t, "the first refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 1 })
	if len(c.GetUTXOs(a1)) != 0 {
		t.Fatal("cache should be empty after an empty listunspent")
	}

	// The notification carries a status only; the utxos are what the next
	// listunspent returns. Hold that reply: the cache must not change.
	stub.mu.Lock()
	stub.failList[sh1] = true // the first listunspent after the change fails; nothing lands
	stub.mu.Unlock()
	stub.push(scripthashNotification(sh1, "s1"))
	waitFor(t, "the listunspent the notification asked for", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) >= 2 })
	if len(c.GetUTXOs(a1)) != 0 {
		t.Fatal("a notification changed the cache without a listunspent reply")
	}
	if _, err := c.LastRefreshOf(a1); err == nil {
		t.Fatal("the failed listunspent left no error on the record")
	}

	// Now the reply lands, on the next notification.
	stub.mu.Lock()
	stub.failList[sh1] = false
	stub.mu.Unlock()
	stub.notify(sh1, "s2", oneUTXO)
	waitFor(t, "the cache to show the output", func() bool { return len(c.GetUTXOs(a1)) == 1 })
	if u := c.GetUTXOs(a1)[0]; u.TxID != txA || u.Value != 100 {
		t.Fatalf("cache holds %+v, want the listunspent reply", u)
	}
}

// A notification for a scripthash the client did not compute, or a malformed
// one, changes nothing and asks the server nothing.
func TestUnknownOrMalformedNotificationIsIgnored(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	stub.set(sh1, "s0", `[]`) // a real status, so a malformed one read as empty would look like a change
	c, _ := startClient(t, stub, a1)
	waitFor(t, "the first refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 1 })

	other := scripthashOf(t, craftAddr(t, 0x99))
	stub.push(
		scripthashNotification(other, "s1"),
		`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":["only-one-param"]}`,
		`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":[42,"s1"]}`,
		`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":["`+sh1+`",42]}`,
		`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":["`+sh1+`",{"a":1}]}`,
		`{"jsonrpc":"2.0","method":"something.else","params":[]}`,
	)
	// Give the reader time to process; nothing should follow.
	time.Sleep(100 * time.Millisecond)
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 1 {
		t.Fatalf("%d listunspent for the tracked address after unrelated notifications, want 1", n)
	}
	if n := stub.count("blockchain.scripthash.listunspent", other); n != 0 {
		t.Fatalf("%d listunspent for an untracked scripthash", n)
	}
	if _, err := c.Call(context.Background(), "x", nil); err != nil {
		t.Fatalf("the connection should be untouched: %v", err)
	}
}

// A flood of notifications for one address costs one listunspent per wake:
// the notifications mark a set, the pass drains it once.
func TestNotificationFloodCoalescesToOneRefreshPerPass(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	c := NewClient(stub.addr(), time.Hour, nil)
	c.HRP = types.Stagenet.HRP
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	// The refresher is not running; drive the pass by hand so the flood lands
	// before the drain.
	if err := c.pass(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	gen := c.liveGen.Load()
	for i := 0; i < 10000; i++ {
		c.noteChange(gen, sh1, fmt.Sprintf("s%d", i))
	}
	if err := c.pass(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 2 {
		t.Fatalf("%d listunspent after a flood of 10000 notifications, want 2 (the first pass and one drain)", n)
	}
	if err := c.pass(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 2 {
		t.Fatalf("a pass with nothing changed made a listunspent: %d", n)
	}
}

// A duplicate notification, the status the client last saw, changes nothing.
func TestDuplicateStatusIsNotAChange(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	stub.set(sh1, "s0", `[]`)
	_, _ = startClient(t, stub, a1)
	waitFor(t, "the first refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 1 })
	stub.push(scripthashNotification(sh1, "s0"))
	time.Sleep(100 * time.Millisecond)
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 1 {
		t.Fatalf("%d listunspent after a duplicate status, want 1", n)
	}
	stub.push(scripthashNotification(sh1, "s1"))
	waitFor(t, "a refresh after a real change", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 2 })
}

// Start subscribes every tracked address; an address tracked later is
// subscribed on the next pass; an address dropped is not refreshed again.
func TestStartSubscribesTrackedAddresses(t *testing.T) {
	stub := newPushStub(t)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	c, _ := startClient(t, stub, a1)
	waitFor(t, "the subscription", func() bool { return stub.count("blockchain.scripthash.subscribe", sh1) == 1 })
	waitFor(t, "the first refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 1 })

	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second subscription", func() bool { return stub.count("blockchain.scripthash.subscribe", sh2) == 1 })
	waitFor(t, "the second refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh2) == 1 })
	if n := stub.count("blockchain.scripthash.subscribe", sh1); n != 1 {
		t.Fatalf("the first address was subscribed %d times", n)
	}

	if err := c.TrackAddresses([]string{a2}); err != nil {
		t.Fatal(err)
	}
	stub.push(scripthashNotification(sh1, "gone"))
	time.Sleep(100 * time.Millisecond)
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 1 {
		t.Fatalf("a dropped address was refreshed: %d listunspent", n)
	}
	if at, _ := c.LastRefreshOf(a1); !at.IsZero() {
		t.Fatal("a dropped address still has a record")
	}
}

// Reconnect re-subscribes every address. An address whose status is the one
// last seen costs one subscribe call and no listunspent, and its record is
// advanced; an address whose status changed during the gap is refreshed. A
// deposit mined during the gap is therefore in the cache after the reconnect.
func TestReconnectResubscribesAndRefreshesOnlyWhatChanged(t *testing.T) {
	stub := newPushStub(t)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	stub.set(sh1, "s1", `[]`)
	stub.set(sh2, "s2", `[]`)
	c, _ := startClient(t, stub, a1, a2)
	waitFor(t, "both refreshed", func() bool {
		return stub.count("blockchain.scripthash.listunspent", sh1) == 1 && stub.count("blockchain.scripthash.listunspent", sh2) == 1
	})
	at1Before, _ := c.LastRefreshOf(a1)

	// The server drops the connection; while the client is away, a2 changes.
	stub.set(sh2, "s2b", oneUTXO)
	time.Sleep(10 * time.Millisecond) // the records' clock must move
	stub.closeConns()
	waitFor(t, "the re-subscriptions", func() bool {
		return stub.count("blockchain.scripthash.subscribe", sh1) == 2 && stub.count("blockchain.scripthash.subscribe", sh2) == 2
	})
	waitFor(t, "a2 refreshed after the gap", func() bool { return stub.count("blockchain.scripthash.listunspent", sh2) == 2 })
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 1 {
		t.Fatalf("an unchanged address was refreshed after the reconnect: %d listunspent", n)
	}
	if len(c.GetUTXOs(a2)) != 1 {
		t.Fatal("the deposit mined during the gap is not in the cache")
	}
	at1After, err := c.LastRefreshOf(a1)
	if err != nil || !at1After.After(at1Before) {
		t.Fatalf("an unchanged address's record did not advance on the re-subscribe: %v -> %v, %v", at1Before, at1After, err)
	}
}

// A ping advances the records of subscribed addresses that are clean, and
// only those: not one with a change in flight, not one whose last attempt
// failed.
func TestPingAdvancesOnlyCleanRecords(t *testing.T) {
	stub := newPushStub(t)
	a1, a2, a3 := craftAddr(t, 0x11), craftAddr(t, 0x22), craftAddr(t, 0x33)
	sh1, sh2, sh3 := scripthashOf(t, a1), scripthashOf(t, a2), scripthashOf(t, a3)
	stub.mu.Lock()
	stub.failList[sh3] = true
	stub.mu.Unlock()
	c := NewClient(stub.addr(), time.Hour, nil)
	c.HRP = types.Stagenet.HRP
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{a1, a2, a3}); err != nil {
		t.Fatal(err)
	}
	if err := c.pass(context.Background(), true); err == nil || !strings.Contains(err.Error(), a3) {
		t.Fatalf("pass should fail on a3 only: %v", err)
	}
	at1, at2 := mustAt(t, c, a1), mustAt(t, c, a2)
	if at3, err := c.LastRefreshOf(a3); !at3.IsZero() || err == nil {
		t.Fatalf("a3 record %v %v, want never refreshed with an error", at3, err)
	}

	// a2 gets a notification; its listunspent is held back by not draining.
	c.noteChange(c.liveGen.Load(), sh2, "changed")
	time.Sleep(10 * time.Millisecond)
	if err := c.ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if at := mustAt(t, c, a1); !at.After(at1) {
		t.Fatal("a clean subscribed record did not advance on the ping")
	}
	if at := mustAt(t, c, a2); !at.Equal(at2) {
		t.Fatalf("a record with a change in flight advanced on the ping: %v -> %v", at2, at)
	}
	if at3, _ := c.LastRefreshOf(a3); !at3.IsZero() {
		t.Fatal("a record whose attempt failed advanced on the ping")
	}
	// Once the pending change is refreshed, a2 is clean again. a3, still
	// failing, stays marked and the pass names it.
	if err := c.pass(context.Background(), false); err == nil || !strings.Contains(err.Error(), a3) || strings.Contains(err.Error(), a2) {
		t.Fatalf("pass after the change: %v, want a3's failure alone", err)
	}
	at2 = mustAt(t, c, a2)
	time.Sleep(10 * time.Millisecond)
	if err := c.ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if at := mustAt(t, c, a2); !at.After(at2) {
		t.Fatal("a record advanced by its listunspent did not advance on the next ping")
	}

	// The guard that matters most: a1 was clean; the indexer then starts
	// refusing it with no notification pending. Its record carries the
	// error, and no ping advances it again until a listunspent succeeds.
	stub.mu.Lock()
	stub.failList[sh1] = true
	stub.mu.Unlock()
	at1 = mustAt(t, c, a1)
	if err := c.pass(context.Background(), true); err == nil || !strings.Contains(err.Error(), a1) {
		t.Fatalf("full pass with a1 failing: %v", err)
	}
	if at, rerr := c.LastRefreshOf(a1); rerr == nil || !at.Equal(at1) {
		t.Fatalf("a1 after the refused listunspent: at %v err %v, want the old moment and the error", at, rerr)
	}
	time.Sleep(10 * time.Millisecond)
	if err := c.ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if at, _ := c.LastRefreshOf(a1); !at.Equal(at1) {
		t.Fatal("a ping advanced an address the indexer refuses to answer for")
	}
}

func mustAt(t *testing.T, c *Client, addr string) time.Time {
	t.Helper()
	at, err := c.LastRefreshOf(addr)
	if err != nil || at.IsZero() {
		t.Fatalf("%s: record %v %v, want a clean record", addr, at, err)
	}
	return at
}

// A listunspent reply that the server sent before it processed a change may
// predate the notification that arrives in front of it. The record stays
// dirty and the address stays marked, so the next pass refreshes it again.
func TestReplyPredatingANotificationLeavesTheChangePending(t *testing.T) {
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	var mu sync.Mutex
	listCalls := 0
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		switch req.Method {
		case "blockchain.scripthash.subscribe":
			return []string{reply(req.ID, `"s0"`)}
		case "blockchain.scripthash.listunspent":
			mu.Lock()
			listCalls++
			n := listCalls
			mu.Unlock()
			if n == 2 {
				// The change lands in front of the reply computed before it.
				return []string{scripthashNotification(sh1, "s1"), reply(req.ID, `[]`)}
			}
			return []string{reply(req.ID, oneUTXO)}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	if err := c.pass(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// The second listunspent: the notification arrives first on the stream.
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the notification to be processed", func() bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.changed[a1]
	})
	c.mu.RLock()
	rec := c.refreshed[a1]
	c.mu.RUnlock()
	if !rec.dirty {
		t.Fatal("a reply that predates a notification cleared the change flag")
	}
	if err := c.pass(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(c.GetUTXOs(a1)) != 1 {
		t.Fatal("the pass after the pending change did not refresh the address")
	}
	c.mu.RLock()
	rec = c.refreshed[a1]
	c.mu.RUnlock()
	if rec.dirty {
		t.Fatal("the refresh that answered the change left the flag set")
	}
}

// The refresher reconnects when the connection is lost, at once, and stops
// trying once Stop is called.
func TestRefresherReconnectsOnLossAndNotAfterStop(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	c, _ := startClient(t, stub, a1)
	waitFor(t, "the subscription", func() bool { return stub.count("blockchain.scripthash.subscribe", sh1) == 1 })
	stub.closeConns()
	waitFor(t, "the re-subscription", func() bool { return stub.count("blockchain.scripthash.subscribe", sh1) == 2 })

	c.Stop()
	time.Sleep(100 * time.Millisecond)
	if n := stub.count("blockchain.scripthash.subscribe", sh1); n != 2 {
		t.Fatalf("the refresher reconnected after Stop: %d subscriptions", n)
	}
	if _, err := c.Call(context.Background(), "x", nil); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("call after Stop: %v", err)
	}
}

// A dropped notification is covered by the reconcile: the full pass on the
// interval refreshes every address whatever the statuses say.
func TestReconcileCoversADroppedNotification(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	stub.set(sh1, "s0", `[]`)
	c := NewClient(stub.addr(), 150*time.Millisecond, nil)
	c.HRP = types.Stagenet.HRP
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	waitFor(t, "the first refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 1 })
	// The server's state changes and it sends no notification.
	stub.set(sh1, "s1", oneUTXO)
	waitFor(t, "the reconcile to refresh the address", func() bool { return len(c.GetUTXOs(a1)) == 1 })
}
