package resilience

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// The reconciler's spent set is the engine's.
var _ SpentSet = (*utxo.SpentSet)(nil)

func recordTransitions(cb *CircuitBreaker) func() []string {
	var mu sync.Mutex
	var seen []string
	cb.OnStateChange = func(from, to string, _ int, _ string) {
		mu.Lock()
		seen = append(seen, from+">"+to)
		mu.Unlock()
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A worker admitted before the reconciler tripped the breaker completes its
// withdrawal and records the success. The book is still wrong; the halt has
// to hold.
func TestTripIsNotClearedByAnInFlightSuccess(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Hour, nil)
	if err := cb.Allow(); err != nil {
		t.Fatal(err)
	}
	cb.Trip(errors.New("reconciler: cache and node disagree"))
	cb.RecordResult(nil)
	err := cb.Allow()
	if !errors.Is(err, ErrHalted) {
		t.Fatalf("Allow after a trip and an in-flight success: %v, want ErrHalted", err)
	}
	if !strings.Contains(err.Error(), "cache and node disagree") {
		t.Errorf("Allow's error does not carry the reason: %v", err)
	}
	if state, _, _, _ := cb.State(); state != CircuitHalted {
		t.Errorf("state %s, want HALTED", state)
	}
}

// Allow while halted refuses with the sentinel and the reason, whatever the
// failure count says.
func TestAllowWhileHaltedIsRefusedWithTheReason(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Hour, nil)
	cb.Trip(errors.New("reconciler: totals differ by 5 shors"))
	for i := 0; i < 3; i++ {
		err := cb.Allow()
		if !errors.Is(err, ErrHalted) || !strings.Contains(err.Error(), "totals differ by 5 shors") {
			t.Fatalf("Allow %d: %v", i, err)
		}
	}
}

// The cooldown is for a node that may have recovered. A book that disagrees
// with the chain does not recover with time, so no probe is admitted.
func TestTripIsNotClearedByTheCooldownProbe(t *testing.T) {
	cb := NewCircuitBreaker(3, 10*time.Millisecond, nil)
	cb.Trip(errors.New("reconciler: cache and node disagree"))
	time.Sleep(30 * time.Millisecond)
	if err := cb.Allow(); !errors.Is(err, ErrHalted) {
		t.Fatalf("Allow after the cooldown: %v, want ErrHalted", err)
	}
	if state, _, _, _ := cb.State(); state != CircuitHalted {
		t.Errorf("state %s, want HALTED", state)
	}
}

// Failures recorded while halted are counted and change nothing: a halt is
// not demoted to an Open that the cooldown would then clear.
func TestFailuresWhileHaltedChangeNoState(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond, nil)
	cb.Trip(errors.New("reconciler: cache and node disagree"))
	transitions := recordTransitions(cb)
	cb.RecordFailure(errors.New("node down"))
	cb.RecordResult(rpc.ErrPermanent)
	time.Sleep(30 * time.Millisecond)
	if err := cb.Allow(); !errors.Is(err, ErrHalted) {
		t.Fatalf("Allow after failures while halted and a cooldown: %v, want ErrHalted", err)
	}
	if _, _, _, failures := cb.State(); failures != 1 {
		t.Errorf("TotalFailures %d, want 1", failures)
	}
	if got := transitions(); len(got) != 0 {
		t.Errorf("transitions while halted: %v", got)
	}
}

// A breaker already Open from failures reads as one that will recover on its
// own. A trip while Open has to say that it will not.
func TestTripWhileOpenReportsTheHalt(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Hour, nil)
	cb.RecordFailure(errors.New("node down"))
	transitions := recordTransitions(cb)
	cb.Trip(errors.New("reconciler: cache and node disagree"))
	cb.Trip(errors.New("reconciler: still wrong"))
	if got := transitions(); len(got) != 1 || got[0] != "OPEN>HALTED" {
		t.Errorf("transitions %v, want [OPEN>HALTED]", got)
	}
}

// Reset is the one exit from a halt, and the alerter that announced the halt
// has to see the exit.
func TestResetAfterAHaltClosesAndReportsIt(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Hour, nil)
	cb.Trip(errors.New("reconciler: cache and node disagree"))
	transitions := recordTransitions(cb)
	cb.Reset()
	if err := cb.Allow(); err != nil {
		t.Fatalf("Allow after Reset: %v", err)
	}
	if state, failures, _, _ := cb.State(); state != CircuitClosed || failures != 0 {
		t.Errorf("state %s failures %d after Reset, want CLOSED and 0", state, failures)
	}
	cb.Reset()
	if got := transitions(); len(got) != 1 || got[0] != "HALTED>CLOSED" {
		t.Errorf("transitions %v, want [HALTED>CLOSED]", got)
	}
}

// spendingSource returns a snapshot, as electrumx.Client.GetAllUTXOs does, and
// the process spends an output at the node after the snapshot is taken.
type spendingSource struct {
	fakeSource
	afterRead func()
}

func (s *spendingSource) GetAllUTXOs() []types.UTXO {
	out := append([]types.UTXO(nil), s.utxos...)
	if s.afterRead != nil {
		s.afterRead()
	}
	return out
}

func ownSpendSetup(t *testing.T) (*Reconciler, *fakeNode, *CircuitBreaker, *utxo.SpentSet, *[]string) {
	t.Helper()
	src := &spendingSource{fakeSource: fakeSource{utxos: []types.UTXO{
		{TxID: rTxA, Vout: 0, Value: 150_000_000, Height: 10},
		{TxID: rTxB, Vout: 1, Value: 50_000_000, Height: 10},
	}}}
	node := &fakeNode{synced: true, outs: map[string]*rpc.TxOut{
		k(rTxA, 0): {Value: 150_000_000},
		k(rTxB, 1): {Value: 50_000_000},
	}}
	spent := utxo.NewSpentSet("", nil)
	// The engine reserves before it sends; the node then has the spend.
	src.afterRead = func() {
		if err := spent.Reserve([]types.UTXO{{TxID: rTxA, Vout: 0}}, "wd-1", time.Hour); err != nil {
			t.Fatal(err)
		}
		node.outs[k(rTxA, 0)] = nil
	}
	cb := NewCircuitBreaker(3, time.Hour, nil)
	r := NewReconciler(src, node, cb, ReconciliationConfig{HaltOnMismatch: true}, nil)
	r.Spent = spent
	var alerts []string
	r.OnAlert = func(m string) { alerts = append(alerts, m) }
	return r, node, cb, spent, &alerts
}

// An output this process spent after the snapshot is not in the node's UTXO
// set and is not a finding: the spent set records the spend.
func TestReconcilerExcludesAnOutpointThisProcessSpent(t *testing.T) {
	r, _, cb, _, alerts := ownSpendSetup(t)
	rep := r.Run(context.Background())
	if !rep.Clean() {
		t.Fatalf("report not clean: findings %+v incomplete %v", rep.Findings, rep.Incomplete)
	}
	if rep.OwnSpends != 1 || rep.Checked != 1 || rep.CacheTotal != 50_000_000 || rep.NodeTotal != 50_000_000 {
		t.Errorf("own %d checked %d cache %d node %d; want 1, 1, 50000000, 50000000",
			rep.OwnSpends, rep.Checked, rep.CacheTotal, rep.NodeTotal)
	}
	if err := cb.Allow(); err != nil {
		t.Errorf("breaker tripped on the process's own spend: %v", err)
	}
	if len(*alerts) != 0 {
		t.Errorf("alerts %q", *alerts)
	}
}

// With a spent set wired, an output the node lacks and the set does not
// record is still a finding, and still halts.
func TestReconcilerStillFindsAnOutpointNobodyHereSpent(t *testing.T) {
	r, node, cb, spent, _ := ownSpendSetup(t)
	node.outs[k(rTxB, 1)] = nil
	rep := r.Run(context.Background())
	if len(rep.Findings) != 1 || rep.Findings[0].TxID != rTxB || !rep.Findings[0].Missing {
		t.Fatalf("findings %+v, want B:1 missing", rep.Findings)
	}
	if rep.OwnSpends != 1 {
		t.Errorf("OwnSpends %d, want 1", rep.OwnSpends)
	}
	if !errors.Is(cb.Allow(), ErrHalted) {
		t.Errorf("breaker not halted on a foreign spend: %v", cb.Allow())
	}
	if spent.IsSpent(rTxB, 1) {
		t.Fatal("test setup: B:1 must not be in the set")
	}
}

// The webhook goes to a third party. It says what happened and that the log
// has the reason; txids, amounts and a proxy's error page stay in the log.
func TestBreakerWebhookCarriesNoErrorTextAndNamesTheHalt(t *testing.T) {
	bodies := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
	}))
	defer srv.Close()
	a := NewAlerter(srv.URL, nil)
	cb := NewCircuitBreaker(3, time.Hour, nil)
	a.WireToCircuitBreaker(cb)
	cb.Trip(errors.New("reconciler: cache and node disagree: 1 finding(s), cache 150000000 shors vs node 0 shors; " + rTxA + ":0 in cache, not in the node's UTXO set"))
	var body string
	select {
	case body = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook post")
	}
	var payload slackPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("payload: %v\n%s", err, body)
	}
	// A trip on a breaker with no failures reports none: the count is real.
	for _, secret := range []string{rTxA, "150000000", "disagree", "Consecutive failures"} {
		if strings.Contains(body, secret) {
			t.Errorf("webhook carries %q:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "HALTED") || !strings.Contains(body, "Reset") {
		t.Errorf("webhook does not name the halt and its exit:\n%s", body)
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].Color != "danger" {
		t.Errorf("attachments %+v", payload.Attachments)
	}
}

// Wiring an alerter while workers are recording is a data race unless the
// callback is written and read under the breaker's lock. Run under -race.
func TestWireToCircuitBreakerDuringUse(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Hour, nil)
	a := NewAlerter("http://127.0.0.1:1/never", nil)
	a.client.Timeout = time.Millisecond
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.WireToCircuitBreaker(cb)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cb.RecordFailure(errors.New("x"))
			cb.Reset()
		}
	}()
	wg.Wait()
}
