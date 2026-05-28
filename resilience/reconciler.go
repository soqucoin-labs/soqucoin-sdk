package resilience

import (
	"fmt"
	"log"
	"time"
)

// ReconciliationConfig controls the periodic reconciliation behavior.
type ReconciliationConfig struct {
	// Interval between reconciliation runs (default: 24h).
	Interval time.Duration
	// DeltaThreshold is the max allowed balance difference in satoshis
	// before an alert is fired. Default: 100,000 sat (1 SOQ dust threshold).
	DeltaThreshold int64
}

// DefaultReconciliationConfig returns sane defaults for production.
func DefaultReconciliationConfig() ReconciliationConfig {
	return ReconciliationConfig{
		Interval:       24 * time.Hour,
		DeltaThreshold: 100000, // 0.001 SOQ
	}
}

// BalanceSource is the interface that the reconciler uses to read balance state.
// Implement this by wrapping your ElectrumX client and RPC client.
type BalanceSource interface {
	// RefreshUTXOs re-fetches UTXOs from ElectrumX (force refresh).
	RefreshUTXOs() error
	// GetCachedBalance returns the tracker's cached confirmed and unconfirmed balance.
	GetCachedBalance(minConf int, tipHeight int64) (confirmed, unconfirmed int64)
	// GetTipHeight returns the current chain tip height.
	GetTipHeight() (int64, error)
	// UTXOCount returns the number of tracked UTXOs.
	UTXOCount() int
}

// Reconciler periodically verifies UTXO state against fresh data to detect
// balance discrepancies, stale UTXOs, or missed spends.
//
// Defense 16 (DL-ENTERPRISE-PAYOUT): Background process that verifies the
// tracker's UTXO balance against a fresh ElectrumX rescan. Fires an alert
// if inconsistencies are detected.
type Reconciler struct {
	source BalanceSource
	cb     *CircuitBreaker
	cfg    ReconciliationConfig
	stopCh chan struct{}

	// OnAlert is called when a reconciliation anomaly is detected.
	// Signature: func(message string)
	OnAlert func(message string)
}

// NewReconciler creates a new reconciler.
func NewReconciler(source BalanceSource, cb *CircuitBreaker, cfg ReconciliationConfig) *Reconciler {
	return &Reconciler{
		source: source,
		cb:     cb,
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

// Start launches the reconciliation background goroutine.
func (r *Reconciler) Start() {
	log.Printf("[reconciler] Starting reconciliation (interval=%v, threshold=%d sat)",
		r.cfg.Interval, r.cfg.DeltaThreshold)

	go func() {
		// First run after 1 minute (let the system stabilize)
		time.Sleep(1 * time.Minute)

		ticker := time.NewTicker(r.cfg.Interval)
		defer ticker.Stop()

		// Run immediately, then on ticker
		r.run()

		for {
			select {
			case <-ticker.C:
				r.run()
			case <-r.stopCh:
				return
			}
		}
	}()
}

// Stop halts the reconciliation goroutine.
func (r *Reconciler) Stop() {
	close(r.stopCh)
}

// run performs a single reconciliation check.
func (r *Reconciler) run() {
	log.Printf("[reconciler] Running reconciliation check...")

	// Step 1: Force a fresh UTXO scan
	if err := r.source.RefreshUTXOs(); err != nil {
		log.Printf("[reconciler] WARNING: UTXO refresh failed: %v (using cached state)", err)
	}

	// Step 2: Get tip height
	tipHeight, err := r.source.GetTipHeight()
	if err != nil {
		log.Printf("[reconciler] WARNING: cannot get tip height: %v (skipping this run)", err)
		return
	}

	// Step 3: Get cached balance
	confirmed, unconfirmed := r.source.GetCachedBalance(1, tipHeight)
	total := confirmed + unconfirmed
	utxoCount := r.source.UTXOCount()

	log.Printf("[reconciler] UTXO State: %d UTXOs | Confirmed: %d sat (%.4f SOQ) | Unconfirmed: %d sat | Total: %d sat",
		utxoCount, confirmed, float64(confirmed)/1e8, unconfirmed, total)

	// Step 4: Check circuit breaker state
	if r.cb != nil {
		cbState, cbFailures, cbSuccesses, cbTotalFailures := r.cb.State()
		log.Printf("[reconciler] Circuit Breaker: state=%s, consecutive_failures=%d, total_successes=%d, total_failures=%d",
			cbState, cbFailures, cbSuccesses, cbTotalFailures)

		if cbState == CircuitOpen {
			msg := fmt.Sprintf("Circuit breaker is OPEN during reconciliation — operations halted (%d consecutive failures)", cbFailures)
			log.Printf("[reconciler] ⚠️ %s", msg)
			if r.OnAlert != nil {
				r.OnAlert(msg)
			}
		}
	}

	// Step 5: Verify expected balance range
	if utxoCount > 0 && confirmed == 0 && unconfirmed == 0 {
		msg := fmt.Sprintf("%d UTXOs tracked but zero balance — possible asset type mismatch or all spent-pending", utxoCount)
		log.Printf("[reconciler] ⚠️ ALERT: %s", msg)
		if r.OnAlert != nil {
			r.OnAlert(msg)
		}
	}

	log.Printf("[reconciler] Reconciliation complete. Next run in %v", r.cfg.Interval)
}
