package electrumx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A call whose context ends while the server is silent returns the context's
// error at once, and the connection stays usable: the reply the server sends
// later carries the old id and is discarded by the next call, which gets its
// own reply. The stream never goes out of step.
func TestCallReturnsWhenTheContextEndsAndTheConnectionStaysUsable(t *testing.T) {
	var mu sync.Mutex
	var heldID int64
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		switch req.Method {
		case "slow":
			// No reply now; it is delivered in front of the next reply.
			mu.Lock()
			heldID = req.ID
			mu.Unlock()
			return nil
		case "echo":
			mu.Lock()
			late := heldID
			heldID = 0
			mu.Unlock()
			if late != 0 {
				return []string{reply(late, `"late"`), reply(req.ID, `"real"`)}
			}
			return []string{reply(req.ID, `"real"`)}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.Call(ctx, "slow", []interface{}{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the call took %v to return after the cancel", time.Since(start))
	}

	// The late reply to the cancelled request arrives first; it is not this
	// call's reply.
	res, err := c.Call(context.Background(), "echo", []interface{}{})
	if err != nil {
		t.Fatalf("call after a cancelled one: %v; the connection should still be usable", err)
	}
	if string(res) != `"real"` {
		t.Fatalf("got %s; the late reply to the cancelled request was returned as this call's reply", res)
	}
	res, err = c.Call(context.Background(), "echo", []interface{}{})
	if err != nil || string(res) != `"real"` {
		t.Fatalf("third call: %s %v", res, err)
	}
}

// A context that has already ended sends nothing.
func TestCallWithAnEndedContextSendsNothing(t *testing.T) {
	var calls int
	var mu sync.Mutex
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Call(ctx, "anything", []interface{}{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("call with an ended context: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("%d requests reached the server under an ended context", calls)
	}
}

// A context's deadline shorter than the call deadline bounds the exchange.
func TestCallHonoursTheContextDeadline(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return nil // never replies
	})
	c := connect(t, stub)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(ctx, "slow", []interface{}{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the call outlived its context by %v", time.Since(start)-100*time.Millisecond)
	}
}

// RefreshAll under an ended context sends nothing and stops at the first
// address, naming only that one: it does not attempt the rest and fail each.
// Records for addresses already refreshed stand.
func TestRefreshAllStopsAtTheContext(t *testing.T) {
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	var calls int
	var mu sync.Mutex
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{reply(req.ID, `[]`)}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.RefreshAll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshAll under an ended context: %v", err)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 0 {
		t.Fatalf("%d requests reached the server under an ended context", n)
	}
	if msg := err.Error(); !strings.Contains(msg, a1) || strings.Contains(msg, a2) {
		t.Fatalf("error should name the address it stopped at and no other: %s", msg)
	}
	if at, _ := c.LastRefreshOf(a1); !at.IsZero() {
		t.Error("an address was refreshed under an ended context")
	}
	if at, _ := c.LastRefresh(); !at.IsZero() {
		t.Error("the pass was recorded as complete")
	}
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if at, _ := c.LastRefresh(); at.IsZero() {
		t.Error("a full pass with a live context was not recorded")
	}
}

// StartPolling ends with its context; Stop still works too.
func TestPollingStopsWithTheContext(t *testing.T) {
	var calls int
	var mu sync.Mutex
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{reply(req.ID, `[]`)}
	})
	c := NewClient(stub.addr(), 20*time.Millisecond, nil)
	c.HRP = types.Stagenet.HRP
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{craftAddr(t, 0x33)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.StartPolling(ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	// A refresh in flight at the cancel may still complete; after that the
	// count must stand still. A loop still running at 20 ms would add about
	// ten in the second window.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 2 {
		t.Fatalf("polling made %d refreshes in 120 ms at a 20 ms interval", n)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after > n+1 {
		t.Fatalf("polling continued after its context ended: %d then %d refreshes", n, after)
	}
}

// A failed write closes the connection: part of a line may be on the wire and
// the stream cannot be trusted. The next call reports ErrNotConnected and a
// Reconnect restores service.
func TestFailedWriteClosesTheConnection(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	// Close the socket underneath the client, so the next write fails.
	c.connMu.Lock()
	c.conn.Close()
	c.connMu.Unlock()
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); err == nil {
		t.Fatal("a write on a closed socket succeeded")
	}
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after a failed write: %v, want ErrNotConnected", err)
	}
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); err != nil {
		t.Fatalf("after reconnect: %v", err)
	}
}
