package deposit

import (
	"context"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// countingLedger counts the questions the Monitor asks the book.
type countingLedger struct {
	*fakeLedger
	isCredited int
}

func (l *countingLedger) IsCredited(ctx context.Context, txid string, vout uint32) (bool, error) {
	l.isCredited++
	return l.fakeLedger.IsCredited(ctx, txid, vout)
}

func (m *Monitor) finalCount() int {
	m.finalMu.Lock()
	defer m.finalMu.Unlock()
	return len(m.final)
}

// The attack first: nothing the indexer says puts an outpoint in the final
// set. An output the indexer reports at a depth past the horizon that the
// ledger has never credited is asked about on every scan, and the set stays
// empty, whether the node refuses it or has never heard of it.
func TestFinalSetTakesNothingFromTheIndexer(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cl := &countingLedger{fakeLedger: led}
	m.Ledger = cl
	// Deep by the indexer's word; the node does not have it.
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 2*types.MaxReorgDepth, Address: a}}
	for i := 1; i <= 3; i++ {
		if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 {
			t.Fatalf("scan %d: %v %+v, want nothing credited", i, err, got)
		}
		if cl.isCredited != i {
			t.Fatalf("scan %d: the ledger was asked %d times, want once per scan", i, cl.isCredited)
		}
	}
	if m.finalCount() != 0 {
		t.Fatal("an outpoint the ledger never credited entered the final set")
	}
	if !al.has(AlertIndexerMismatch) {
		t.Fatal("an output the node does not have was not alarmed")
	}
}

// The ledger is asked about a credited outpoint on every scan while it is
// pending, and not at all once the ledger has called it final, whether the
// Monitor marked it final itself or found it credited and absent from
// Pending. A new Monitor starts with an empty set and asks again.
func TestFinalSetSkipsTheLedgerOnlyForWhatTheLedgerCalledFinal(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	cl := &countingLedger{fakeLedger: led}
	m.Ledger = cl
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	node.outs[key("aa", 0)] = txout(t, a, 1000, 41, false)

	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("first scan: %v %+v", err, got)
	}
	if cl.isCredited != 1 {
		t.Fatalf("first scan asked the ledger %d times", cl.isCredited)
	}
	// Pending: asked again on every scan, the set stays empty.
	for i := 2; i <= 3; i++ {
		if got, _ := m.Scan(context.Background()); len(got) != 0 {
			t.Fatalf("scan %d credited again: %+v", i, got)
		}
		if cl.isCredited != i {
			t.Fatalf("scan %d: asked %d times while pending, want once per scan", i, cl.isCredited)
		}
	}
	if m.finalCount() != 0 {
		t.Fatal("a pending outpoint entered the final set")
	}

	// The node takes it past the horizon: recheckPending marks it final and
	// the set takes it. From here the ledger is not asked.
	node.outs[key("aa", 0)] = txout(t, a, 1000, types.MaxReorgDepth+1, false)
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !led.final[key("aa", 0)] {
		t.Fatal("the outpoint was not marked final in the ledger")
	}
	if m.finalCount() != 1 {
		t.Fatalf("final set holds %d, want the one final outpoint", m.finalCount())
	}
	asked := cl.isCredited
	for i := 0; i < 3; i++ {
		if _, err := m.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if cl.isCredited != asked {
		t.Fatalf("the ledger was asked %d more times about a final outpoint", cl.isCredited-asked)
	}

	// A new Monitor over the same ledger: the set is empty, the ledger says
	// credited and Pending does not list it, so it is final on the first
	// answer and not asked again.
	m2 := &Monitor{Cache: cache, Node: node, Ledger: cl, Addresses: m.Addresses, Required: m.Required, OnAlert: m.OnAlert, now: m.now}
	asked = cl.isCredited
	if _, err := m2.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cl.isCredited != asked+1 || m2.finalCount() != 1 {
		t.Fatalf("new monitor: asked %d times, set holds %d; want one question and one entry", cl.isCredited-asked, m2.finalCount())
	}
	if _, err := m2.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cl.isCredited != asked+1 {
		t.Fatal("the new monitor asked again about an outpoint it had found final")
	}
}

// A credit the operator reverses after a vanish alarm is credited again when
// the output is back, exactly as before: a pending outpoint is never in the
// set. A final credit is not re-asked, by design: the node's horizon makes
// it impossible to reorganise away.
func TestReversedPendingCreditIsCreditedAgain(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	node.outs[key("aa", 0)] = txout(t, a, 1000, 41, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("first credit: %v %+v", err, got)
	}
	// The operator reverses the credit in the book after a vanish alarm.
	delete(led.credited, key("aa", 0))
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("re-credit of a reversed pending deposit: %v %+v", err, got)
	}
}

// Entries of a scanned address whose outpoints the cache no longer lists are
// pruned: a swept deposit does not stay in memory for the life of the
// process. An address skipped as stale keeps its entries.
func TestFinalSetPrunesWhatTheCacheNoLongerLists(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	node.outs[key("aa", 0)] = txout(t, a, 1000, types.MaxReorgDepth+1, false)
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Scan(context.Background()); err != nil { // marks final
		t.Fatal(err)
	}
	if !led.final[key("aa", 0)] || m.finalCount() != 1 {
		t.Fatalf("setup: final=%v set=%d", led.final[key("aa", 0)], m.finalCount())
	}

	// The address is stale: nothing learned, the entry stays.
	per := &fakeCachePerAddr{fakeCache: *cache, ats: map[string]time.Time{}, errs: map[string]error{}}
	m.Cache = per
	if _, err := m.Scan(context.Background()); err == nil {
		t.Fatal("a scan with every address stale should pause")
	}
	if m.finalCount() != 1 {
		t.Fatal("a stale address lost its final entries")
	}

	// Fresh again, and the output is gone from the cache: swept.
	per.ats[a] = m.now()
	per.utxos[a] = nil
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.finalCount() != 0 {
		t.Fatal("a swept output stayed in the final set")
	}
}
