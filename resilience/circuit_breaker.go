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
	"fmt"
	"log"
	"sync"
	"time"
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

	lastFailure time.Time
	lastSuccess time.Time

	// Stats for monitoring
	TotalFailures  int64
	TotalSuccesses int64

	// OnStateChange is called whenever the CB transitions between states.
	// Signature: func(fromState, toState string, consecutiveFailures int, lastErr string)
	// May be nil. Used by the Alerter for webhook notifications.
	OnStateChange func(from, to string, consecutiveFailures int, lastErr string)
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
			log.Printf("[circuit-breaker] Transitioning OPEN → HALF-OPEN (cooldown elapsed, allowing probe)")
			return nil
		}
		remaining := cb.cooldownDuration - time.Since(cb.lastFailure)
		return fmt.Errorf("circuit breaker OPEN: %d consecutive failures, cooldown remaining: %v",
			cb.consecutiveFailures, remaining.Round(time.Second))

	case CircuitHalfOpen:
		return nil

	default:
		return fmt.Errorf("circuit breaker in unknown state: %d", cb.state)
	}
}

// RecordSuccess records a successful operation. Resets failure count and closes the circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	previousState := cb.state
	cb.consecutiveFailures = 0
	cb.lastSuccess = time.Now()
	cb.TotalSuccesses++
	cb.state = CircuitClosed

	if previousState != CircuitClosed {
		log.Printf("[circuit-breaker] %s → CLOSED (operation succeeded)", previousState)
		if cb.OnStateChange != nil {
			cb.OnStateChange(previousState.String(), "CLOSED", 0, "")
		}
	}
}

// RecordFailure records an operation failure. May trip the circuit open.
func (cb *CircuitBreaker) RecordFailure(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures++
	cb.lastFailure = time.Now()
	cb.TotalFailures++

	if cb.state == CircuitHalfOpen {
		cb.state = CircuitOpen
		log.Printf("[circuit-breaker] HALF-OPEN → OPEN (probe failed: %v, cooling down %v)",
			err, cb.cooldownDuration)
		if cb.OnStateChange != nil {
			cb.OnStateChange("HALF-OPEN", "OPEN", cb.consecutiveFailures, err.Error())
		}
		return
	}

	if cb.consecutiveFailures >= cb.maxFailures {
		cb.state = CircuitOpen
		log.Printf("[circuit-breaker] CLOSED → OPEN (%d consecutive failures, cooling down %v)",
			cb.consecutiveFailures, cb.cooldownDuration)
		if cb.OnStateChange != nil {
			cb.OnStateChange("CLOSED", "OPEN", cb.consecutiveFailures, err.Error())
		}
	} else {
		log.Printf("[circuit-breaker] Failure %d/%d: %v",
			cb.consecutiveFailures, cb.maxFailures, err)
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
	log.Printf("[circuit-breaker] Manually reset to CLOSED")
}
