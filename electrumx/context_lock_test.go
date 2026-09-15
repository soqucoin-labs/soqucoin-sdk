package electrumx

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Calls run concurrently on the one connection: a call waiting for a slow
// reply does not hold the connection for the calls behind it. The second call
// gets its own reply while the first is still waiting, and the first returns
// with its own context.
func TestCallsRunConcurrentlyOnOneConnection(t *testing.T) {
	var mu sync.Mutex
	methods := map[string]int{}
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		methods[req.Method]++
		mu.Unlock()
		if req.Method == "slow" {
			return nil // never replies
		}
		return []string{reply(req.ID, `"real"`)}
	})
	c := connect(t, stub)

	slowCtx, cancelSlow := context.WithCancel(context.Background())
	defer cancelSlow()
	slow := make(chan error, 1)
	go func() {
		_, err := c.Call(slowCtx, "slow", []interface{}{})
		slow <- err
	}()
	waitUntil := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := methods["slow"]
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(waitUntil) {
			t.Fatal("the slow call never reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	res, err := c.Call(ctx, "echo", []interface{}{})
	if err != nil || string(res) != `"real"` {
		t.Fatalf("call beside a slow one: %s %v", res, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the call waited %v behind the slow one", time.Since(start))
	}
	select {
	case err := <-slow:
		t.Fatalf("the slow call returned %v before its context ended", err)
	default:
	}

	cancelSlow()
	if err := <-slow; !errors.Is(err, context.Canceled) {
		t.Fatalf("slow call: %v, want context.Canceled", err)
	}
	if _, err := c.Call(context.Background(), "after", []interface{}{}); err != nil {
		t.Fatalf("call after the cancel: %v", err)
	}
}

// A pass cut short during one address's call names that address alone in the
// returned error. No record takes the context's error: a shutdown says
// nothing about the indexer. The addresses after it are not attempted.
func TestRefreshAllCutMidCallNamesTheAddressItStoppedAt(t *testing.T) {
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	var mu sync.Mutex
	calls := 0
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method != "blockchain.scripthash.listunspent" {
			return []string{reply(req.ID, `null`)}
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil // the first address's reply never comes
		}
		return []string{reply(req.ID, `[]`)}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.RefreshAll(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RefreshAll cut mid-call: %v, want context.DeadlineExceeded", err)
	}
	if msg := err.Error(); !strings.Contains(msg, a1) || strings.Contains(msg, a2) {
		t.Fatalf("error should name the address whose call was cut and no other: %s", msg)
	}
	if at, rerr := c.LastRefreshOf(a1); rerr != nil || !at.IsZero() {
		t.Errorf("the address whose call was cut took the context's error into its record: at %v err %v", at, rerr)
	}
	if at, rerr := c.LastRefreshOf(a2); rerr != nil || !at.IsZero() {
		t.Errorf("the address after the cut was touched: at %v err %v", at, rerr)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d requests reached the server; the pass should have stopped after the first", n)
	}
}

// blockedWriteConn is a connection whose Write blocks, as a write does when
// the peer has stopped reading and the socket's send buffer is full. It
// returns when the write deadline is moved into the past or the connection
// is closed. With refuseCancel set it refuses the past deadline, the shape of
// a socket closed underneath the client, so only Close can free the write.
type blockedWriteConn struct {
	net.Conn
	refuseCancel bool
	once         sync.Once
	freed        chan struct{}
	closed       bool
	mu           sync.Mutex
}

var (
	errWriteTimeout = errors.New("write timeout")
	errDeadlineNo   = errors.New("deadline refused")
)

func newBlockedWriteConn(inner net.Conn, refuseCancel bool) *blockedWriteConn {
	return &blockedWriteConn{Conn: inner, refuseCancel: refuseCancel, freed: make(chan struct{})}
}

func (b *blockedWriteConn) Write([]byte) (int, error) {
	<-b.freed
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, net.ErrClosed
	}
	return 0, errWriteTimeout
}

func (b *blockedWriteConn) SetWriteDeadline(at time.Time) error {
	if at.After(time.Now()) {
		return nil // the call's backstop
	}
	if b.refuseCancel {
		return errDeadlineNo
	}
	b.once.Do(func() { close(b.freed) })
	return nil
}

func (b *blockedWriteConn) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.once.Do(func() { close(b.freed) })
	return b.Conn.Close()
}

// A context that ends during a blocked write frees the write through the
// write deadline, and the call returns the context's error at once. Part of
// a line may be on the wire, so the connection is dropped: the next call
// reports ErrNotConnected and a Reconnect restores service.
func TestCancelDuringABlockedWriteDropsTheConnection(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
			return []string{reply(req.ID, `null`)}
		})
		c := connect(t, stub)
		c.lockConnBlocking()
		c.conn = newBlockedWriteConn(c.conn, refuse)
		c.unlockConn()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := c.Call(ctx, "anything", []interface{}{})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("refuse=%v: blocked write: %v, want the context's error", refuse, err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("refuse=%v: the call waited %v: the cancel did not free the write", refuse, time.Since(start))
		}
		if _, err := c.Call(context.Background(), "anything", []interface{}{}); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("refuse=%v: after a cut write: %v, want ErrNotConnected", refuse, err)
		}
		if err := c.Reconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Call(context.Background(), "anything", []interface{}{}); err != nil {
			t.Fatalf("refuse=%v: after reconnect: %v", refuse, err)
		}
	}
}
