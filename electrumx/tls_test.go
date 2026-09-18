// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// An ElectrumX server sees every address a client tracks, so plaintext over an
// untrusted path discloses the whole deposit set and lets an attacker alter the
// balances the caller acts on. Transport security here is an integrity property
// and not only a privacy one.
//
// Four things are worth pinning, because each fails silently: TLS is really
// negotiated rather than configured and ignored, certificate verification is
// really on by default, a reconnect cannot drop back to plaintext, and
// plaintext to a host that is not loopback is refused before anything is
// dialled unless the operator has said the path is private. The reconnect one
// matters most, since the client reconnects on its own after a lost
// connection, after two reply timeouts in a row and after a panic in the
// refresher.

package electrumx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// electrumStub is a minimal ElectrumX server that answers the server.version
// handshake Connect performs, over TLS or plaintext.
type electrumStub struct {
	ln       net.Listener
	handshak chan bool // true if the connection reaching us was TLS
}

func newStub(t *testing.T, cfg *tls.Config) *electrumStub {
	t.Helper()
	var ln net.Listener
	var err error
	if cfg != nil {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", cfg)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &electrumStub{ln: ln, handshak: make(chan bool, 8)}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *electrumStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_, isTLS := c.(*tls.Conn)
			select {
			case s.handshak <- isTLS:
			default:
			}
			dec := json.NewDecoder(c)
			for {
				var req request
				if err := dec.Decode(&req); err != nil {
					return
				}
				resp := struct {
					ID     int64           `json:"id"`
					Result json.RawMessage `json:"result"`
				}{ID: req.ID, Result: json.RawMessage(`"ElectrumX 1.16"`)}
				b, _ := json.Marshal(resp)
				if _, err := c.Write(append(b, '\n')); err != nil {
					return
				}
			}
		}(conn)
	}
}

func (s *electrumStub) addr() string { return s.ln.Addr().String() }

// selfSigned returns a certificate for 127.0.0.1 and a pool trusting it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	cert, pool := testCert(t)
	return cert, pool
}

func TestConnectNegotiatesTLSWhenConfigured(t *testing.T) {
	cert, pool := selfSigned(t)
	stub := newStub(t, &tls.Config{Certificates: []tls.Certificate{cert}})

	c := NewClient(stub.addr(), time.Second, nil)
	c.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect over TLS: %v", err)
	}
	defer c.Stop()

	if wasTLS := <-stub.handshak; !wasTLS {
		t.Fatal("server saw a plaintext connection despite TLSConfig being set")
	}
	if _, ok := c.conn.(*tls.Conn); !ok {
		t.Fatalf("client connection is %T, want *tls.Conn", c.conn)
	}
}

func TestConnectStaysPlaintextWhenNotConfigured(t *testing.T) {
	stub := newStub(t, nil)
	c := NewClient(stub.addr(), time.Second, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Stop()
	if wasTLS := <-stub.handshak; wasTLS {
		t.Fatal("connection was TLS with no TLSConfig set")
	}
}

// The property that matters most: a reconnect must not downgrade. Reconnect is
// invoked automatically after two failed polls and after a panic, so a downgrade
// there would be silent and long-lived.
func TestReconnectDoesNotDowngradeToPlaintext(t *testing.T) {
	cert, pool := selfSigned(t)
	stub := newStub(t, &tls.Config{Certificates: []tls.Certificate{cert}})

	c := NewClient(stub.addr(), time.Second, nil)
	c.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Stop()
	<-stub.handshak

	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if wasTLS := <-stub.handshak; !wasTLS {
		t.Fatal("reconnect fell back to plaintext")
	}
	if _, ok := c.conn.(*tls.Conn); !ok {
		t.Fatalf("connection after reconnect is %T, want *tls.Conn", c.conn)
	}
}

// UseTLS must verify the chain. If it silently accepted any certificate it
// would be worse than plaintext, because it would look secure.
func TestUseTLSRejectsUntrustedCertificate(t *testing.T) {
	cert, _ := selfSigned(t)
	stub := newStub(t, &tls.Config{Certificates: []tls.Certificate{cert}})

	c := NewClient(stub.addr(), time.Second, nil)
	c.UseTLS() // system roots only; the stub's cert is not among them
	err := c.Connect(context.Background())
	if err == nil {
		c.Stop()
		t.Fatal("UseTLS accepted a certificate signed by an untrusted CA")
	}
	var unknown x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if !asErr(err, &unknown) && !asErr(err, &hostErr) {
		t.Logf("error was %v", err)
	}
}

// Connecting with TLS to a server that speaks plaintext must fail rather than
// fall through.
func TestTLSToPlaintextServerFails(t *testing.T) {
	stub := newStub(t, nil)
	c := NewClient(stub.addr(), time.Second, nil)
	c.UseTLS()
	if err := c.Connect(context.Background()); err == nil {
		c.Stop()
		t.Fatal("TLS client connected to a plaintext server")
	}
}

// Plaintext to a host that is not loopback is refused with nothing dialled.
// The context here would end a dial in 100 ms, so a client that dialled
// reports the deadline rather than the refusal. AllowPlaintext lifts the
// refusal, and so does a TLSConfig, since the refusal is about plaintext.
func TestPlaintextToAHostThatIsNotLoopbackIsRefusedBeforeTheDial(t *testing.T) {
	c := NewClient("10.255.255.1:50001", time.Second, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Connect(ctx); !errors.Is(err, ErrPlaintextRemote) {
		t.Fatalf("Connect to a remote host in plaintext: %v, want ErrPlaintextRemote", err)
	}

	c.AllowPlaintext = true
	if err := c.Connect(ctx); err == nil || errors.Is(err, ErrPlaintextRemote) {
		t.Fatalf("Connect with AllowPlaintext: %v, want a dial error", err)
	}

	c.AllowPlaintext = false
	c.UseTLS()
	if err := c.Connect(ctx); err == nil || errors.Is(err, ErrPlaintextRemote) {
		t.Fatalf("Connect with TLS: %v, want a dial error", err)
	}
}

// Loopback is read from the host as written by the one rule the rpc client
// applies: the name localhost or a loopback IP literal, with the port split
// off and brackets removed. Nothing listens on port 1, so every spelling that
// passes the refusal reports a dial error instead.
func TestLoopbackSpellingsTakePlaintextAndOtherHostsDoNot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	for _, host := range []string{"localhost:1", "127.0.0.1:1", "127.5.6.7:1", "[::1]:1"} {
		err := NewClient(host, time.Second, nil).Connect(ctx)
		if err == nil || errors.Is(err, ErrPlaintextRemote) {
			t.Errorf("Connect(%q) in plaintext: %v, want a dial error", host, err)
		}
	}
	for _, host := range []string{"127.1:1", "localhost.:1", "host.docker.internal:1", "example.invalid:1", "10.0.0.5"} {
		err := NewClient(host, time.Second, nil).Connect(ctx)
		if !errors.Is(err, ErrPlaintextRemote) {
			t.Errorf("Connect(%q) in plaintext: %v, want ErrPlaintextRemote", host, err)
		}
	}
}
