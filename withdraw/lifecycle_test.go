package withdraw

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// fakeChain answers Abandon's questions from two maps.
type fakeChain struct {
	known map[string]bool
	spent map[string]bool // "txid:vout"
	err   error
	calls int
}

func (c *fakeChain) KnowsTransaction(_ context.Context, txid string) (bool, error) {
	c.calls++
	return c.known[txid], c.err
}

func (c *fakeChain) Unspent(_ context.Context, txid string, vout uint32) (bool, error) {
	c.calls++
	return !c.spent[fmt.Sprintf("%s:%d", txid, vout)], c.err
}

// A rejection on the attempt after a lost reply holds the intent. Peers may
// hold the first transmission for the node's mempool expiry and mine it after
// the node forgot it; failing the intent and releasing its inputs would let
// the exchange pay again from other coins while the first payment can still
// confirm. Nothing is sent for a held intent, and its inputs stay spent.
func TestRejectionAfterALostReplyHoldsTheIntent(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.ReservationTTL = 20 * time.Millisecond // a mere reservation would lapse below
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("lost reply: %v", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if !w1.MaybeRelayed || w1.State != StateBuilt {
		t.Fatalf("after the lost reply %+v: want Built with MaybeRelayed", w1)
	}

	net.setMode("reject")
	_, err := e.Process(context.Background(), "w1")
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("rejection after a lost reply: %v, want ErrHeld", err)
	}
	if errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("ErrHeld wraps the rejection, so the breaker reads a held intent as one bad request: %v", err)
	}
	w1, _, _ = e.Store.Get(context.Background(), "w1")
	if w1.State != StateBuilt || w1.Hold != HoldRejectedAfterUnknown || w1.RawHex == "" || w1.Attempts != 2 {
		t.Fatalf("after the rejection %+v: want Built, held, bytes kept, two attempts", w1)
	}
	if !strings.Contains(w1.LastError, "min relay fee") {
		t.Fatalf("LastError does not record the rejection: %q", w1.LastError)
	}
	time.Sleep(40 * time.Millisecond)
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("inputs free after the TTL although the bytes may be on the network")
	}

	// Nothing is sent for it again, by Process or by Recover, whatever the node
	// would now answer, and the spent entry survives Recover.
	net.setMode("ok")
	sent := net.sentCount()
	if _, err := e.Process(context.Background(), "w1"); !errors.Is(err, ErrHeld) {
		t.Fatalf("process of a held intent: %v, want ErrHeld", err)
	}
	if err := e.Recover(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatalf("recover over a held intent: %v, want ErrHeld reported", err)
	}
	if net.sentCount() != sent {
		t.Fatalf("a held intent was sent %d times", net.sentCount()-sent)
	}
	if !spent.IsSpent(w1.Inputs[0].TxID, w1.Inputs[0].Vout) {
		t.Fatal("recover disturbed the spent entry of a held intent")
	}
	// And the coins it holds are not available to the next withdrawal.
	e.Submit(context.Background(), "w2", dst, 4_500_000, 1000)
	if w2, err := e.Process(context.Background(), "w2"); err == nil || w2.State != StateFailed {
		t.Fatalf("w2 %+v %v: built on inputs a held intent may have spent", w2, err)
	}
}

// ctxNet reports every send as ended by the context, however it is wrapped.
type ctxNet struct{}

func (ctxNet) Broadcast(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("post: %w", context.DeadlineExceeded)
}

// A send the context ended is a lost reply, and it marks the intent as
// possibly relayed like one.
func TestContextEndingInTheSendMarksTheIntentMaybeRelayed(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{}, coins())
	e.Broadcaster = ctxNet{}
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if _, err := e.Process(context.Background(), "w1"); !contextEnded(err) {
		t.Fatalf("process: %v", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if w1.State != StateBuilt || !w1.MaybeRelayed {
		t.Fatalf("%+v: want Built with MaybeRelayed", w1)
	}
}

// A node that refuses the credentials has taken nothing: the intent stays
// Built and is not marked as possibly relayed, so a later rejection can still
// fail it, and the same bytes go out once the credential is fixed.
func TestUnauthorizedNodeHoldsTheIntentWithoutAVerdict(t *testing.T) {
	net := &fakeNet{mode: "unauthorized"}
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	_, err := e.Process(context.Background(), "w1")
	if !errors.Is(err, rpc.ErrUnauthorized) || errors.Is(err, rpc.ErrUnknownOutcome) || errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("unauthorized: %v, want ErrUnauthorized alone", err)
	}
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if w1.State != StateBuilt || w1.MaybeRelayed || w1.Attempts != 1 || w1.LastError == "" {
		t.Fatalf("%+v: want Built, not relayed, one attempt recorded", w1)
	}
	net.setMode("ok")
	if _, err := e.Process(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	w1, _, _ = e.Store.Get(context.Background(), "w1")
	if w1.State != StateBroadcast || net.sentCount() != 2 || net.sent[0] != net.sent[1] {
		t.Fatalf("%+v sent %v: want Broadcast with the same bytes", w1, net.sent)
	}
}

// Rebroadcast sends the stored bytes of a Broadcast intent and changes no
// state on any outcome. The node forgetting a transaction is not proof that
// the network has, so no reply here is a verdict.
func TestRebroadcastSendsTheSameBytesAndChangesNoState(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	w1, err := e.Process(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	first := net.sent[0]
	for i, mode := range []string{"ok", "lost", "reject", "transient", "unauthorized"} {
		net.setMode(mode)
		err := e.Rebroadcast(context.Background(), w1)
		stored, _, _ := e.Store.Get(context.Background(), "w1")
		if stored.State != StateBroadcast || stored.Attempts != i+2 || net.sent[len(net.sent)-1] != first {
			t.Fatalf("%s: %+v, sent %v: want Broadcast, the attempt counted, the same bytes", mode, stored, net.sent)
		}
		if !spent.IsSpent(stored.Inputs[0].TxID, stored.Inputs[0].Vout) {
			t.Fatalf("%s: inputs freed by a rebroadcast", mode)
		}
		if mode == "ok" {
			if err != nil || stored.LastError != "" {
				t.Fatalf("ok: err %v, LastError %q", err, stored.LastError)
			}
			continue
		}
		if err == nil || !strings.Contains(stored.LastError, "rebroadcast") {
			t.Fatalf("%s: err %v, LastError %q: the refusal must be reported and recorded", mode, err, stored.LastError)
		}
	}
	// Only a Broadcast intent has bytes the node once took; a Built one goes
	// through Broadcast and its hold check.
	e.Submit(context.Background(), "w2", dst, 1_000_000, 1000)
	w2, _, _ := e.Store.Get(context.Background(), "w2")
	if err := e.Build(context.Background(), w2); err != nil {
		t.Fatal(err)
	}
	sent := net.sentCount()
	if err := e.Rebroadcast(context.Background(), w2); !errors.Is(err, ErrWrongState) || net.sentCount() != sent {
		t.Fatalf("rebroadcast of a Built intent: %v, sent %d", err, net.sentCount()-sent)
	}
}

// backdate writes the stored record with UpdatedAt moved into the past, as a
// record that has sat untouched for that long would read.
func backdate(t *testing.T, store Store, id string, by time.Duration) {
	t.Helper()
	in, _, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	in.UpdatedAt = in.UpdatedAt.Add(-by)
	if err := store.Update(context.Background(), in, in.State); err != nil {
		t.Fatal(err)
	}
}

func outpoint(o Outpoint) string { return fmt.Sprintf("%s:%d", o.TxID, o.Vout) }

// Abandon frees an intent's inputs only when every check passes, and each
// refusal leaves the record and the spent set as they were.
func TestAbandonRefusesUntilEveryCheckPasses(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	chain := &fakeChain{known: map[string]bool{}, spent: map[string]bool{}}
	e.Chain = chain
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	w1, err := e.Process(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	input := w1.Inputs[0]
	unchanged := func(step string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotAbandonable) {
			t.Fatalf("%s: %v, want ErrNotAbandonable", step, err)
		}
		stored, _, _ := e.Store.Get(context.Background(), "w1")
		if stored.State != StateBroadcast || !spent.IsSpent(input.TxID, input.Vout) {
			t.Fatalf("%s: refused and yet changed: %+v spent=%v", step, stored, spent.IsSpent(input.TxID, input.Vout))
		}
	}

	e.Chain = nil
	if err := e.Abandon(context.Background(), w1); err == nil || errors.Is(err, ErrNotAbandonable) {
		t.Fatalf("without a Chain: %v", err)
	}
	e.Chain = chain
	unchanged("before the wait", e.Abandon(context.Background(), w1))
	if chain.calls != 0 {
		t.Fatal("the node was asked before the wait was over")
	}
	backdate(t, e.Store, "w1", DefaultAbandonAfter+time.Minute)

	chain.known[w1.TxID] = true
	unchanged("while the node knows the transaction", e.Abandon(context.Background(), w1))
	chain.known[w1.TxID] = false

	chain.spent[outpoint(input)] = true
	unchanged("while an input is spent", e.Abandon(context.Background(), w1))
	delete(chain.spent, outpoint(input))

	chain.err = rpc.ErrNodeSyncing
	if err := e.Abandon(context.Background(), w1); !errors.Is(err, rpc.ErrNodeSyncing) || errors.Is(err, ErrNotAbandonable) {
		t.Fatalf("with a node behind: %v, want the node's error as it is", err)
	}
	chain.err = nil
	if stored, _, _ := e.Store.Get(context.Background(), "w1"); stored.State != StateBroadcast {
		t.Fatalf("a Chain error changed the record: %+v", stored)
	}

	stale := *w1 // a second operator's copy, read before the first abandons
	if err := e.Abandon(context.Background(), w1); err != nil {
		t.Fatalf("every check passes: %v", err)
	}
	stored, _, _ := e.Store.Get(context.Background(), "w1")
	if stored.State != StateFailed || stored.RawHex == "" || stored.TxID == "" || !strings.Contains(stored.LastError, "abandoned") {
		t.Fatalf("after abandon %+v: want Failed with the bytes and the record of it", stored)
	}
	if spent.IsSpent(input.TxID, input.Vout) {
		t.Fatal("abandon left the inputs spent")
	}
	if err := e.Abandon(context.Background(), &stale); !errors.Is(err, ErrWrongState) {
		t.Fatalf("second operator: %v, want ErrWrongState", err)
	}
	// The inputs are back in selection.
	e.Submit(context.Background(), "w2", dst, 4_500_000, 1000)
	if w2, err := e.Process(context.Background(), "w2"); err != nil || w2.Inputs[0].TxID != input.TxID {
		t.Fatalf("w2 %+v %v: the abandoned inputs are not selectable", w2, err)
	}
}

// A held Built intent is abandoned the same way, with the node asked about
// both txids; a Built intent that is not held belongs to Broadcast.
func TestAbandonOfAHeldBuiltIntentAsksAboutBothTxIDs(t *testing.T) {
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "mismatch"}
	e := newEngine(t, NewMemStore(), spent, net, coins())
	chain := &fakeChain{known: map[string]bool{}, spent: map[string]bool{}}
	e.Chain = chain
	e.Submit(context.Background(), "w1", dst, 4_500_000, 1000)
	e.Process(context.Background(), "w1")
	w1, _, _ := e.Store.Get(context.Background(), "w1")
	if w1.NodeTxID == "" {
		t.Fatalf("not held: %+v", w1)
	}
	backdate(t, e.Store, "w1", DefaultAbandonAfter+time.Minute)
	chain.known[w1.NodeTxID] = true
	if err := e.Abandon(context.Background(), w1); !errors.Is(err, ErrNotAbandonable) {
		t.Fatalf("the node knows the txid it accepted: %v", err)
	}
	chain.known[w1.NodeTxID] = false
	if err := e.Abandon(context.Background(), w1); err != nil {
		t.Fatal(err)
	}
	if stored, _, _ := e.Store.Get(context.Background(), "w1"); stored.State != StateFailed || spent.IsSpent(stored.Inputs[0].TxID, stored.Inputs[0].Vout) {
		t.Fatalf("%+v: want Failed with the inputs free", stored)
	}

	net.setMode("ok")
	e.Submit(context.Background(), "w2", dst, 1_000_000, 1000)
	w2, _, _ := e.Store.Get(context.Background(), "w2")
	if err := e.Build(context.Background(), w2); err != nil {
		t.Fatal(err)
	}
	backdate(t, e.Store, "w2", DefaultAbandonAfter+time.Minute)
	if err := e.Abandon(context.Background(), w2); !errors.Is(err, ErrWrongState) {
		t.Fatalf("a Built intent that is not held: %v, want ErrWrongState", err)
	}
}

type countingConfirmer struct{ calls int }

func (c *countingConfirmer) Confirmations(context.Context, string) (int64, error) {
	c.calls++
	return 100, nil
}

// With RequiredConfirmations at zero nothing would ever confirm and the spent
// set would only grow; the misconfiguration is refused before the node is asked.
func TestZeroRequiredConfirmationsIsRefused(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	confirmer := &countingConfirmer{}
	e.Confirmer, e.RequiredConfirmations = confirmer, 0
	e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	w1, err := e.Process(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateConfirmations(context.Background(), w1); err == nil {
		t.Fatal("RequiredConfirmations 0 accepted")
	}
	if stored, _, _ := e.Store.Get(context.Background(), "w1"); stored.State != StateBroadcast || confirmer.calls != 0 {
		t.Fatalf("%+v, confirmer called %d times: want Broadcast and no node call", stored, confirmer.calls)
	}
}
