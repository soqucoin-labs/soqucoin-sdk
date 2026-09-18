package withdraw

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	mode   string // "ok", "lost", "reject", "transient", "mismatch", "mismatch-blank"
	sent   []string
	builds int
}

func (f *fakeNet) Broadcast(_ context.Context, rawHex, txid string) (string, error) {
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
	case "mismatch":
		return "node-" + txid, fmt.Errorf("broadcast: %w: node returned txid %s for a transaction the caller computed as %s", rpc.ErrTxIDMismatch, "node-"+txid, txid)
	case "mismatch-blank": // a third-party Broadcaster that reports the kind without the txid
		return "", fmt.Errorf("broadcast: %w", rpc.ErrTxIDMismatch)
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
		Select: func(_ context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			sel, _, err := cs.SelectUTXOs(coins, amount+1000, 1, 1000, nil)
			return sel, err
		},
		BuildSign: func(_ context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
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
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	a, created, err := e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if err != nil || !created {
		t.Fatalf("first submit: %v created=%v", err, created)
	}
	b, created, err := e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("second submit: %v created=%v", err, created)
	}
	if _, _, err := e.Submit(context.Background(), "w1", dst, 2_000_000, 1000); !errors.Is(err, ErrConflict) {
		t.Errorf("same id, different amount accepted: %v", err)
	}
	if _, _, err := e.Submit(context.Background(), "", dst, 1, 1); !errors.Is(err, ErrInvalidIntent) {
		t.Errorf("empty id accepted: %v", err)
	}
}

// Two withdrawals must never select the same input. The first reserves the
// largest coin at build time; the second sees it as spent and takes the next.
func TestBuildReservesInputsAgainstConcurrentWithdrawals(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	e := newEngine(t, NewMemStore(), spent, &fakeNet{mode: "ok"}, coins())
	w1, _, _ := e.Submit(context.Background(), "w1", dst, 2_000_000, 1000)
	w2, _, _ := e.Submit(context.Background(), "w2", dst, 2_000_000, 1000)
	if err := e.Build(context.Background(), w1); err != nil {
		t.Fatal(err)
	}
	if err := e.Build(context.Background(), w2); err != nil {
		t.Fatal(err)
	}
	if w1.Inputs[0].TxID != txA || w2.Inputs[0].TxID != txB {
		t.Fatalf("w1 took %s, w2 took %s: the second withdrawal must not see the first one's input", w1.Inputs[0].TxID, w2.Inputs[0].TxID)
	}
	if !spent.IsSpent(txA, 0) || !spent.IsSpent(txB, 0) {
		t.Error("built intents' inputs are not reserved in the spent set")
	}
	// A third withdrawal that needs 2M finds only the 1M coin left.
	w3, _, _ := e.Submit(context.Background(), "w3", dst, 2_000_000, 1000)
	if err := e.Build(context.Background(), w3); err == nil || w3.State != StateFailed {
		t.Errorf("third withdrawal should fail for insufficient funds, got err=%v state=%s", err, w3.State)
	}
}

// The double-payment case. A lost reply leaves the intent Built with the
// attempt recorded; the retry sends the SAME bytes; the builder never runs
// again for that intent.
func TestLostReplyRetriesSameBytesAndNeverRebuilds(t *testing.T) {
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), net, coins())
	w, _, _ := e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("lost reply: %v, want ErrUnknownOutcome", err)
	}
	w, _, _ = e.Store.Get(context.Background(), "w1")
	if w.State != StateBuilt || w.Attempts != 1 || w.RawHex == "" {
		t.Fatalf("after lost reply: %+v", w)
	}
	// Retry twice more while replies are still lost, then the network heals.
	e.Process(context.Background(), "w1")
	e.Process(context.Background(), "w1")
	net.setMode("ok")
	if _, err := e.Process(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	w, _, _ = e.Store.Get(context.Background(), "w1")
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
	e := newEngine(t, store, utxo.NewSpentSet(spentPath, nil), net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	e.Process(context.Background(), "w1") // built, broadcast lost
	first, _, _ := store.Get(context.Background(), "w1")

	// New process: fresh store, fresh spent set from disk, healed network. The
	// builder in the new engine would produce DIFFERENT bytes; it must not run.
	store2, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	net2 := &fakeNet{mode: "ok"}
	e2 := newEngine(t, store2, utxo.NewSpentSet(spentPath, nil), net2, coins())
	e2.BuildSign = func(context.Context, []types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("Recover rebuilt a transaction that was already persisted")
		return "", "", nil
	}
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _, _ := store2.Get(context.Background(), "w1")
	if after.State != StateBroadcast || after.RawHex != first.RawHex || after.TxID != first.TxID {
		t.Fatalf("recovered intent %+v differs from persisted %+v", after, first)
	}
	if len(net2.sent) != 1 || net2.sent[0] != first.RawHex {
		t.Fatalf("recover sent %v, want exactly the persisted bytes", net2.sent)
	}
}

func TestPermanentRejectionFailsAndReleasesInputs(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "reject"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("rejection: %v", err)
	}
	w, _, _ := e.Store.Get(context.Background(), "w1")
	if w.State != StateFailed || w.TxID == "" {
		t.Fatalf("rejected intent %+v: want failed with the transaction kept for the audit trail", w)
	}
	if spent.IsSpent(txA, 0) {
		t.Error("inputs of a rejected transaction must be released for the next withdrawal")
	}
	// Retrying a failed intent is refused; a new intent can use the coin.
	if err := e.Broadcast(context.Background(), w); !errors.Is(err, ErrWrongState) {
		t.Errorf("broadcast of a failed intent: %v", err)
	}
	net.setMode("ok")
	e.Submit(context.Background(), "w2", dst, 1_000_000, 1000)
	w2, err := e.Process(context.Background(), "w2")
	if err != nil || w2.Inputs[0].TxID != txA {
		t.Fatalf("released input not reusable: %v %+v", err, w2)
	}
}

// The persisted record must exist before the network sees the transaction.
type failingStore struct {
	*MemStore
	failBuilt bool
}

func (f *failingStore) Update(_ context.Context, in *Intent, from State) error {
	if f.failBuilt && in.State == StateBuilt {
		return errors.New("disk full")
	}
	return f.MemStore.Update(context.Background(), in, from)
}

func TestNothingIsBroadcastUnlessPersistedFirst(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	store := &failingStore{MemStore: NewMemStore(), failBuilt: true}
	e := newEngine(t, store, spent, net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	_, err := e.Process(context.Background(), "w1")
	if err == nil {
		t.Fatal("persist failure was not reported")
	}
	if net.sentCount() != 0 {
		t.Fatal("a transaction whose record could not be persisted was broadcast")
	}
	if spent.IsSpent(txA, 0) {
		t.Error("reservation was not released after the persist failure")
	}
	w, _, _ := store.Get(context.Background(), "w1")
	if w.State != StateCreated {
		t.Errorf("intent state %s, want created (retryable)", w.State)
	}
}

type fakeConfirmer struct{ n int64 }

func (f fakeConfirmer) Confirmations(context.Context, string) (int64, error) { return f.n, nil }

func TestConfirmationsCompleteTheIntent(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	e := newEngine(t, NewMemStore(), spent, &fakeNet{mode: "ok"}, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	w, err := e.Process(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	e.Confirmer = fakeConfirmer{n: 2}
	if err := e.UpdateConfirmations(context.Background(), w); err != nil || w.State != StateBroadcast || w.Confirmations != 2 {
		t.Fatalf("2 confs: %v %+v", err, w)
	}
	e.Confirmer = fakeConfirmer{n: 3}
	if err := e.UpdateConfirmations(context.Background(), w); err != nil || w.State != StateConfirmed {
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
	if err := s.Create(context.Background(), &Intent{ID: "b", State: StateBuilt, RawHex: "00", CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), &Intent{ID: "a", State: StateBroadcast, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := s2.List(context.Background())
	if len(all) != 2 || all[0].ID != "a" || all[1].ID != "b" {
		t.Fatalf("list after reload: %+v", all)
	}
	built, _ := s2.List(context.Background(), StateBuilt)
	if len(built) != 1 || built[0].RawHex != "00" {
		t.Fatalf("filtered list: %+v", built)
	}
}

// breakStoreFile replaces the store's file with a non-empty directory, so the
// next write fails at the rename, on any platform and as any user. The
// returned func clears the path again.
func breakStoreFile(t *testing.T, path string) (fix func()) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

// When the write fails, the store keeps reporting the record it held before,
// so Get and the caller that treated the error as "not saved" agree. A record
// whose first save failed is not reported at all. Once the path is writable
// the next write puts the whole store on disk, and a reload shows only what
// was saved.
func TestFileStorePutKeepsThePreviousRecordWhenTheWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intents.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	created := &Intent{ID: "w1", State: StateCreated, CreatedAt: time.Now().UTC()}
	if err := s.Create(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	fix := breakStoreFile(t, path)

	built := *created
	built.State, built.RawHex, built.TxID = StateBuilt, "00", "t"
	if err := s.Update(context.Background(), &built, StateCreated); err == nil {
		t.Fatal("a write onto a directory succeeded")
	}
	got, ok, _ := s.Get(context.Background(), "w1")
	if !ok || got.State != StateCreated || got.RawHex != "" {
		t.Fatalf("after the failed write, Get reports %+v; want the Created record", got)
	}
	if list, _ := s.List(context.Background(), StateBuilt); len(list) != 0 {
		t.Fatalf("List reports a Built intent whose save failed: %+v", list)
	}
	if err := s.Create(context.Background(), &Intent{ID: "w2", State: StateCreated, CreatedAt: time.Now().UTC()}); err == nil {
		t.Fatal("a write onto a directory succeeded")
	}
	if _, ok, _ := s.Get(context.Background(), "w2"); ok {
		t.Fatal("a record whose first save failed is reported")
	}

	fix()
	if err := s.Update(context.Background(), &built, StateCreated); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := s2.List(context.Background())
	if len(all) != 1 || all[0].ID != "w1" || all[0].State != StateBuilt {
		t.Fatalf("reloaded store: %+v; want only w1, Built", all)
	}
}

// The case the store rule exists for. A Built save fails: Build releases the
// inputs and reverts the caller's intent to Created. If the store still
// reported Built, the next Process would broadcast a transaction whose inputs
// are free for any other withdrawal to take. With the store keeping its
// previous record, the next Process builds again, on inputs it reserves, and
// only that transaction is sent.
func TestFailedBuiltSaveIsNotBroadcastByTheNextProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intents.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, spent, net, coins())
	if _, _, err := e.Submit(context.Background(), "w1", dst, 1_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	fix := breakStoreFile(t, path)

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := e.Process(context.Background(), "w1"); err == nil {
			t.Fatalf("attempt %d: persist failure was not reported", attempt)
		}
		if n := net.sentCount(); n != 0 {
			t.Fatalf("attempt %d: %d transactions broadcast although no record of them was saved", attempt, n)
		}
		if held := spent.ReservedIntents(); len(held) != 0 {
			t.Fatalf("attempt %d: reservations %v kept after the failed save", attempt, held)
		}
		w, ok, err := store.Get(context.Background(), "w1")
		if err != nil || !ok || w.State != StateCreated {
			t.Fatalf("attempt %d: store reports %+v, %v; want w1 Created", attempt, w, err)
		}
	}

	fix()
	w, err := e.Process(context.Background(), "w1")
	if err != nil || w.State != StateBroadcast || net.sentCount() != 1 {
		t.Fatalf("after the disk is back: %+v, %v, %d sent; want one broadcast", w, err, net.sentCount())
	}
	for _, in := range w.Inputs {
		if !spent.IsSpent(in.TxID, in.Vout) {
			t.Errorf("broadcast input %s:%d is not marked spent", in.TxID, in.Vout)
		}
	}
	reloaded, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := reloaded.Get(context.Background(), "w1"); !ok || got.State != StateBroadcast || got.RawHex != w.RawHex {
		t.Fatalf("file holds %+v; want the broadcast record", got)
	}
}

// The reservation of a Built intent is renewed on every unsettled broadcast
// attempt. With a 40 ms TTL and a network that keeps losing replies, a second
// withdrawal submitted after several TTLs must still not select the first
// one's inputs; without renewal the spent set forgets them after the first
// TTL and the two withdrawals share an input.
func TestUnsettledBroadcastRenewsTheReservation(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.ReservationTTL = 40 * time.Millisecond
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000) // only txA (5,000,000) covers it alone
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("first attempt: %v", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	for i := 0; i < 3; i++ {
		time.Sleep(30 * time.Millisecond) // inside the TTL each time, past it in total
		if err := e.Broadcast(context.Background(), w1); !errors.Is(err, rpc.ErrUnknownOutcome) || errors.Is(err, ErrReservationLost) {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("reservation expired although the intent was retried inside every TTL")
	}
	net.setMode("ok")
	e.Submit(context.Background(), "w2", dst, 4_500_000, 1000)
	w2, err := e.Process(context.Background(), "w2")
	if err == nil {
		for _, o := range w2.Inputs {
			if o.TxID == w1.Inputs[0].TxID && o.Vout == w1.Inputs[0].Vout {
				t.Fatalf("w2 selected w1's input %s:%d", o.TxID, o.Vout)
			}
		}
		t.Fatalf("w2 built on %+v; the only coin large enough is held by w1", w2.Inputs)
	}
	if w2.State != StateFailed {
		t.Fatalf("w2 %+v, want failed for lack of free coins", w2)
	}
	// w1 still completes on its own bytes once the network heals.
	if err := e.Broadcast(context.Background(), w1); err != nil {
		t.Fatal(err)
	}
	if net.builds != 1 {
		t.Errorf("builder ran %d times, want 1: w2 must fail at selection and never reach the builder", net.builds)
	}
}

// If the reservation did expire and another withdrawal took the input before
// the retry, the retry must not release or fail the first intent (its bytes
// may be in the mempool) and must say what happened.
func TestRetryAfterLostReservationReportsAndHolds(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.ReservationTTL = 20 * time.Millisecond
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	e.Process(context.Background(), "w1")
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	time.Sleep(40 * time.Millisecond) // no retry inside the TTL: the reservation lapses
	net.setMode("ok")
	e.Submit(context.Background(), "w2", dst, 4_500_000, 1000)
	w2, err := e.Process(context.Background(), "w2")
	if err != nil || w2.Inputs[0].TxID != w1.Inputs[0].TxID {
		t.Fatalf("w2 should have taken the lapsed input: %v %+v", err, w2)
	}
	net.setMode("lost")
	err = e.Broadcast(context.Background(), w1)
	if !errors.Is(err, ErrReservationLost) || !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("retry after the input was taken: %v, want ErrReservationLost wrapping the broadcast error", err)
	}
	w1, _, _ = e.Store.Get(context.Background(), "w1")
	if w1.State != StateBuilt || w1.RawHex == "" {
		t.Fatalf("w1 %+v, want still Built with its bytes", w1)
	}
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("w2's broadcast entry was released by w1's retry")
	}
}

// The node accepted the bytes under a different txid: the payment is out on
// this intent's inputs. The engine must keep the intent Built, record the
// node's txid, and mark the inputs spent so that they never expire back into
// selection; the next withdrawal must not be able to select them.
func TestTxIDMismatchHoldsInputsAndRecordsTheNodeTxID(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "mismatch"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.ReservationTTL = 20 * time.Millisecond // a mere reservation would lapse below
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	_, err := e.Process(context.Background(), "w1")
	if !errors.Is(err, rpc.ErrTxIDMismatch) || errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("mismatch: %v, want ErrTxIDMismatch and not ErrPermanent", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if w1.State != StateBuilt || w1.RawHex == "" || w1.NodeTxID != "node-"+w1.TxID {
		t.Fatalf("after mismatch %+v: want Built, bytes kept, NodeTxID recorded", w1)
	}
	time.Sleep(40 * time.Millisecond)
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("inputs free after the TTL although the node accepted the transaction")
	}
	net.setMode("ok")
	e.Submit(context.Background(), "w2", dst, 4_500_000, 1000)
	if w2, err := e.Process(context.Background(), "w2"); err == nil || w2.State != StateFailed {
		t.Fatalf("w2 %+v %v: must not build on inputs the node has already spent", w2, err)
	}
	// A retry sends nothing, even when the node would now answer "already in
	// chain" for these bytes (fakeNet "ok" is that answer): the intent must
	// not become a Broadcast intent under a txid the node does not know.
	net.setMode("ok")
	sent := net.sentCount()
	if err := e.Broadcast(context.Background(), w1); !errors.Is(err, ErrHeld) {
		t.Fatalf("retry of a held intent: %v, want ErrHeld", err)
	}
	w1, _, _ = e.Store.Get(context.Background(), "w1")
	if w1.State != StateBuilt || w1.Attempts != 1 || net.sentCount() != sent {
		t.Fatalf("retry changed or sent something: %+v, sent %d", w1, net.sentCount()-sent)
	}
	// Recover leaves it alone too and reports it: no broadcast, ErrHeld.
	if err := e.Recover(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatalf("recover over a held intent: %v, want ErrHeld reported", err)
	}
	if net.sentCount() != sent {
		t.Fatal("recover broadcast a held intent")
	}
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("recover disturbed the spent entry of a held intent")
	}
}

// A Broadcaster that reports the mismatch kind without the node's txid must
// still arm the hold.
func TestTxIDMismatchWithoutANodeTxIDStillHolds(t *testing.T) {
	net := &fakeNet{mode: "mismatch-blank"}
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), net, coins())
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, rpc.ErrTxIDMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if w1.NodeTxID == "" || w1.State != StateBuilt {
		t.Fatalf("hold not armed: %+v", w1)
	}
	net.setMode("ok")
	if err := e.Broadcast(context.Background(), w1); !errors.Is(err, ErrHeld) {
		t.Fatalf("retry: %v, want ErrHeld", err)
	}
}

// failingSpentSet returns a file-backed spent set whose every write fails.
func failingSpentSet(t *testing.T) *utxo.SpentSet {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return utxo.NewSpentSet(filepath.Join(blocker, "spent.json"), nil)
}

// The spent set cannot be written. Build reserves nothing and sends nothing;
// the intent stays Created for a retry once the disk is fixed.
func TestBuildWithUnwritableSpentSetSendsNothing(t *testing.T) {
	spent := failingSpentSet(t)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	_, err := e.Process(context.Background(), "w1")
	if !errors.Is(err, utxo.ErrPersist) {
		t.Fatalf("process: %v, want ErrPersist", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if w1.State != StateCreated || w1.RawHex != "" || net.sentCount() != 0 || net.builds != 0 {
		t.Fatalf("something happened on an unwritable spent set: %+v, sent %d, built %d", w1, net.sentCount(), net.builds)
	}
	if spent.IsSpent(txA, 0) || spent.IsSpent(txB, 0) || spent.IsSpent(txC, 0) {
		t.Fatal("an unpersisted reservation was kept")
	}
}

// The disk fails between a successful broadcast and the spent-set write. The
// intent is still recorded as Broadcast (the payment is out), the error is
// returned so the operator hears about it, this process refuses the inputs,
// and Recover over the intent store re-marks them in a fresh spent set.
func TestBroadcastWithUnwritableSpentSetReportsAndRecoverRemarks(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	store := NewMemStore()
	e := newEngine(t, store, spent, net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if err := e.Build(context.Background(), w1); err != nil {
		t.Fatal(err)
	}
	e.Spent = failingSpentSet(t) // the disk goes away after the build
	err := e.Broadcast(context.Background(), w1)
	if !errors.Is(err, utxo.ErrPersist) {
		t.Fatalf("broadcast: %v, want ErrPersist reported", err)
	}
	w1, _, _ = store.Get(context.Background(), "w1")
	if w1.State != StateBroadcast || w1.TxID == "" {
		t.Fatalf("intent %+v, want Broadcast with its txid: the payment is out", w1)
	}
	if !e.Spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("this process must keep refusing the inputs even though the write failed")
	}
	// Restart with an empty spent set: only the intent store knows the spend.
	fresh := utxo.NewSpentSet("", nil)
	e2 := newEngine(t, store, fresh, &fakeNet{mode: "ok"}, coins())
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !fresh.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("Recover did not re-mark a Broadcast intent's inputs")
	}
	e2.Submit(context.Background(), "w2", dst, 4_500_000, 1000) // only txA covers it; txA is w1's input
	if w2, err := e2.Process(context.Background(), "w2"); err == nil || w2.State != StateFailed {
		t.Fatalf("w2 %+v %v: built on an input the previous process spent", w2, err)
	}
}

// Recover re-marks a held intent's inputs under the node's txid, and when the
// spent set cannot be written it still holds them in memory and reports.
func TestRecoverRemarksHeldIntentsAndReportsAFailedWrite(t *testing.T) {
	store := NewMemStore()
	net := &fakeNet{mode: "mismatch"}
	e := newEngine(t, store, utxo.NewSpentSet("", nil), net, coins())
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, rpc.ErrTxIDMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	w1, _, _ := store.Get(context.Background(), "w1")

	fresh := utxo.NewSpentSet("", nil)
	e2 := newEngine(t, store, fresh, &fakeNet{mode: "ok"}, coins())
	if err := e2.Recover(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatalf("recover: %v, want ErrHeld reported", err)
	}
	if !fresh.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("a held intent's inputs were not re-marked from the store")
	}

	failing := failingSpentSet(t)
	e3 := newEngine(t, store, failing, &fakeNet{mode: "ok"}, coins())
	err := e3.Recover(context.Background())
	if !errors.Is(err, utxo.ErrPersist) || !errors.Is(err, ErrHeld) {
		t.Fatalf("recover on an unwritable set: %v, want both ErrPersist and ErrHeld", err)
	}
	if !failing.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("re-marked inputs must stay held in memory when the write fails")
	}
}

// A selector error the node will clear by itself (behind its headers for one
// block, warming up after a restart, unreachable) must not end the
// withdrawal. The intent stays Created with the attempt recorded, nothing is
// reserved or built, and the next Build goes through with no new id. An
// insufficient-funds error still fails it.
func TestTransientSelectorErrorKeepsTheIntentCreated(t *testing.T) {
	transients := []error{
		fmt.Errorf("%w (blocks 100, headers 101, initialblockdownload false)", rpc.ErrNodeSyncing),
		fmt.Errorf("getblockchaininfo: %w", &rpc.Error{Code: rpc.CodeInWarmup, Message: "Loading block index..."}),
		fmt.Errorf("getblockchaininfo: %w", rpc.ErrTransient),
	}
	for i, transient := range transients {
		spent := utxo.NewSpentSet("", nil)
		net := &fakeNet{mode: "ok"}
		e := newEngine(t, NewMemStore(), spent, net, coins())
		realSelect := e.Select
		remaining := 2
		e.Select = func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			if remaining > 0 {
				remaining--
				return nil, transient
			}
			return realSelect(ctx, amount, feeRate)
		}
		e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
		for attempt := 1; attempt <= 2; attempt++ {
			_, err := e.Process(context.Background(), "w1")
			if !errors.Is(err, rpc.ErrTransient) || errors.Is(err, rpc.ErrPermanent) {
				t.Fatalf("case %d attempt %d: %v, want the transient error back", i, attempt, err)
			}
			w, _, _ := e.Store.Get(context.Background(), "w1")
			if w.State != StateCreated || w.Attempts != attempt || w.LastError == "" || w.RawHex != "" || w.Inputs != nil {
				t.Fatalf("case %d attempt %d: %+v, want Created with the attempt recorded and nothing built", i, attempt, w)
			}
		}
		if spent.IsSpent(txA, 0) || spent.IsSpent(txB, 0) || spent.IsSpent(txC, 0) || net.builds != 0 || net.sentCount() != 0 {
			t.Fatalf("case %d: a transient selector error reserved, built or sent something", i)
		}
		w, err := e.Process(context.Background(), "w1")
		if err != nil || w.State != StateBroadcast || w.Attempts != 3 {
			t.Fatalf("case %d: after the node caught up: %v %+v", i, err, w)
		}
	}
	// The selector's own refusal is permanent: nothing the node does later
	// changes the answer, so the intent fails and the caller learns now.
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	e.Submit(context.Background(), "w1", dst, 50_000_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); err == nil || errors.Is(err, rpc.ErrTransient) {
		t.Fatalf("insufficient funds: %v, want a permanent failure", err)
	}
	if w, _, _ := e.Store.Get(context.Background(), "w1"); w.State != StateFailed {
		t.Fatalf("insufficient funds left the intent %s, want failed", w.State)
	}
}

// The previous process reserved inputs and stopped before the Built save (or
// its release after a failed build never reached the disk). The store knows
// the intent as Created or Failed; nothing signed exists for it. Recover
// frees those coins for the next withdrawal, and only those: a Built intent
// keeps its reservation and is re-sent, and a reservation under an id the
// store has never seen is kept and reported, because Submit persists the
// intent before anything is reserved, so an unknown id means the store is
// not the one that was running and a Built intent it knew may be in flight.
func TestRecoverReleasesReservationsOfIntentsWithNothingBuilt(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	spentPath := filepath.Join(dir, "spent.json")
	spent := utxo.NewSpentSet(spentPath, nil)
	e := newEngine(t, store, spent, &fakeNet{mode: "lost"}, coins())
	// created: crash between Reserve (txA) and the Built save.
	e.Submit(context.Background(), "created", dst, 4_500_000, 1000)
	if err := spent.Reserve([]types.UTXO{coins()[0]}, "created", time.Hour); err != nil {
		t.Fatal(err)
	}
	// failed: the build failed and the release of txB never reached the disk.
	e.Submit(context.Background(), "failed", dst, 2_500_000, 1000)
	if err := spent.Reserve([]types.UTXO{coins()[1]}, "failed", time.Hour); err != nil {
		t.Fatal(err)
	}
	failed, _, _ := store.Get(context.Background(), "failed")
	failed.State = StateFailed
	if err := store.Update(context.Background(), failed, StateCreated); err != nil {
		t.Fatal(err)
	}
	// ghost: an intent the store never saw (written by a process that lost its store).
	if err := spent.Reserve([]types.UTXO{{TxID: txB, Vout: 7, Value: 1}}, "ghost", time.Hour); err != nil {
		t.Fatal(err)
	}
	// built: a real Built intent on the one free coin, txC; broadcast reply lost.
	e.Submit(context.Background(), "built", dst, 500_000, 1000)
	e.Process(context.Background(), "built")
	built, _, _ := store.Get(context.Background(), "built")
	if built.State != StateBuilt || built.Inputs[0].TxID != txC {
		t.Fatalf("setup: %+v", built)
	}

	// Restart with the files: the healed network re-sends built; created's
	// and failed's reservations are released; ghost's is kept and reported;
	// a withdrawal can now take txA.
	store2, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := utxo.NewSpentSet(spentPath, nil)
	if got := fresh.ReservedIntents(); len(got) != 4 {
		t.Fatalf("setup: reservations on disk %v, want built, created, failed, ghost", got)
	}
	net2 := &fakeNet{mode: "ok"}
	e2 := newEngine(t, store2, fresh, net2, coins())
	realBuildSign := e2.BuildSign
	e2.BuildSign = func(context.Context, []types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("Recover built something")
		return "", "", nil
	}
	err = e2.Recover(context.Background())
	if !errors.Is(err, ErrUnknownReservation) || errors.Is(err, ErrHeld) || errors.Is(err, utxo.ErrPersist) {
		t.Fatalf("recover: %v, want ErrUnknownReservation for ghost and neither ErrHeld nor ErrPersist", err)
	}
	e2.BuildSign = realBuildSign
	if got := fresh.ReservedIntents(); len(got) != 1 || got[0] != "ghost" {
		t.Fatalf("reservations after recover %v, want only ghost (built's became a broadcast entry)", got)
	}
	if fresh.IsSpent(txA, 0) || fresh.IsSpent(txB, 0) {
		t.Fatal("a reservation of a Created or Failed intent survived Recover")
	}
	if !fresh.IsSpent(txB, 7) {
		t.Fatal("the reservation of an id the store does not know was released")
	}
	if !fresh.IsSpent(txC, 0) {
		t.Fatal("the Built intent's input was released")
	}
	if b, _, _ := store2.Get(context.Background(), "built"); b.State != StateBroadcast || len(net2.sent) != 1 || net2.sent[0] != built.RawHex {
		t.Fatalf("built intent after recover: %+v sent %v", b, net2.sent)
	}
	if c, _, _ := store2.Get(context.Background(), "created"); c.State != StateCreated {
		t.Fatalf("created intent is %s after recover; releasing its reservation must not change it", c.State)
	}
	// The released 5,000,000 coin is selectable again.
	e2.Submit(context.Background(), "next", dst, 4_500_000, 1000)
	if w, err := e2.Process(context.Background(), "next"); err != nil || w.Inputs[0].TxID != txA {
		t.Fatalf("released input not reusable: %v %+v", err, w)
	}
	// The same file reloaded shows the releases were persisted: only ghost's
	// reservation remains, and txA is spent under next's broadcast, not held
	// for created.
	reloaded := utxo.NewSpentSet(spentPath, nil)
	if got := reloaded.ReservedIntents(); len(got) != 1 || got[0] != "ghost" || !reloaded.IsSpent(txA, 0) || reloaded.IsSpent(txB, 0) {
		t.Fatalf("on disk after recover: reservations %v, txA spent %v, txB spent %v", got, reloaded.IsSpent(txA, 0), reloaded.IsSpent(txB, 0))
	}
}

// A store that cannot be read keeps every reservation: a stale hold costs
// time, a released input under a Built intent costs money.
type unreadableStore struct{ *MemStore }

var errStoreDown = errors.New("database unreachable")

func (u *unreadableStore) Get(context.Context, string) (*Intent, bool, error) {
	return nil, false, errStoreDown
}

func TestRecoverKeepsReservationsWhenTheStoreCannotBeRead(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	if err := spent.Reserve([]types.UTXO{coins()[0]}, "maybe-built", time.Hour); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, &unreadableStore{NewMemStore()}, spent, &fakeNet{mode: "ok"}, coins())
	err := e.Recover(context.Background())
	// The store's own error is what the operator must see; a read failure is
	// not an unknown intent, so it must not be reported as one.
	if !errors.Is(err, errStoreDown) || errors.Is(err, ErrUnknownReservation) {
		t.Fatalf("recover over an unreadable store: %v, want the store's error and not ErrUnknownReservation", err)
	}
	if !spent.IsSpent(txA, 0) {
		t.Fatal("a reservation was released although the store could not say what its intent is")
	}
}

// A Built intent's reservation is never released by the orphan sweep, even
// for an instant: if Recover stops before re-reserving (the Built list
// failed), the inputs are still held and the operator stops as the guide
// says, with nothing exposed.
type builtListFailingStore struct{ *MemStore }

func (s *builtListFailingStore) List(_ context.Context, states ...State) ([]*Intent, error) {
	for _, st := range states {
		if st == StateBuilt {
			return nil, errors.New("database unreachable")
		}
	}
	return s.MemStore.List(context.Background(), states...)
}

func TestRecoverNeverReleasesABuiltIntentsReservation(t *testing.T) {
	store := &builtListFailingStore{NewMemStore()}
	spent := utxo.NewSpentSet("", nil)
	e := newEngine(t, store, spent, &fakeNet{mode: "lost"}, coins())
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	e.Process(context.Background(), "w1") // Built on txA, reply lost
	if w, _, _ := store.Get(context.Background(), "w1"); w.State != StateBuilt {
		t.Fatalf("setup: %+v", w)
	}
	if err := e.Recover(context.Background()); err == nil {
		t.Fatal("recover with a failing Built list returned nil")
	}
	if !spent.IsSpent(txA, 0) {
		t.Fatal("the orphan sweep released a Built intent's input")
	}
}

// The documented default: long, because Recover frees orphaned reservations
// and an expired reservation on a Built intent is a double-spend window.
func TestReservationTTLDefault(t *testing.T) {
	e := &Engine{}
	if e.reservationTTL() != DefaultReservationTTL || DefaultReservationTTL != 4*time.Hour {
		t.Fatalf("default TTL %v, documented as 4 hours", e.reservationTTL())
	}
}
