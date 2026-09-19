package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A null result from sendrawtransaction is not a txid. The node has said
// nothing the caller can check, so the transaction is treated as accepted
// under an unknown txid, which is the hold the engine applies to a mismatch,
// and the message says what the node returned rather than reporting an
// empty txid as the one it accepted.
func TestNullResultIsAMismatchThatNamesTheMissingTxID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":null,"error":null,"id":1}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p", nil)
	txid := strings.Repeat("ab", 32)
	got, err := c.Broadcast(context.Background(), "00", txid)
	if !errors.Is(err, ErrTxIDMismatch) {
		t.Fatalf("not a mismatch: %v", err)
	}
	if got != "" {
		t.Fatalf("a txid was reported for a null result: %q", got)
	}
	if !strings.Contains(err.Error(), "no txid") || strings.Contains(err.Error(), "returned txid  ") {
		t.Fatalf("the message does not say the node returned no txid: %v", err)
	}
}
