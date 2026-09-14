package rpc

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

// The guard reads the URL. Loopback is the name "localhost" or an IP literal
// in 127.0.0.0/8 or ::1; every other name is remote, resolved or not. A URL
// with no usable host is neither and is left to fail in the request.
func TestHostOfClassifiesLoopback(t *testing.T) {
	for _, tc := range []struct {
		url      string
		host     string
		loopback bool
	}{
		{"http://127.0.0.1:33389", "127.0.0.1", true},
		{"http://127.5.6.7:33389", "127.5.6.7", true},
		{"http://localhost:33389", "localhost", true},
		{"http://LOCALHOST:33389", "LOCALHOST", true},
		{"http://[::1]:33389", "::1", true},
		{"http://[::ffff:127.0.0.1]:33389", "::ffff:127.0.0.1", true},
		{"https://127.0.0.1:33389/", "127.0.0.1", true},
		{"http://10.0.0.5:33389", "10.0.0.5", false},
		{"http://192.168.1.20:33389", "192.168.1.20", false},
		{"http://0.0.0.0:33389", "0.0.0.0", false},
		{"http://[::]:33389", "::", false},
		{"http://[fe80::1]:33389", "fe80::1", false},
		{"https://node.internal:33389", "node.internal", false},
		{"http://localhost.example.com:33389", "localhost.example.com", false},
		{"http://user:pass@node.internal:33389", "node.internal", false},
		{"http:///nohost", "", false},
		{"127.0.0.1:33389", "", false}, // no scheme: does not parse as a URL
	} {
		host, loopback := hostOf(tc.url)
		if host != tc.host || loopback != tc.loopback {
			t.Errorf("hostOf(%q) = %q, %v; want %q, %v", tc.url, host, loopback, tc.host, tc.loopback)
		}
	}
}

// countingTransport fails every request and counts the attempts, so a test
// can tell a request that was refused before the wire from one that was sent.
type countingTransport struct{ sent int32 }

func (ct *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	atomic.AddInt32(&ct.sent, 1)
	return nil, errors.New("test transport: connection refused")
}

// A URL that is not loopback is refused before anything is sent, as a
// permanent error, until AllowRemote is set; then the request goes out. The
// same applies through Broadcast, where the refusal must not read as an
// unknown outcome.
func TestRemoteNodeRefusedUntilAllowed(t *testing.T) {
	transport := &countingTransport{}
	c := NewClient("http://10.0.0.5:33389", "u", "p")
	c.client.Transport = transport

	_, err := c.GetBlockCount()
	if !errors.Is(err, ErrRemoteNode) || !errors.Is(err, ErrPermanent) || errors.Is(err, ErrTransient) {
		t.Fatalf("remote node: %v; want ErrRemoteNode (permanent)", err)
	}
	_, err = c.Broadcast("00", someTxID)
	if !errors.Is(err, ErrRemoteNode) || errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("broadcast to a remote node: %v; want ErrRemoteNode, not an unknown outcome", err)
	}
	if err := c.RequireSynced(); !errors.Is(err, ErrRemoteNode) {
		t.Fatalf("RequireSynced on a remote node: %v; want ErrRemoteNode", err)
	}
	if n := atomic.LoadInt32(&transport.sent); n != 0 {
		t.Fatalf("%d requests reached the transport before AllowRemote was set", n)
	}

	c.AllowRemote = true
	_, err = c.GetBlockCount()
	if errors.Is(err, ErrRemoteNode) || !errors.Is(err, ErrTransient) {
		t.Fatalf("with AllowRemote: %v; want the transport's failure (transient), not ErrRemoteNode", err)
	}
	if n := atomic.LoadInt32(&transport.sent); n != 1 {
		t.Fatalf("%d requests sent with AllowRemote, want 1", n)
	}
}

// A loopback URL needs no flag; the existing tests all run against one.
func TestLoopbackNodeNeedsNoFlag(t *testing.T) {
	c, seen := rpcServer(t, func(string, []interface{}) string { return ok("7") })
	if c.AllowRemote {
		t.Fatal("test server client has AllowRemote set")
	}
	n, err := c.GetBlockCount()
	if err != nil || n != 7 || len(*seen) != 1 {
		t.Fatalf("loopback call: %d, %v, %d requests", n, err, len(*seen))
	}
}
