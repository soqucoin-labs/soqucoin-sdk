package withdraw

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// The attack this pins. A Built intent's record reached the store and only its
// durability is unconfirmed. If Build treated that as "not saved" and released
// the inputs, the store would hold a signed transaction whose inputs the next
// withdrawal selects: the broadcaster reads the store, so both transactions
// would go out and one would be rejected as a double-spend of its own inputs,
// permanently failing a withdrawal a user asked for.
//
// So the intent stays Built with its reservation, and the error is returned.
func TestBuildKeepsTheReservationWhenTheSaveLandedButIsNotKnownDurable(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	spent := utxo.NewSpentSet(filepath.Join(dir, "spent_set.json"), nil)
	e := newEngine(t, store, spent, &fakeNet{mode: "ok"}, coins())

	w1, _, err := e.Submit(context.Background(), "w1", dst, 2_000_000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	restore := failAfterTheRename(t)
	err = e.Build(context.Background(), w1)
	if !errors.Is(err, atomicfile.ErrWrittenNotDurable) {
		t.Fatalf("Build returned %v, want an error wrapping ErrWrittenNotDurable", err)
	}
	if w1.State != StateBuilt || w1.TxID == "" || len(w1.Inputs) == 0 {
		t.Fatalf("the intent is %s with txid %q and %d inputs; the record is in the store, so it is Built", w1.State, w1.TxID, len(w1.Inputs))
	}
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("the built intent's input is not reserved: the next withdrawal can select the input of a signed transaction")
	}
	got, ok, _ := store.Get(context.Background(), "w1")
	if !ok || got.State != StateBuilt {
		t.Fatalf("the store reports %+v; the write landed before the failure, so it holds the Built record", got)
	}

	// The second withdrawal must not be able to take those inputs, which is
	// the consequence the release would have had. The write step is healthy
	// again from here, so only the reservation decides.
	restore()
	w2, _, err := e.Submit(context.Background(), "w2", dst, 2_000_000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Build(context.Background(), w2); err == nil {
		if w2.Inputs[0].TxID == w1.Inputs[0].TxID && w2.Inputs[0].Vout == w1.Inputs[0].Vout {
			t.Fatalf("the second withdrawal took %s:%d, the input of the first one's signed transaction", w2.Inputs[0].TxID, w2.Inputs[0].Vout)
		}
	}
}

// A save that failed before the rename is a different fact: the store does not
// have the record, so the inputs are released and the intent waits as Created
// for another Build. This is the behaviour the durability case must not take
// over.
func TestBuildReleasesWhenTheSaveDidNotLand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	spent := utxo.NewSpentSet(filepath.Join(dir, "spent_set.json"), nil)
	e := newEngine(t, store, spent, &fakeNet{mode: "ok"}, coins())
	w1, _, err := e.Submit(context.Background(), "w1", dst, 2_000_000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	fix := breakStoreFile(t, path)
	err = e.Build(context.Background(), w1)
	switch {
	case err == nil:
		t.Fatal("Build succeeded with an unwritable store")
	case errors.Is(err, atomicfile.ErrWrittenNotDurable):
		t.Fatalf("a write that never landed was reported as written: %v", err)
	}
	if w1.State != StateCreated || w1.TxID != "" || len(w1.Inputs) != 0 {
		t.Fatalf("the intent is %s with txid %q; nothing was saved, so it is Created with nothing built", w1.State, w1.TxID)
	}
	if spent.Size() != 0 {
		t.Fatalf("the spent set holds %d entries; an intent with nothing saved holds no inputs", spent.Size())
	}
	fix()
}
