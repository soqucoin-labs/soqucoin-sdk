package electrumx

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A caller queued behind another call's stalled exchange returns when its own
// context ends, not when the other call's 30-second backstop does. Nothing of
// the queued request reaches the server, and the connection is usable once
// the holding call has returned.
func TestQueuedCallReturnsWhenItsOwnContextEnds(t *testing.T) {
	var mu sync.Mutex
	methods := map[string]int{}
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		methods[req.Method]++
		mu.Unlock()
		if req.Method == "slow" {
			return nil // never replies; the connection stays held
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)

	holdCtx, releaseHold := context.WithCancel(context.Background())
	defer releaseHold()
	held := make(chan error, 1)
	go func() {
		_, err := c.Call(holdCtx, "slow", []interface{}{})
		held <- err
	}()
	// The connection is held once the slow request has reached the server.
	waitUntil := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := methods["slow"]
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(waitUntil) {
			t.Fatal("the holding call never reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(ctx, "queued", []interface{}{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call: %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the queued call waited %v behind the held connection", time.Since(start))
	}
	mu.Lock()
	q := methods["queued"]
	mu.Unlock()
	if q != 0 {
		t.Fatalf("the queued request reached the server %d time(s)", q)
	}

	releaseHold()
	if err := <-held; !errors.Is(err, context.Canceled) {
		t.Fatalf("holding call: %v, want context.Canceled", err)
	}
	if _, err := c.Call(context.Background(), "after", []interface{}{}); err != nil {
		t.Fatalf("call after the hold: %v", err)
	}
}

// A pass cut short during one address's call names that address alone. The
// address carries the error in its record; the addresses after it are not
// attempted and their records are untouched.
func TestRefreshAllCutMidCallNamesTheAddressItStoppedAt(t *testing.T) {
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	var mu sync.Mutex
	calls := 0
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
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
	if _, rerr := c.LastRefreshOf(a1); rerr == nil {
		t.Error("the address whose call was cut carries no error")
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

// deadlineRefusingConn accepts its first deadline, the call's backstop, and
// refuses every later one: the shape of a socket closed underneath the client
// between the start of a call and the cancel.
type deadlineRefusingConn struct {
	net.Conn
	n atomic.Int32
}

var errDeadlineRefused = errors.New("deadline refused")

func (d *deadlineRefusingConn) SetDeadline(at time.Time) error {
	if d.n.Add(1) > 1 {
		return errDeadlineRefused
	}
	return d.Conn.SetDeadline(at)
}

// A socket that refuses the cancel's deadline is closed instead, so the call
// still returns with its context and does not wait out the backstop, and the
// connection is dropped: the next call reports ErrNotConnected and a Reconnect
// restores service.
func TestSocketRefusingTheCancelDeadlineIsClosedAndDropped(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "slow" {
			return nil
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	c.lockConnBlocking()
	c.conn = &deadlineRefusingConn{Conn: c.conn}
	c.unlockConn()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(ctx, "slow", []interface{}{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call on the refusing socket: %v, want the context's error", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the call waited %v: the refused deadline left the read blocked", time.Since(start))
	}
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after the refused deadline: %v, want ErrNotConnected", err)
	}
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), "anything", []interface{}{}); err != nil {
		t.Fatalf("after reconnect: %v", err)
	}
}
