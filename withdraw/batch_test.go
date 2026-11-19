// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package withdraw

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// payeeAddr is a witness version 1 address on hrp whose program is fill.
func payeeAddr(t *testing.T, hrp string, fill byte) string {
	t.Helper()
	return payeeAddrN(t, hrp, int(fill))
}

// payeeAddrN is a distinct address for every n.
func payeeAddrN(t *testing.T, hrp string, n int) string {
	t.Helper()
	prog := make([]byte, 32)
	prog[0], prog[1], prog[2] = byte(n), byte(n>>8), 0x5a
	a, err := address.Encode(hrp, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func payees(t *testing.T, n int, amount int64) []Payee {
	t.Helper()
	out := make([]Payee, n)
	for i := range out {
		out[i] = Payee{Address: payeeAddrN(t, types.Stagenet.HRP, i+1), Amount: amount + int64(i)}
	}
	return out
}

// batchRecorder is a BatchBuildSigner that records what it was asked to pay
// and names the transaction after its inputs, as newEngine's signer does.
type batchRecorder struct {
	mu     sync.Mutex
	builds int
	got    [][]Payee
}

func (b *batchRecorder) sign(_ context.Context, inputs []types.UTXO, ps []Payee, _ int64) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.builds++
	b.got = append(b.got, ps)
	id := ""
	for _, u := range inputs {
		id += u.TxID[:4] + fmt.Sprint(u.Vout)
	}
	return "batchhex-" + id, "batchtxid-" + id, nil
}

func bigCoins() []types.UTXO {
	return []types.UTXO{
		{TxID: txA, Vout: 0, Value: 500_000_000_000, Height: 10, Address: "ssq1pa"},
		{TxID: txB, Vout: 0, Value: 300_000_000_000, Height: 10, Address: "ssq1pb"},
	}
}

func newBatchEngine(t *testing.T, store Store, spent *utxo.SpentSet, net *fakeNet) (*Engine, *batchRecorder) {
	t.Helper()
	e := newEngine(t, store, spent, net, bigCoins())
	rec := &batchRecorder{}
	e.BuildSignBatch = rec.sign
	return e, rec
}

// What a batch submission must never record: a payee the caller did not
// mean, an amount the node refuses, a batch the engine cannot build. Each is
// refused whole and the store holds nothing afterwards.
func TestSubmitBatchRefusesAndRecordsNothing(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	e, _ := newBatchEngine(t, store, utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"})
	good := payees(t, 3, 1_000_000_000)

	with := func(edit func(ps []Payee) []Payee) []Payee {
		return edit(append([]Payee(nil), good...))
	}
	floorScript, _ := address.ScriptFor(good[0].Address)
	floor := tx.MinOutputValue(floorScript)

	cases := []struct {
		name   string
		id     string
		payees []Payee
		rate   int64
	}{
		{"empty id", "", good, 1000},
		{"zero fee rate", "b", good, 0},
		{"no payee", "b", nil, 1000},
		{"one more payee than the cap", "b", payees(t, MaxBatchPayees+1, 1_000_000_000), 1000},
		{"an address on another network", "b", with(func(ps []Payee) []Payee {
			ps[1].Address = payeeAddr(t, types.Mainnet.HRP, 9)
			return ps
		}), 1000},
		{"an empty address", "b", with(func(ps []Payee) []Payee { ps[2].Address = ""; return ps }), 1000},
		{"an address named twice", "b", with(func(ps []Payee) []Payee { ps[2].Address = ps[0].Address; return ps }), 1000},
		{"an address named twice in the other case", "b", with(func(ps []Payee) []Payee {
			ps[2].Address = strings.ToUpper(ps[0].Address)
			return ps
		}), 1000},
		{"an amount of zero", "b", with(func(ps []Payee) []Payee { ps[0].Amount = 0; return ps }), 1000},
		{"a negative amount", "b", with(func(ps []Payee) []Payee { ps[0].Amount = -5; return ps }), 1000},
		{"an amount one shor under the relay floor", "b", with(func(ps []Payee) []Payee { ps[1].Amount = floor - 1; return ps }), 1000},
		{"an amount above MaxMoney", "b", with(func(ps []Payee) []Payee { ps[1].Amount = types.MaxMoney + 1; return ps }), 1000},
		{"a total above MaxMoney", "b", with(func(ps []Payee) []Payee {
			ps[0].Amount, ps[1].Amount = types.MaxMoney, types.MaxMoney
			return ps
		}), 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, created, err := e.SubmitBatch(ctx, c.id, c.payees, c.rate)
			if !errors.Is(err, ErrInvalidIntent) || created || in != nil {
				t.Errorf("err = %v, created = %v, intent = %v; want ErrInvalidIntent and nothing", err, created, in)
			}
		})
	}

	// The cap itself is accepted, so the cap is the bound.
	if _, _, err := e.SubmitBatch(ctx, "at-cap", payees(t, MaxBatchPayees, 1_000_000_000), 1000); err != nil {
		t.Errorf("a batch of MaxBatchPayees was refused: %v", err)
	}
	// The amount exactly at the floor is accepted, so the floor is the bound.
	atFloor := with(func(ps []Payee) []Payee { ps[1].Amount = floor; return ps })
	if _, _, err := e.SubmitBatch(ctx, "at-floor", atFloor, 1000); err != nil {
		t.Errorf("an amount at the relay floor was refused: %v", err)
	}

	noBatch := newEngine(t, store, utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, bigCoins())
	if _, _, err := noBatch.SubmitBatch(ctx, "b", good, 1000); !errors.Is(err, ErrInvalidIntent) {
		t.Errorf("an engine with no BuildSignBatch accepted a batch: %v", err)
	}

	all, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("the store holds %d intents after the refusals, want only the two accepted", len(all))
	}
}

// One id is one batch. The same batch again is the same intent; anything
// else under that id is a conflict, and so is the id of a single withdrawal.
func TestSubmitBatchIsIdempotentAndConflictSafe(t *testing.T) {
	ctx := context.Background()
	e, _ := newBatchEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"})
	ps := payees(t, 4, 1_000_000_000)

	a, created, err := e.SubmitBatch(ctx, "run-1", ps, 1000)
	if err != nil || !created {
		t.Fatalf("first submit: %v created=%v", err, created)
	}
	if a.Address != "" || a.Amount != 4_000_000_006 || len(a.Payees) != 4 {
		t.Fatalf("recorded address %q amount %d payees %d", a.Address, a.Amount, len(a.Payees))
	}
	b, created, err := e.SubmitBatch(ctx, "run-1", ps, 1000)
	if err != nil || created || b.ID != "run-1" {
		t.Fatalf("second submit of the same batch: %v created=%v", err, created)
	}
	upper := append([]Payee(nil), ps...)
	upper[0].Address = strings.ToUpper(upper[0].Address)
	if _, created, err := e.SubmitBatch(ctx, "run-1", upper, 1000); err != nil || created {
		t.Errorf("the same address in upper case read as another batch: %v created=%v", err, created)
	}

	swapped := append([]Payee(nil), ps...)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	onePaidMore := append([]Payee(nil), ps...)
	onePaidMore[2].Amount++
	movedBetweenTwo := append([]Payee(nil), ps...) // the same total
	movedBetweenTwo[0].Amount--
	movedBetweenTwo[3].Amount++
	conflicts := map[string]func() error{
		"another order":     func() error { _, _, err := e.SubmitBatch(ctx, "run-1", swapped, 1000); return err },
		"one amount raised": func() error { _, _, err := e.SubmitBatch(ctx, "run-1", onePaidMore, 1000); return err },
		"one shor moved between two payees": func() error {
			_, _, err := e.SubmitBatch(ctx, "run-1", movedBetweenTwo, 1000)
			return err
		},
		"one payee fewer":  func() error { _, _, err := e.SubmitBatch(ctx, "run-1", ps[:3], 1000); return err },
		"another fee rate": func() error { _, _, err := e.SubmitBatch(ctx, "run-1", ps, 1001); return err },
		"a single withdrawal under the batch's id": func() error {
			_, _, err := e.Submit(ctx, "run-1", ps[0].Address, a.Amount, 1000)
			return err
		},
	}
	for name, try := range conflicts {
		if err := try(); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: err = %v, want ErrConflict", name, err)
		}
	}

	if _, _, err := e.Submit(ctx, "single-1", ps[0].Address, ps[0].Amount, 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.SubmitBatch(ctx, "single-1", ps[:1], 1000); !errors.Is(err, ErrConflict) {
		t.Errorf("a batch under a single withdrawal's id: err = %v, want ErrConflict", err)
	}

	// Editing the returned intent does not change whom the stored batch pays.
	a.Payees[0].Amount = 1
	stored, _, _ := e.Store.Get(ctx, "run-1")
	if stored.Payees[0].Amount != ps[0].Amount {
		t.Error("an edit of the returned intent reached the stored record")
	}
}

// A batch is built once, from the payees as recorded and in their order, and
// sent once. The selector is asked for the total and the fee the further
// outputs add. A lost reply resends the same bytes and never builds again.
func TestBatchIsBuiltOnceAndASecondProcessSendsNothingNew(t *testing.T) {
	ctx := context.Background()
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "lost"}
	e, rec := newBatchEngine(t, NewMemStore(), spent, net)
	var asked []int64
	inner := e.Select
	e.Select = func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
		asked = append(asked, amount)
		return inner(ctx, amount, feeRate)
	}
	ps := payees(t, 40, 2_000_000_000)
	in, _, err := e.SubmitBatch(ctx, "run-7", ps, 1000)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.Process(ctx, "run-7"); err == nil {
		t.Fatal("a lost reply was reported as success")
	}
	got, _, _ := e.Store.Get(ctx, "run-7")
	if got.State != StateBuilt || got.RawHex == "" || !got.MaybeRelayed {
		t.Fatalf("after a lost reply: state %s, bytes %q, maybe relayed %v", got.State, got.RawHex, got.MaybeRelayed)
	}
	if want := in.Amount + tx.BatchFeeAllowance(40, 1000); len(asked) != 1 || asked[0] != want {
		t.Errorf("selector asked for %v, want one request of %d (the total and the further outputs' fee)", asked, want)
	}
	if !spent.IsSpent(txA, 0) {
		t.Error("the built batch's input is not reserved")
	}

	net.setMode("ok")
	done, err := e.Process(ctx, "run-7")
	if err != nil || done.State != StateBroadcast {
		t.Fatalf("second process: %v, state %s", err, done.State)
	}
	if _, err := e.Process(ctx, "run-7"); err != nil {
		t.Fatalf("third process: %v", err)
	}
	if rec.builds != 1 {
		t.Errorf("the batch was built %d times, want once", rec.builds)
	}
	if net.sentCount() != 2 || net.sent[0] != net.sent[1] {
		t.Errorf("sent %d times %v, want the same bytes twice", net.sentCount(), net.sent)
	}
	if len(rec.got) != 1 || !samePayees(rec.got[0], ps) {
		t.Error("the signer was not given the payees as submitted, in order")
	}
	if _, created, err := e.SubmitBatch(ctx, "run-7", ps, 1000); err != nil || created {
		t.Errorf("the batch submitted again after it was sent: %v created=%v", err, created)
	}
}

// A stored batch that no longer passes the rules is failed before anything
// is selected or signed, and a batch held by an engine with no batch signer
// waits as it is.
func TestBuildChecksTheStoredBatch(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	spent := utxo.NewSpentSet("", nil)
	e, rec := newBatchEngine(t, store, spent, &fakeNet{mode: "ok"})
	ps := payees(t, 3, 1_000_000_000)

	tamper := func(id string, edit func(in *Intent)) *Intent {
		t.Helper()
		if _, _, err := e.SubmitBatch(ctx, id, ps, 1000); err != nil {
			t.Fatal(err)
		}
		in, _, _ := store.Get(ctx, id)
		in.Payees = append([]Payee(nil), in.Payees...)
		edit(in)
		if err := store.Update(ctx, in, StateCreated); err != nil {
			t.Fatal(err)
		}
		return in
	}
	bad := map[string]*Intent{
		"amount is not the sum": tamper("t1", func(in *Intent) { in.Amount += 5 }),
		"a payee raised":        tamper("t2", func(in *Intent) { in.Payees[1].Amount += 5 }),
		"a payee on another network": tamper("t3", func(in *Intent) {
			in.Payees[0].Address = payeeAddr(t, types.Mainnet.HRP, 7)
		}),
		"a payee repeated": tamper("t4", func(in *Intent) { in.Payees[2].Address = in.Payees[0].Address }),
	}
	for name, in := range bad {
		err := e.Build(ctx, in)
		if !errors.Is(err, ErrInvalidIntent) || in.State != StateFailed {
			t.Errorf("%s: err = %v, state %s; want ErrInvalidIntent and Failed", name, err, in.State)
		}
	}
	if rec.builds != 0 || spent.Size() != 0 {
		t.Errorf("a refused batch reached the signer %d times and left %d spent entries", rec.builds, spent.Size())
	}

	if _, _, err := e.SubmitBatch(ctx, "waits", ps, 1000); err != nil {
		t.Fatal(err)
	}
	single := newEngine(t, store, spent, &fakeNet{mode: "ok"}, bigCoins())
	in, err := single.Process(ctx, "waits")
	if !errors.Is(err, ErrInvalidIntent) || in.State != StateCreated || in.RawHex != "" {
		t.Errorf("an engine with no batch signer: err = %v, state %s", err, in.State)
	}
	if done, err := e.Process(ctx, "waits"); err != nil || done.State != StateBroadcast {
		t.Errorf("the configured engine then: %v, state %s", err, done.State)
	}
}

// The file store keeps a batch across a restart, and the transaction the real
// builder signs for it pays output i to payee i.
func TestBatchSurvivesARestartAndPaysOutputsInPayeeOrder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m := keys.NewManager(filepath.Join(dir, "k.enc"), "pw")
	kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	change, _ := address.ScriptFor(kp.Address)
	hot := []types.UTXO{{TxID: txA, Vout: 0, Value: 900_000_000_000, Height: 10, Address: kp.Address}}

	var built *tx.Transaction
	wire := func(store Store, spent *utxo.SpentSet, net *fakeNet) *Engine {
		e := newEngine(t, store, spent, net, hot)
		e.BuildSignBatch = func(_ context.Context, inputs []types.UTXO, ps []Payee, feeRate int64) (string, string, error) {
			outs := make([]tx.TxOutput, len(ps))
			for i, p := range ps {
				spk, err := address.ScriptFor(p.Address)
				if err != nil {
					return "", "", err
				}
				outs[i] = tx.TxOutput{Value: p.Amount, ScriptPubKey: spk}
			}
			tr, err := tx.BuildSignedBatch(inputs, outs, change, feeRate, m)
			if err != nil {
				return "", "", err
			}
			built = tr
			return tr.SerializeHex(), tr.TxID(), nil
		}
		return e
	}

	storePath, spentPath := filepath.Join(dir, "intents.json"), filepath.Join(dir, "spent.json")
	fs, err := NewFileStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	ps := payees(t, 60, 3_000_000_000)
	if _, _, err := wire(fs, utxo.NewSpentSet(spentPath, nil), &fakeNet{mode: "ok"}).SubmitBatch(ctx, "run-9", ps, types.RecommendedFeeRate); err != nil {
		t.Fatal(err)
	}

	// A new process: the store and the spent set are read from disk.
	fs2, err := NewFileStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	net := &fakeNet{mode: "ok"}
	e2 := wire(fs2, utxo.NewSpentSet(spentPath, nil), net)
	stored, ok, err := fs2.Get(ctx, "run-9")
	if err != nil || !ok || !samePayees(stored.Payees, ps) {
		t.Fatalf("the batch read back after a restart: ok=%v err=%v payees=%d", ok, err, len(stored.Payees))
	}
	done, err := e2.Process(ctx, "run-9")
	if err != nil || done.State != StateBroadcast {
		t.Fatalf("process: %v, state %s", err, done.State)
	}
	if len(built.Outputs) != len(ps)+1 {
		t.Fatalf("outputs = %d, want %d payees and the change", len(built.Outputs), len(ps))
	}
	for i, p := range ps {
		spk, _ := address.ScriptFor(p.Address)
		if built.Outputs[i].Value != p.Amount || string(built.Outputs[i].ScriptPubKey) != string(spk) {
			t.Errorf("output %d does not pay payee %d the recorded amount", i, i)
		}
	}
	if net.sentCount() != 1 || net.sent[0] != built.SerializeHex() || done.TxID != built.TxID() {
		t.Error("the bytes sent are not the transaction the builder signed")
	}
}
