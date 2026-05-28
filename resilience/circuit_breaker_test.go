package resilience

import (
	"errors"
	"testing"
	"time"
)

func TestCircuitBreakerStartsClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second)
	state, failures, _, _ := cb.State()
	if state != CircuitClosed {
		t.Errorf("expected CLOSED, got %s", state)
	}
	if failures != 0 {
		t.Errorf("expected 0 failures, got %d", failures)
	}
}

func TestCircuitBreakerAllowsWhenClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second)
	if err := cb.Allow(); err != nil {
		t.Errorf("expected Allow() to succeed when closed, got: %v", err)
	}
}

func TestCircuitBreakerTripsAfterMaxFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, 5*time.Second)

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
	cb := NewCircuitBreaker(3, 5*time.Second)

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
	cb := NewCircuitBreaker(3, 5*time.Second)

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
	cb := NewCircuitBreaker(1, 10*time.Millisecond)

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
	cb := NewCircuitBreaker(1, 10*time.Millisecond)

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
	cb := NewCircuitBreaker(1, 10*time.Millisecond)

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
	cb := NewCircuitBreaker(1, 5*time.Second)
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
	cb := NewCircuitBreaker(1, 5*time.Second)

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
	cb := NewCircuitBreaker(5, 5*time.Second) // High threshold so it stays closed

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
