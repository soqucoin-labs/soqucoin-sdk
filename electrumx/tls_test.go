// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// The security guide told integrators to "always use TLS when connecting to
// ElectrumX servers" and showed an electrumx.Dial(..., WithTLS()) call. Neither
// the function nor TLS support existed: the client had no reference to crypto/tls
// at all, so the advice could not be followed. These tests cover the support that
// closes that gap.
//
// An ElectrumX server sees every address a client tracks, so plaintext over an
// untrusted path discloses the whole deposit set and lets an attacker alter the
// balances the caller acts on. The properties worth pinning are therefore: TLS is
// really negotiated, verification is really on by default, and a reconnect cannot
// silently drop back to plaintext.

package electrumx

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
				resp := response{ID: req.ID, Result: json.RawMessage(`"ElectrumX 1.16"`)}
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

	c := NewClient(stub.addr(), time.Second)
	c.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if err := c.Connect(); err != nil {
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
	c := NewClient(stub.addr(), time.Second)
	if err := c.Connect(); err != nil {
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

	c := NewClient(stub.addr(), time.Second)
	c.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Stop()
	<-stub.handshak

	if err := c.Reconnect(); err != nil {
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

	c := NewClient(stub.addr(), time.Second)
	c.UseTLS() // system roots only; the stub's cert is not among them
	err := c.Connect()
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
	c := NewClient(stub.addr(), time.Second)
	c.UseTLS()
	if err := c.Connect(); err == nil {
		c.Stop()
		t.Fatal("TLS client connected to a plaintext server")
	}
}
