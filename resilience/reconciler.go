package resilience

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
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
	// until a human has looked. A trip halts the breaker until its Reset; no
	// cooldown and no later success clears it.
	HaltOnMismatch bool
}

// DefaultReconciliationConfig returns production defaults: daily, strict,
// halting.
func DefaultReconciliationConfig() ReconciliationConfig {
	return ReconciliationConfig{Interval: 24 * time.Hour, InitialDelay: time.Minute, HaltOnMismatch: true}
}

// UTXOSource is the indexer-side cache under test. *electrumx.Client satisfies it.
type UTXOSource interface {
	RefreshAll(ctx context.Context) error
	LastRefresh() (time.Time, error)
	GetAllUTXOs() []types.UTXO
}

// Node is the independent source of truth. *rpc.Client satisfies it.
type Node interface {
	RequireSynced(ctx context.Context) error
	GetTxOut(ctx context.Context, txid string, vout uint32, includeMempool bool) (*rpc.TxOut, error)
}

// SpentSet is this process's record of the inputs it has reserved or spent.
// *utxo.SpentSet satisfies it; give the reconciler the same set the
// withdraw.Engine writes.
type SpentSet interface {
	IsSpent(txid string, vout uint32) bool
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
	OwnSpends  int   // outpoints the node lacks and this process's SpentSet records; not findings, not in the totals
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
	log      *slog.Logger

	// OnAlert receives a human-readable message for every run that is not
	// clean. OnReport, if set, receives every report.
	OnAlert  func(message string)
	OnReport func(Report)

	// Spent is the withdraw.Engine's spent set. An output the node no longer
	// has because this process spent it is not a disagreement between the
	// cache and the chain; without Spent every such output is a finding and,
	// with HaltOnMismatch, halts payouts. Set before Start, and Start after
	// the engine's Recover has re-marked the set from the intent store.
	Spent SpentSet
}

// NewReconciler wires a reconciler. cb may be nil, in which case
// HaltOnMismatch has no effect beyond the alert. logger receives every run's
// verdict; nil discards.
func NewReconciler(source UTXOSource, node Node, cb *CircuitBreaker, cfg ReconciliationConfig, logger *slog.Logger) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if cfg.InitialDelay < 0 {
		cfg.InitialDelay = 0
	}
	return &Reconciler{source: source, node: node, cb: cb, cfg: cfg, stopCh: make(chan struct{}), log: logutil.Or(logger)}
}

// Start launches the background goroutine. The initial delay and the ticker
// both honour Stop and ctx; every run is made under ctx.
func (r *Reconciler) Start(ctx context.Context) {
	r.log.Info("reconciler starting", "interval", r.cfg.Interval, "initial_delay", r.cfg.InitialDelay,
		"threshold_shors", r.cfg.DeltaThreshold, "halt_on_mismatch", r.cfg.HaltOnMismatch)
	go func() {
		select {
		case <-time.After(r.cfg.InitialDelay):
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		}
		// A timer and the context can be ready together and select picks
		// either; a run must not start once the context has ended.
		if ctx.Err() != nil {
			return
		}
		r.Run(ctx)
		ticker := time.NewTicker(r.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				r.Run(ctx)
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			}
		}
	}()
}

// Stop halts the goroutine. Safe to call more than once.
func (r *Reconciler) Stop() { r.stopOnce.Do(func() { close(r.stopCh) }) }

// Run performs one reconciliation and returns its report. It also alerts and,
// when configured, trips the breaker. A run the context ends before it has
// found anything is Incomplete and is returned and reported through OnReport,
// but it neither alerts nor trips the breaker: the book was not checked, it
// was not found wrong, and a shutdown must not page the operator or halt the
// next start. A run the context ends after it has recorded a mismatch alerts
// and trips like any other: what it found is real whatever ended it.
//
// Whether the context ended is read from ctx itself, not from the error
// chain: a source or node cut off by the cancel may report the failure in
// its own words, without wrapping the context's error.
func (r *Reconciler) Run(ctx context.Context) Report {
	rep := r.reconcile(ctx)
	if r.OnReport != nil {
		r.OnReport(rep)
	}
	if rep.Clean() {
		r.log.Info("reconciliation clean", "outpoints", rep.Checked, "own_spends", rep.OwnSpends, "node_total_shors", rep.NodeTotal)
		return rep
	}
	if len(rep.Findings) == 0 && (ctx.Err() != nil || errors.Is(rep.Incomplete, context.Canceled) || errors.Is(rep.Incomplete, context.DeadlineExceeded)) {
		r.log.Warn("reconciliation not completed: the context ended", "err", rep.Incomplete)
		return rep
	}
	msg := r.describe(rep)
	r.log.Error("reconciliation alert", "msg", msg)
	if r.OnAlert != nil {
		r.OnAlert(msg)
	}
	if r.cfg.HaltOnMismatch && r.cb != nil {
		r.cb.Trip(errors.New("reconciler: " + msg))
	}
	return rep
}

// describe renders a report that is not clean. A run that recorded findings
// and then could not complete names both: the findings are what the operator
// acts on, the cause of the stop is why the list may be short.
func (r *Reconciler) describe(rep Report) string {
	if rep.Incomplete != nil && len(rep.Findings) == 0 {
		return fmt.Sprintf("reconciliation could not complete: %v", rep.Incomplete)
	}
	delta := rep.CacheTotal - rep.NodeTotal
	msg := fmt.Sprintf("cache and node disagree: %d finding(s), cache %d shors vs node %d shors (delta %d)",
		len(rep.Findings), rep.CacheTotal, rep.NodeTotal, delta)
	if rep.Incomplete != nil {
		msg += fmt.Sprintf("; the run then stopped: %v", rep.Incomplete)
	}
	for i, f := range rep.Findings {
		if i == 5 {
			msg += fmt.Sprintf("; and %d more", len(rep.Findings)-5)
			break
		}
		msg += fmt.Sprintf("; %s:%d %s", f.TxID, f.Vout, f.Reason)
	}
	return msg
}

func (r *Reconciler) reconcile(ctx context.Context) Report {
	rep := Report{At: time.Now()}

	// A refresh that fails is itself a finding: the cache under test is stale.
	if err := r.source.RefreshAll(ctx); err != nil {
		rep.Incomplete = fmt.Errorf("indexer refresh failed: %w", err)
		return rep
	}
	if at, err := r.source.LastRefresh(); err != nil || at.IsZero() {
		rep.Incomplete = fmt.Errorf("indexer cache not fresh (last %v, err %v)", at, err)
		return rep
	}
	// Only a caught-up node can give a verdict.
	if err := r.node.RequireSynced(ctx); err != nil {
		rep.Incomplete = err
		return rep
	}

	for _, u := range r.source.GetAllUTXOs() {
		if u.AssetType != types.AssetTypeSOQ {
			continue
		}
		out, err := r.node.GetTxOut(ctx, u.TxID, u.Vout, true)
		if err != nil {
			rep.Incomplete = fmt.Errorf("gettxout %s:%d: %w", u.TxID, u.Vout, err)
			return rep
		}
		// Read after the lookup: the engine writes the set before it sends,
		// so whenever the node answers null for our own spend, the entry is
		// already there, including for a spend made during this pass.
		if out == nil && r.Spent != nil && r.Spent.IsSpent(u.TxID, u.Vout) {
			rep.OwnSpends++
			continue
		}
		rep.Checked++
		rep.CacheTotal += u.Value
		switch {
		case out == nil:
			rep.Findings = append(rep.Findings, Finding{TxID: u.TxID, Vout: u.Vout, Address: u.Address,
				CacheVal: u.Value, Missing: true, Reason: "in cache, not in the node's UTXO set"})
		case out.Value != u.Value:
			rep.NodeTotal += out.Value
			rep.Findings = append(rep.Findings, Finding{TxID: u.TxID, Vout: u.Vout, Address: u.Address,
				CacheVal: u.Value, NodeVal: out.Value,
				Reason: fmt.Sprintf("cache value %d, node value %d", u.Value, out.Value)})
		default:
			rep.NodeTotal += out.Value
		}
	}
	if len(rep.Findings) == 0 && absInt64(rep.CacheTotal-rep.NodeTotal) > r.cfg.DeltaThreshold {
		rep.Findings = append(rep.Findings, Finding{Reason: fmt.Sprintf("totals differ by %d shors", rep.CacheTotal-rep.NodeTotal)})
	}
	return rep
}
