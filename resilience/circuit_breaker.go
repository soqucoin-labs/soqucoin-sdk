// Package resilience provides production-hardened operational patterns for
// Soqucoin payment automation.
//
// This package was extracted from the canonical soq-signer service (v1.0.0-alpha)
// which has been running in production since May 2026.
//
// Components:
//   - CircuitBreaker: Prevents cascading failures by halting operations after
//     consecutive failures, then gradually recovering (standard CB pattern).
//     A Trip, which the Reconciler uses, halts it until an operator's Reset.
//   - Reconciler: Periodically verifies the indexer cache against the node,
//     outpoint by outpoint; given the spent set, it excludes this process's
//     own spends.
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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
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
	// CircuitHalted — tripped by a caller that found a reason to stop
	// outright; no cooldown and no success leaves it, only Reset.
	CircuitHalted
)

func (s CircuitBreakerState) String() string {
	switch s {
	case CircuitClosed:
		return "CLOSED"
	case CircuitOpen:
		return "OPEN"
	case CircuitHalfOpen:
		return "HALF-OPEN"
	case CircuitHalted:
		return "HALTED"
	default:
		return "UNKNOWN"
	}
}

// ErrHalted is what Allow returns while the breaker is halted. The error
// carries the reason Trip was given; Reset is the only exit.
var ErrHalted = errors.New("resilience: circuit breaker halted; Reset is the only exit")

// CircuitBreaker prevents cascading failures in automated payment systems.
//
// State machine:
//
//	CLOSED → (maxFailures consecutive errors) → OPEN
//	OPEN   → (cooldown elapses)               → HALF-OPEN
//	HALF-OPEN → (probe succeeds)              → CLOSED
//	HALF-OPEN → (probe fails)                 → OPEN
//	any    → (Trip)                           → HALTED
//	HALTED → (Reset)                          → CLOSED
//
// Defense 14 (DL-ENTERPRISE-PAYOUT): This is the standard circuit breaker
// pattern adapted for blockchain payout systems. Without it, a node outage
// causes infinite payout retries, burning fees on doomed transactions. The
// cooldown is for a node that may have recovered; HALTED is for a book that
// does not match the chain, which time does not repair.
type CircuitBreaker struct {
	mu sync.Mutex

	state               CircuitBreakerState
	consecutiveFailures int
	maxFailures         int
	cooldownDuration    time.Duration
	probing             bool   // HALF-OPEN: one probe is in flight
	haltReason          string // HALTED: what Trip was told

	lastFailure time.Time
	lastSuccess time.Time
	log         *slog.Logger

	// Stats for monitoring
	TotalFailures  int64
	TotalSuccesses int64

	// OnStateChange is called whenever the CB transitions between states.
	// Signature: func(fromState, toState string, consecutiveFailures int, lastErr string)
	// May be nil. Used by the Alerter for webhook notifications. It is invoked
	// after the breaker's lock is released, so it may read the breaker. Set it
	// before the breaker is in use, or through Alerter.WireToCircuitBreaker,
	// which takes the lock.
	OnStateChange func(from, to string, consecutiveFailures int, lastErr string)

	// PerRequestErrors extends the set of errors that describe ONE request
	// rather than the system, and so must never count as a failure. Address,
	// amount, node-rejection and withdrawal-state errors from this SDK are
	// always in the set (see perRequestSentinels).
	PerRequestErrors []error
}

// perRequestSentinels are the errors this SDK raises about one request. A
// malformed address, an amount below the floor, insufficient funds or a node
// rejection of one transaction says nothing about whether the next withdrawal
// can succeed. Neither does a withdrawal the engine refuses to act on: an
// intent in a state that does not allow the call (most often a second worker
// that lost the build race for one id), an id that is unknown or malformed,
// or an idempotency key reused for a different payment. Each is a fact about
// that request, and counting them lets a caller with a worker pool, or an
// unauthenticated user with three bad requests, halt every withdrawal.
//
// withdraw.ErrHeld and withdraw.ErrReservationLost are deliberately absent:
// each means an operator must resolve a transaction before anything else is
// sent, so each counts and the breaker is the thing that stops the run. So
// is withdraw.ErrStale: the engine re-reads under its lock before every
// write, so a store refusing a write as stale means a second process is
// moving the same records, which is a deployment fault and not a request.
var perRequestSentinels = []error{
	context.Canceled,
	rpc.ErrPermanent,
	soqaddr.ErrInvalidChecksum, soqaddr.ErrInvalidLength, soqaddr.ErrInvalidHRP,
	soqaddr.ErrInvalidChar, soqaddr.ErrUnsupportedWitnessVersion, soqaddr.ErrInvalidVersion,
	tx.ErrInvalidAmount, tx.ErrBelowDust, tx.ErrFeeTooHigh, tx.ErrInsufficientFunds, tx.ErrInputOverflow,
	withdraw.ErrWrongState, withdraw.ErrInvalidIntent, withdraw.ErrConflict,
}

// perRequest reports whether err is about the request, not the system:
// perRequestSentinels, and whatever the caller added to PerRequestErrors. A
// cancelled context is the caller's own stop signal and says nothing about
// the system either; a deadline that expired does, and counts.
func (cb *CircuitBreaker) perRequest(err error) bool {
	if err == nil {
		return false
	}
	for _, e := range perRequestSentinels {
		if errors.Is(err, e) {
			return true
		}
	}
	for _, e := range cb.PerRequestErrors {
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

// Trip halts the breaker regardless of the failure count, for callers that
// have found a reason to stop outright (the reconciler on a mismatch). A halt
// outlasts the cooldown and every recorded success; Reset is the only exit.
// The callback fires whenever the breaker was not already halted, so a
// breaker that was Open from failures still reports that it will not recover
// on its own.
func (cb *CircuitBreaker) Trip(err error) {
	if err == nil {
		err = errors.New("unspecified reason")
	}
	cb.mu.Lock()
	prev := cb.state
	cb.state = CircuitHalted
	cb.haltReason = err.Error()
	cb.probing = false
	n := cb.consecutiveFailures // the real count; a halt is not a failure tally
	notify := cb.OnStateChange
	cb.mu.Unlock()
	cb.logger().Error("circuit breaker halted", "from", prev.String(), "err", err)
	if prev != CircuitHalted && notify != nil {
		notify(prev.String(), CircuitHalted.String(), n, err.Error())
	}
}

// NewCircuitBreaker creates a new circuit breaker.
//
// Parameters:
//   - maxFailures: consecutive failures before tripping (recommended: 3)
//   - cooldown: duration to wait before probing (recommended: 15-30 min)
//   - logger: where transitions and counted failures go; nil discards
func NewCircuitBreaker(maxFailures int, cooldown time.Duration, logger *slog.Logger) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		maxFailures:      maxFailures,
		cooldownDuration: cooldown,
		log:              logutil.Or(logger),
	}
}

// logger returns the injected logger, or a discarding one for a breaker
// built as a literal.
func (cb *CircuitBreaker) logger() *slog.Logger { return logutil.Or(cb.log) }

// Allow checks if an operation should proceed.
// Returns nil if allowed, or an error explaining why it's blocked.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return nil

	case CircuitHalted:
		return fmt.Errorf("%w: %s", ErrHalted, cb.haltReason)

	case CircuitOpen:
		if time.Since(cb.lastFailure) >= cb.cooldownDuration {
			cb.state = CircuitHalfOpen
			cb.probing = true
			cb.logger().Info("circuit breaker half-open: cooldown elapsed, one probe allowed")
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

// RecordSuccess records a successful operation. Resets failure count and
// closes the circuit. While halted it counts the success and changes
// nothing: a withdrawal that went through says the node works, and the halt
// is about the book.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	previousState := cb.state
	cb.lastSuccess = time.Now()
	cb.TotalSuccesses++
	if cb.state == CircuitHalted {
		cb.mu.Unlock()
		return
	}
	cb.consecutiveFailures = 0
	cb.state = CircuitClosed
	cb.probing = false
	notify := cb.OnStateChange
	cb.mu.Unlock()

	if previousState != CircuitClosed {
		cb.logger().Info("circuit breaker closed", "from", previousState.String())
		if notify != nil {
			notify(previousState.String(), "CLOSED", 0, "")
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
	case cb.state == CircuitHalted:
		// Counted, and the halt stands: an Open would let the cooldown end it.
	case cb.state == CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.probing = false
		from, tripped = "HALF-OPEN", true
	case cb.consecutiveFailures >= cb.maxFailures && cb.state != CircuitOpen:
		cb.state = CircuitOpen
		from, tripped = "CLOSED", true
	}
	notify := cb.OnStateChange
	cb.mu.Unlock()

	if !tripped {
		cb.logger().Warn("circuit breaker counted a failure", "failures", n, "max", cb.maxFailures, "err", err)
		return
	}
	cb.logger().Error("circuit breaker open", "from", from, "failures", n, "err", err, "cooldown", cb.cooldownDuration)
	if notify != nil {
		notify(from, "OPEN", n, err.Error())
	}
}

// State returns the current circuit breaker state and stats.
func (cb *CircuitBreaker) State() (CircuitBreakerState, int, int64, int64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state, cb.consecutiveFailures, cb.TotalSuccesses, cb.TotalFailures
}

// Reset forces the circuit breaker back to CLOSED state. It is the only exit
// from HALTED, and the exit is reported through OnStateChange as the halt was.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	prev := cb.state
	cb.state = CircuitClosed
	cb.consecutiveFailures = 0
	cb.probing = false
	cb.haltReason = ""
	notify := cb.OnStateChange
	cb.mu.Unlock()
	cb.logger().Info("circuit breaker reset to closed", "from", prev.String())
	if prev != CircuitClosed && notify != nil {
		notify(prev.String(), "CLOSED", 0, "")
	}
}
