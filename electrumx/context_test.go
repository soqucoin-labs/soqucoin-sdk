package electrumx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
		if req.Method == "anything" {
			mu.Lock()
			calls++
			mu.Unlock()
		}
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
		if req.Method == "blockchain.scripthash.listunspent" {
			mu.Lock()
			calls++
			mu.Unlock()
		}
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

// countingHandler counts the records a logger receives.
type countingHandler struct {
	mu sync.Mutex
	n  int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(context.Context, slog.Record) error {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// StartPolling ends with its context; Stop still works too. The goroutine is
// observed through the logger: a loop that outlived its context would go on
// failing its refresh, and logging it, at every tick, whether or not any
// request reaches the server.
func TestPollingStopsWithTheContext(t *testing.T) {
	var calls int
	var mu sync.Mutex
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{reply(req.ID, `[]`)}
	})
	logs := &countingHandler{}
	c := NewClient(stub.addr(), 20*time.Millisecond, slog.New(logs))
	setHRP(t, c, types.Stagenet.HRP)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{craftAddr(t, 0x33)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	// A refresh in flight at the cancel may still complete and log; after
	// that both the request count and the log count must stand still. A loop
	// that ignored its context would log a failed refresh at every 20 ms tick,
	// about ten in the second window, while no request reaches the server.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 2 {
		t.Fatalf("polling made %d refreshes in 120 ms at a 20 ms interval", n)
	}
	logged := logs.count()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after > n+1 {
		t.Fatalf("polling continued after its context ended: %d then %d refreshes", n, after)
	}
	if loggedAfter := logs.count(); loggedAfter > logged+1 {
		t.Fatalf("the polling goroutine is still running after its context ended: %d log records became %d", logged, loggedAfter)
	}
}

// failingWriteConn is a connection whose deadline can be set but whose write
// fails: the shape of a write cut short by the context or the peer.
type failingWriteConn struct{ net.Conn }

var errWriteCut = errors.New("write cut short")

func (f failingWriteConn) Write([]byte) (int, error) { return 0, errWriteCut }

// A write that fails closes the connection: part of a line may be on the
// wire and the stream cannot be trusted. So does a socket that refuses a
// deadline. In both cases the next call reports ErrNotConnected and a
// Reconnect restores service.
func TestFailedWriteClosesTheConnection(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)

	// The write fails; the deadline before it succeeded.
	c.lockConnBlocking()
	c.conn = failingWriteConn{c.conn}
	c.unlockConn()
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); !errors.Is(err, errWriteCut) {
		t.Fatalf("write on the failing connection: %v, want the write error", err)
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

	// The socket is closed underneath the client: the reader notices and
	// drops it, or the next write fails and drops it; either way the call
	// after that reports ErrNotConnected.
	c.lockConnBlocking()
	c.conn.Close()
	c.unlockConn()
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); err == nil {
		t.Fatal("a call on a closed socket succeeded")
	}
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after a closed socket: %v, want ErrNotConnected", err)
	}
}

// A reply the server leaves half sent poisons the line: the reader is waiting
// for the rest of it, the call's context ends and the call returns, and the
// next reply the server sends lands on the same line and cannot be parsed.
// That drops the connection, fails the call waiting on it, and a Reconnect
// restores service. Nothing is ever paired with a call by position.
func TestPartialReplyPoisonsTheLineUntilTheConnectionIsRebuilt(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "half" {
			// Half a line, no newline; the rest never comes.
			return []string{"{\"jsonrpc\":\"2.0\",\"id\":" + fmt.Sprint(req.ID) + ",\"result\":[{\"tx_hash\":\"aa\x00"}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "half", []interface{}{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("half a reply: %v, want the context's error", err)
	}
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after a partial reply: %v, want ErrNotConnected", err)
	}
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Call(context.Background(), "anything", []interface{}{}); err != nil || string(res) != "null" {
		t.Fatalf("after reconnect: %s %v", res, err)
	}
}
