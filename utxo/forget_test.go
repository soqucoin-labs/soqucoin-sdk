package utxo

import (
	"errors"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// An input another withdrawal has recorded as spent, in a transaction the
// chain has not confirmed, refuses the whole mark: writing over it would
// attribute the input to the transaction that cannot confirm, and the set
// would then say the wrong payment is in flight.
func TestMarkBroadcastRefusesAnotherWithdrawalsUnconfirmedEntry(t *testing.T) {
	ss := NewSpentSet("", nil)
	a, b := rUTXOs()[0], rUTXOs()[1]
	c := types.UTXO{TxID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Vout: 0}
	if err := ss.MarkBroadcastFor([]types.UTXO{a, b}, "tx1", "w1"); err != nil {
		t.Fatal(err)
	}
	err := ss.MarkBroadcastFor([]types.UTXO{c, a}, "tx2", "w2")
	if !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("mark over another withdrawal's entry: %v, want ErrAlreadyReserved", err)
	}
	if e := ss.entries[SpentKey{a.TxID, a.Vout}]; e.SpentInTx != "tx1" || e.IntentID != "w1" {
		t.Fatalf("w1's entry was replaced: %+v", e)
	}
	if ss.IsSpent(c.TxID, c.Vout) {
		t.Fatal("a refused mark wrote the other input: all-or-nothing")
	}
	// The mark without an intent id owns nothing either.
	if err := ss.MarkBroadcast([]types.UTXO{a}, "tx3"); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("mark with no intent over w1's entry: %v", err)
	}
	// The owner may re-mark (Recover does), and so may the same transaction.
	if err := ss.MarkBroadcastFor([]types.UTXO{a, b}, "tx1", "w1"); err != nil {
		t.Fatalf("owner re-mark refused: %v", err)
	}
	if err := ss.MarkBroadcast([]types.UTXO{a}, "tx1"); err != nil {
		t.Fatalf("same transaction re-marked without an id refused: %v", err)
	}
	// A confirmed entry is history and is replaced.
	if err := ss.ConfirmSpentAll([]types.UTXO{b}); err != nil {
		t.Fatal(err)
	}
	if err := ss.MarkBroadcastFor([]types.UTXO{b}, "tx4", "w4"); err != nil {
		t.Fatalf("mark over a confirmed entry refused: %v", err)
	}
	// A reservation is not a send and is replaced as before.
	if err := ss.Reserve([]types.UTXO{c}, "w5", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := ss.MarkBroadcastFor([]types.UTXO{c}, "tx5", "w5"); err != nil {
		t.Fatalf("mark over the owner's reservation refused: %v", err)
	}
}

// Forget drops exactly the entries one withdrawal owns, of either kind, and
// refuses an empty id, which would drop every entry written without one.
func TestForgetDropsOnlyTheWithdrawalsOwnEntries(t *testing.T) {
	dir := t.TempDir()
	ss, err := OpenSpentSet(dir+"/spent.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := rUTXOs()[0], rUTXOs()[1]
	c := types.UTXO{TxID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Vout: 0}
	d := types.UTXO{TxID: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Vout: 0}
	if err := ss.MarkBroadcastFor([]types.UTXO{a}, "tx1", "w1"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Reserve([]types.UTXO{b}, "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := ss.MarkBroadcastFor([]types.UTXO{c}, "tx2", "w2"); err != nil {
		t.Fatal(err)
	}
	if err := ss.MarkBroadcast([]types.UTXO{d}, "tx3"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Forget(""); err == nil {
		t.Fatal("Forget with no id accepted")
	}
	if err := ss.Forget("w1"); err != nil {
		t.Fatal(err)
	}
	if ss.IsSpent(a.TxID, a.Vout) || ss.IsSpent(b.TxID, b.Vout) {
		t.Fatal("w1's entries survive Forget")
	}
	if !ss.IsSpent(c.TxID, c.Vout) || !ss.IsSpent(d.TxID, d.Vout) {
		t.Fatal("Forget dropped an entry w1 does not own")
	}
	reopened, err := OpenSpentSet(dir+"/spent.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.IsSpent(a.TxID, a.Vout) || !reopened.IsSpent(c.TxID, c.Vout) {
		t.Fatal("Forget was not persisted")
	}
	if err := ss.Forget("nobody"); err != nil {
		t.Fatalf("Forget of an id with no entries: %v", err)
	}
}
