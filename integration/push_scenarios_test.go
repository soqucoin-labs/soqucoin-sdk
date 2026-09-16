//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The push scenarios run the real deposit package against the real node with
// the electrumx.Client fed by the in-process fake ElectrumX over the harness
// scanner. Every deposit in the harness is a coinbase, so a new one is buried
// under coinbase maturity before it can be credited.
//
// What each scenario claims about where the client learned of a deposit is
// asserted on the indexer's notification counters, not on a quiet interval.
// A scenario that sleeps and then says "not in the cache yet" is asserting
// that this machine's reconcile tick had not fired, which is a fact about the
// machine; the counters are facts about the run.

// mineDepositAndBury pays one coinbase to addr and buries it past maturity,
// advancing the indexer after each stage as a real one would. The fake's state
// is the scanner, so advance is also how the indexer comes to know a block
// exists at all.
func (f *fixture) mineDepositAndBury(t *testing.T, idx *fakeIndexer, addr string) {
	t.Helper()
	f.n.mine(addr, 1)
	idx.advance()
	f.n.mine(f.hot, int(types.Regtest.CoinbaseMaturity)+5)
	idx.advance()
}

// 10. A deposit the server pushes is credited after the node check. The client
// learns of it from the notification and not from a poll: the reconcile
// interval and the ping interval are both an hour, so the one listunspent
// after the initial refresh can only be the one the notification asked for.
func TestPushedDepositIsCreditedAfterTheNodeCheck(t *testing.T) {
	f := setup(t)
	idx, elx := f.indexer(t, time.Hour, f.dep)
	led := newLedger()
	m := f.monitorOver(t, elx, led, 30, f.dep)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("initial scan: %v %+v, want the buried coinbase", err, got)
	}
	sh := idx.scripthash(f.dep)
	subsBefore, listBefore := idx.count("blockchain.scripthash.subscribe", sh), idx.count("blockchain.scripthash.listunspent", sh)

	f.mineDepositAndBury(t, idx, f.dep)
	waitUntil(t, "the pushed deposit to reach the cache", func() bool { return len(elx.GetUTXOs(f.dep)) == 2 })

	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("scan after the push: %v %+v, want the new deposit", err, got)
	}
	if again, _ := m.Scan(context.Background()); len(again) != 0 {
		t.Fatal("deposit credited twice")
	}
	if sent, suppressed := idx.notifications(); sent != 1 || suppressed != 0 {
		t.Errorf("the server wrote %d notifications and withheld %d; one deposit on one subscribed address is one push", sent, suppressed)
	}
	if n := idx.count("blockchain.scripthash.subscribe", sh) - subsBefore; n != 0 {
		t.Errorf("%d subscribe calls after the initial one; the subscription is per connection", n)
	}
	if n := idx.count("blockchain.scripthash.listunspent", sh) - listBefore; n != 1 {
		t.Errorf("%d listunspent after the push, want exactly one: the one the notification asked for", n)
	}
	if len(f.alerts) != 0 {
		t.Errorf("alerts on a clean push credit: %v", f.alertMsgs)
	}
}

// 11. The server never sends the notification. The deposit still reaches the
// cache, and the reconcile is the only thing that can have put it there: the
// start pass is long finished, nothing was pushed, the ping interval is an
// hour, and no connection was lost. Then it is credited. The bound the guide
// states.
func TestDroppedNotificationIsCoveredByTheReconcile(t *testing.T) {
	f := setup(t)
	idx, elx := f.indexer(t, 2*time.Second, f.dep)
	led := newLedger()
	m := f.monitorOver(t, elx, led, 30, f.dep)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("initial scan: %v %+v", err, got)
	}

	sh := idx.scripthash(f.dep)
	subsBefore := idx.count("blockchain.scripthash.subscribe", sh)

	idx.setDrop(true)
	f.mineDepositAndBury(t, idx, f.dep)
	// The server had a change to report and withheld it; without this the
	// scenario would pass against an indexer that simply saw nothing.
	waitUntil(t, "the withheld notification", func() bool { _, s := idx.notifications(); return s > 0 })

	waitUntil(t, "the reconcile to refresh the address", func() bool { return len(elx.GetUTXOs(f.dep)) == 2 })
	if sent, _ := idx.notifications(); sent != 0 {
		t.Fatalf("the server pushed %d notifications after all, so this run did not test the reconcile path", sent)
	}
	// The other candidate the comment rules out: a lost connection would
	// re-subscribe, and the subscribe reply's status would fill the cache
	// while this scenario still named the reconcile.
	if n := idx.count("blockchain.scripthash.subscribe", sh); n != subsBefore {
		t.Fatalf("the address was subscribed again (%d -> %d), so the cache may have been filled by a re-subscribe rather than by the reconcile", subsBefore, n)
	}
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("scan after the reconcile: %v %+v", err, got)
	}
}

// 12. The connection is lost while a deposit is mined. The refresher
// reconnects and re-subscribes; the subscribe reply carries a new status, so
// the address is refreshed and the deposit mined during the gap is credited
// with no notification ever having been sent.
//
// Only the deposit block is mined during the outage. Burying it there too
// would hold the indexer down for sixty-odd blocks, and the wait after
// service is restored is set by the refresher's backoff, which doubles.
// Reconnect attempts fall at 0, 1, 3, 7, 15, 31 and 63 seconds after the
// loss, and the pass that re-subscribes runs one further step after the
// attempt that succeeds: so a sub-second outage is answered in about three
// seconds, and the 30-second bound below holds until the outage itself runs
// past about fifteen. The maturity blocks pay the hot wallet and change
// nothing the deposit address is subscribed to, so mining them after the
// reconnect tests the same thing.
func TestReconnectResubscribesAndPicksUpTheGap(t *testing.T) {
	f := setup(t)
	idx, elx := f.indexer(t, time.Hour, f.dep)
	led := newLedger()
	m := f.monitorOver(t, elx, led, 30, f.dep)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("initial scan: %v %+v", err, got)
	}
	sh := idx.scripthash(f.dep)
	subsBefore := idx.count("blockchain.scripthash.subscribe", sh)

	// The indexer goes down: connections are refused until it is back, so the
	// deposit is mined while no client is connected and no notification can
	// reach one.
	idx.setPaused(true)
	idx.closeClients()
	waitUntil(t, "no client connected", func() bool { return idx.connCount() == 0 })

	f.n.mine(f.dep, 1)
	idx.advance() // the indexer's own scanner reads the block; there is nobody to tell
	if n := idx.count("blockchain.scripthash.subscribe", sh); n != subsBefore {
		t.Fatalf("a subscription was made while the indexer was down: %d -> %d", subsBefore, n)
	}
	if sent, _ := idx.notifications(); sent != 0 {
		t.Fatalf("%d notifications went out while no client was connected", sent)
	}

	idx.setPaused(false)
	// A wait on the backoff schedule above, not on a round trip: one step to
	// the dial that succeeds and one more to the pass that subscribes.
	waitUpTo(t, 30*time.Second, "the re-subscription", func() bool {
		return idx.count("blockchain.scripthash.subscribe", sh) == subsBefore+1
	})
	waitUpTo(t, 30*time.Second, "the deposit mined during the gap to reach the cache", func() bool {
		return len(elx.GetUTXOs(f.dep)) == 2
	})
	if sent, _ := idx.notifications(); sent != 0 {
		t.Errorf("the gap deposit was pushed (%d notifications); the subscribe reply's status is what this scenario tests", sent)
	}

	// Bury it: the maturity blocks pay the hot wallet, which the deposit
	// address's subscription says nothing about.
	f.n.mine(f.hot, int(types.Regtest.CoinbaseMaturity)+5)
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("scan after the reconnect: %v %+v", err, got)
	}
}

// 13. The indexer fails one address and answers the other. The failed address
// is skipped and alarmed as stale; the other's deposit is credited; the pass
// does not pause.
func TestOneFailingAddressIsSkippedWhileTheRestAreCredited(t *testing.T) {
	f := setup(t)
	_, dep2 := newKey(t)
	f.scan = newScanner(f.n, f.hot, f.dep, dep2)
	if err := f.scan.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx := newFakeIndexer(t, f.scan, f.dep, dep2)
	idx.setFail(dep2, true)
	elx, cancel := startClientOn(t, idx, time.Hour, f.dep, dep2)
	defer cancel()
	waitUntil(t, "the first address's refresh", func() bool { at, _ := elx.LastRefreshOf(f.dep); return !at.IsZero() })
	waitUntil(t, "the second address's failure", func() bool { _, err := elx.LastRefreshOf(dep2); return err != nil })

	led := newLedger()
	m := f.monitorOver(t, elx, led, 30, f.dep, dep2)
	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan with one failing address paused: %v", err)
	}
	if len(got) != 1 || got[0].Address != f.dep {
		t.Fatalf("credited %+v, want the deposit at the answered address only", got)
	}
	stale := false
	for _, k := range f.alerts {
		if k == deposit.AlertCacheStale {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("the failed address was not alarmed as stale; alerts %v", f.alertMsgs)
	}
}
