// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package rpc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// rpcServer stands up a fake soqucoind. handler receives the decoded method and
// params and returns the raw JSON body to reply with.
func rpcServer(t *testing.T, handler func(method string, params []interface{}) string) (*Client, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string        `json:"jsonrpc"`
			ID      int           `json:"id"`
			Method  string        `json:"method"`
			Params  []interface{} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server could not decode request: %v", err)
			return
		}
		r.Body.Close()
		seen = append(seen, r)
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(handler(req.Method, req.Params))); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "rpcuser", "rpcpass"), &seen
}

func ok(result string) string {
	return `{"result":` + result + `,"error":null,"id":1}`
}

// ── Call plumbing ──────────────────────────────────────────────────────────

func TestCallSendsBasicAuthAndJSONRPC(t *testing.T) {
	c, seen := rpcServer(t, func(method string, params []interface{}) string {
		if method != "getblockcount" {
			t.Errorf("method = %q, want getblockcount", method)
		}
		return ok(`123`)
	})
	if _, err := c.Call("getblockcount"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	req := (*seen)[0]
	user, pass, hasAuth := req.BasicAuth()
	if !hasAuth {
		t.Error("no basic auth header; soqucoind would reject with 401")
	}
	if user != "rpcuser" || pass != "rpcpass" {
		t.Errorf("basic auth = %q/%q, want rpcuser/rpcpass", user, pass)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if req.Method != "POST" {
		t.Errorf("HTTP method = %q, want POST", req.Method)
	}
}

// A nil params slice must marshal as [] rather than null; some RPC servers
// reject null params outright.
func TestCallMarshalsEmptyParamsAsArray(t *testing.T) {
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		sent, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		_, _ = w.Write([]byte(ok(`0`)))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	if _, err := c.Call("getblockcount"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatalf("decode sent body %q: %v", sent, err)
	}
	if string(req["params"]) != "[]" {
		t.Errorf("params = %s, want [] (null params are rejected by some nodes)", req["params"])
	}
	if string(req["jsonrpc"]) != `"1.0"` {
		t.Errorf("jsonrpc = %s, want \"1.0\"", req["jsonrpc"])
	}
}

// An RPC-level error must surface as an error, never as a zero value that a
// caller might mistake for a successful result.
func TestCallSurfacesRPCError(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string {
		return `{"result":null,"error":{"code":-25,"message":"missing inputs"},"id":1}`
	})
	_, err := c.Call("sendrawtransaction", "deadbeef")
	if err == nil {
		t.Fatal("RPC error was swallowed")
	}
	if !strings.Contains(err.Error(), "missing inputs") {
		t.Errorf("error = %q, want it to mention the server message", err)
	}
}

func TestCallSurfacesUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "p")
	if _, err := c.Call("getblockcount"); err == nil {
		t.Error("a non-JSON error page was accepted as a result")
	}
}

func TestCallFailsOnUnreachableNode(t *testing.T) {
	// Port 1 on loopback is reliably closed.
	c := NewClient("http://127.0.0.1:1", "u", "p")
	if _, err := c.Call("getblockcount"); err == nil {
		t.Error("unreachable node did not produce an error")
	}
}

// ── Typed wrappers ─────────────────────────────────────────────────────────

func TestGetBlockCount(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string { return ok(`847221`) })
	got, err := c.GetBlockCount()
	if err != nil {
		t.Fatalf("GetBlockCount: %v", err)
	}
	if got != 847221 {
		t.Errorf("height = %d, want 847221", got)
	}
}

func TestSendRawTransactionReturnsTxID(t *testing.T) {
	const txid = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	c, _ := rpcServer(t, func(method string, params []interface{}) string {
		if method != "sendrawtransaction" {
			t.Errorf("method = %q", method)
		}
		if len(params) == 0 || params[0] != "0200000001ff" {
			t.Errorf("params = %v, want the raw hex first", params)
		}
		return ok(`"` + txid + `"`)
	})
	got, err := c.SendRawTransaction("0200000001ff")
	if err != nil {
		t.Fatalf("SendRawTransaction: %v", err)
	}
	if got != txid {
		t.Errorf("txid = %q, want %q", got, txid)
	}
}

// A rejected broadcast must return an error. Treating it as success would let a
// caller mark a withdrawal as sent when nothing reached the network.
func TestSendRawTransactionPropagatesRejection(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string {
		return `{"result":null,"error":{"code":-26,"message":"non-mandatory-script-verify-flag"},"id":1}`
	})
	if _, err := c.SendRawTransaction("0200000001ff"); err == nil {
		t.Error("a rejected broadcast reported success")
	}
}

// ── GetTxOut and Defense 11 ────────────────────────────────────────────────

// A null gettxout result means the output is spent. It must come back as
// (nil, nil) so VerifyUTXO can distinguish "spent" from "call failed".
func TestGetTxOutNullMeansSpent(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string { return ok(`null`) })
	out, err := c.GetTxOut("aa", 0, true)
	if err != nil {
		t.Fatalf("GetTxOut: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for a spent output, got %+v", out)
	}
}

func TestVerifyUTXODistinguishesSpentFromError(t *testing.T) {
	t.Run("spent", func(t *testing.T) {
		c, _ := rpcServer(t, func(string, []interface{}) string { return ok(`null`) })
		exists, _, err := c.VerifyUTXO("aa", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exists {
			t.Error("spent output reported as existing")
		}
	})
	t.Run("unspent", func(t *testing.T) {
		c, _ := rpcServer(t, func(string, []interface{}) string {
			return ok(`{"value":1.5,"confirmations":300,"assetType":1}`)
		})
		exists, assetType, err := c.VerifyUTXO("aa", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exists {
			t.Error("unspent output reported as spent")
		}
		if assetType != 1 {
			t.Errorf("assetType = %d, want 1 (USDSOQ)", assetType)
		}
	})
	t.Run("node error is not silently spent", func(t *testing.T) {
		c, _ := rpcServer(t, func(string, []interface{}) string {
			return `{"result":null,"error":{"code":-8,"message":"bad txid"},"id":1}`
		})
		exists, _, err := c.VerifyUTXO("zz", 0)
		if err == nil {
			t.Fatal("a node error was reported as a clean answer")
		}
		if exists {
			t.Error("errored verification reported the UTXO as existing")
		}
	})
}

// Defense 11: a stale UTXO must be dropped from the result AND evicted from the
// caller's cache, or the next coin selection picks it again and every signing
// attempt fails on the same input.
func TestVerifyAndFilterUTXOsDropsAndEvictsSpent(t *testing.T) {
	const liveTxID, spentTxID = "aa", "bb"
	c, _ := rpcServer(t, func(_ string, params []interface{}) string {
		if len(params) > 0 && params[0] == spentTxID {
			return ok(`null`) // spent
		}
		return ok(`{"value":1.0,"confirmations":300,"assetType":0}`)
	})

	in := []types.UTXO{
		{TxID: liveTxID, Vout: 0, Value: 100_000_000},
		{TxID: spentTxID, Vout: 1, Value: 200_000_000},
	}

	var evicted []string
	out, err := c.VerifyAndFilterUTXOs(in,
		func(txid string, _ uint32) { evicted = append(evicted, txid) },
		nil,
	)
	if err != nil {
		t.Fatalf("VerifyAndFilterUTXOs: %v", err)
	}
	if len(out) != 1 || out[0].TxID != liveTxID {
		t.Errorf("kept = %+v, want only the live UTXO", out)
	}
	if len(evicted) != 1 || evicted[0] != spentTxID {
		t.Errorf("evicted = %v, want [%s]", evicted, spentTxID)
	}
}

// The asset type reported by the node is authoritative; the callback exists so a
// cache holding a stale or unset type gets corrected before signing.
func TestVerifyAndFilterUTXOsStampsAssetType(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string {
		return ok(`{"value":1.0,"confirmations":300,"assetType":1}`)
	})
	type stamp struct {
		txid      string
		assetType uint8
	}
	var stamps []stamp
	_, err := c.VerifyAndFilterUTXOs(
		[]types.UTXO{{TxID: "aa", Vout: 0, Value: 1}},
		nil,
		func(txid string, _ uint32, at uint8) { stamps = append(stamps, stamp{txid, at}) },
	)
	if err != nil {
		t.Fatalf("VerifyAndFilterUTXOs: %v", err)
	}
	if len(stamps) != 1 || stamps[0].assetType != 1 {
		t.Errorf("stamps = %+v, want one entry with assetType 1", stamps)
	}
}

func TestVerifyAndFilterUTXOsToleratesNilCallbacks(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string { return ok(`null`) })
	out, err := c.VerifyAndFilterUTXOs([]types.UTXO{{TxID: "aa", Vout: 0}}, nil, nil)
	if err != nil {
		t.Fatalf("nil callbacks should be tolerated: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("kept %d spent UTXOs", len(out))
	}
}

func TestVerifyAndFilterUTXOsEmptyInput(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string {
		t.Error("no RPC call should be made for an empty input set")
		return ok(`null`)
	})
	out, err := c.VerifyAndFilterUTXOs(nil, nil, nil)
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want empty", out)
	}
}

// ── Fee estimation ─────────────────────────────────────────────────────────

func TestEstimateSmartFee(t *testing.T) {
	c, _ := rpcServer(t, func(method string, params []interface{}) string {
		if method != "estimatesmartfee" {
			t.Errorf("method = %q", method)
		}
		return ok(`{"feerate":0.00012345,"blocks":6}`)
	})
	got, err := c.EstimateSmartFee(6)
	if err != nil {
		t.Fatalf("EstimateSmartFee: %v", err)
	}
	if got != 0.00012345 {
		t.Errorf("feerate = %v, want 0.00012345", got)
	}
}
