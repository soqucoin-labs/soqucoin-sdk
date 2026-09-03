// Package resilience provides production-hardened operational patterns for
// Soqucoin payment automation.
//
// This package was extracted from the canonical soq-signer service (v1.0.0-alpha)
// which has been running in production since May 2026.
//
// Components:
//   - CircuitBreaker: Prevents cascading failures by halting operations after
//     consecutive failures, then gradually recovering (standard CB pattern).
//   - Reconciler: Periodically verifies UTXO state against fresh data to detect
//     balance discrepancies, stale UTXOs, or missed spends.
//   - Alerter: Sends webhook notifications (Slack-compatible) on important state
//     changes like circuit breaker transitions.
//
// These patterns are CRITICAL for any system doing automated payouts on Soqucoin.
// Without them, a node outage or ElectrumX desync can cause:
//   - Infinite retry loops (circuit breaker prevents)
//   - Silent balance drift (reconciler catches)
//   - Unnoticed failures (alerter surfaces)
//
// Copyright (c) 2025-2026 Soqucoin Labs Inc. MIT License.
package resilience

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
)

// CircuitBreakerState represents the circuit breaker's current state.
type CircuitBreakerState int

const (
	// CircuitClosed — normal operation, requests proceed.
	CircuitClosed CircuitBreakerState = iota
	// CircuitOpen — too many failures, requests blocked until cooldown.
	CircuitOpen
	// CircuitHalfOpen — cooldown elapsed, allowing ONE probe attempt.
	CircuitHalfOpen
)

func (s CircuitBreakerState) String() string {
	switch s {
	case CircuitClosed:
		return "CLOSED"
	case CircuitOpen:
		return "OPEN"
	case CircuitHalfOpen:
		return "HALF-OPEN"
	default:
		return "UNKNOWN"
	}
}

// CircuitBreaker prevents cascading failures in automated payment systems.
//
// State machine:
//
//	CLOSED → (maxFailures consecutive errors) → OPEN
//	OPEN   → (cooldown elapses)               → HALF-OPEN
//	HALF-OPEN → (probe succeeds)              → CLOSED
//	HALF-OPEN → (probe fails)                 → OPEN
//
// Defense 14 (DL-ENTERPRISE-PAYOUT): This is the standard circuit breaker
// pattern adapted for blockchain payout systems. Without it, a node outage
// causes infinite payout retries, burning fees on doomed transactions.
type CircuitBreaker struct {
	mu sync.Mutex

	state               CircuitBreakerState
	consecutiveFailures int
	maxFailures         int
	cooldownDuration    time.Duration
	probing             bool // HALF-OPEN: one probe is in flight

	lastFailure time.Time
	lastSuccess time.Time

	// Stats for monitoring
	TotalFailures  int64
	TotalSuccesses int64

	// OnStateChange is called whenever the CB transitions between states.
	// Signature: func(fromState, toState string, consecutiveFailures int, lastErr string)
	// May be nil. Used by the Alerter for webhook notifications. It is invoked
	// after the breaker's lock is released, so it may read the breaker.
	OnStateChange func(from, to string, consecutiveFailures int, lastErr string)

	// PerRequestErrors extends the set of errors that describe ONE request
	// rather than the system, and so must never count as a failure. Address,
	// amount and node-rejection errors from this SDK are always in the set.
	PerRequestErrors []error
}

// perRequest reports whether err is about the request, not the system. A
// malformed address, an amount below the floor, insufficient funds or a
// node rejection of one transaction says nothing about whether the next
// withdrawal can succeed; feeding such errors to the breaker lets an
// unauthenticated user halt every withdrawal with three bad requests.
func (cb *CircuitBreaker) perRequest(err error) bool {
	if err == nil {
		return false
	}
	builtin := []error{
		rpc.ErrPermanent,
		soqaddr.ErrInvalidChecksum, soqaddr.ErrInvalidLength, soqaddr.ErrInvalidHRP,
		soqaddr.ErrInvalidChar, soqaddr.ErrUnsupportedWitnessVersion, soqaddr.ErrInvalidVersion,
		tx.ErrInvalidAmount, tx.ErrBelowDust, tx.ErrFeeTooHigh, tx.ErrInsufficientFunds, tx.ErrInputOverflow,
	}
	for _, e := range append(builtin, cb.PerRequestErrors...) {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// RecordResult is the entry point callers should use. A nil error is a
// success. A per-request error (see perRequest) is neither: the breaker is
// untouched and false is returned. Anything else (transport failures,
// transient node states, unknown outcomes, unclassified errors) counts as a
// failure. Returns true when the result changed the breaker's counters.
func (cb *CircuitBreaker) RecordResult(err error) bool {
	switch {
	case err == nil:
		cb.RecordSuccess()
		return true
	case cb.perRequest(err):
		cb.mu.Lock()
		if cb.state == CircuitHalfOpen {
			cb.probing = false // the probe completed; it just told us nothing about the system
		}
		cb.mu.Unlock()
		return false
	default:
		cb.RecordFailure(err)
		return true
	}
}

// Trip forces the breaker OPEN regardless of the failure count, for callers
// that have found a reason to halt outright (the reconciler on a mismatch).
func (cb *CircuitBreaker) Trip(err error) {
	cb.mu.Lock()
	prev := cb.state
	cb.state = CircuitOpen
	cb.probing = false
	cb.lastFailure = time.Now()
	if cb.consecutiveFailures < cb.maxFailures {
		cb.consecutiveFailures = cb.maxFailures
	}
	n := cb.consecutiveFailures
	cb.mu.Unlock()
	log.Printf("[circuit-breaker] %s → OPEN (tripped: %v)", prev, err)
	if prev != CircuitOpen && cb.OnStateChange != nil {
		cb.OnStateChange(prev.String(), "OPEN", n, err.Error())
	}
}

// NewCircuitBreaker creates a new circuit breaker.
//
// Parameters:
//   - maxFailures: consecutive failures before tripping (recommended: 3)
//   - cooldown: duration to wait before probing (recommended: 15-30 min)
func NewCircuitBreaker(maxFailures int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		maxFailures:      maxFailures,
		cooldownDuration: cooldown,
	}
}

// Allow checks if an operation should proceed.
// Returns nil if allowed, or an error explaining why it's blocked.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return nil

	case CircuitOpen:
		if time.Since(cb.lastFailure) >= cb.cooldownDuration {
			cb.state = CircuitHalfOpen
			cb.probing = true
			log.Printf("[circuit-breaker] Transitioning OPEN → HALF-OPEN (cooldown elapsed, allowing ONE probe)")
			return nil
		}
		remaining := cb.cooldownDuration - time.Since(cb.lastFailure)
		return fmt.Errorf("circuit breaker OPEN: %d consecutive failures, cooldown remaining: %v",
			cb.consecutiveFailures, remaining.Round(time.Second))

	case CircuitHalfOpen:
		// Exactly one probe at a time. Admitting everyone while half-open lets
		// a burst of doomed operations through before the probe has answered.
		if cb.probing {
			return errors.New("circuit breaker HALF-OPEN: a probe is already in flight")
		}
		cb.probing = true
		return nil

	default:
		return fmt.Errorf("circuit breaker in unknown state: %d", cb.state)
	}
}

// RecordSuccess records a successful operation. Resets failure count and closes the circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	previousState := cb.state
	cb.consecutiveFailures = 0
	cb.lastSuccess = time.Now()
	cb.TotalSuccesses++
	cb.state = CircuitClosed
	cb.probing = false
	cb.mu.Unlock()

	if previousState != CircuitClosed {
		log.Printf("[circuit-breaker] %s → CLOSED (operation succeeded)", previousState)
		if cb.OnStateChange != nil {
			cb.OnStateChange(previousState.String(), "CLOSED", 0, "")
		}
	}
}

// RecordFailure records an operation failure. May trip the circuit open.
//
// Prefer RecordResult, which refuses to count per-request errors.
func (cb *CircuitBreaker) RecordFailure(err error) {
	if err == nil {
		err = errors.New("unspecified failure")
	}
	cb.mu.Lock()
	cb.consecutiveFailures++
	cb.lastFailure = time.Now()
	cb.TotalFailures++
	n := cb.consecutiveFailures
	var from string
	tripped := false
	switch {
	case cb.state == CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.probing = false
		from, tripped = "HALF-OPEN", true
	case cb.consecutiveFailures >= cb.maxFailures && cb.state != CircuitOpen:
		cb.state = CircuitOpen
		from, tripped = "CLOSED", true
	}
	cb.mu.Unlock()

	if !tripped {
		log.Printf("[circuit-breaker] Failure %d/%d: %v", n, cb.maxFailures, err)
		return
	}
	log.Printf("[circuit-breaker] %s → OPEN (%d consecutive failures: %v, cooling down %v)", from, n, err, cb.cooldownDuration)
	if cb.OnStateChange != nil {
		cb.OnStateChange(from, "OPEN", n, err.Error())
	}
}

// State returns the current circuit breaker state and stats.
func (cb *CircuitBreaker) State() (CircuitBreakerState, int, int64, int64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state, cb.consecutiveFailures, cb.TotalSuccesses, cb.TotalFailures
}

// Reset forces the circuit breaker back to CLOSED state.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = CircuitClosed
	cb.consecutiveFailures = 0
	cb.probing = false
	log.Printf("[circuit-breaker] Manually reset to CLOSED")
}
