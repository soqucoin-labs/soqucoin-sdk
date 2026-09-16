package withdraw

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// slowFirstSend keeps the winner of a race inside Broadcast long enough for the
// loser to re-read the intent while it is still Built. Without that the winner
// has already saved Broadcast by the time the loser looks, and the loser's own
// state check hides the defect the test below is for.
type slowFirstSend struct {
	inner *fakeNet
	calls atomic.Int32
	delay time.Duration
}

func (s *slowFirstSend) Broadcast(ctx context.Context, rawHex, txid string) (string, error) {
	if s.calls.Add(1) == 1 {
		time.Sleep(s.delay)
	}
	return s.inner.Broadcast(ctx, rawHex, txid)
}

// A withdrawal worker pool is the ordinary shape for an exchange, so two
// workers can take the same job. Intent's documentation calls ID an
// idempotency key that never produces a second transaction, and the package
// header names double payment as the first of the two failure modes it exists
// to prevent.
//
// Build's state check reads the caller's copy of the intent. Without the
// re-read under e.mu, both workers pass that check holding a Created copy, the
// lock serialises them into two builds, each selects different inputs because
// the other's are reserved, and each broadcasts. The recipient is paid twice.
func TestConcurrentProcessOfOneIntentBuildsOneTransaction(t *testing.T) {
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), net, coins())
	e.Broadcaster = &slowFirstSend{inner: net, delay: 200 * time.Millisecond}
	ctx := context.Background()
	if _, _, err := e.Submit(ctx, "w1", dst, 1_000_000, 1000); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Hold both workers in the window between reading the intent as Created
	// and building it, so the race is a test rather than a matter of timing.
	// Without the barrier the interleaving is left to the scheduler and the
	// defect escapes some runs, which makes the test unable to pin it.
	var arrived sync.WaitGroup
	arrived.Add(2)
	beforeBuild = func() { arrived.Done(); arrived.Wait() }
	t.Cleanup(func() { beforeBuild = func() {} })

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.Process(ctx, "w1")
		}()
	}
	wg.Wait()

	net.mu.Lock()
	builds, sent := net.builds, append([]string(nil), net.sent...)
	net.mu.Unlock()

	if builds != 1 {
		t.Errorf("built %d transactions for one intent, want 1", builds)
	}
	// One send, not merely one set of bytes sent twice. The worker that loses
	// the build race stops on the state error rather than following the intent
	// into Broadcast: two workers in Broadcast at once on separate copies would
	// have the second re-reserve inputs the first had already marked spent,
	// raise ErrReservationLost naming a withdrawal that does not exist, and save
	// Built over the Broadcast state the first recorded.
	if len(sent) != 1 {
		t.Fatalf("broadcast %d times for one intent, want 1: %v", len(sent), sent)
	}
}

// Failed is terminal and nothing was sent. Submit answers the same id with no
// error, so an exchange retrying a payout from a cron or a queue redelivery
// reaches Process a second time. Returning nil there reports a withdrawal as
// sent with an empty txid, and a caller feeding that nil to a circuit breaker
// records a success.
func TestProcessOfAnAlreadyFailedIntentReportsTheFailure(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	ctx := context.Background()
	// More than the coins can cover: the selector's error is not transient, so
	// Build fails the intent permanently.
	if _, _, err := e.Submit(ctx, "w1", dst, 100_000_000, 1000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	in, err := e.Process(ctx, "w1")
	if err == nil {
		t.Fatalf("first Process returned no error; state is %s", in.State)
	}
	if in.State != StateFailed {
		t.Fatalf("intent is %s after a permanent selector error, want %s", in.State, StateFailed)
	}

	in, err = e.Process(ctx, "w1")
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("second Process on a Failed intent returned %v, want ErrFailed", err)
	}
	// One withdrawal that cannot succeed says nothing about the node or the
	// indexer. resilience.CircuitBreaker reads rpc.ErrPermanent to decide that,
	// and counts anything it does not recognise as a systemic failure, so
	// without the wrapped sentinel three retries of one bad payout would open
	// the breaker and stop every withdrawal for its cooldown.
	if !errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("a failed intent is not reported as a per-request fault: %v", err)
	}
	if in.State != StateFailed {
		t.Fatalf("intent is %s on the second call, want %s", in.State, StateFailed)
	}
}
