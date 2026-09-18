package utxo

import (
	"errors"
	"os"
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
	ss := NewSpentSet("", nil)
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
	ss := NewSpentSet("", nil)
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
	ss := NewSpentSet("", nil)
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
	ss := NewSpentSet(path, nil)
	ss.MarkBroadcast(rUTXOs(), "txid-broadcast")
	ss.mu.Lock()
	for k, e := range ss.entries {
		e.SpentAt = time.Now().Add(-48 * time.Hour)
		ss.entries[k] = e
	}
	ss.mu.Unlock()
	ss.persist()

	ss2 := NewSpentSet(path, nil)
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
	ss3 := NewSpentSet(path, nil)
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
	ss := NewSpentSet("", nil)
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

// unwritablePath returns a spent-set path whose parent is a regular file, so
// the directory cannot be created and no write can succeed.
func unwritablePath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "spent.json")
}

// A write that fails is reported. Reserve reserves nothing (a reservation
// the next process would not see is not a reservation); MarkBroadcast keeps
// the entries, because the transaction is already out.
func TestPersistFailureIsReportedNotSwallowed(t *testing.T) {
	ss := NewSpentSet(unwritablePath(t), nil)
	err := ss.Reserve(rUTXOs(), "w1", time.Hour)
	if !errors.Is(err, ErrPersist) {
		t.Fatalf("Reserve on an unwritable set: %v, want ErrPersist", err)
	}
	if ss.IsSpent(rTxA, 0) || ss.IsSpent(rTxB, 1) {
		t.Fatal("a reservation that could not be written was kept in memory")
	}
	err = ss.MarkBroadcast(rUTXOs(), "txid-broadcast")
	if !errors.Is(err, ErrPersist) {
		t.Fatalf("MarkBroadcast on an unwritable set: %v, want ErrPersist", err)
	}
	if !ss.IsSpent(rTxA, 0) || !ss.IsSpent(rTxB, 1) {
		t.Fatal("broadcast entries must stay in memory even when the write failed")
	}
	if err := ss.ConfirmSpent(rTxA, 0); !errors.Is(err, ErrPersist) {
		t.Fatalf("ConfirmSpent: %v, want ErrPersist", err)
	}
}

// A failed Reserve rolls back to exactly the previous state, including an
// own expired reservation it was renewing.
func TestReserveRollbackRestoresThePreviousEntry(t *testing.T) {
	ss := NewSpentSet("", nil)
	if err := ss.Reserve(rUTXOs()[:1], "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	ss.filePath = unwritablePath(t) // the disk goes away
	if err := ss.Reserve(rUTXOs()[:1], "w1", time.Hour); !errors.Is(err, ErrPersist) {
		t.Fatalf("renewal: %v", err)
	}
	if !ss.IsSpent(rTxA, 0) {
		t.Fatal("the earlier reservation was dropped by a failed renewal")
	}
	if ss.IsSpent(rTxB, 1) {
		t.Fatal("an input that was never reserved appeared")
	}
}

// A spent-set file that exists but cannot be parsed must refuse to open:
// starting empty would forget every unconfirmed spend.
func TestOpenSpentSetRefusesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spent.json")
	if _, err := OpenSpentSet(path, nil); err != nil {
		t.Fatalf("missing file is a first run: %v", err)
	}
	ss, _ := OpenSpentSet(path, nil)
	if err := ss.MarkBroadcast(rUTXOs(), "txid-broadcast"); err != nil {
		t.Fatal(err)
	}
	ss2, err := OpenSpentSet(path, nil)
	if err != nil || !ss2.IsSpent(rTxA, 0) {
		t.Fatalf("round trip: %v %v", err, ss2)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpentSet(path, nil); !errors.Is(err, ErrSpentSetUnreadable) {
		t.Fatalf("corrupt file opened: %v, want ErrSpentSetUnreadable", err)
	}
	if _, err := OpenSpentSet(unwritablePath(t), nil); !errors.Is(err, ErrSpentSetUnreadable) {
		t.Fatalf("uncreatable directory: %v, want ErrSpentSetUnreadable", err)
	}
	if _, err := OpenSpentSet("", nil); err == nil {
		t.Fatal("OpenSpentSet with no path must refuse; NewSpentSet is the in-memory form")
	}
	// The older constructor keeps its documented behaviour: it logs and starts empty.
	if legacy := NewSpentSet(path, nil); legacy.IsSpent(rTxA, 0) {
		t.Fatal("NewSpentSet on a corrupt file should have started empty")
	}
}

// The same outpoint listed twice must roll back to the state before the first
// write, not to the reservation the first pass created.
func TestReserveRollbackWithDuplicateOutpoint(t *testing.T) {
	ss := NewSpentSet(unwritablePath(t), nil)
	u := rUTXOs()[0]
	if err := ss.Reserve([]types.UTXO{u, u}, "w1", time.Hour); !errors.Is(err, ErrPersist) {
		t.Fatalf("got %v", err)
	}
	if ss.IsSpent(u.TxID, u.Vout) {
		t.Fatal("a phantom reservation survived the rollback")
	}
}

// Release, Prune and ConfirmSpentAll report a failed write too.
func TestReleasePruneAndConfirmReportPersistFailure(t *testing.T) {
	ss := NewSpentSet("", nil)
	if err := ss.Reserve(rUTXOs()[:1], "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := ss.MarkBroadcast(rUTXOs()[1:], "txid-broadcast"); err != nil {
		t.Fatal(err)
	}
	ss.filePath = unwritablePath(t)
	if err := ss.Release("w1"); !errors.Is(err, ErrPersist) {
		t.Errorf("Release: %v", err)
	}
	if err := ss.ConfirmSpentAll(rUTXOs()[1:]); !errors.Is(err, ErrPersist) {
		t.Errorf("ConfirmSpentAll: %v", err)
	}
	ss.entries[SpentKey{rTxB, 1}] = SpentEntry{TxID: rTxB, Vout: 1, Confirmed: true, SpentAt: time.Now().Add(-3 * time.Hour)}
	if err := ss.Prune(); !errors.Is(err, ErrPersist) {
		t.Errorf("Prune: %v", err)
	}
	// Nothing to change must not touch the disk: a set whose entries are all
	// confirmed already, on an unwritable path, reports no error.
	quiet := NewSpentSet("", nil)
	if err := quiet.MarkBroadcast(rUTXOs(), "txid-broadcast"); err != nil {
		t.Fatal(err)
	}
	if err := quiet.ConfirmSpentAll(rUTXOs()); err != nil {
		t.Fatal(err)
	}
	quiet.filePath = unwritablePath(t)
	if err := quiet.ConfirmSpentAll(rUTXOs()); err != nil {
		t.Errorf("no-op confirm wrote to disk: %v", err)
	}
	if err := quiet.ConfirmSpentAll([]types.UTXO{{TxID: "not-tracked", Vout: 0}}); err != nil {
		t.Errorf("confirming an untracked input wrote to disk: %v", err)
	}
}

// ReservedIntents names every intent holding a reservation, live or expired,
// once each; broadcast and confirmed entries are not reservations.
func TestReservedIntentsListsReservationsOnly(t *testing.T) {
	ss := NewSpentSet("", nil)
	if got := ss.ReservedIntents(); len(got) != 0 {
		t.Fatalf("empty set: %v", got)
	}
	if err := ss.Reserve(rUTXOs(), "w2", time.Hour); err != nil { // two inputs, one id
		t.Fatal(err)
	}
	expired := types.UTXO{TxID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Vout: 0}
	if err := ss.Reserve([]types.UTXO{expired}, "w1", time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	sent := types.UTXO{TxID: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Vout: 0}
	if err := ss.MarkBroadcastFor([]types.UTXO{sent}, "txid-d", "w3"); err != nil {
		t.Fatal(err)
	}
	got := ss.ReservedIntents()
	if len(got) != 2 || got[0] != "w1" || got[1] != "w2" {
		t.Fatalf("reserved intents %v, want [w1 w2]: expired w1 listed, w2 once, broadcast w3 absent", got)
	}
	if err := ss.MarkBroadcastFor(rUTXOs(), "txid-ab", "w2"); err != nil {
		t.Fatal(err)
	}
	if got := ss.ReservedIntents(); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("after w2 broadcast: %v, want [w1]", got)
	}
}

// A withdrawal's retry after a lost reply renews what it holds. When an
// earlier attempt succeeded and its record did not land, what it holds is the
// permanent entry of its own send. That entry is accepted, not refused as
// another withdrawal's, and it is left as it is: turned into a reservation it
// would expire, and the inputs of a transaction in a mempool would come back
// into selection.
func TestReserveLeavesTheCallersOwnBroadcastEntryPermanent(t *testing.T) {
	ss := NewSpentSet("", nil)
	if err := ss.Reserve(rUTXOs(), "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := ss.MarkBroadcastFor(rUTXOs(), "txid-sent", "w1"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Reserve(rUTXOs(), "w1", time.Millisecond); err != nil {
		t.Fatalf("a withdrawal's own sent inputs were refused to it: %v", err)
	}
	ss.mu.Lock()
	e := ss.entries[SpentKey{rTxA, 0}]
	ss.mu.Unlock()
	if e.reserved() || !e.ExpiresAt.IsZero() || e.SpentInTx != "txid-sent" {
		t.Fatalf("the record of the send became %+v; want it left as the permanent entry", e)
	}
	time.Sleep(5 * time.Millisecond)
	if !ss.IsSpent(rTxA, 0) || !ss.IsSpent(rTxB, 1) {
		t.Fatal("an input of a sent transaction expired out of the set")
	}
	if err := ss.Reserve(rUTXOs()[:1], "w2", time.Hour); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("another withdrawal took a sent input: %v", err)
	}
}
