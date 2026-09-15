package rpc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The money seam. The caller's context ends while the node is holding the
// reply to sendrawtransaction. The node may have accepted the transaction, so
// the only honest answer is "unknown": never a rejection, never ErrPermanent,
// and the context's own error stays visible in the chain. The next attempt
// with a live context finds the node already knows the transaction and
// reports the txid, so a retry of the same bytes is the recovery.
func TestBroadcastCancelledMidFlightIsUnknownNotPermanent(t *testing.T) {
	var sends atomic.Int32
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		decodeBody(t, r, &req)
		switch req.Method {
		case "sendrawtransaction":
			if sends.Add(1) == 1 {
				// First send: the node has it; the reply never leaves. Hold until
				// the client gives up, then drop the connection.
				received <- struct{}{}
				<-r.Context().Done()
				return
			}
			// The retry: the node knows the transaction already.
			w.Write([]byte(rpcErr(CodeTransactionAlreadyInChain, "transaction already in block chain")))
		case "getrawtransaction", "gettxout":
			// The resolution lookup runs on the same, ended, context and must
			// not reach here; if it does, answer nothing useful.
			w.Write([]byte(`{"result":null,"error":null,"id":1}`))
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Broadcast(ctx, "00", someTxID)
		done <- err
	}()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the node never received the broadcast")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast did not return after the context was cancelled")
	}
	if !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("cancelled broadcast: %v, want ErrUnknownOutcome", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled broadcast: %v, the context's error is not in the chain", err)
	}
	if errors.Is(err, ErrPermanent) {
		t.Fatalf("cancelled broadcast classified as permanent: %v; a caller would release the inputs of a transaction that may be in the mempool", err)
	}

	// The retry, with a live context, sends the same bytes and settles.
	txid, err := c.Broadcast(context.Background(), "00", someTxID)
	if err != nil || txid != someTxID {
		t.Fatalf("retry: %s %v, want the txid and no error", txid, err)
	}
	if n := sends.Load(); n != 2 {
		t.Errorf("sendrawtransaction reached the node %d times, want 2", n)
	}
}

// A deadline is the same lost reply as a cancel.
func TestBroadcastDeadlineIsUnknownNotPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // the server notices a departed client only once the body is drained
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Broadcast(ctx, "00", someTxID)
	if !errors.Is(err, ErrUnknownOutcome) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPermanent) {
		t.Fatalf("deadline mid-broadcast: %v, want unknown outcome carrying DeadlineExceeded, never permanent", err)
	}
}

// Every other call ends promptly when its context does, and reports the
// context's error as transient: a caller that retries later is right to.
func TestCallEndsWithTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // the server notices a departed client only once the body is drained
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := c.GetBlockCount(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("GetBlockCount with a cancelled context: %v", err)
	}
	if !errors.Is(err, ErrTransient) || errors.Is(err, ErrPermanent) {
		t.Errorf("a context error must be transient, never permanent: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the call took %v to notice the cancelled context", time.Since(start))
	}
}
