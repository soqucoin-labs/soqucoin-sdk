package resilience

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const (
	rTxA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rTxB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeSource struct {
	utxos      []types.UTXO
	refreshErr error
	at         time.Time
}

func (s *fakeSource) RefreshAll() error {
	if s.refreshErr != nil {
		return s.refreshErr
	}
	s.at = time.Now()
	return nil
}
func (s *fakeSource) LastRefresh() (time.Time, error) { return s.at, s.refreshErr }
func (s *fakeSource) GetAllUTXOs() []types.UTXO       { return s.utxos }

type fakeNode struct {
	synced bool
	outs   map[string]*rpc.TxOut
}

func k(txid string, vout uint32) string { return fmt.Sprintf("%s:%d", txid, vout) }

func (n *fakeNode) RequireSynced() error {
	if !n.synced {
		return rpc.ErrNodeSyncing
	}
	return nil
}
func (n *fakeNode) GetTxOut(txid string, vout uint32, _ bool) (*rpc.TxOut, error) {
	return n.outs[k(txid, vout)], nil
}

func recon(t *testing.T, halt bool) (*Reconciler, *fakeSource, *fakeNode, *CircuitBreaker, *[]string) {
	t.Helper()
	src := &fakeSource{utxos: []types.UTXO{
		{TxID: rTxA, Vout: 0, Value: 150_000_000, Height: 10},
		{TxID: rTxB, Vout: 1, Value: 50_000_000, Height: 10},
	}}
	node := &fakeNode{synced: true, outs: map[string]*rpc.TxOut{
		k(rTxA, 0): {Value: 1.5},
		k(rTxB, 1): {Value: 0.5},
	}}
	cb := NewCircuitBreaker(3, time.Hour)
	var alerts []string
	r := NewReconciler(src, node, cb, ReconciliationConfig{HaltOnMismatch: halt})
	r.OnAlert = func(m string) { alerts = append(alerts, m) }
	return r, src, node, cb, &alerts
}

func TestReconcilerCleanWhenNodeAgrees(t *testing.T) {
	r, _, _, cb, alerts := recon(t, true)
	rep := r.Run()
	if !rep.Clean() || rep.Checked != 2 || rep.NodeTotal != 200_000_000 {
		t.Fatalf("clean run reported %+v", rep)
	}
	if len(*alerts) != 0 {
		t.Errorf("alerts on a clean run: %v", *alerts)
	}
	if st, _, _, _ := cb.State(); st != CircuitClosed {
		t.Error("breaker tripped on a clean run")
	}
}

// The reconciler's whole reason to exist: the cache says one thing and the
// node says another. Earlier versions compared the cache to itself.
func TestReconcilerDetectsMismatchAndHalts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(n *fakeNode)
	}{
		{"output missing on node", func(n *fakeNode) { delete(n.outs, k(rTxA, 0)) }},
		{"value differs", func(n *fakeNode) { n.outs[k(rTxA, 0)] = &rpc.TxOut{Value: 1.4} }},
	}
	for _, tc := range cases {
		r, _, node, cb, alerts := recon(t, true)
		tc.mutate(node)
		rep := r.Run()
		if rep.Clean() || len(rep.Findings) == 0 {
			t.Fatalf("%s: not detected: %+v", tc.name, rep)
		}
		if len(*alerts) != 1 {
			t.Errorf("%s: alerts %v", tc.name, *alerts)
		}
		if st, _, _, _ := cb.State(); st != CircuitOpen {
			t.Errorf("%s: HaltOnMismatch did not open the breaker", tc.name)
		}
		if err := cb.Allow(); err == nil {
			t.Errorf("%s: withdrawals still allowed after a book mismatch", tc.name)
		}
	}
}

func TestReconcilerIncompleteIsAFinding(t *testing.T) {
	r, src, _, cb, alerts := recon(t, true)
	src.refreshErr = errors.New("indexer down")
	rep := r.Run()
	if rep.Incomplete == nil || rep.Clean() {
		t.Fatalf("refresh failure not reported: %+v", rep)
	}
	if len(*alerts) != 1 || cb.Allow() == nil {
		t.Errorf("refresh failure must alert and halt: alerts=%v", *alerts)
	}

	r2, _, node2, cb2, alerts2 := recon(t, true)
	node2.synced = false
	rep = r2.Run()
	if !errors.Is(rep.Incomplete, rpc.ErrNodeSyncing) || len(*alerts2) != 1 {
		t.Errorf("syncing node: %+v alerts=%v", rep, *alerts2)
	}
	if cb2.Allow() == nil {
		t.Error("no verdict from a syncing node must still halt when HaltOnMismatch is set")
	}
}

func TestReconcilerAlertsWithoutHaltingWhenConfigured(t *testing.T) {
	r, _, node, cb, alerts := recon(t, false)
	delete(node.outs, k(rTxB, 1))
	r.Run()
	if len(*alerts) != 1 {
		t.Fatalf("alerts %v", *alerts)
	}
	if cb.Allow() != nil {
		t.Error("breaker tripped although HaltOnMismatch is off")
	}
}

func TestReconcilerStartStopHonoursInitialDelayAndIsIdempotent(t *testing.T) {
	r, _, _, _, _ := recon(t, false)
	r.cfg.InitialDelay = time.Hour // would block the old implementation's Sleep
	runs := 0
	r.OnReport = func(Report) { runs++ }
	r.Start()
	r.Stop()
	r.Stop()
	time.Sleep(20 * time.Millisecond)
	if runs != 0 {
		t.Errorf("a run happened despite Stop during the initial delay")
	}
	if NewReconciler(&fakeSource{}, &fakeNode{}, nil, ReconciliationConfig{Interval: 0}).cfg.Interval <= 0 {
		t.Error("zero interval not defaulted (NewTicker would panic)")
	}
}

// ── circuit breaker additions ──────────────────────────────────────────────

// HALF-OPEN admits exactly one probe.
func TestBreakerHalfOpenAdmitsOneProbe(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure(errors.New("x"))
	time.Sleep(5 * time.Millisecond)
	admitted := 0
	for i := 0; i < 5; i++ {
		if cb.Allow() == nil {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("HALF-OPEN admitted %d callers, want exactly one probe", admitted)
	}
	cb.RecordSuccess()
	if cb.Allow() != nil {
		t.Error("closed after a successful probe, but Allow refused")
	}
}

// Per-request errors must not move the breaker; systemic ones must.
func TestRecordResultIgnoresPerRequestErrors(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Hour)
	perRequest := []error{
		fmt.Errorf("decode: %w", address.ErrInvalidChecksum),
		fmt.Errorf("build: %w", tx.ErrBelowDust),
		fmt.Errorf("build: %w", tx.ErrInsufficientFunds),
		fmt.Errorf("broadcast: %w", &rpc.Error{Code: rpc.CodeVerifyRejected, Message: "rejected"}),
	}
	for i := 0; i < 3; i++ {
		for _, e := range perRequest {
			if cb.RecordResult(e) {
				t.Errorf("per-request error counted: %v", e)
			}
		}
	}
	if err := cb.Allow(); err != nil {
		t.Fatalf("twelve bad requests halted the system: %v", err)
	}
	systemic := []error{
		fmt.Errorf("node: %w", rpc.ErrTransient),
		fmt.Errorf("broadcast: %w", rpc.ErrUnknownOutcome),
		errors.New("something unclassified"),
	}
	for _, e := range systemic {
		if !cb.RecordResult(e) {
			t.Errorf("systemic error not counted: %v", e)
		}
	}
	if cb.Allow() == nil {
		t.Error("three systemic failures did not open the breaker")
	}
	custom := errors.New("my per-request error")
	cb2 := NewCircuitBreaker(1, time.Hour)
	cb2.PerRequestErrors = []error{custom}
	if cb2.RecordResult(fmt.Errorf("wrapped: %w", custom)) || cb2.Allow() != nil {
		t.Error("PerRequestErrors extension not honoured")
	}
}

// Callbacks run outside the lock, so a callback may read the breaker.
func TestStateChangeCallbackMayReadTheBreaker(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Hour)
	done := make(chan struct{})
	cb.OnStateChange = func(from, to string, n int, lastErr string) {
		cb.State() // would deadlock under the old implementation
		close(done)
	}
	go cb.RecordFailure(errors.New("x"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnStateChange deadlocked reading the breaker")
	}
}

func TestTripOpensImmediately(t *testing.T) {
	cb := NewCircuitBreaker(5, time.Hour)
	var mu sync.Mutex
	var transitions []string
	cb.OnStateChange = func(from, to string, _ int, _ string) {
		mu.Lock()
		transitions = append(transitions, from+">"+to)
		mu.Unlock()
	}
	cb.Trip(errors.New("reconciler mismatch"))
	if cb.Allow() == nil {
		t.Fatal("Trip did not open the breaker")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 1 || transitions[0] != "CLOSED>OPEN" {
		t.Errorf("transitions %v", transitions)
	}
}
