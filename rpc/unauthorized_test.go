package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A 401 or 403 is the node, or a proxy in front of it, refusing the
// credentials or the host before any handler ran. It is not a lost reply: at a
// broadcast a lost reply is an outcome the caller must treat as possibly
// taken effect, which sends an operator looking for a mempool race instead of
// a password. And it is not the request's fault either.
func TestUnauthorizedIsNeitherALostReplyNorARejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		var requests int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&requests, 1)
			w.WriteHeader(status)
		}))
		defer srv.Close()
		c := NewClient(srv.URL, "u", "wrong", nil)

		_, err := c.Call(context.Background(), "getblockcount")
		if !errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrTransient) || errors.Is(err, ErrPermanent) {
			t.Fatalf("status %d: Call returned %v, want ErrUnauthorized alone", status, err)
		}
		_, err = c.Broadcast(context.Background(), "00", someTxID)
		if !errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrUnknownOutcome) {
			t.Fatalf("status %d: Broadcast returned %v, want ErrUnauthorized and no unknown outcome", status, err)
		}
		if n := atomic.LoadInt32(&requests); n != 2 {
			t.Fatalf("status %d: %d requests; a refused credential must not start the resolution lookup", status, n)
		}
	}

	// A proxy error page on any other status is still a transport failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p", nil)
	if _, err := c.Call(context.Background(), "getblockcount"); !errors.Is(err, ErrTransient) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("502 with an error page: %v, want ErrTransient", err)
	}
}
