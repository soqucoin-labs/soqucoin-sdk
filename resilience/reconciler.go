package resilience

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// ReconciliationConfig controls the periodic reconciliation behaviour.
type ReconciliationConfig struct {
	// Interval between runs (default 24h; values <= 0 use the default).
	Interval time.Duration
	// InitialDelay before the first run, so the caches settle (default 1m).
	InitialDelay time.Duration
	// DeltaThreshold is the largest tolerated difference, in shors, between
	// the cached spendable total and what the node confirms for the same
	// outpoints. Default 0: any difference is a finding. Individual outpoint
	// mismatches are findings regardless of this value.
	DeltaThreshold int64
	// HaltOnMismatch trips the circuit breaker when a run finds a mismatch or
	// cannot complete a verification. This is the right default for a system
	// that moves money: a book that does not match the chain must stop paying
	// until a human has looked.
	HaltOnMismatch bool
}

// DefaultReconciliationConfig returns production defaults: daily, strict,
// halting.
func DefaultReconciliationConfig() ReconciliationConfig {
	return ReconciliationConfig{Interval: 24 * time.Hour, InitialDelay: time.Minute, HaltOnMismatch: true}
}

// UTXOSource is the indexer-side cache under test. *electrumx.Client satisfies it.
type UTXOSource interface {
	RefreshAll() error
	LastRefresh() (time.Time, error)
	GetAllUTXOs() []types.UTXO
}

// Node is the independent source of truth. *rpc.Client satisfies it.
type Node interface {
	RequireSynced() error
	GetTxOut(txid string, vout uint32, includeMempool bool) (*rpc.TxOut, error)
}

// Finding is one disagreement between the cache and the node.
type Finding struct {
	TxID     string
	Vout     uint32
	Address  string
	CacheVal int64 // shors the cache believes
	NodeVal  int64 // shors the node confirms; 0 with Missing set
	Missing  bool  // the node does not have the output
	Reason   string
}

// Report is the outcome of one reconciliation run.
type Report struct {
	At         time.Time
	Checked    int   // outpoints verified against the node
	CacheTotal int64 // spendable shors per the cache
	NodeTotal  int64 // shors the node confirmed for the same outpoints
	Findings   []Finding
	Incomplete error // set when the run could not verify (refresh failed, node syncing, RPC error)
}

// Clean reports whether the run completed and found nothing.
func (r Report) Clean() bool {
	return r.Incomplete == nil && len(r.Findings) == 0
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Reconciler periodically verifies the indexer cache against the node,
// outpoint by outpoint. Earlier versions refreshed the cache and then read the
// same cache back, which could not detect anything; this one asks the node.
type Reconciler struct {
	source UTXOSource
	node   Node
	cb     *CircuitBreaker
	cfg    ReconciliationConfig

	stopCh   chan struct{}
	stopOnce sync.Once

	// OnAlert receives a human-readable message for every run that is not
	// clean. OnReport, if set, receives every report.
	OnAlert  func(message string)
	OnReport func(Report)
}

// NewReconciler wires a reconciler. cb may be nil, in which case
// HaltOnMismatch has no effect beyond the alert.
func NewReconciler(source UTXOSource, node Node, cb *CircuitBreaker, cfg ReconciliationConfig) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if cfg.InitialDelay < 0 {
		cfg.InitialDelay = 0
	}
	return &Reconciler{source: source, node: node, cb: cb, cfg: cfg, stopCh: make(chan struct{})}
}

// Start launches the background goroutine. The initial delay and the ticker
// both honour Stop.
func (r *Reconciler) Start() {
	log.Printf("[reconciler] starting (interval %v, initial delay %v, threshold %d shors, halt-on-mismatch %v)",
		r.cfg.Interval, r.cfg.InitialDelay, r.cfg.DeltaThreshold, r.cfg.HaltOnMismatch)
	go func() {
		select {
		case <-time.After(r.cfg.InitialDelay):
		case <-r.stopCh:
			return
		}
		r.Run()
		ticker := time.NewTicker(r.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.Run()
			case <-r.stopCh:
				return
			}
		}
	}()
}

// Stop halts the goroutine. Safe to call more than once.
func (r *Reconciler) Stop() { r.stopOnce.Do(func() { close(r.stopCh) }) }

// Run performs one reconciliation and returns its report. It also alerts and,
// when configured, trips the breaker.
func (r *Reconciler) Run() Report {
	rep := r.reconcile()
	if r.OnReport != nil {
		r.OnReport(rep)
	}
	if rep.Clean() {
		log.Printf("[reconciler] clean: %d outpoints, %d shors confirmed by the node", rep.Checked, rep.NodeTotal)
		return rep
	}
	msg := r.describe(rep)
	log.Printf("[reconciler] ALERT: %s", msg)
	if r.OnAlert != nil {
		r.OnAlert(msg)
	}
	if r.cfg.HaltOnMismatch && r.cb != nil {
		r.cb.Trip(errors.New("reconciler: " + msg))
	}
	return rep
}

func (r *Reconciler) describe(rep Report) string {
	if rep.Incomplete != nil {
		return fmt.Sprintf("reconciliation could not complete: %v", rep.Incomplete)
	}
	delta := rep.CacheTotal - rep.NodeTotal
	msg := fmt.Sprintf("cache and node disagree: %d finding(s), cache %d shors vs node %d shors (delta %d)",
		len(rep.Findings), rep.CacheTotal, rep.NodeTotal, delta)
	for i, f := range rep.Findings {
		if i == 5 {
			msg += fmt.Sprintf("; and %d more", len(rep.Findings)-5)
			break
		}
		msg += fmt.Sprintf("; %s:%d %s", shortID(f.TxID, 12), f.Vout, f.Reason)
	}
	return msg
}

func (r *Reconciler) reconcile() Report {
	rep := Report{At: time.Now()}

	// A refresh that fails is itself a finding: the cache under test is stale.
	if err := r.source.RefreshAll(); err != nil {
		rep.Incomplete = fmt.Errorf("indexer refresh failed: %w", err)
		return rep
	}
	if at, err := r.source.LastRefresh(); err != nil || at.IsZero() {
		rep.Incomplete = fmt.Errorf("indexer cache not fresh (last %v, err %v)", at, err)
		return rep
	}
	// Only a caught-up node can give a verdict.
	if err := r.node.RequireSynced(); err != nil {
		rep.Incomplete = err
		return rep
	}

	for _, u := range r.source.GetAllUTXOs() {
		if u.SpentPending || u.AssetType != types.AssetTypeSOQ {
			continue
		}
		rep.Checked++
		rep.CacheTotal += u.Value
		out, err := r.node.GetTxOut(u.TxID, u.Vout, true)
		if err != nil {
			rep.Incomplete = fmt.Errorf("gettxout %s:%d: %w", shortID(u.TxID, 12), u.Vout, err)
			return rep
		}
		switch {
		case out == nil:
			rep.Findings = append(rep.Findings, Finding{TxID: u.TxID, Vout: u.Vout, Address: u.Address,
				CacheVal: u.Value, Missing: true, Reason: "in cache, not in the node's UTXO set"})
		case out.ValueShors() != u.Value:
			rep.NodeTotal += out.ValueShors()
			rep.Findings = append(rep.Findings, Finding{TxID: u.TxID, Vout: u.Vout, Address: u.Address,
				CacheVal: u.Value, NodeVal: out.ValueShors(),
				Reason: fmt.Sprintf("cache value %d, node value %d", u.Value, out.ValueShors())})
		default:
			rep.NodeTotal += out.ValueShors()
		}
	}
	if len(rep.Findings) == 0 && absInt64(rep.CacheTotal-rep.NodeTotal) > r.cfg.DeltaThreshold {
		rep.Findings = append(rep.Findings, Finding{Reason: fmt.Sprintf("totals differ by %d shors", rep.CacheTotal-rep.NodeTotal)})
	}
	return rep
}

// shortID truncates an identifier for logging without panicking on short input.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
