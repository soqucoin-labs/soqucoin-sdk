// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package client

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func init() { log.SetOutput(io.Discard) } // the package logs each queued payment

// capture is what a request to the fake signer looked like.
type capture struct {
	path   string
	auth   string
	ctype  string
	method string
	body   []byte
}

// signerServer stands up a fake soq-signer. reply is keyed by request path.
func signerServer(t *testing.T, health int, reply map[string]struct {
	status int
	body   string
}) (*Client, *[]capture) {
	t.Helper()
	var seen []capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, capture{
			path: r.URL.Path, auth: r.Header.Get("Authorization"),
			ctype: r.Header.Get("Content-Type"), method: r.Method, body: body,
		})
		if r.URL.Path == "/health" {
			w.WriteHeader(health)
			return
		}
		rep, ok := reply[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(Config{URL: srv.URL, APIToken: "tok-abc"}), &seen
}

type reply = struct {
	status int
	body   string
}

const fakeTxID = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"

func okSend(txid string) reply {
	return reply{200, `{"txid":"` + txid + `","inputs":2,"outputs":3,"elapsed":"120ms"}`}
}

// ── Config defaults ────────────────────────────────────────────────────────

func TestNewClientDefaultsFeeRate(t *testing.T) {
	for _, in := range []int64{0, -1, -1000} {
		c := NewClient(Config{URL: "http://x", FeeRate: in})
		if c.config.FeeRate != types.RecommendedFeeRate {
			t.Errorf("FeeRate %d became %d, want the %d sat/vB default", in, c.config.FeeRate, types.RecommendedFeeRate)
		}
	}
	c := NewClient(Config{URL: "http://x", FeeRate: 25})
	if c.config.FeeRate != 25 {
		t.Errorf("explicit FeeRate = %d, want 25 preserved", c.config.FeeRate)
	}
}

// ── Auth and transport ─────────────────────────────────────────────────────

func TestSendSendsBearerTokenAndJSON(t *testing.T) {
	c, seen := signerServer(t, 200, map[string]reply{"/api/v1/send": okSend(fakeTxID)})
	if _, err := c.Send("ssq1pdest", 150_000); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1 (Send must not health-check)", len(*seen))
	}
	got := (*seen)[0]
	if got.auth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want %q", got.auth, "Bearer tok-abc")
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type = %q", got.ctype)
	}
	if got.method != "POST" {
		t.Errorf("method = %q, want POST", got.method)
	}

	var req SendRequest
	if err := json.Unmarshal(got.body, &req); err != nil {
		t.Fatalf("decode body %q: %v", got.body, err)
	}
	if req.Address != "ssq1pdest" {
		t.Errorf("address = %q", req.Address)
	}
	if req.Amount != 150_000 {
		t.Errorf("amount = %d, want 150000 satoshis passed through unchanged", req.Amount)
	}
	if req.FeeRate != types.RecommendedFeeRate {
		t.Errorf("fee_rate = %d, want the default %d", req.FeeRate, types.RecommendedFeeRate)
	}
}

func TestSendReturnsTxID(t *testing.T) {
	c, _ := signerServer(t, 200, map[string]reply{"/api/v1/send": okSend(fakeTxID)})
	got, err := c.Send("ssq1pdest", 1)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != fakeTxID {
		t.Errorf("txid = %q, want %q", got, fakeTxID)
	}
}

// A failed send must never return a nil error with an empty txid; a caller would
// record the payout as complete.
func TestSendSurfacesServerError(t *testing.T) {
	c, _ := signerServer(t, 200, map[string]reply{
		"/api/v1/send": {400, `{"error":"insufficient funds"}`},
	})
	txid, err := c.Send("ssq1pdest", 999_999_999_999)
	if err == nil {
		t.Fatal("a 400 response was reported as success")
	}
	if txid != "" {
		t.Errorf("txid = %q on failure, want empty", txid)
	}
	if !strings.Contains(err.Error(), "insufficient funds") {
		t.Errorf("error = %q, want the server message included", err)
	}
}

func TestSendSurfacesNonJSONError(t *testing.T) {
	c, _ := signerServer(t, 200, map[string]reply{
		"/api/v1/send": {502, "<html>bad gateway</html>"},
	})
	if _, err := c.Send("ssq1pdest", 1); err == nil {
		t.Error("a non-JSON 502 was accepted as success")
	}
}

func TestSendFailsWhenSignerUnreachable(t *testing.T) {
	c := NewClient(Config{URL: "http://127.0.0.1:1", APIToken: "t"})
	if _, err := c.Send("ssq1pdest", 1); err == nil {
		t.Error("unreachable signer did not error")
	}
}

// ── HealthCheck ────────────────────────────────────────────────────────────

func TestHealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		c, _ := signerServer(t, 200, nil)
		if err := c.HealthCheck(); err != nil {
			t.Errorf("HealthCheck: %v", err)
		}
	})
	t.Run("unhealthy status is an error", func(t *testing.T) {
		c, _ := signerServer(t, 503, nil)
		if err := c.HealthCheck(); err == nil {
			t.Error("HTTP 503 reported as healthy")
		}
	})
	t.Run("unreachable is an error", func(t *testing.T) {
		c := NewClient(Config{URL: "http://127.0.0.1:1"})
		if err := c.HealthCheck(); err == nil {
			t.Error("unreachable signer reported as healthy")
		}
	})
}

// ── SendMany: the SOQ-to-satoshi conversion ────────────────────────────────

// Regression. int64(soq * 1e8) truncates, and binary floating point cannot hold
// most decimal SOQ amounts exactly, so the truncation is systematically SHORT and
// never over: 0.29 SOQ evaluates to 28999999.999999996 and truncated to
// 28,999,999 — one satoshi less than owed, silently, on every payout run.
func TestSendManyRoundsRatherThanTruncates(t *testing.T) {
	cases := map[float64]int64{
		0.29:       29_000_000,  // truncation gave 28999999
		8.2:        820_000_000, // truncation gave 819999999
		0.57:       57_000_000,  // truncation gave 56999999
		0.1:        10_000_000,
		1.1:        110_000_000,
		1234.56:    123_456_000_000,
		1:          100_000_000,
		0.00000001: 1, // one satoshi
	}
	for soq, wantSat := range cases {
		c, seen := signerServer(t, 200, map[string]reply{"/api/v1/sendmany": okSend(fakeTxID)})
		if _, err := c.SendMany(map[string]float64{"ssq1pdest": soq}); err != nil {
			t.Fatalf("%.8f SOQ: SendMany: %v", soq, err)
		}
		var req SendManyRequest
		body := (*seen)[len(*seen)-1].body
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		if got := req.Recipients["ssq1pdest"]; got != wantSat {
			t.Errorf("%.8f SOQ -> %d sat, want %d (short by %d)", soq, got, wantSat, wantSat-got)
		}
	}
}

func TestSendManyHealthChecksBeforeSending(t *testing.T) {
	t.Run("healthy signer proceeds", func(t *testing.T) {
		c, seen := signerServer(t, 200, map[string]reply{"/api/v1/sendmany": okSend(fakeTxID)})
		if _, err := c.SendMany(map[string]float64{"ssq1pdest": 1}); err != nil {
			t.Fatalf("SendMany: %v", err)
		}
		if len(*seen) != 2 || (*seen)[0].path != "/health" {
			t.Errorf("paths = %v, want /health then /api/v1/sendmany", pathsOf(*seen))
		}
	})
	// If the signer is down, no batch should be attempted at all.
	t.Run("unhealthy signer sends nothing", func(t *testing.T) {
		c, seen := signerServer(t, 503, map[string]reply{"/api/v1/sendmany": okSend(fakeTxID)})
		if _, err := c.SendMany(map[string]float64{"ssq1pdest": 1}); err == nil {
			t.Fatal("SendMany proceeded despite a failed health check")
		}
		for _, s := range *seen {
			if s.path == "/api/v1/sendmany" {
				t.Error("a batch was submitted after the health check failed")
			}
		}
	})
}

func TestSendManyRejectsEmptyBatch(t *testing.T) {
	c, seen := signerServer(t, 200, nil)
	if _, err := c.SendMany(nil); err == nil {
		t.Error("nil batch accepted")
	}
	if _, err := c.SendMany(map[string]float64{}); err == nil {
		t.Error("empty batch accepted")
	}
	if len(*seen) != 0 {
		t.Errorf("made %d requests for an empty batch, want 0", len(*seen))
	}
}

// Non-positive amounts are dropped, and if that leaves nothing the call must fail
// rather than submit an empty recipients map.
func TestSendManyFiltersNonPositiveAmounts(t *testing.T) {
	t.Run("mixed batch drops the bad entries", func(t *testing.T) {
		c, seen := signerServer(t, 200, map[string]reply{"/api/v1/sendmany": okSend(fakeTxID)})
		_, err := c.SendMany(map[string]float64{
			"ssq1pgood": 2.5,
			"ssq1pzero": 0,
			"ssq1pneg":  -1.25,
			"ssq1pdust": 0.000000001, // rounds to 0 satoshis
		})
		if err != nil {
			t.Fatalf("SendMany: %v", err)
		}
		var req SendManyRequest
		if err := json.Unmarshal((*seen)[len(*seen)-1].body, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Recipients) != 1 {
			t.Errorf("recipients = %v, want only ssq1pgood", req.Recipients)
		}
		if req.Recipients["ssq1pgood"] != 250_000_000 {
			t.Errorf("good amount = %d, want 250000000", req.Recipients["ssq1pgood"])
		}
	})
	t.Run("all-bad batch errors and sends nothing", func(t *testing.T) {
		c, seen := signerServer(t, 200, map[string]reply{"/api/v1/sendmany": okSend(fakeTxID)})
		if _, err := c.SendMany(map[string]float64{"a": 0, "b": -5}); err == nil {
			t.Error("a batch with no valid recipients reported success")
		}
		for _, s := range *seen {
			if s.path == "/api/v1/sendmany" {
				t.Error("submitted a batch with no valid recipients")
			}
		}
	})
}

func TestSendManySurfacesServerError(t *testing.T) {
	c, _ := signerServer(t, 200, map[string]reply{
		"/api/v1/sendmany": {400, `{"error":"txn-mempool-conflict"}`},
	})
	txid, err := c.SendMany(map[string]float64{"ssq1pdest": 1})
	if err == nil {
		t.Fatal("a 400 batch response was reported as success")
	}
	if txid != "" {
		t.Errorf("txid = %q on failure, want empty", txid)
	}
	if !strings.Contains(err.Error(), "txn-mempool-conflict") {
		t.Errorf("error = %q, want the server message included", err)
	}
}

func TestSendManyCarriesFeeRate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req SendManyRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.FeeRate != 33 {
			t.Errorf("fee_rate = %d, want 33", req.FeeRate)
		}
		_, _ = w.Write([]byte(okSend(fakeTxID).body))
	}))
	defer srv.Close()

	c := NewClient(Config{URL: srv.URL, APIToken: "t", FeeRate: 33})
	if _, err := c.SendMany(map[string]float64{"ssq1pdest": 1}); err != nil {
		t.Fatalf("SendMany: %v", err)
	}
}

func pathsOf(cs []capture) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.path
	}
	return out
}
