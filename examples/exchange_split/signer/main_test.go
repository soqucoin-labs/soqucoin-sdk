package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

const (
	txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hot = "ssq1photwallet"
)

func store(t *testing.T) *split.DirStore {
	t.Helper()
	s, err := split.OpenDirStore(filepath.Join(t.TempDir(), "intents"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *split.DirStore, id string, state withdraw.State, txid string, inputs ...withdraw.Outpoint) {
	t.Helper()
	in := &withdraw.Intent{
		ID: id, Address: "ssq1pdestination", Amount: 1000,
		FeeRate: types.RecommendedFeeRate, State: state, TxID: txid,
		Inputs: inputs, CreatedAt: time.Now().UTC(),
	}
	if err := s.Put(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func outpoint(txid string, vout uint32) withdraw.Outpoint {
	return withdraw.Outpoint{TxID: txid, Vout: vout, Value: 5_000_000, Address: hot}
}

// The attack. A withdrawal is signed and waiting for the broadcaster, and this
// process no longer holds its reservation: the set was loaded after the TTL
// expired, or this signer is on a new host with an empty state directory. Its
// inputs are still unspent on chain, so they are in the watcher's snapshot.
// Without a pass over the store's Built intents, the next withdrawal selects
// them and two signed transactions stand over one input.
func TestReconcileReservesABuiltWithdrawalThisSetHasNoReservationFor(t *testing.T) {
	s := store(t)
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent_set.json"), nil)
	put(t, s, "w1", withdraw.StateBuilt, "txid-1", outpoint(txA, 0))

	reconcile(context.Background(), s, spent, time.Hour)

	if !spent.IsSpent(txA, 0) {
		t.Fatal("the built withdrawal's input is not held: the next selection would offer it to another withdrawal")
	}
	if got := spent.ReservedIntents(); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("reserved intents are %v, want the built withdrawal w1", got)
	}
}

// A withdrawal the broadcaster has sent must be recorded as spent here rather
// than left as a reservation, because a reservation expires and the
// transaction may sit in a mempool for as long as the fee market takes.
func TestReconcileTurnsASentWithdrawalsReservationIntoASpend(t *testing.T) {
	s := store(t)
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent_set.json"), nil)
	if err := spent.Reserve([]types.UTXO{{TxID: txA, Vout: 0, Value: 5_000_000, Address: hot}}, "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	put(t, s, "w1", withdraw.StateBroadcast, "txid-1", outpoint(txA, 0))

	reconcile(context.Background(), s, spent, time.Hour)

	if got := spent.ReservedIntents(); len(got) != 0 {
		t.Fatalf("still reserved: %v; a sent withdrawal's inputs are spent and do not expire", got)
	}
	if !spent.IsSpent(txA, 0) {
		t.Fatal("the sent withdrawal's input is not in the spent set at all")
	}
}

// Marking rewrites the whole file and fsyncs twice. A withdrawal already
// recorded as a spend here must be left alone, or the set is rewritten once
// per historic withdrawal on every pass, forever.
func TestReconcileDoesNotRewriteTheSetForWithdrawalsAlreadyRecorded(t *testing.T) {
	s := store(t)
	path := filepath.Join(t.TempDir(), "spent_set.json")
	spent := utxo.NewSpentSet(path, nil)
	put(t, s, "w1", withdraw.StateBroadcast, "txid-1", outpoint(txA, 0))
	put(t, s, "w2", withdraw.StateConfirmed, "txid-2", outpoint(txB, 0))

	reconcile(context.Background(), s, spent, time.Hour)
	first := spentAtOf(t, path, txA)

	reconcile(context.Background(), s, spent, time.Hour)
	if second := spentAtOf(t, path, txA); !second.Equal(first) {
		t.Fatalf("the entry was rewritten: %s then %s", first, second)
	}
	// The confirmed withdrawal's entry was never created, so nothing is
	// carried for it either.
	if spent.IsSpent(txB, 0) {
		t.Fatal("an input of a confirmed withdrawal was added to the set; it cannot come back and the snapshot is read from the node")
	}
}

// A reservation held for a withdrawal with nothing built is released: the
// previous run stopped between reserving and saving, so the coins are free.
// A reservation whose intent the store does not know is kept, because
// releasing one that belongs to a signed transaction costs a payment.
func TestReconcileReleasesOnlyReservationsWithNothingBuilt(t *testing.T) {
	s := store(t)
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent_set.json"), nil)
	coins := func(txid string) []types.UTXO {
		return []types.UTXO{{TxID: txid, Vout: 0, Value: 5_000_000, Address: hot}}
	}
	if err := spent.Reserve(coins(txA), "w-created", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := spent.Reserve(coins(txB), "w-unknown", time.Hour); err != nil {
		t.Fatal(err)
	}
	put(t, s, "w-created", withdraw.StateCreated, "")

	reconcile(context.Background(), s, spent, time.Hour)

	if spent.IsSpent(txA, 0) {
		t.Error("the reservation of a withdrawal with nothing built was kept")
	}
	if !spent.IsSpent(txB, 0) {
		t.Error("a reservation whose withdrawal the store does not know was released")
	}
}

// spentAtOf reads one entry's spent_at out of the set's own file, which is how
// a rewrite shows up: MarkBroadcastFor stamps it with the time of the pass.
func spentAtOf(t *testing.T, path, txid string) time.Time {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Entries []struct {
			TxID    string    `json:"txid"`
			SpentAt time.Time `json:"spent_at"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, e := range file.Entries {
		if e.TxID == txid {
			return e.SpentAt
		}
	}
	t.Fatalf("no entry for %s in %s", txid, path)
	return time.Time{}
}
