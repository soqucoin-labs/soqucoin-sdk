package rpc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const someTxID = "8836beca0000000000000000000000000000000000000000000000000000eedd"

func rpcErr(code int, msg string) string {
	return `{"result":null,"error":{"code":` + itoa(code) + `,"message":"` + msg + `"},"id":1}`
}

func itoa(i int) string { return strconv.Itoa(i) }

func decodeBody(t *testing.T, r *http.Request, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("decode request: %v", err)
	}
	r.Body.Close()
}

// Callers classify with errors.Is and read codes with errors.As; never by
// string matching on the message.
func TestErrorKinds(t *testing.T) {
	cases := []struct {
		code      int
		transient bool
		permanent bool
		inChain   bool
	}{
		{CodeInWarmup, true, false, false},
		{CodeClientInInitialDownload, true, false, false},
		{CodeVerifyRejected, false, true, false},
		{CodeVerifyError, false, true, false},
		{CodeInvalidParameter, false, true, false},
		{CodeTransactionAlreadyInChain, false, false, true},
	}
	for _, tc := range cases {
		c, _ := rpcServer(t, func(string, []interface{}) string { return rpcErr(tc.code, "x") })
		_, err := c.GetBlockCount()
		if err == nil {
			t.Fatalf("code %d: no error", tc.code)
		}
		var re *Error
		if !errors.As(err, &re) || re.Code != tc.code {
			t.Errorf("code %d: errors.As did not expose the code: %v", tc.code, err)
		}
		if errors.Is(err, ErrTransient) != tc.transient {
			t.Errorf("code %d: transient=%v, want %v", tc.code, errors.Is(err, ErrTransient), tc.transient)
		}
		if errors.Is(err, ErrPermanent) != tc.permanent {
			t.Errorf("code %d: permanent=%v, want %v", tc.code, errors.Is(err, ErrPermanent), tc.permanent)
		}
		if errors.Is(err, ErrAlreadyInChain) != tc.inChain {
			t.Errorf("code %d: inChain=%v, want %v", tc.code, errors.Is(err, ErrAlreadyInChain), tc.inChain)
		}
	}
}

func TestUnreachableNodeIsTransient(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "u", "p")
	_, err := c.GetBlockCount()
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("unreachable node: %v, want ErrTransient", err)
	}
	if errors.Is(err, ErrUnknownOutcome) {
		t.Error("a read-only call must never report an unknown outcome")
	}
}

// A lost reply to sendrawtransaction is NOT a rejection. SendRawTransaction
// says so; Broadcast resolves it against the node.
func TestSendRawTransactionTimeoutIsUnknownOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // longer than the client timeout below
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "u", "p")
	c.SetTimeout(50 * time.Millisecond)
	_, err := c.SendRawTransaction("00")
	if !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("timed-out broadcast reported as %v; a caller treating this as failure pays twice", err)
	}
}

func TestBroadcastResolvesLostReplyAgainstTheNode(t *testing.T) {
	var sawLookup atomic.Bool
	mk := func(known bool) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Method string `json:"method"`
			}
			decodeBody(t, r, &req)
			w.Header().Set("Content-Type", "application/json")
			switch req.Method {
			case "sendrawtransaction":
				time.Sleep(300 * time.Millisecond) // reply is lost
			case "getrawtransaction":
				sawLookup.Store(true)
				if known {
					w.Write([]byte(ok(`"00"`)))
				} else {
					w.Write([]byte(rpcErr(CodeInvalidAddressOrKey, "No such mempool or blockchain transaction")))
				}
			case "gettxout":
				w.Write([]byte(ok(`null`)))
			default:
				w.Write([]byte(ok(`null`)))
			}
		}))
		t.Cleanup(srv.Close)
		c := NewClient(srv.URL, "u", "p")
		c.SetTimeout(50 * time.Millisecond)
		return c
	}

	t.Run("node knows the tx: success", func(t *testing.T) {
		c := mk(true)
		txid, err := c.Broadcast("00", someTxID)
		if err != nil || txid != someTxID {
			t.Fatalf("Broadcast: %s %v; the node had the transaction, this is a success", txid, err)
		}
		if !sawLookup.Load() {
			t.Error("Broadcast did not ask the node after the lost reply")
		}
	})
	t.Run("node does not know it: still unknown, never a rejection", func(t *testing.T) {
		c := mk(false)
		_, err := c.Broadcast("00", someTxID)
		if !errors.Is(err, ErrUnknownOutcome) {
			t.Fatalf("got %v, want ErrUnknownOutcome", err)
		}
		if errors.Is(err, ErrPermanent) {
			t.Error("an unresolved broadcast must not be classified as rejected")
		}
	})
}

func TestBroadcastAlreadyInChainIsSuccess(t *testing.T) {
	c, _ := rpcServer(t, func(method string, _ []interface{}) string {
		if method == "sendrawtransaction" {
			return rpcErr(CodeTransactionAlreadyInChain, "transaction already in block chain")
		}
		return ok(`null`)
	})
	txid, err := c.Broadcast("00", someTxID)
	if err != nil || txid != someTxID {
		t.Fatalf("already-in-chain reported as failure: %s %v", txid, err)
	}
}

func TestBroadcastRejectionIsPermanent(t *testing.T) {
	c, _ := rpcServer(t, func(method string, _ []interface{}) string {
		return rpcErr(CodeVerifyRejected, "min relay fee not met")
	})
	_, err := c.Broadcast("00", someTxID)
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("rejection: %v, want ErrPermanent", err)
	}
	if errors.Is(err, ErrUnknownOutcome) {
		t.Error("a node rejection is a known outcome")
	}
}

func TestBroadcastRefusesTxIDMismatch(t *testing.T) {
	c, _ := rpcServer(t, func(method string, _ []interface{}) string {
		return ok(`"` + someTxID + `"`)
	})
	if _, err := c.Broadcast("00", "not-the-same"); !errors.Is(err, ErrPermanent) {
		t.Fatalf("node/caller txid disagreement not surfaced: %v", err)
	}
}

// On a syncing node a nil gettxout is not a spend. Nothing may be evicted.
func TestVerifyAndFilterRefusesWhileNodeIsSyncing(t *testing.T) {
	evicted := 0
	for _, info := range []string{
		`{"blocks":100,"headers":100,"initialblockdownload":true}`,
		`{"blocks":90,"headers":100,"initialblockdownload":false}`,
	} {
		c, _ := rpcServer(t, func(method string, _ []interface{}) string {
			if method == "getblockchaininfo" {
				return ok(info)
			}
			return ok(`null`) // every output "spent" on this lagging node
		})
		_, err := c.VerifyAndFilterUTXOs([]types.UTXO{{TxID: someTxID, Vout: 0}},
			func(string, uint32) { evicted++ }, nil)
		if !errors.Is(err, ErrNodeSyncing) || !errors.Is(err, ErrTransient) {
			t.Errorf("%s: got %v, want ErrNodeSyncing (transient)", info, err)
		}
	}
	if evicted != 0 {
		t.Fatalf("%d live UTXOs evicted on a syncing node", evicted)
	}
}

func TestVerifyAndFilterSkipsImmatureCoinbaseWithoutEvicting(t *testing.T) {
	evicted := 0
	c, _ := rpcServer(t, func(method string, params []interface{}) string {
		switch method {
		case "getblockchaininfo":
			return ok(`{"blocks":1000,"headers":1000,"initialblockdownload":false}`)
		case "gettxout":
			if params[0] == "young" {
				return ok(`{"value":88,"confirmations":10,"coinbase":true}`)
			}
			return ok(`{"value":88,"confirmations":300,"coinbase":true}`)
		}
		return ok(`null`)
	})
	got, err := c.VerifyAndFilterUTXOs([]types.UTXO{{TxID: "young", Vout: 0}, {TxID: "old", Vout: 0}},
		func(string, uint32) { evicted++ }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TxID != "old" {
		t.Fatalf("selected %+v, want only the mature coinbase", got)
	}
	if evicted != 0 {
		t.Error("an immature coinbase is real; it must not be evicted from the cache")
	}
}

func TestTxOutValueShors(t *testing.T) {
	for _, tc := range []struct {
		soq   float64
		shors int64
	}{{1.5, 150_000_000}, {0.00000001, 1}, {88, 8_800_000_000}, {12345.67891234, 1_234_567_891_234}} {
		if got := (&TxOut{Value: tc.soq}).ValueShors(); got != tc.shors {
			t.Errorf("%v SOQ -> %d shors, want %d", tc.soq, got, tc.shors)
		}
	}
}
