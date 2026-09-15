package electrumx

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// One address the indexer refuses never rebuilds the connection: an
// application error says nothing about the connection, so the healthy
// addresses keep their one subscription while the refused one is retried on
// the backoff ladder.
func TestARefusedAddressDoesNotRebuildTheConnection(t *testing.T) {
	stub := newPushStub(t)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	stub.mu.Lock()
	stub.failList[sh2] = true
	stub.mu.Unlock()
	_, _ = startClient(t, stub, a1, a2)
	waitFor(t, "the first pass", func() bool { return stub.count("blockchain.scripthash.listunspent", sh2) >= 1 })
	time.Sleep(3200 * time.Millisecond) // retries at 1 s and 2 s
	if n := stub.count("blockchain.scripthash.subscribe", sh1); n != 1 {
		t.Fatalf("the healthy address was subscribed %d times: a refused address rebuilt the connection", n)
	}
	if n := stub.count("blockchain.scripthash.listunspent", sh2); n < 2 {
		t.Fatalf("the refused address was retried %d times in 3 s, want at least the 1 s retry", n-1)
	}
	// Scope: the reconcile is an hour away here, so this says only that the
	// retries after a failure are confined to the address that failed. It says
	// nothing about whether the reconcile still runs, and must not be read as
	// saying so; that is TestARefusedAddressDoesNotStarveTheReconcile.
	if n := stub.count("blockchain.scripthash.listunspent", sh1); n != 1 {
		t.Fatalf("the healthy address was refreshed %d times; a retry is scoped to the address that failed", n)
	}
}

// A context that ends during a subscribe call leaves no error on the record:
// a shutdown says nothing about the indexer.
func TestContextEndingDuringSubscribeLeavesTheRecordClean(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "blockchain.scripthash.subscribe" {
			return nil // never replies
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	a1 := craftAddr(t, 0x11)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.subscribe(ctx, a1); err == nil {
		t.Fatal("subscribe under a context that ended succeeded")
	}
	if at, err := c.LastRefreshOf(a1); err != nil || !at.IsZero() {
		t.Fatalf("record after a cancelled subscribe: at %v err %v, want untouched", at, err)
	}
}

// A header carried by a connection that is no longer live does not move the
// tip: the new connection's own headers subscription reports it.
func TestHeaderFromAReplacedConnectionDoesNotMoveTheTip(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "blockchain.headers.subscribe" {
			return []string{reply(req.ID, `{"height":500,"hex":"00"}`)}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	if c.LastTip() != 500 {
		t.Fatalf("tip after connect %d", c.LastTip())
	}
	stale := c.liveGen.Load() - 1
	c.handleNotification(stale, incoming{Method: "blockchain.headers.subscribe", Params: json.RawMessage(`[{"height":7,"hex":"00"}]`)})
	if c.LastTip() != 500 {
		t.Fatalf("a header from generation %d moved the tip to %d", stale, c.LastTip())
	}
	c.handleNotification(c.liveGen.Load(), incoming{Method: "blockchain.headers.subscribe", Params: json.RawMessage(`[{"height":501,"hex":"00"}]`)})
	if c.LastTip() != 501 {
		t.Fatalf("a header from the live generation did not move the tip: %d", c.LastTip())
	}
}

// A write that fails on a live context is a lost connection: the socket is
// gone and dropLocked has already forgotten it. Reported as a plain error it
// is classified as one the server chose to send, which never rebuilds the
// connection, so the refresher waits out a backoff before discovering through
// the next call that there is nothing there.
func TestAFailedWriteReportsALostConnection(t *testing.T) {
	broken := errors.New("write tcp 127.0.0.1:1: broken pipe")

	err := writeFailure(context.Background(), broken)
	if !errIsConnection(err) {
		t.Errorf("a failed write reported %v, which the refresher reads as an application error", err)
	}
	if !errors.Is(err, broken) {
		t.Error("the underlying write error is no longer in the chain")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeFailure(ctx, broken); !errors.Is(err, context.Canceled) {
		t.Errorf("a write cut off by the caller's context reported %v, want the context error", err)
	}
}
