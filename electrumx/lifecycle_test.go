package electrumx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// hangupServer accepts connections and answers the first n lines (n = 0:
// accept and hang up at once; n = 3: complete the handshake, then hang up).
// With hold set it then keeps the socket open and silent instead of closing
// it. It counts accepts.
type hangupServer struct {
	ln      net.Listener
	answer  int
	hold    bool
	accepts atomic.Int64
}

func newHangupServer(t *testing.T, answer int, hold bool) *hangupServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &hangupServer{ln: ln, answer: answer, hold: hold}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepts.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for i := 0; i < s.answer; i++ {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req request
					if json.Unmarshal(line, &req) != nil {
						return
					}
					var res string
					switch req.Method {
					case "server.version":
						res = `"ElectrumX 1.16"`
					case "server.features":
						res = fmt.Sprintf(`{"genesis_hash":%q}`, types.Stagenet.GenesisHash)
					default:
						res = `{"height":1,"hex":"00"}`
					}
					c.Write([]byte(reply(req.ID, res) + "\n"))
				}
				if s.hold {
					io.Copy(io.Discard, c) // silent until the peer or the cleanup closes it
				}
			}(c)
		}
	}()
	return s
}

// A server that accepts and hangs up before the handshake fails Connect at
// once, and Stop is not held behind it. Before this, Connect waited out the
// 30-second call deadline because the reader could not report the loss
// while Connect held the connection lock.
func TestConnectFailsAtOnceWhenTheServerHangsUpInTheHandshake(t *testing.T) {
	srv := newHangupServer(t, 0, false)
	c := NewClient(srv.ln.Addr().String(), time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	start := time.Now()
	err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect succeeded against a server that hung up")
	}
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Connect: %v, want the connection loss", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Connect took %v against a server that hung up at once", d)
	}

	// Stop while a Connect is in its handshake against a server that answers
	// server.version and then keeps the socket open and silent: Stop must not
	// wait for the call deadline, and the Connect must fail.
	held := newHangupServer(t, 1, true)
	c2 := NewClient(held.ln.Addr().String(), time.Hour, nil)
	setHRP(t, c2, types.Stagenet.HRP)
	connErr := make(chan error, 1)
	go func() { connErr <- c2.Connect(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	stopped := make(chan struct{})
	go func() { c2.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop waited more than 3 s behind a Connect in its handshake")
	}
	if err := <-connErr; err == nil {
		t.Fatal("Connect succeeded after Stop")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Stop took %v", d)
	}
}

// A server that completes the handshake and then hangs up is redialled at
// the backoff ladder's pace, not in a tight loop: a handful of connections in
// two seconds, not thousands.
func TestReconnectIsPacedWhenTheConnectionDiesAfterTheHandshake(t *testing.T) {
	srv := newHangupServer(t, 3, false)
	c := NewClient(srv.ln.Addr().String(), time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{craftAddr(t, 0x11)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	time.Sleep(2 * time.Second)
	// Ladder: the first connection, then retries at 1 s and 2 s at most.
	if n := srv.accepts.Load(); n > 5 {
		t.Fatalf("%d connections in 2 s against a server that hangs up after the handshake; the reconnect is not paced", n)
	}
	if n := srv.accepts.Load(); n < 2 {
		t.Fatalf("%d connections in 2 s; the refresher did not reconnect at all", n)
	}
}

// A re-subscribe whose status equals the one last seen still refreshes an
// address whose last attempt failed: the shortcut is for a clean record only.
func TestUnchangedStatusDoesNotSkipARefreshAfterAFailure(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	stub.set(sh1, "s0", `[]`)
	c := NewClient(stub.addr(), time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pass(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// The indexer refuses the address; the record carries the error.
	stub.mu.Lock()
	stub.failList[sh1] = true
	stub.mu.Unlock()
	if _, err := c.pass(context.Background(), true); err == nil {
		t.Fatal("full pass with a failing listunspent succeeded")
	}
	stub.mu.Lock()
	stub.failList[sh1] = false
	stub.mu.Unlock()
	// The failed address is also marked changed by the pass; clear that so
	// the shortcut's own guard is what decides here.
	c.mu.Lock()
	c.changed = map[string]bool{}
	c.mu.Unlock()
	before := stub.count("blockchain.scripthash.listunspent", sh1)
	// Reconnect: the status is unchanged, but the record is not clean.
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pass(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != before+1 {
		t.Fatalf("%d listunspent after the reconnect, want one: an address with a failed record must be refreshed", n-before)
	}
	if at, err := c.LastRefreshOf(a1); err != nil || at.IsZero() {
		t.Fatalf("record after the refresh: %v %v", at, err)
	}
}

// A notification carried by a connection that is no longer live changes
// nothing: the new connection re-subscribes and learns the state itself.
func TestNotificationFromAReplacedConnectionIsIgnored(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	c := NewClient(stub.addr(), time.Hour, nil)
	setHRP(t, c, types.Stagenet.HRP)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pass(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	old := c.liveGen.Load()
	c.noteChange(old+1, sh1, "from-the-future")
	c.noteChange(old-1, sh1, "from-the-past")
	c.mu.RLock()
	changed, dirty := c.changed[a1], c.refreshed[a1].dirty
	c.mu.RUnlock()
	if changed || dirty {
		t.Fatal("a notification from another generation marked the address")
	}
	c.noteChange(old, sh1, "now")
	c.mu.RLock()
	changed = c.changed[a1]
	c.mu.RUnlock()
	if !changed {
		t.Fatal("a notification from the live generation did not mark the address")
	}
}

// TrackAddresses may replace the set while Start is running. Under the race
// detector this pins the locking of everything both sides touch.
func TestTrackAddressesWhileRunningIsSafe(t *testing.T) {
	stub := newPushStub(t)
	addrs := []string{craftAddr(t, 0x11), craftAddr(t, 0x22), craftAddr(t, 0x33)}
	c, _ := startClient(t, stub, addrs[0])
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := c.TrackAddresses(addrs[:1+i%3]); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for i := 0; i < 50; i++ {
		c.GetHistory(context.Background(), addrs[i%3])
		c.LastRefreshOf(addrs[i%3])
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()
	waitFor(t, "the final set to be subscribed", func() bool {
		return c.isSubscribed(addrs[0]) && c.isSubscribed(addrs[1])
	})
}

// A listunspent that fails after a notification is retried on the backoff
// ladder, not left for the reconcile: the address stays marked and the next
// wake refreshes it.
func TestFailedRefreshAfterANotificationIsRetried(t *testing.T) {
	stub := newPushStub(t)
	a1 := craftAddr(t, 0x11)
	sh1 := scripthashOf(t, a1)
	stub.set(sh1, "s0", `[]`)
	c, _ := startClient(t, stub, a1)
	waitFor(t, "the first refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) == 1 })
	stub.mu.Lock()
	stub.failList[sh1] = true
	stub.mu.Unlock()
	stub.notify(sh1, "s1", oneUTXO)
	waitFor(t, "the failed refresh", func() bool { return stub.count("blockchain.scripthash.listunspent", sh1) >= 2 })
	stub.mu.Lock()
	stub.failList[sh1] = false
	stub.mu.Unlock()
	// The retry is due after one second of backoff.
	deadline := time.Now().Add(4 * time.Second)
	for len(c.GetUTXOs(a1)) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the failed refresh was not retried within 4 s; %d listunspent so far", stub.count("blockchain.scripthash.listunspent", sh1))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := c.LastRefreshOf(a1); err != nil {
		t.Fatalf("record after the retry still carries the error: %v", err)
	}
	if !strings.Contains(fmt.Sprint(c.GetUTXOs(a1)[0].TxID), txA) {
		t.Fatal("the retry did not land the listunspent reply")
	}
}

// One address the indexer refuses on every pass must not starve the
// reconcile. The full pass is the only safety net against a notification the
// server never sent, so a tick that lands during a backoff is deferred and
// made up by the pass after it, not dropped. The policy that decides this is
// model-tested in refreshpolicy_model_test.go; the same statement is made here
// against a real refresher and a real server.
func TestARefusedAddressDoesNotStarveTheReconcile(t *testing.T) {
	stub := newPushStub(t)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	stub.mu.Lock()
	stub.failList[sh2] = true // the indexer refuses this address, always
	stub.mu.Unlock()
	_, _ = startClientEvery(t, stub, 150*time.Millisecond, a1, a2)

	waitFor(t, "the first pass", func() bool {
		return stub.count("blockchain.scripthash.listunspent", sh2) >= 1
	})
	// Reconcile ticks land every 150 ms throughout the one-second backoff and
	// are deferred; the retry's pass must therefore be full and refresh the
	// healthy address again. Without the deferral the count stays at the one
	// refresh of the opening pass, for the life of the process.
	waitFor(t, "the deferred reconcile to be made up", func() bool {
		return stub.count("blockchain.scripthash.listunspent", sh1) >= 2
	})
	if n := stub.count("blockchain.scripthash.subscribe", sh1); n != 1 {
		t.Fatalf("the healthy address was subscribed %d times: the refused address rebuilt the connection", n)
	}
}
