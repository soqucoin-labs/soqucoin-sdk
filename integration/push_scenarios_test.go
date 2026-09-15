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
// scanner. Every deposit in the harness is a coinbase, so a new one is
// buried under coinbase maturity before it can be credited.

// mineDepositAndBury pays one coinbase to addr and buries it past maturity,
// advancing the indexer after each block as a real one would.
func (f *fixture) mineDepositAndBury(t *testing.T, idx *fakeIndexer, addr string) {
	t.Helper()
	f.n.mine(addr, 1)
	idx.advance()
	f.n.mine(f.hot, int(types.Regtest.CoinbaseMaturity)+5)
	idx.advance()
}

// 10. A deposit the server pushes is credited after the node check. The
// client learns of it from the notification, not from a poll: the reconcile
// interval is an hour, and the deposit address saw exactly one subscribe and
// one listunspent after its initial refresh.
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

// 11. The server never sends the notification. The deposit is not in the
// cache until the reconcile pass, which refreshes every address on its
// interval; then it is credited. The bound the guide states.
func TestDroppedNotificationIsCoveredByTheReconcile(t *testing.T) {
	f := setup(t)
	idx, elx := f.indexer(t, 2*time.Second, f.dep)
	led := newLedger()
	m := f.monitorOver(t, elx, led, 30, f.dep)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("initial scan: %v %+v", err, got)
	}
	idx.setDrop(true)
	f.mineDepositAndBury(t, idx, f.dep)
	time.Sleep(300 * time.Millisecond)
	if n := len(elx.GetUTXOs(f.dep)); n != 1 {
		t.Fatalf("cache shows %d outputs with the notification dropped and no reconcile yet, want 1", n)
	}
	waitUntil(t, "the reconcile to refresh the address", func() bool { return len(elx.GetUTXOs(f.dep)) == 2 })
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("scan after the reconcile: %v %+v", err, got)
	}
}

// 12. The connection is lost while a deposit is mined. The refresher
// reconnects and re-subscribes; the subscribe reply carries a new status, so
// the address is refreshed and the deposit mined during the gap is credited.
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

	// The indexer goes down: connections are refused until it is back, so
	// the deposit is mined while no client is connected and no notification
	// can reach it.
	idx.setPaused(true)
	idx.closeClients()
	waitUntil(t, "no client connected", func() bool { return idx.connCount() == 0 })
	f.mineDepositAndBury(t, idx, f.dep)
	if n := idx.count("blockchain.scripthash.subscribe", sh); n != subsBefore {
		t.Fatalf("a subscription was made while the indexer was down: %d -> %d", subsBefore, n)
	}
	idx.setPaused(false)
	waitUntil(t, "the re-subscription", func() bool { return idx.count("blockchain.scripthash.subscribe", sh) == subsBefore+1 })
	waitUntil(t, "the deposit mined during the gap to reach the cache", func() bool { return len(elx.GetUTXOs(f.dep)) == 2 })
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("scan after the reconnect: %v %+v", err, got)
	}
}

// 13. The indexer fails one address and answers the other. The failed
// address is skipped and alarmed as stale; the other's deposit is credited;
// the pass does not pause.
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
