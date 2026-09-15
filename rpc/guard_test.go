package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	c := NewClient("https://10.0.0.5:33389", "u", "p", nil)
	c.client.Transport = transport

	_, err := c.GetBlockCount(context.Background())
	if !errors.Is(err, ErrRemoteNode) || !errors.Is(err, ErrPermanent) || errors.Is(err, ErrTransient) {
		t.Fatalf("remote node: %v; want ErrRemoteNode (permanent)", err)
	}
	_, err = c.Broadcast(context.Background(), "00", someTxID)
	if !errors.Is(err, ErrRemoteNode) || errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("broadcast to a remote node: %v; want ErrRemoteNode, not an unknown outcome", err)
	}
	if err := c.RequireSynced(context.Background()); !errors.Is(err, ErrRemoteNode) {
		t.Fatalf("RequireSynced on a remote node: %v; want ErrRemoteNode", err)
	}
	if n := atomic.LoadInt32(&transport.sent); n != 0 {
		t.Fatalf("%d requests reached the transport before AllowRemote was set", n)
	}

	c.AllowRemote = true
	_, err = c.GetBlockCount(context.Background())
	if errors.Is(err, ErrRemoteNode) || !errors.Is(err, ErrTransient) {
		t.Fatalf("with AllowRemote: %v; want the transport's failure (transient), not ErrRemoteNode", err)
	}
	if n := atomic.LoadInt32(&transport.sent); n != 1 {
		t.Fatalf("%d requests sent with AllowRemote, want 1", n)
	}
}

// AllowRemote permits a remote host, not plaintext to it. A non-loopback URL
// whose scheme is not https is refused before anything is sent even with
// AllowRemote set, through every entry point, and never as an unknown
// outcome. Without AllowRemote the same URL reads as ErrRemoteNode, so an
// operator who did not intend a remote host learns that first.
func TestPlaintextRemoteRefusedEvenWhenAllowed(t *testing.T) {
	transport := &countingTransport{}
	c := NewClient("http://10.0.0.5:33389", "u", "p", nil)
	c.client.Transport = transport

	if _, err := c.GetBlockCount(context.Background()); !errors.Is(err, ErrRemoteNode) || errors.Is(err, ErrPlaintextRemote) {
		t.Fatalf("plaintext remote without AllowRemote: %v; want ErrRemoteNode first", err)
	}

	c.AllowRemote = true
	_, err := c.GetBlockCount(context.Background())
	if !errors.Is(err, ErrPlaintextRemote) || !errors.Is(err, ErrPermanent) || errors.Is(err, ErrTransient) || errors.Is(err, ErrRemoteNode) {
		t.Fatalf("plaintext remote with AllowRemote: %v; want ErrPlaintextRemote (permanent)", err)
	}
	_, err = c.Broadcast(context.Background(), "00", someTxID)
	if !errors.Is(err, ErrPlaintextRemote) || errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("broadcast over plaintext to a remote node: %v; want ErrPlaintextRemote, not an unknown outcome", err)
	}
	if err := c.RequireSynced(context.Background()); !errors.Is(err, ErrPlaintextRemote) {
		t.Fatalf("RequireSynced over plaintext to a remote node: %v; want ErrPlaintextRemote", err)
	}
	if _, err := c.FeeRateShorsPerVB(context.Background(), 6); !errors.Is(err, ErrPlaintextRemote) {
		t.Fatalf("FeeRateShorsPerVB over plaintext to a remote node: %v; want ErrPlaintextRemote", err)
	}
	if n := atomic.LoadInt32(&transport.sent); n != 0 {
		t.Fatalf("%d requests reached the transport over plaintext to a remote host", n)
	}
}

// The four scheme and host combinations, with AllowRemote set: loopback is
// permitted over either scheme (the node itself speaks plaintext on
// localhost), a remote host only over https. The scheme is read without
// regard to case.
func TestSchemeAndHostCombinations(t *testing.T) {
	for _, tc := range []struct {
		url  string
		sent bool
		want error
	}{
		{"http://127.0.0.1:33389", true, nil},
		{"https://127.0.0.1:33389", true, nil},
		{"http://localhost:33389", true, nil},
		{"https://node.internal:33389", true, nil},
		{"HTTPS://node.internal:33389", true, nil},
		{"http://node.internal:33389", false, ErrPlaintextRemote},
		{"HTTP://10.0.0.5:33389", false, ErrPlaintextRemote},
		{"ws://node.internal:33389", false, ErrPlaintextRemote},
		{"http://user:pass@node.internal:33389", false, ErrPlaintextRemote},
	} {
		transport := &countingTransport{}
		c := NewClient(tc.url, "u", "p", nil)
		c.client.Transport = transport
		c.AllowRemote = true
		_, err := c.GetBlockCount(context.Background())
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: %v; want %v", tc.url, err, tc.want)
		}
		if tc.want == nil && (errors.Is(err, ErrPlaintextRemote) || errors.Is(err, ErrRemoteNode)) {
			t.Errorf("%s: refused: %v", tc.url, err)
		}
		if got := atomic.LoadInt32(&transport.sent) == 1; got != tc.sent {
			t.Errorf("%s: sent=%v, want %v", tc.url, got, tc.sent)
		}
	}
}

// A redirect is not followed. A node never sends one; a listener on the
// loopback address that did could otherwise send the client, and its trust in
// the reply, to another host with AllowRemote unset.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var reached int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&reached, 1)
		w.Write([]byte(ok("99")))
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	c := NewClient(redirector.URL, "u", "p", nil)
	n, err := c.GetBlockCount(context.Background())
	if err == nil || n == 99 {
		t.Fatalf("redirect was followed: %d, %v", n, err)
	}
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("a redirect reply is not JSON-RPC and should read as a transport failure: %v", err)
	}
	if atomic.LoadInt32(&reached) != 0 {
		t.Fatal("the redirect target received a request")
	}
}

// A loopback URL needs no flag; the existing tests all run against one.
func TestLoopbackNodeNeedsNoFlag(t *testing.T) {
	c, seen := rpcServer(t, func(string, []interface{}) string { return ok("7") })
	if c.AllowRemote {
		t.Fatal("test server client has AllowRemote set")
	}
	n, err := c.GetBlockCount(context.Background())
	if err != nil || n != 7 || len(*seen) != 1 {
		t.Fatalf("loopback call: %d, %v, %d requests", n, err, len(*seen))
	}
}
