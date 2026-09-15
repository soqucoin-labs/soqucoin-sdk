package resilience

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
)

// cancellingSource ends the context when refreshed and reports the failure in
// its own words, without the context's error in the chain: the shape of an
// indexer client cut off by the cancel mid-exchange.
type cancellingSource struct {
	fakeSource
	cancel context.CancelFunc
}

var errIndexerReset = errors.New("indexer: connection reset")

func (s *cancellingSource) RefreshAll(context.Context) error {
	s.cancel()
	return errIndexerReset
}

// Whether the context ended is read from the context, not from the error: a
// run cut off by the cancel whose source reports the failure in its own words
// is Incomplete without alerting or tripping. The same words under a live
// context are an indexer failure and do both.
func TestRunEndedByTheContextIsReadFromTheContextNotTheError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &cancellingSource{cancel: cancel}
	node := &fakeNode{synced: true}
	cb := NewCircuitBreaker(3, time.Minute, nil)
	r := NewReconciler(src, node, cb, DefaultReconciliationConfig(), nil)
	alerted := false
	r.OnAlert = func(string) { alerted = true }
	rep := r.Run(ctx)
	if rep.Incomplete == nil || errors.Is(rep.Incomplete, context.Canceled) || len(rep.Findings) != 0 {
		t.Fatalf("report %+v, want Incomplete without the context's error in its chain and no findings", rep)
	}
	if alerted {
		t.Fatal("a run the context ended alerted because its source did not wrap the context's error")
	}
	if st, _, _, _ := cb.State(); st != CircuitClosed {
		t.Fatalf("breaker %s after a run the context ended, want CLOSED", st)
	}

	// The control: the same failure under a live context alerts and trips.
	src2 := &fakeSource{refreshErr: errIndexerReset}
	cb2 := NewCircuitBreaker(3, time.Minute, nil)
	r2 := NewReconciler(src2, node, cb2, DefaultReconciliationConfig(), nil)
	alerted2 := false
	r2.OnAlert = func(string) { alerted2 = true }
	r2.Run(context.Background())
	if st, _, _, _ := cb2.State(); st != CircuitOpen || !alerted2 {
		t.Fatalf("breaker %s alerted=%v after a real indexer failure, want OPEN and alerted", st, alerted2)
	}
}

// Start under an ended context never runs, even with no initial delay, where
// the timer and the context are ready together and select picks either. The
// loop gives the race enough draws that a Start choosing the timer would show.
func TestStartUnderAnEndedContextNeverRuns(t *testing.T) {
	for i := 0; i < 64; i++ {
		src := &fakeSource{}
		node := &fakeNode{synced: true, outs: map[string]*rpc.TxOut{}}
		r := NewReconciler(src, node, nil, ReconciliationConfig{Interval: time.Hour}, nil)
		var ran atomic.Bool
		r.OnReport = func(Report) { ran.Store(true) }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.Start(ctx)
		time.Sleep(2 * time.Millisecond)
		r.Stop()
		if ran.Load() {
			t.Fatalf("draw %d: a run started under an ended context", i)
		}
	}
}
