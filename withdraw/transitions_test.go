package withdraw

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// gatedStore holds the first Create inside the store until released, which
// keeps one Submit in flight while a second Submit of the same id, and the
// worker serving it, run the intent to Broadcast.
type gatedStore struct {
	*MemStore
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedStore) Create(ctx context.Context, in *Intent) error {
	g.mu.Lock()
	arm := g.armed
	g.armed = false
	g.mu.Unlock()
	if arm {
		close(g.entered)
		<-g.release
	}
	return g.MemStore.Create(ctx, in)
}

// A client retries a withdrawal request under the same idempotency key while
// the first request is still in flight, and a worker serves the retry to
// Broadcast before the first request reaches the store. The first request's
// write must not land as Created over the sent record: the next Process would
// then build a second transaction from other inputs and pay the recipient
// twice, which is the first failure mode the package header names.
func TestDuplicateSubmitInFlightNeverPaysTwice(t *testing.T) {
	ctx := context.Background()
	store := &gatedStore{MemStore: NewMemStore(), armed: true, entered: make(chan struct{}), release: make(chan struct{})}
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, utxo.NewSpentSet("", nil), net, coins())

	type result struct {
		in      *Intent
		created bool
		err     error
	}
	slow := make(chan result, 1)
	go func() {
		in, created, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000)
		slow <- result{in, created, err}
	}()
	<-store.entered

	if _, created, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000); err != nil || !created {
		t.Fatalf("the retry's submit: created=%v err=%v", created, err)
	}
	first, err := e.Process(ctx, "w1")
	if err != nil || first.State != StateBroadcast {
		t.Fatalf("the retry's process: err=%v state=%s", err, first.State)
	}

	close(store.release)
	r := <-slow
	if r.err != nil || r.created {
		t.Fatalf("the first submit, landing after the send: created=%v err=%v; want the stored record with created=false", r.created, r.err)
	}
	if r.in.State != StateBroadcast || r.in.TxID != first.TxID {
		t.Fatalf("the first submit answered %s %q; want the Broadcast record %q", r.in.State, r.in.TxID, first.TxID)
	}
	stored, _, _ := store.Get(ctx, "w1")
	if stored.State != StateBroadcast {
		t.Fatalf("stored state is %s after the late submit; want Broadcast", stored.State)
	}
	// A later redelivery finds the sent intent and builds nothing.
	if _, err := e.Process(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if net.builds != 1 || net.sentCount() != 1 {
		t.Fatalf("built %d and sent %d transactions for one idempotency key; want 1 and 1", net.builds, net.sentCount())
	}
}

// Two workers hold the Built record of one intent: a queue redelivery while
// the first is mid-broadcast, or Recover running beside a worker. The first
// sends and the store says Broadcast. The second must stop on the stored
// state. Without the re-read it sends the same bytes, its lost reply renews a
// reservation over inputs the first marked spent, and it saves Built over the
// Broadcast the first recorded; before Reserve accepted the intent's own sent
// entries, it also reported ErrReservationLost naming a conflicting
// withdrawal that does not exist, which the guide says halts every payout.
func TestSecondSenderOfABuiltIntentStopsOnTheStoredState(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, utxo.NewSpentSet("", nil), net, coins())
	if _, _, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	w, _, _ := store.Get(ctx, "w1")
	if err := e.Build(ctx, w); err != nil {
		t.Fatal(err)
	}
	copyA, _, _ := store.Get(ctx, "w1")
	copyB, _, _ := store.Get(ctx, "w1")

	if err := e.Broadcast(ctx, copyA); err != nil {
		t.Fatal(err)
	}
	net.setMode("lost")
	err := e.Broadcast(ctx, copyB)
	if !errors.Is(err, ErrWrongState) {
		t.Fatalf("the second sender got %v; want ErrWrongState from the stored record", err)
	}
	if errors.Is(err, ErrReservationLost) {
		t.Fatalf("the second sender named a conflicting withdrawal that does not exist: %v", err)
	}
	stored, _, _ := store.Get(ctx, "w1")
	if stored.State != StateBroadcast || stored.Attempts != 1 {
		t.Fatalf("stored record is %s after %d attempts; want Broadcast after the one attempt that sent it", stored.State, stored.Attempts)
	}
	if net.sentCount() != 1 {
		t.Fatalf("sent %d times for one Built intent; want 1", net.sentCount())
	}
}

// updateFailOnce fails the first Update that writes the given state, as a
// disk that fills between the node's answer and the record of it does, and
// stores nothing for that call.
type updateFailOnce struct {
	*MemStore
	state State
	done  bool
}

func (p *updateFailOnce) Update(ctx context.Context, in *Intent, from State) error {
	if !p.done && in.State == p.state {
		p.done = true
		return errors.New("disk full")
	}
	return p.MemStore.Update(ctx, in, from)
}

// A send succeeds, the Broadcast save fails, and the store still says Built
// while the spent set holds the inputs as sent under the intent's own txid.
// The retry's reply is lost. Renewing the reservation then meets the intent's
// own sent entries, which are not another withdrawal taking the inputs and
// must not be reported as one, nor turned into a reservation that expires.
func TestRetryAfterOwnBroadcastSaveFailedIsNotAReservationLoss(t *testing.T) {
	ctx := context.Background()
	store := &updateFailOnce{MemStore: NewMemStore(), state: StateBroadcast}
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, spent, net, coins())
	if _, _, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	got, err := e.Process(ctx, "w1")
	if err == nil || got.State != StateBroadcast {
		t.Fatalf("first process: err=%v state=%s; want the send in the copy and the save error returned", err, got.State)
	}
	stored, _, _ := store.Get(ctx, "w1")
	if stored.State != StateBuilt {
		t.Fatalf("store after the failed save: %s, want Built", stored.State)
	}
	for _, o := range got.Inputs {
		if !spent.IsSpent(o.TxID, o.Vout) {
			t.Fatalf("input %s:%d of a sent transaction is not held", o.TxID, o.Vout)
		}
	}

	net.setMode("lost")
	_, err = e.Process(ctx, "w1")
	if !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("the retry: %v, want the lost reply reported", err)
	}
	if errors.Is(err, ErrReservationLost) {
		t.Fatalf("the retry read the intent's own sent inputs as another withdrawal's: %v", err)
	}

	net.setMode("ok")
	got, err = e.Process(ctx, "w1")
	if err != nil || got.State != StateBroadcast {
		t.Fatalf("the next retry: err=%v state=%s; want Broadcast recorded", err, got.State)
	}
	for _, hex := range net.sent {
		if hex != got.RawHex {
			t.Fatalf("a retry sent different bytes: %s vs %s", hex, got.RawHex)
		}
	}
}

// queuedConfirmer answers each call from the front of its queue.
type queuedConfirmer struct{ q []int64 }

func (c *queuedConfirmer) Confirmations(context.Context, string) (int64, error) {
	n := c.q[0]
	c.q = c.q[1:]
	return n, nil
}

// Two confirmation loops hold copies of one Broadcast intent. The first reads
// the node at the threshold and saves Confirmed. The second loop read the node
// one block earlier, or a node one block behind, and saves after. Confirmed is
// terminal: the second must write nothing, so a ledger acting on the
// Confirmed transition sees it once and the record never reads Broadcast
// again after it read Confirmed.
func TestStaleConfirmationUpdateDoesNotRegressAConfirmedRecord(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, utxo.NewSpentSet("", nil), net, coins()) // RequiredConfirmations 3
	e.Confirmer = &queuedConfirmer{q: []int64{3, 2}}
	if _, _, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Process(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	copyA, _, _ := store.Get(ctx, "w1")
	copyB, _, _ := store.Get(ctx, "w1")

	if err := e.UpdateConfirmations(ctx, copyA); err != nil || copyA.State != StateConfirmed {
		t.Fatalf("first loop: err=%v state=%s; want Confirmed", err, copyA.State)
	}
	err := e.UpdateConfirmations(ctx, copyB)
	if !errors.Is(err, ErrWrongState) {
		t.Fatalf("second loop, on a copy the store has moved past: %v, want ErrWrongState", err)
	}
	stored, _, _ := store.Get(ctx, "w1")
	if stored.State != StateConfirmed || stored.Confirmations != 3 {
		t.Fatalf("stored record is %s at %d confirmations; want Confirmed at 3", stored.State, stored.Confirmations)
	}
}

// Two Built intents in the store share an input: the first one's reservation
// lapsed while nothing retried it, and the second selected the same coin.
// Recover re-reserves the older one and sends it. The younger one's inputs
// are then held by the older one's send, so it cannot be re-reserved, and it
// must not be sent: two signed transactions over one input is the case
// ErrReservationLost exists for, and the guide promises that every Built
// intent Recover sends has been re-reserved first.
func TestRecoverDoesNotSendABuiltIntentItCouldNotReReserve(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	shared := Outpoint{TxID: txA, Vout: 0, Value: 5_000_000, Address: "ssq1pa"}
	older := &Intent{ID: "older", Address: dst, Amount: 1, FeeRate: 1, State: StateBuilt, TxID: "txid-older", RawHex: "hex-older", Inputs: []Outpoint{shared}, CreatedAt: time.Now().Add(-time.Hour)}
	younger := &Intent{ID: "younger", Address: dst, Amount: 1, FeeRate: 1, State: StateBuilt, TxID: "txid-younger", RawHex: "hex-younger", Inputs: []Outpoint{shared}, CreatedAt: time.Now()}
	for _, in := range []*Intent{older, younger} {
		if err := store.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	net := &fakeNet{mode: "ok"}
	e := &Engine{Store: store, Spent: utxo.NewSpentSet("", nil), Broadcaster: net}

	err := e.Recover(ctx)
	if !errors.Is(err, ErrReservationLost) {
		t.Fatalf("recover: %v, want ErrReservationLost for the intent it could not re-reserve", err)
	}
	if net.sentCount() != 1 || net.sent[0] != "hex-older" {
		t.Fatalf("recover sent %v; want only the older intent's bytes", net.sent)
	}
	o, _, _ := store.Get(ctx, "older")
	y, _, _ := store.Get(ctx, "younger")
	if o.State != StateBroadcast || y.State != StateBuilt {
		t.Fatalf("older is %s and younger is %s; want Broadcast and Built", o.State, y.State)
	}

	// The broadcaster's next pass lists every Built intent and sends each,
	// whatever Recover reported a moment earlier. The refusal has to live
	// where the send starts, or the second transaction goes out one pass
	// later.
	err = e.Broadcast(ctx, y)
	if !errors.Is(err, ErrReservationLost) {
		t.Fatalf("broadcast of the intent whose inputs another holds: %v, want ErrReservationLost", err)
	}
	if net.sentCount() != 1 {
		t.Fatalf("sent %v; the second transaction over the shared input went out", net.sent)
	}
	y, _, _ = store.Get(ctx, "younger")
	if y.State != StateBuilt || y.RawHex == "" {
		t.Fatalf("younger is %+v; want Built with its bytes", y)
	}
}

// A worker's context has already ended when it calls Broadcast, as a saturated
// pool's per-task deadline does. Against a store that honours the context, the
// re-read of the record must not turn that into a call that never happened:
// the send still runs on the caller's context and is a lost reply, the attempt
// is recorded and the reservation renewed, as the package doc promises for a
// context ending anywhere inside Broadcast.
func TestBroadcastUnderAnEndedContextStillHoldsTheIntent(t *testing.T) {
	store := ctxStore{NewMemStore()}
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "lost"}
	e := newEngine(t, store, spent, net, coins())
	e.ReservationTTL = 40 * time.Millisecond
	if _, _, err := e.Submit(context.Background(), "w1", dst, 2_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	in, _, _ := store.Get(context.Background(), "w1")
	if err := e.Build(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // inside the TTL; the renewal below carries it past

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.Broadcast(ended, in)
	if !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Fatalf("broadcast under an ended context: %v, want the lost reply reported", err)
	}
	stored, _, _ := store.Get(context.Background(), "w1")
	if stored.State != StateBuilt || stored.Attempts != 1 {
		t.Fatalf("stored record is %s after %d attempts; want Built after the one attempt", stored.State, stored.Attempts)
	}
	time.Sleep(20 * time.Millisecond) // past the original TTL, inside the renewed one
	if !spent.IsSpent(in.Inputs[0].TxID, in.Inputs[0].Vout) {
		t.Fatal("the reservation was not renewed: the input of a signed transaction is free")
	}
}

// The node rejects the transaction for good, and the save of the Failed
// record fails. Were the inputs released before the save, the store would
// hold a Built intent with signed bytes whose inputs the next withdrawal can
// select. The verdict lands first: a failed save leaves the intent Built with
// its inputs held. The store then carries the mark the attempt set before the
// send and no outcome for it, so the next attempt's rejection holds the
// intent rather than failing it: the inputs stay spent, and Abandon is the
// way out after the wait.
func TestPermanentRejectionWhoseSaveFailsKeepsTheInputsReserved(t *testing.T) {
	ctx := context.Background()
	store := &updateFailOnce{MemStore: NewMemStore(), state: StateFailed}
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "reject"}
	e := newEngine(t, store, spent, net, coins())
	if _, _, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	w, _, _ := store.Get(ctx, "w1")
	if err := e.Build(ctx, w); err != nil {
		t.Fatal(err)
	}

	err := e.Broadcast(ctx, w)
	if !errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("broadcast: %v, want the rejection reported beside the save error", err)
	}
	stored, _, _ := store.Get(ctx, "w1")
	if stored.State != StateBuilt {
		t.Fatalf("store after the failed save: %s, want Built", stored.State)
	}
	for _, o := range stored.Inputs {
		if !spent.IsSpent(o.TxID, o.Vout) {
			t.Fatalf("input %s:%d was released before the verdict was recorded", o.TxID, o.Vout)
		}
	}

	got, err := e.Process(ctx, "w1")
	if !errors.Is(err, ErrHeld) || got.State != StateBuilt || got.Hold == "" {
		t.Fatalf("the next attempt: err=%v state=%s hold=%q; want the intent held", err, got.State, got.Hold)
	}
	for _, o := range stored.Inputs {
		if !spent.IsSpent(o.TxID, o.Vout) {
			t.Fatalf("input %s:%d was released for an attempt the store shows no outcome for", o.TxID, o.Vout)
		}
	}
}

// A save that fails after the network answered leaves the caller's copy ahead
// of the store: Broadcast sent and could not record it, so the copy says
// Broadcast while the store says Built. The caller retries with the object it
// holds. The stored record decides, and the retry sends the same bytes; a
// check on the copy's own state would have refused the retry for as long as
// the caller held that object.
func TestRetryWithACopyAheadOfTheStoreActsOnTheStoredRecord(t *testing.T) {
	ctx := context.Background()
	store := &updateFailOnce{MemStore: NewMemStore(), state: StateBroadcast}
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, utxo.NewSpentSet("", nil), net, coins())
	if _, _, err := e.Submit(ctx, "w1", dst, 2_000_000, 1000); err != nil {
		t.Fatal(err)
	}
	in, _, _ := store.Get(ctx, "w1")
	if err := e.Build(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := e.Broadcast(ctx, in); err == nil || in.State != StateBroadcast {
		t.Fatalf("first send: err=%v copy=%s; want the save error with the copy ahead of the store", err, in.State)
	}
	if stored, _, _ := store.Get(ctx, "w1"); stored.State != StateBuilt {
		t.Fatalf("store after the failed save: %s, want Built", stored.State)
	}
	if err := e.Broadcast(ctx, in); err != nil {
		t.Fatalf("retry with the copy the caller holds: %v, want the send retried from the stored record", err)
	}
	stored, _, _ := store.Get(ctx, "w1")
	if stored.State != StateBroadcast || net.sentCount() != 2 || net.sent[0] != net.sent[1] {
		t.Fatalf("after the retry: state=%s sent=%v; want Broadcast recorded and the same bytes twice", stored.State, net.sent)
	}

	// The same shape for a Failed save: the copy says Failed, the store says
	// Created, and Build on the copy builds.
	failing := &updateFailOnce{MemStore: NewMemStore(), state: StateFailed}
	e2 := newEngine(t, failing, utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	if _, _, err := e2.Submit(ctx, "w2", dst, 100_000_000, 1000); err != nil { // more than the coins cover
		t.Fatal(err)
	}
	w2, _, _ := failing.Get(ctx, "w2")
	if err := e2.Build(ctx, w2); err == nil || w2.State != StateFailed {
		t.Fatalf("first build: err=%v copy=%s; want the selector's failure and the save error", err, w2.State)
	}
	if err := e2.Build(ctx, w2); err == nil {
		t.Fatal("retry of Build on the Failed copy returned no error; the selector still cannot cover the amount")
	}
	if stored, _, _ := failing.Get(ctx, "w2"); stored.State != StateFailed {
		t.Fatalf("store after the retry: %s, want the Failed verdict recorded this time", stored.State)
	}
}
