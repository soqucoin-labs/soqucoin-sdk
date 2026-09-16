package resilience

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// A withdrawal worker pool is the ordinary exchange shape, so two workers
// take the same job and one of them loses the build race in withdraw.Build.
// Its Process returns ErrWrongState, and the documented loop hands every
// Process error to RecordResult. Unrecognised errors count as systemic, so
// three race losses in a row would open the breaker and stop every
// withdrawal for its cooldown: one queue redelivering three duplicates halts
// payouts, and nothing is wrong with the node. The same holds for an unknown
// or malformed id and for an idempotency key reused for another payment.
func TestWithdrawalStateErrorsAreNotSystemic(t *testing.T) {
	perRequest := []error{
		fmt.Errorf("%w: w1 is Built", withdraw.ErrWrongState),
		fmt.Errorf("%w: unknown intent w2", withdraw.ErrInvalidIntent),
		fmt.Errorf("submit: %w", withdraw.ErrConflict),
		fmt.Errorf("%w: w3: selector", withdraw.ErrFailed),
	}
	cb := NewCircuitBreaker(3, time.Hour, nil)
	for i := 0; i < 3; i++ {
		for _, e := range perRequest {
			if cb.RecordResult(e) {
				t.Errorf("counted as systemic: %v", e)
			}
		}
	}
	if err := cb.Allow(); err != nil {
		t.Fatalf("twelve refused withdrawals halted every withdrawal: %v", err)
	}

	// The boundary. An intent held after a txid mismatch, and a built intent
	// whose inputs another withdrawal took, are both conditions an operator
	// resolves before anything else is sent; the breaker is what stops the
	// run, so these still count.
	for _, e := range []error{
		fmt.Errorf("broadcast: %w", withdraw.ErrHeld),
		fmt.Errorf("broadcast: %w", withdraw.ErrReservationLost),
	} {
		if !NewCircuitBreaker(1, time.Hour, nil).RecordResult(e) {
			t.Errorf("not counted, so nothing stops the run: %v", e)
		}
	}
}

func TestCircuitBreakerStartsClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second, nil)
	state, failures, _, _ := cb.State()
	if state != CircuitClosed {
		t.Errorf("expected CLOSED, got %s", state)
	}
	if failures != 0 {
		t.Errorf("expected 0 failures, got %d", failures)
	}
}

func TestCircuitBreakerAllowsWhenClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second, nil)
	if err := cb.Allow(); err != nil {
		t.Errorf("expected Allow() to succeed when closed, got: %v", err)
	}
}

func TestCircuitBreakerTripsAfterMaxFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second, nil)

	// Record 3 consecutive failures
	for i := 0; i < 3; i++ {
		cb.RecordFailure(errors.New("test failure"))
	}

	state, failures, _, _ := cb.State()
	if state != CircuitOpen {
		t.Errorf("expected OPEN after 3 failures, got %s", state)
	}
	if failures != 3 {
		t.Errorf("expected 3 consecutive failures, got %d", failures)
	}

	// Should block requests
	if err := cb.Allow(); err == nil {
		t.Error("expected Allow() to fail when circuit is OPEN")
	}
}

func TestCircuitBreakerDoesNotTripBeforeThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second, nil)

	// Record 2 failures (below threshold)
	cb.RecordFailure(errors.New("fail 1"))
	cb.RecordFailure(errors.New("fail 2"))

	state, _, _, _ := cb.State()
	if state != CircuitClosed {
		t.Errorf("expected CLOSED after 2/3 failures, got %s", state)
	}

	if err := cb.Allow(); err != nil {
		t.Errorf("expected Allow() to succeed at 2/3 failures, got: %v", err)
	}
}

func TestCircuitBreakerResetsOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second, nil)

	// 2 failures then 1 success
	cb.RecordFailure(errors.New("fail"))
	cb.RecordFailure(errors.New("fail"))
	cb.RecordSuccess()

	state, failures, successes, _ := cb.State()
	if state != CircuitClosed {
		t.Errorf("expected CLOSED after success, got %s", state)
	}
	if failures != 0 {
		t.Errorf("expected 0 consecutive failures after success, got %d", failures)
	}
	if successes != 1 {
		t.Errorf("expected 1 total success, got %d", successes)
	}
}

func TestCircuitBreakerHalfOpenAfterCooldown(t *testing.T) {
	// Use a very short cooldown for testing
	cb := NewCircuitBreaker(1, 10*time.Millisecond, nil)

	cb.RecordFailure(errors.New("fail"))

	state, _, _, _ := cb.State()
	if state != CircuitOpen {
		t.Fatalf("expected OPEN, got %s", state)
	}

	// Wait for cooldown
	time.Sleep(15 * time.Millisecond)

	// Allow should transition to HALF-OPEN and succeed
	if err := cb.Allow(); err != nil {
		t.Errorf("expected Allow() to succeed after cooldown, got: %v", err)
	}

	state, _, _, _ = cb.State()
	if state != CircuitHalfOpen {
		t.Errorf("expected HALF-OPEN after cooldown, got %s", state)
	}
}

func TestCircuitBreakerHalfOpenProbeSuccess(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond, nil)

	cb.RecordFailure(errors.New("fail"))
	time.Sleep(15 * time.Millisecond)
	cb.Allow() // Transitions to HALF-OPEN

	// Probe succeeds
	cb.RecordSuccess()

	state, failures, _, _ := cb.State()
	if state != CircuitClosed {
		t.Errorf("expected CLOSED after probe success, got %s", state)
	}
	if failures != 0 {
		t.Errorf("expected 0 failures after probe success, got %d", failures)
	}
}

func TestCircuitBreakerHalfOpenProbeFail(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond, nil)

	cb.RecordFailure(errors.New("fail"))
	time.Sleep(15 * time.Millisecond)
	cb.Allow() // Transitions to HALF-OPEN

	// Probe fails
	cb.RecordFailure(errors.New("probe fail"))

	state, _, _, _ := cb.State()
	if state != CircuitOpen {
		t.Errorf("expected OPEN after probe failure, got %s", state)
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker(1, 5*time.Second, nil)
	cb.RecordFailure(errors.New("fail"))

	state, _, _, _ := cb.State()
	if state != CircuitOpen {
		t.Fatalf("expected OPEN, got %s", state)
	}

	cb.Reset()

	state, failures, _, _ := cb.State()
	if state != CircuitClosed {
		t.Errorf("expected CLOSED after reset, got %s", state)
	}
	if failures != 0 {
		t.Errorf("expected 0 failures after reset, got %d", failures)
	}
}

func TestCircuitBreakerOnStateChangeCallback(t *testing.T) {
	cb := NewCircuitBreaker(1, 5*time.Second, nil)

	var called bool
	var capturedFrom, capturedTo string
	cb.OnStateChange = func(from, to string, failures int, lastErr string) {
		called = true
		capturedFrom = from
		capturedTo = to
	}

	cb.RecordFailure(errors.New("test"))

	if !called {
		t.Error("expected OnStateChange to be called")
	}
	if capturedFrom != "CLOSED" || capturedTo != "OPEN" {
		t.Errorf("expected CLOSED→OPEN, got %s→%s", capturedFrom, capturedTo)
	}
}

func TestCircuitBreakerTotalStats(t *testing.T) {
	cb := NewCircuitBreaker(5, 5*time.Second, nil) // High threshold so it stays closed

	cb.RecordSuccess()
	cb.RecordSuccess()
	cb.RecordFailure(errors.New("f"))
	cb.RecordSuccess()

	_, _, successes, failures := cb.State()
	if successes != 3 {
		t.Errorf("expected 3 total successes, got %d", successes)
	}
	if failures != 1 {
		t.Errorf("expected 1 total failure, got %d", failures)
	}
}

// A node that accepts a transaction under a different txid than the SDK
// computed is a systemic disagreement between signer and node, not a bad
// request: it must count toward opening the breaker.
func TestTxIDMismatchCountsAsSystemic(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Minute, nil)
	if !cb.RecordResult(fmt.Errorf("broadcast: %w", rpc.ErrTxIDMismatch)) {
		t.Fatal("txid mismatch was ignored as a per-request error")
	}
	if cb.Allow() == nil {
		t.Fatal("breaker did not open on a txid mismatch")
	}
}

// A cancelled context is the operator's own stop signal, not a failure of
// the system: it neither counts nor resets. A deadline that expired counts.
func TestContextCanceledIsNotAFailure(t *testing.T) {
	cb := NewCircuitBreaker(2, time.Minute, nil)
	for i := 0; i < 5; i++ {
		if cb.RecordResult(fmt.Errorf("process: %w", context.Canceled)) {
			t.Fatal("a cancelled context changed the breaker")
		}
	}
	if st, n, _, _ := cb.State(); st != CircuitClosed || n != 0 {
		t.Fatalf("after five cancels: %s %d", st, n)
	}
	cb.RecordResult(fmt.Errorf("node: %w", context.DeadlineExceeded))
	cb.RecordResult(fmt.Errorf("node: %w", context.DeadlineExceeded))
	if st, _, _, _ := cb.State(); st != CircuitOpen {
		t.Fatalf("two deadlines: %s, want OPEN", st)
	}
}
