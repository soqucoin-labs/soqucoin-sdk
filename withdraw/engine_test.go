package withdraw

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

const (
	txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	txC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	dst = "ssq1pdestination"
)

// fakeNet records every broadcast and answers according to mode.
type fakeNet struct {
	mu     sync.Mutex
	mode   string // "ok", "lost", "reject", "transient"
	sent   []string
	builds int
}

func (f *fakeNet) Broadcast(rawHex, txid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, rawHex)
	switch f.mode {
	case "lost":
		return "", fmt.Errorf("broadcast of %s: %w: timeout", txid, rpc.ErrUnknownOutcome)
	case "reject":
		return "", fmt.Errorf("broadcast: %w", &rpc.Error{Code: rpc.CodeVerifyRejected, Message: "min relay fee not met"})
	case "transient":
		return "", fmt.Errorf("broadcast: %w", rpc.ErrTransient)
	}
	return txid, nil
}

func (f *fakeNet) setMode(m string) { f.mu.Lock(); f.mode = m; f.mu.Unlock() }
func (f *fakeNet) sentCount() int   { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

// newEngine wires a real CoinSelector over a real SpentSet to a fake network.
// The "signer" just names the transaction after its inputs so the test can
// see which bytes were sent.
func newEngine(t *testing.T, store Store, spent *utxo.SpentSet, net *fakeNet, coins []types.UTXO) *Engine {
	t.Helper()
	cs := &utxo.CoinSelector{SpentSet: spent}
	e := &Engine{
		Store: store, Spent: spent, Broadcaster: net,
		RequiredConfirmations: 3, ReservationTTL: time.Hour,
		Select: func(amount, feeRate int64) ([]types.UTXO, error) {
			sel, _, err := cs.SelectUTXOs(coins, amount+1000, 1, 1000, nil)
			return sel, err
		},
		BuildSign: func(inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
			net.mu.Lock()
			net.builds++
			net.mu.Unlock()
			id := ""
			for _, u := range inputs {
				id += u.TxID[:4] + fmt.Sprint(u.Vout)
			}
			return "hex-" + id, "txid-" + id, nil
		},
	}
	return e
}

func coins() []types.UTXO {
	return []types.UTXO{
		{TxID: txA, Vout: 0, Value: 5_000_000, Height: 10, Address: "ssq1pa"},
		{TxID: txB, Vout: 0, Value: 3_000_000, Height: 10, Address: "ssq1pb"},
		{TxID: txC, Vout: 0, Value: 1_000_000, Height: 10, Address: "ssq1pc"},
	}
}

func TestSubmitIsIdempotentAndConflictSafe(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet(""), &fakeNet{mode: "ok"}, coins())
	a, created, err := e.Submit("w1", dst, 1_000_000, 1000)
	if err != nil || !created {
		t.Fatalf("first submit: %v created=%v", err, created)
	}
	b, created, err := e.Submit("w1", dst, 1_000_000, 1000)
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("second submit: %v created=%v", err, created)
	}
	if _, _, err := e.Submit("w1", dst, 2_000_000, 1000); !errors.Is(err, ErrConflict) {
		t.Errorf("same id, different amount accepted: %v", err)
	}
	if _, _, err := e.Submit("", dst, 1, 1); !errors.Is(err, ErrInvalidIntent) {
		t.Errorf("empty id accepted: %v", err)
	}
}

// Two withdrawals must never select the same input. The first reserves the
// largest coin at build time; the second sees it as spent and takes the next.
func TestBuildReservesInputsAgainstConcurrentWithdrawals(t *testing.T) {
	spent := utxo.NewSpentSet("")
	e := newEngine(t, NewMemStore(), spent, &fakeNet{mode: "ok"}, coins())
	w1, _, _ := e.Submit("w1", dst, 2_000_000, 1000)
	w2, _, _ := e.Submit("w2", dst, 2_000_000, 1000)
	if err := e.Build(w1); err != nil {
		t.Fatal(err)
	}
	if err := e.Build(w2); err != nil {
		t.Fatal(err)
	}
	if w1.Inputs[0].TxID != txA || w2.Inputs[0].TxID != txB {
		t.Fatalf("w1 took %s, w2 took %s: the second withdrawal must not see the first one's input", w1.Inputs[0].TxID, w2.Inputs[0].TxID)
	}
	if !spent.IsSpent(txA, 0) || !spent.IsSpent(txB, 0) {
		t.Error("built intents' inputs are not reserved in the spent set")
	}
	// A third withdrawal that needs 2M finds only the 1M coin left.
	w3, _, _ := e.Submit("w3", dst, 2_000_000, 1000)
	if err := e.Build(w3); err == nil || w3.State != StateFailed {
		t.Errorf("third withdrawal should fail for insufficient funds, got err=%v state=%s", err, w3.State)
	}
}

// The double-payment case. A lost reply leaves the intent Built with the
// attempt recorded; the retry sends the SAME bytes; the builder never runs
// again for that intent.
func TestLostReplyRetriesSameBytesAndNeverRebuilds(t *testing.T) {
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet(""), net, coins())
	w, _, _ := e.Submit("w1", dst, 1_000_000, 1000)
	if _, err := e.Process("w1"); !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("lost reply: %v, want ErrUnknownOutcome", err)
	}
	w, _, _ = e.Store.Get("w1")
	if w.State != StateBuilt || w.Attempts != 1 || w.RawHex == "" {
		t.Fatalf("after lost reply: %+v", w)
	}
	// Retry twice more while replies are still lost, then the network heals.
	e.Process("w1")
	e.Process("w1")
	net.setMode("ok")
	if _, err := e.Process("w1"); err != nil {
		t.Fatal(err)
	}
	w, _, _ = e.Store.Get("w1")
	if w.State != StateBroadcast {
		t.Fatalf("state %s, want broadcast", w.State)
	}
	if net.builds != 1 {
		t.Fatalf("builder ran %d times; a retry must reuse the persisted transaction", net.builds)
	}
	for _, hex := range net.sent {
		if hex != w.RawHex {
			t.Fatalf("a retry sent different bytes: %s vs %s", hex, w.RawHex)
		}
	}
	if len(net.sent) != 4 {
		t.Errorf("expected 4 broadcasts of the same bytes, saw %d", len(net.sent))
	}
	if !e.Spent.IsSpent(txA, 0) {
		t.Error("inputs of a broadcast intent must be marked spent")
	}
}

// Restart after the signed transaction was persisted but before the network
// acknowledged it: Recover re-sends the persisted bytes and does not rebuild.
func TestRecoverRebroadcastsPersistedBytes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	spentPath := filepath.Join(dir, "spent.json")
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, store, utxo.NewSpentSet(spentPath), net, coins())
	e.Submit("w1", dst, 1_000_000, 1000)
	e.Process("w1") // built, broadcast lost
	first, _, _ := store.Get("w1")

	// New process: fresh store, fresh spent set from disk, healed network. The
	// builder in the new engine would produce DIFFERENT bytes; it must not run.
	store2, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	net2 := &fakeNet{mode: "ok"}
	e2 := newEngine(t, store2, utxo.NewSpentSet(spentPath), net2, coins())
	e2.BuildSign = func([]types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("Recover rebuilt a transaction that was already persisted")
		return "", "", nil
	}
	if err := e2.Recover(); err != nil {
		t.Fatal(err)
	}
	after, _, _ := store2.Get("w1")
	if after.State != StateBroadcast || after.RawHex != first.RawHex || after.TxID != first.TxID {
		t.Fatalf("recovered intent %+v differs from persisted %+v", after, first)
	}
	if len(net2.sent) != 1 || net2.sent[0] != first.RawHex {
		t.Fatalf("recover sent %v, want exactly the persisted bytes", net2.sent)
	}
}

func TestPermanentRejectionFailsAndReleasesInputs(t *testing.T) {
	spent := utxo.NewSpentSet("")
	net := &fakeNet{mode: "reject"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.Submit("w1", dst, 1_000_000, 1000)
	if _, err := e.Process("w1"); !errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("rejection: %v", err)
	}
	w, _, _ := e.Store.Get("w1")
	if w.State != StateFailed || w.TxID == "" {
		t.Fatalf("rejected intent %+v: want failed with the transaction kept for the audit trail", w)
	}
	if spent.IsSpent(txA, 0) {
		t.Error("inputs of a rejected transaction must be released for the next withdrawal")
	}
	// Retrying a failed intent is refused; a new intent can use the coin.
	if err := e.Broadcast(w); !errors.Is(err, ErrWrongState) {
		t.Errorf("broadcast of a failed intent: %v", err)
	}
	net.setMode("ok")
	e.Submit("w2", dst, 1_000_000, 1000)
	w2, err := e.Process("w2")
	if err != nil || w2.Inputs[0].TxID != txA {
		t.Fatalf("released input not reusable: %v %+v", err, w2)
	}
}

// The persisted record must exist before the network sees the transaction.
type failingStore struct {
	*MemStore
	failBuilt bool
}

func (f *failingStore) Put(in *Intent) error {
	if f.failBuilt && in.State == StateBuilt {
		return errors.New("disk full")
	}
	return f.MemStore.Put(in)
}

func TestNothingIsBroadcastUnlessPersistedFirst(t *testing.T) {
	spent := utxo.NewSpentSet("")
	net := &fakeNet{mode: "ok"}
	store := &failingStore{MemStore: NewMemStore(), failBuilt: true}
	e := newEngine(t, store, spent, net, coins())
	e.Submit("w1", dst, 1_000_000, 1000)
	_, err := e.Process("w1")
	if err == nil {
		t.Fatal("persist failure was not reported")
	}
	if net.sentCount() != 0 {
		t.Fatal("a transaction whose record could not be persisted was broadcast")
	}
	if spent.IsSpent(txA, 0) {
		t.Error("reservation was not released after the persist failure")
	}
	w, _, _ := store.Get("w1")
	if w.State != StateCreated {
		t.Errorf("intent state %s, want created (retryable)", w.State)
	}
}

type fakeConfirmer struct{ n int64 }

func (f fakeConfirmer) Confirmations(string) (int64, error) { return f.n, nil }

func TestConfirmationsCompleteTheIntent(t *testing.T) {
	spent := utxo.NewSpentSet("")
	e := newEngine(t, NewMemStore(), spent, &fakeNet{mode: "ok"}, coins())
	e.Submit("w1", dst, 1_000_000, 1000)
	w, err := e.Process("w1")
	if err != nil {
		t.Fatal(err)
	}
	e.Confirmer = fakeConfirmer{n: 2}
	if err := e.UpdateConfirmations(w); err != nil || w.State != StateBroadcast || w.Confirmations != 2 {
		t.Fatalf("2 confs: %v %+v", err, w)
	}
	e.Confirmer = fakeConfirmer{n: 3}
	if err := e.UpdateConfirmations(w); err != nil || w.State != StateConfirmed {
		t.Fatalf("3 confs: %v %+v", err, w)
	}
	spent.Prune() // confirmed entries age out later; still present now
	if !spent.IsSpent(txA, 0) {
		t.Error("confirmed spend should remain in the set until pruned by age")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x", "intents.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Put(&Intent{ID: "b", State: StateBuilt, RawHex: "00", CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(&Intent{ID: "a", State: StateBroadcast, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := s2.List()
	if len(all) != 2 || all[0].ID != "a" || all[1].ID != "b" {
		t.Fatalf("list after reload: %+v", all)
	}
	built, _ := s2.List(StateBuilt)
	if len(built) != 1 || built[0].RawHex != "00" {
		t.Fatalf("filtered list: %+v", built)
	}
}
