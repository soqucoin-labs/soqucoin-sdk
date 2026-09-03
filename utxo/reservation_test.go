package utxo

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const (
	rTxA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rTxB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func rUTXOs() []types.UTXO {
	return []types.UTXO{{TxID: rTxA, Vout: 0, Value: 100}, {TxID: rTxB, Vout: 1, Value: 200}}
}

// Reservation is all-or-nothing and exclusive between intents.
func TestReserveIsExclusiveAndAtomic(t *testing.T) {
	ss := NewSpentSet("")
	if err := ss.Reserve(rUTXOs(), "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if !ss.IsSpent(rTxA, 0) || !ss.IsSpent(rTxB, 1) {
		t.Fatal("reserved inputs must read as spent to coin selection")
	}
	// Another intent wanting one reserved and one free input gets nothing.
	free := types.UTXO{TxID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Vout: 0}
	err := ss.Reserve([]types.UTXO{free, rUTXOs()[0]}, "w2", time.Hour)
	if !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("overlapping reservation accepted: %v", err)
	}
	if ss.IsSpent(free.TxID, 0) {
		t.Error("a refused reservation must reserve nothing")
	}
	// The same intent may re-reserve its own inputs (restart recovery).
	if err := ss.Reserve(rUTXOs(), "w1", time.Hour); err != nil {
		t.Errorf("re-reservation by the owner refused: %v", err)
	}
}

func TestReleaseDropsOnlyReservationsOfThatIntent(t *testing.T) {
	ss := NewSpentSet("")
	ss.Reserve(rUTXOs()[:1], "w1", time.Hour)
	ss.Reserve(rUTXOs()[1:], "w2", time.Hour)
	ss.Release("w1")
	if ss.IsSpent(rTxA, 0) {
		t.Error("w1's reservation not released")
	}
	if !ss.IsSpent(rTxB, 1) {
		t.Error("w2's reservation was released by w1")
	}
	// A broadcast entry is not a reservation and cannot be released.
	ss.MarkBroadcastFor(rUTXOs()[:1], "txid-broadcast", "w1")
	ss.Release("w1")
	if !ss.IsSpent(rTxA, 0) {
		t.Error("Release dropped a broadcast entry; spent inputs must stay spent until confirmed")
	}
}

func TestExpiredReservationIsFree(t *testing.T) {
	ss := NewSpentSet("")
	ss.Reserve(rUTXOs(), "w1", 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if ss.IsSpent(rTxA, 0) {
		t.Error("expired reservation still blocks selection")
	}
	if err := ss.Reserve(rUTXOs(), "w2", time.Hour); err != nil {
		t.Errorf("expired reservation blocked a new one: %v", err)
	}
}

// An unconfirmed broadcast must survive a restart regardless of age. The old
// loader dropped entries older than two hours, so a slow confirmation plus a
// restart re-exposed inputs that were still spent in the mempool.
func TestUnconfirmedBroadcastSurvivesReloadRegardlessOfAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spent.json")
	ss := NewSpentSet(path)
	ss.MarkBroadcast(rUTXOs(), "txid-broadcast")
	ss.mu.Lock()
	for k, e := range ss.entries {
		e.SpentAt = time.Now().Add(-48 * time.Hour)
		ss.entries[k] = e
	}
	ss.mu.Unlock()
	ss.persist()

	ss2 := NewSpentSet(path)
	if !ss2.IsSpent(rTxA, 0) || !ss2.IsSpent(rTxB, 1) {
		t.Fatal("two-day-old unconfirmed broadcast entries were dropped on reload")
	}
	// Confirmed entries older than two hours are dropped; expired reservations too.
	ss2.ConfirmSpent(rTxA, 0)
	ss2.Reserve([]types.UTXO{{TxID: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Vout: 0}}, "w9", time.Millisecond)
	ss2.mu.Lock()
	e := ss2.entries[SpentKey{rTxA, 0}]
	e.SpentAt = time.Now().Add(-3 * time.Hour)
	ss2.entries[SpentKey{rTxA, 0}] = e
	ss2.mu.Unlock()
	ss2.persist()
	time.Sleep(5 * time.Millisecond)
	ss3 := NewSpentSet(path)
	if ss3.IsSpent(rTxA, 0) {
		t.Error("old confirmed entry kept")
	}
	if !ss3.IsSpent(rTxB, 1) {
		t.Error("unconfirmed entry dropped")
	}
	if ss3.IsSpent("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", 0) {
		t.Error("expired reservation kept")
	}
}

func TestSelectorSkipsReservedInputs(t *testing.T) {
	ss := NewSpentSet("")
	cs := &CoinSelector{SpentSet: ss}
	coins := []types.UTXO{
		{TxID: rTxA, Vout: 0, Value: 500, Height: 1, Address: "x"},
		{TxID: rTxB, Vout: 1, Value: 400, Height: 1, Address: "x"},
	}
	ss.Reserve(coins[:1], "w1", time.Hour)
	sel, _, err := cs.SelectUTXOs(coins, 300, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 1 || sel[0].TxID != rTxB {
		t.Fatalf("selector picked %+v; the reserved input must be skipped", sel)
	}
}
