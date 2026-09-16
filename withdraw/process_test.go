package withdraw

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

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
	ctx := context.Background()
	if _, _, err := e.Submit(ctx, "w1", dst, 1_000_000, 1000); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Hold both workers in the window between reading the intent as Created
	// and building it, so the race is a test rather than a matter of timing:
	// without the barrier this defect showed in four runs out of five.
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
	// A second send of the same bytes is what Recover does by design and costs
	// the recipient nothing. A second set of bytes is the double payment.
	for _, raw := range sent {
		if raw != sent[0] {
			t.Fatalf("two different transactions went out for one intent: %v", sent)
		}
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
	if in.TxID != "" {
		t.Fatalf("a failed intent carries txid %q", in.TxID)
	}
}
