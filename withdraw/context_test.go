package withdraw

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// cancellingNet is a Broadcaster whose first send is interrupted by the
// caller's context: the bytes reach the network, the reply does not reach the
// caller. It reports the context's error bare, as a third-party Broadcaster
// might, so the engine's own classification is what is under test.
type cancellingNet struct {
	fakeNet
	cancel context.CancelFunc
	first  bool
}

func (c *cancellingNet) Broadcast(ctx context.Context, rawHex, txid string) (string, error) {
	c.mu.Lock()
	c.sent = append(c.sent, rawHex)
	first := !c.first
	c.first = true
	c.mu.Unlock()
	if first {
		c.cancel()
		<-ctx.Done()
		return "", ctx.Err()
	}
	return txid, nil
}

// The money seam. A context that ends during the broadcast is a lost reply:
// the intent stays Built with its reservation renewed, a restart sends the
// same bytes, the builder never runs again, and the network sees one
// transaction twice, never two transactions.
func TestCancelledBroadcastIsHeldAndRecoverSendsTheSameBytes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	spentPath := filepath.Join(dir, "spent.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	net := &cancellingNet{cancel: cancel}
	e := newEngine(t, store, utxo.NewSpentSet(spentPath, nil), &net.fakeNet, coins())
	e.Broadcaster = net

	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	in, err := e.Process(ctx, "w1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled broadcast: %v, want the context's error back", err)
	}
	if errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("cancelled broadcast classified as permanent: %v", err)
	}
	if in.State != StateBuilt || in.RawHex == "" || in.Attempts != 1 || in.LastError == "" {
		t.Fatalf("after the cancel: %+v, want Built with the attempt recorded", in)
	}
	if !e.Spent.IsSpent(in.Inputs[0].TxID, in.Inputs[0].Vout) {
		t.Fatal("the reservation of a Built intent was released by a cancel")
	}
	persisted, _, _ := store.Get(context.Background(), "w1")
	if persisted.State != StateBuilt || persisted.RawHex != in.RawHex {
		t.Fatalf("persisted %+v, want the Built intent with its bytes", persisted)
	}

	// Restart: a new engine over the same files, a live context, a network
	// that answers. Recover sends the persisted bytes; nothing is rebuilt.
	store2, err := NewFileStore(filepath.Join(dir, "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	e2 := newEngine(t, store2, utxo.NewSpentSet(spentPath, nil), &net.fakeNet, coins())
	e2.Broadcaster = net
	e2.BuildSign = func(context.Context, []types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("Recover rebuilt a transaction after a cancelled broadcast")
		return "", "", nil
	}
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	after, _, _ := store2.Get(context.Background(), "w1")
	if after.State != StateBroadcast || after.TxID != in.TxID || after.RawHex != in.RawHex {
		t.Fatalf("recovered %+v, want Broadcast under the same txid and bytes", after)
	}
	if got := net.sentCount(); got != 2 {
		t.Fatalf("the network saw %d sends, want 2 of the same bytes", got)
	}
	if net.sent[0] != net.sent[1] {
		t.Fatalf("the retry sent different bytes: %s then %s", net.sent[0], net.sent[1])
	}
	if !e2.Spent.IsSpent(in.Inputs[0].TxID, in.Inputs[0].Vout) {
		t.Error("inputs of the recovered broadcast are not marked spent")
	}
}

// A third-party Broadcaster that wraps the context's error in something the
// engine would otherwise read as permanent must still not fail the intent.
// The context check comes before the permanent branch.
func TestCancelledBroadcastWrappedAsPermanentIsStillHeld(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.Broadcaster = broadcastFunc(func(ctx context.Context, rawHex, txid string) (string, error) {
		return "", fmt.Errorf("broadcast: %w: %w", rpc.ErrPermanent, context.Canceled)
	})
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	in, err := e.Process(context.Background(), "w1")
	if err == nil || in.State != StateBuilt {
		t.Fatalf("state %s err %v, want Built and the error back", in.State, err)
	}
	if !spent.IsSpent(in.Inputs[0].TxID, in.Inputs[0].Vout) {
		t.Fatal("a context error dressed as permanent released the reservation")
	}
}

type broadcastFunc func(ctx context.Context, rawHex, txid string) (string, error)

func (f broadcastFunc) Broadcast(ctx context.Context, rawHex, txid string) (string, error) {
	return f(ctx, rawHex, txid)
}

// The transient-error rule of v0.3.6, extended: a context that ends while the
// selector is working keeps the intent Created, whether the selector returns
// the context's error bare, wrapped in a transport error, or any other error
// once the context has ended. Nothing is reserved, nothing is built.
func TestCancelledSelectKeepsTheIntentCreated(t *testing.T) {
	cases := []struct {
		name string
		err  func(ctx context.Context) error
	}{
		{"bare context error", func(ctx context.Context) error { return ctx.Err() }},
		{"wrapped context error", func(ctx context.Context) error { return fmt.Errorf("gettxout: %w", ctx.Err()) }},
		{"deadline", func(context.Context) error { return fmt.Errorf("send request: %w", context.DeadlineExceeded) }},
		{"unrelated error after the context ended", func(context.Context) error { return errors.New("insufficient funds: have 0, need 1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spent := utxo.NewSpentSet("", nil)
			net := &fakeNet{mode: "ok"}
			e := newEngine(t, NewMemStore(), spent, net, coins())
			realSelect := e.Select
			e.Select = func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
				if ctx.Err() != nil {
					return nil, tc.err(ctx)
				}
				return realSelect(ctx, amount, feeRate)
			}
			e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			in, err := e.Process(ctx, "w1")
			if err == nil {
				t.Fatal("a cancelled Build returned no error")
			}
			if in.State != StateCreated || in.Attempts != 1 || in.LastError == "" || in.RawHex != "" || in.Inputs != nil {
				t.Fatalf("%+v, want Created with the attempt recorded and nothing built", in)
			}
			if spent.IsSpent(txA, 0) || spent.IsSpent(txB, 0) || spent.IsSpent(txC, 0) || net.builds != 0 || net.sentCount() != 0 {
				t.Fatal("a cancelled Build reserved, built or sent something")
			}
			// With a live context the same intent goes through under its id.
			in, err = e.Process(context.Background(), "w1")
			if err != nil || in.State != StateBroadcast || in.Attempts != 2 {
				t.Fatalf("after the cancel: %v %+v", err, in)
			}
		})
	}
}

// A remote signer whose reply is lost to the context has handed nothing to
// this process, so nothing can be on the network: the reservation is released
// and the intent stays Created. A signer that refuses for its own reasons
// still fails the intent.
func TestCancelledBuildSignReleasesAndKeepsCreated(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	realSign := e.BuildSign
	var cancelSigning context.CancelFunc
	e.BuildSign = func(ctx context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
		if cancelSigning != nil {
			cancelSigning()
			cancelSigning = nil
			return "", "", fmt.Errorf("signer: %w", ctx.Err())
		}
		return realSign(ctx, inputs, to, amount, feeRate)
	}
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelSigning = cancel
	in, err := e.Process(ctx, "w1")
	if !errors.Is(err, context.Canceled) || in.State != StateCreated {
		t.Fatalf("cancelled signing: %v state %s, want the context's error and Created", err, in.State)
	}
	if spent.IsSpent(txA, 0) {
		t.Fatal("the reservation taken before signing was not released")
	}
	if net.sentCount() != 0 {
		t.Fatal("something was sent after a cancelled signing")
	}
	in, err = e.Process(context.Background(), "w1")
	if err != nil || in.State != StateBroadcast {
		t.Fatalf("retry: %v %+v", err, in)
	}

	// The control: a signer error that is not the context's fails the intent.
	e2 := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	e2.BuildSign = func(context.Context, []types.UTXO, string, int64, int64) (string, string, error) {
		return "", "", errors.New("key not in keystore")
	}
	e2.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if in, err := e2.Process(context.Background(), "w1"); err == nil || in.State != StateFailed {
		t.Fatalf("signer refusal: %v state %s, want Failed", err, in.State)
	}
}

// Recover's local passes always complete; its re-broadcast pass stops once
// the context has ended, leaving every Built intent Built and re-reserved
// for the next Recover. Nothing is sent, nothing is failed.
func TestRecoverStopsSendingOnceTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(filepath.Join(dir, "intents.json"))
	spentPath := filepath.Join(dir, "spent.json")
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, store, utxo.NewSpentSet(spentPath, nil), net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	e.Submit(context.Background(), "w2", dst, 1_000_000, 1000)
	e.Process(context.Background(), "w1")
	e.Process(context.Background(), "w2")
	if got := net.sentCount(); got != 2 {
		t.Fatalf("setup: %d sends", got)
	}

	store2, _ := NewFileStore(filepath.Join(dir, "intents.json"))
	net2 := &fakeNet{mode: "ok"}
	spent2 := utxo.NewSpentSet(spentPath, nil)
	e2 := newEngine(t, store2, spent2, net2, coins())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e2.Recover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("recover with an ended context: %v, want the context's error among the results", err)
	}
	if net2.sentCount() != 0 {
		t.Fatalf("recover sent %d transactions under an ended context", net2.sentCount())
	}
	for _, id := range []string{"w1", "w2"} {
		in, _, _ := store2.Get(context.Background(), id)
		if in.State != StateBuilt {
			t.Fatalf("%s is %s after an interrupted Recover, want Built", id, in.State)
		}
		if !spent2.IsSpent(in.Inputs[0].TxID, in.Inputs[0].Vout) {
			t.Fatalf("%s lost its reservation in an interrupted Recover", id)
		}
	}
	// The next Recover, with a live context, sends both.
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if net2.sentCount() != 2 {
		t.Fatalf("second recover sent %d, want 2", net2.sentCount())
	}
}

// ctxStore is a Store that honours its context, as a database-backed store
// does: a Get, Put or List under an ended context fails with its error.
type ctxStore struct{ *MemStore }

func (s ctxStore) Get(ctx context.Context, id string) (*Intent, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return s.MemStore.Get(ctx, id)
}

func (s ctxStore) Put(ctx context.Context, in *Intent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemStore.Put(ctx, in)
}

func (s ctxStore) List(ctx context.Context, states ...State) ([]*Intent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.MemStore.List(ctx, states...)
}

// The record of what the network did must land even when the caller's context
// has ended by the time it is written. Here the node accepts the bytes under
// another txid and the caller's context ends in the same instant; the store
// honours contexts. The hold (NodeTxID) must reach the store, or a restart
// would re-send an intent the operator has to resolve by hand.
func TestStateRecordsAreWrittenAfterTheContextEnds(t *testing.T) {
	store := ctxStore{NewMemStore()}
	spent := utxo.NewSpentSet("", nil)
	e := newEngine(t, store, spent, &fakeNet{mode: "ok"}, coins())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Broadcaster = broadcastFunc(func(ctx context.Context, rawHex, txid string) (string, error) {
		cancel() // the reply and the cancel arrive together
		return "node-" + txid, fmt.Errorf("broadcast: %w: node returned txid %s", rpc.ErrTxIDMismatch, "node-"+txid)
	})
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	in, _, _ := store.Get(context.Background(), "w1")
	if err := e.Build(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	err := e.Broadcast(ctx, in)
	if !errors.Is(err, rpc.ErrTxIDMismatch) || errors.Is(err, context.Canceled) {
		t.Fatalf("mismatch with a cancel: %v; the save must not have failed on the context", err)
	}
	persisted, _, _ := store.Get(context.Background(), "w1")
	if persisted.State != StateBuilt || persisted.NodeTxID != "node-"+in.TxID {
		t.Fatalf("persisted %+v; the hold was lost to the caller's cancel", persisted)
	}
	// And a lost reply's attempt record lands the same way.
	e2 := newEngine(t, ctxStore{NewMemStore()}, utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	e2.Broadcaster = broadcastFunc(func(ctx context.Context, rawHex, txid string) (string, error) {
		cancel2()
		return "", ctx.Err()
	})
	e2.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	in2, err := e2.Process(ctx2, "w1")
	if !errors.Is(err, context.Canceled) || in2.State != StateBuilt {
		t.Fatalf("cancelled broadcast: %v %s", err, in2.State)
	}
	p2, _, _ := e2.Store.Get(context.Background(), "w1")
	if p2.Attempts != 1 || p2.LastError == "" {
		t.Fatalf("attempt record lost to the cancel: %+v", p2)
	}
}

// Recover's repair passes run whatever the caller's context says; only the
// re-send loop stops. With a store that honours contexts and a Recover
// called under an ended context, every Broadcast intent's inputs are still
// re-marked and the orphan reservation is still released.
func TestRecoverRepairsTheSpentSetUnderAnEndedContext(t *testing.T) {
	mem := NewMemStore()
	spent := utxo.NewSpentSet("", nil)
	e := newEngine(t, ctxStore{mem}, spent, &fakeNet{mode: "ok"}, coins())
	e.Submit(context.Background(), "sent", dst, 1_000_000, 1000)
	sent, err := e.Process(context.Background(), "sent")
	if err != nil || sent.State != StateBroadcast {
		t.Fatalf("setup: %v %+v", err, sent)
	}
	// A Created intent holding a reservation: the previous process stopped
	// between reserving and persisting Built.
	e.Submit(context.Background(), "orphan", dst, 1_000_000, 1000)
	if err := spent.Reserve([]types.UTXO{coins()[1]}, "orphan", time.Hour); err != nil {
		t.Fatal(err)
	}

	// New process: an empty spent set, the same store, an ended context.
	spent2 := utxo.NewSpentSet("", nil)
	if err := spent2.Reserve([]types.UTXO{coins()[1]}, "orphan", time.Hour); err != nil {
		t.Fatal(err)
	}
	e2 := newEngine(t, ctxStore{mem}, spent2, &fakeNet{mode: "ok"}, coins())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = e2.Recover(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("recover: %v", err)
	}
	if !spent2.IsSpent(sent.Inputs[0].TxID, sent.Inputs[0].Vout) {
		t.Fatal("a Broadcast intent's inputs were not re-marked because the context had ended")
	}
	if spent2.IsSpent(coins()[1].TxID, coins()[1].Vout) {
		t.Fatal("the orphan reservation was not released because the context had ended")
	}
}

// The context checks in Build and Broadcast also cover an error that carries
// a context error from somewhere else: a selector or a remote signer with a
// per-call deadline of its own, or a Broadcaster reporting one, while the
// caller's context is still live. Deleting the errors.Is checks and keeping
// only ctx.Err() would fail these.
func TestForeignContextErrorsAreTransient(t *testing.T) {
	// Selector's own deadline: stays Created.
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	realSelect := e.Select
	first := true
	e.Select = func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
		if first {
			first = false
			return nil, fmt.Errorf("indexer: %w", context.DeadlineExceeded)
		}
		return realSelect(ctx, amount, feeRate)
	}
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if in, err := e.Process(context.Background(), "w1"); err == nil || in.State != StateCreated {
		t.Fatalf("selector deadline: %v %s, want Created", err, in.State)
	}
	if in, err := e.Process(context.Background(), "w1"); err != nil || in.State != StateBroadcast {
		t.Fatalf("retry: %v %s", err, in.State)
	}

	// Signer's own deadline: reservation released, stays Created.
	e = newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	realSign := e.BuildSign
	first = true
	e.BuildSign = func(ctx context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
		if first {
			first = false
			return "", "", fmt.Errorf("hsm: %w", context.DeadlineExceeded)
		}
		return realSign(ctx, inputs, to, amount, feeRate)
	}
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if in, err := e.Process(context.Background(), "w1"); err == nil || in.State != StateCreated || e.Spent.IsSpent(txA, 0) {
		t.Fatalf("signer deadline: %v %s reserved=%v, want Created and released", err, in.State, e.Spent.IsSpent(txA, 0))
	}
	if in, err := e.Process(context.Background(), "w1"); err != nil || in.State != StateBroadcast {
		t.Fatalf("retry: %v %s", err, in.State)
	}

	// Broadcaster's own deadline: held Built, reservation kept.
	e = newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	e.Broadcaster = broadcastFunc(func(context.Context, string, string) (string, error) {
		return "", fmt.Errorf("node: %w", context.DeadlineExceeded)
	})
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	in, err := e.Process(context.Background(), "w1")
	if !errors.Is(err, context.DeadlineExceeded) || in.State != StateBuilt || !e.Spent.IsSpent(in.Inputs[0].TxID, in.Inputs[0].Vout) {
		t.Fatalf("broadcaster deadline: %v %s", err, in.State)
	}
}
