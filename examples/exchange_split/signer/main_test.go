package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
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

	if err := reconcile(context.Background(), s, spent, time.Hour); err != nil {
		t.Fatal(err)
	}

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

	if err := reconcile(context.Background(), s, spent, time.Hour); err != nil {
		t.Fatal(err)
	}

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

	if err := reconcile(context.Background(), s, spent, time.Hour); err != nil {
		t.Fatal(err)
	}
	first := spentAtOf(t, path, txA)

	if err := reconcile(context.Background(), s, spent, time.Hour); err != nil {
		t.Fatal(err)
	}
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

	if err := reconcile(context.Background(), s, spent, time.Hour); err != nil {
		t.Fatal(err)
	}

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

// promotingStore is a store whose records move while it is being read, which is
// what the shared directory does: the broadcaster writes it while the signer
// reads it. promote runs once, before the nth List call, so a test can put a
// state transition exactly inside the window between two listings.
type promotingStore struct {
	withdraw.Store
	calls   int
	before  int
	promote func()
}

func (s *promotingStore) List(ctx context.Context, states ...withdraw.State) ([]*withdraw.Intent, error) {
	s.calls++
	if s.calls == s.before && s.promote != nil {
		s.promote()
		s.promote = nil
	}
	return s.Store.List(ctx, states...)
}

// The window between the two listings. reconcile reads Built and then the sent
// states, so a withdrawal the broadcaster promotes in between appears in both
// and its inputs are recorded as a spend. Read in the other order it appeared
// in neither, and its inputs were left unheld for the rest of the pass: a
// signer whose reservation had expired could then select the input of a
// transaction the broadcaster was sending.
func TestReconcileHoldsTheInputsOfAWithdrawalPromotedBetweenItsListings(t *testing.T) {
	s := store(t)
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent_set.json"), nil)
	put(t, s, "w1", withdraw.StateBuilt, "txid-1", outpoint(txA, 0))

	// Promote w1 before the second listing, which is the sent one.
	moving := &promotingStore{Store: s, before: 2, promote: func() {
		put(t, s, "w1", withdraw.StateBroadcast, "txid-1", outpoint(txA, 0))
	}}
	if err := reconcile(context.Background(), moving, spent, time.Hour); err != nil {
		t.Fatal(err)
	}

	if !spent.IsSpent(txA, 0) {
		t.Fatal("the input is held by nothing after the promotion: the next selection would offer it")
	}
	if got := spent.ReservedIntents(); len(got) != 0 {
		t.Fatalf("still a reservation: %v; the withdrawal is sent, and a reservation expires", got)
	}
}

// A reconciliation that could not finish must stop the pass. It used to log
// that nothing would be selected and then select anyway.
func TestReconcileReportsAStoreItCouldNotRead(t *testing.T) {
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent_set.json"), nil)
	if err := reconcile(context.Background(), failingStore{}, spent, time.Hour); err == nil {
		t.Fatal("reconcile reported success on a store it could not read")
	}
}

type failingStore struct{ withdraw.Store }

func (failingStore) List(context.Context, ...withdraw.State) ([]*withdraw.Intent, error) {
	return nil, errors.New("injected store failure")
}

// The fee target. A wallet holding one output must be able to pay nearly all of
// it: the fee of a one-input payment is about 1,073 vB, so charging every
// payout for utxo.MaxInputsPerTX made the last 0.78 SOQ of any hot wallet
// unspendable at the recommended rate. This is the case that refused the
// verification transaction of PR 57 before the fix.
func TestSelectForFeeChargesTheInputsItTakes(t *testing.T) {
	snap := split.Snapshot{
		Tip: 1000, HotAddress: hot,
		Outputs: []split.Output{{TxID: txA, Vout: 0, Value: 2_500_000_000, Height: 900, Address: hot}},
	}
	selector := utxo.NewCoinSelector(utxo.NewSpentSet(filepath.Join(t.TempDir(), "s.json"), nil))

	selected, err := selectForFee(selector, snap, 2_497_900_000, types.RecommendedFeeRate, 1)
	if err != nil {
		t.Fatalf("a payment of 24.979 from an output of 25 was refused: %v", err)
	}
	if len(selected) != 1 || selected[0].TxID != txA {
		t.Fatalf("selected %+v, want the one output", selected)
	}
	// The budget still has to cover the fee of what it took.
	if got := selected[0].Value - 2_497_900_000; got < vsizeFor(1)*types.RecommendedFeeRate {
		t.Fatalf("headroom over the payment is %d shors, under the %d the fee needs", got, vsizeFor(1)*types.RecommendedFeeRate)
	}
}

// With several inputs the target has to grow with them, or the selection comes
// back short of its own fee.
func TestSelectForFeeGrowsWithTheInputCount(t *testing.T) {
	snap := split.Snapshot{Tip: 1000, HotAddress: hot}
	for i := range 3 {
		snap.Outputs = append(snap.Outputs, split.Output{
			TxID: txA[:62] + fmt.Sprintf("%02d", i), Vout: uint32(i),
			Value: 1_200_000_000, Height: 900, Address: hot,
		})
	}
	selector := utxo.NewCoinSelector(utxo.NewSpentSet(filepath.Join(t.TempDir(), "s.json"), nil))

	selected, err := selectForFee(selector, snap, 3_000_000_000, types.RecommendedFeeRate, 1)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, u := range selected {
		total += u.Value
	}
	need := 3_000_000_000 + vsizeFor(len(selected))*types.RecommendedFeeRate
	if total < need {
		t.Fatalf("%d inputs totalling %d, under the %d the payment and its own fee need", len(selected), total, need)
	}
}

// The other half of the fee target. Inputs that cover a one-input fee but not
// the fee of the number it takes must be refused, because the transaction
// built from them would pay less than its own weight and the node would refuse
// it after the signing. Accepting the first selection whatever its size is the
// defect this pins.
func TestSelectForFeeRefusesInputsThatCannotPayTheirOwnFee(t *testing.T) {
	const amount = 3_000_000_000
	snap := split.Snapshot{Tip: 1000, HotAddress: hot}
	// Three outputs that together clear the one-input target by 100,000 shors
	// and fall 1,800,000 short of the three-input one.
	for i := range 3 {
		snap.Outputs = append(snap.Outputs, split.Output{
			TxID: txA[:62] + fmt.Sprintf("%02d", i), Vout: uint32(i),
			Value: 1_000_400_000, Height: 900, Address: hot,
		})
	}
	selector := utxo.NewCoinSelector(utxo.NewSpentSet(filepath.Join(t.TempDir(), "s.json"), nil))

	one := amount + vsizeFor(1)*types.RecommendedFeeRate
	three := amount + vsizeFor(3)*types.RecommendedFeeRate
	if total := int64(3 * 1_000_400_000); total < one || total >= three {
		t.Fatalf("the fixture no longer sits between the two targets: total %d, one %d, three %d", total, one, three)
	}
	if selected, err := selectForFee(selector, snap, amount, types.RecommendedFeeRate, 1); err == nil {
		t.Fatalf("selected %d inputs that cannot pay the fee of %d inputs", len(selected), len(selected))
	}
}

// vsizeFor is a fee target, so it must never fall under the vsize of the
// transaction the selection it sizes will actually produce. Every other fee
// test in this file measures a selection against vsizeFor itself, so the
// budget was only ever compared with the budget. That is how a constant 26 vB
// per input under the measured figure survived: it makes selectForFee accept a
// selection whose total cannot pay the transaction's real fee, BuildSend then
// computes a negative change and returns ErrInsufficientFunds, and the engine
// reads that as permanent and fails the withdrawal for good.
func TestVsizeForNeverUndersizesTheTransactionItBudgets(t *testing.T) {
	recipient := tx.ScriptP2WPKH(make([]byte, 32))
	change := tx.ScriptP2WPKH(bytes.Repeat([]byte{1}, 32))
	for _, n := range []int{1, 2, 3, 5, 10, 40, utxo.MaxInputsPerTX} {
		inputs := make([]types.UTXO, n)
		for i := range inputs {
			inputs[i] = types.UTXO{
				TxID:   fmt.Sprintf("%060d%04d", 0, i),
				Vout:   uint32(i),
				Value:  10_000_000_000,
				Height: 900, Address: "sq1pa3n373z2lgva3m53nssuwm7jl0dz697uzul7wh55ct7maf00xe4s2m80fs",
			}
		}
		tr, err := tx.BuildSendTransaction(inputs, recipient, 1_000_000_000, change, types.RecommendedFeeRate)
		if err != nil {
			t.Fatalf("n=%d: build: %v", n, err)
		}
		if budget, real := vsizeFor(n), tr.VSize(); budget < real {
			t.Errorf("n=%d: fee target %d vB, transaction %d vB, short by %d", n, budget, real, real-budget)
		}
	}
}
