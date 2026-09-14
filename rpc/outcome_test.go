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

// The node accepted the bytes but named them differently. That is its own
// kind: not permanent (the inputs are spent now), not transient, not unknown
// (the node answered). The node's txid is returned so the caller can record it.
func TestBroadcastReportsTxIDMismatchAsItsOwnKind(t *testing.T) {
	c, _ := rpcServer(t, func(method string, _ []interface{}) string {
		return ok(`"` + someTxID + `"`)
	})
	got, err := c.Broadcast("00", "not-the-same")
	if !errors.Is(err, ErrTxIDMismatch) {
		t.Fatalf("node/caller txid disagreement not surfaced: %v", err)
	}
	for _, kind := range []error{ErrPermanent, ErrTransient, ErrUnknownOutcome, ErrAlreadyInChain} {
		if errors.Is(err, kind) {
			t.Errorf("mismatch must not also be %v", kind)
		}
	}
	if got != someTxID {
		t.Errorf("node txid %q not returned", got)
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

// The node prints the value as a decimal SOQ figure; TxOut.Value holds it as
// shors, exactly, including past 2^53 shors where a float64 cannot.
func TestTxOutValueIsExactShors(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		shors int64
	}{
		{"1.50000000", 150_000_000}, {"0.00000001", 1}, {"88.00000000", 8_800_000_000},
		{"12345.67891234", 1_234_567_891_234},
		{"90071992.54740993", 1<<53 + 1},
	} {
		c, _ := rpcServer(t, func(method string, _ []interface{}) string {
			return ok(`{"bestblock":"00","confirmations":3,"value":` + tc.raw + `,"scriptPubKey":{"hex":"5120aa"},"coinbase":false,"assettype":0}`)
		})
		out, err := c.GetTxOut(someTxID, 0, true)
		if err != nil {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		if out.Value != tc.shors || out.Confirmations != 3 || out.ScriptPubKey.Hex != "5120aa" {
			t.Errorf("%s -> %+v, want value %d", tc.raw, out, tc.shors)
		}
	}
}

// A reply whose value is not the node's decimal form is an error, never a
// zero the caller could mistake for a real amount.
func TestTxOutRefusesNonDecimalValue(t *testing.T) {
	for _, body := range []string{
		`{"confirmations":3,"value":-1,"scriptPubKey":{"hex":"51"}}`,
		`{"confirmations":3,"value":1e-8,"scriptPubKey":{"hex":"51"}}`,
		`{"confirmations":3,"value":0.000000001,"scriptPubKey":{"hex":"51"}}`,
		`{"confirmations":3,"scriptPubKey":{"hex":"51"}}`,
	} {
		c, _ := rpcServer(t, func(string, []interface{}) string { return ok(body) })
		out, err := c.GetTxOut(someTxID, 0, true)
		if !errors.Is(err, types.ErrAmountFormat) {
			t.Errorf("%s: got %+v, %v; want ErrAmountFormat", body, out, err)
		}
	}
}

// The maturity applied is the configured network's, at the exact boundary:
// 240, the value inherited from upstream that the SDK itself carried through
// v0.3.4, is immature on mainnet and 288 is mature; regtest matures at 60;
// stagenet is held to 288 although consensus allows 30 below height 100,000.
// An unset Network means mainnet.
func TestVerifyAndFilterAppliesTheNetworkMaturityAtTheBoundary(t *testing.T) {
	cases := []struct {
		name    string
		network types.Network
		confs   int
		mature  bool
	}{
		{"mainnet by default, 240", types.Network{}, 240, false},
		{"mainnet by default, 287", types.Network{}, 287, false},
		{"mainnet by default, 288", types.Network{}, 288, true},
		{"mainnet, 240", types.Mainnet, 240, false},
		{"mainnet, 288", types.Mainnet, 288, true},
		{"regtest, 59", types.Regtest, 59, false},
		{"regtest, 60", types.Regtest, 60, true},
		{"stagenet, 30", types.Stagenet, 30, false},
		{"stagenet, 288", types.Stagenet, 288, true},
		{"hand-built network without a maturity, 287", types.Network{ChainID: "main"}, 287, false},
		{"hand-built network without a maturity, 288", types.Network{ChainID: "main"}, 288, true},
	}
	for _, c := range cases {
		evicted := 0
		chain := c.network.ChainID
		if chain == "" {
			chain = types.Mainnet.ChainID
		}
		cl, _ := rpcServer(t, func(method string, _ []interface{}) string {
			switch method {
			case "getblockchaininfo":
				return ok(`{"chain":"` + chain + `","blocks":1000,"headers":1000,"initialblockdownload":false}`)
			case "gettxout":
				return ok(`{"value":88,"confirmations":` + itoa(c.confs) + `,"coinbase":true}`)
			}
			return ok(`null`)
		})
		cl.Network = c.network
		got, err := cl.VerifyAndFilterUTXOs([]types.UTXO{{TxID: someTxID, Vout: 0}},
			func(string, uint32) { evicted++ }, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (len(got) == 1) != c.mature {
			t.Errorf("%s: %d outputs selected, want mature=%v", c.name, len(got), c.mature)
		}
		if evicted != 0 {
			t.Errorf("%s: an immature coinbase was evicted from the cache", c.name)
		}
	}
}

// A mainnet deployment pointed at a stagenet node, or the reverse, fails
// closed and permanently before any output is selected or evicted. Without a
// configured Network the check is off, as before.
func TestRequireSyncedRefusesANodeOnAnotherChain(t *testing.T) {
	cases := []struct {
		name    string
		network types.Network
		chain   string
		refuse  bool
	}{
		{"mainnet client, stagenet node", types.Mainnet, "stagenet", true},
		{"stagenet client, mainnet node", types.Stagenet, "main", true},
		{"mainnet client, regtest node", types.Mainnet, "regtest", true},
		{"regtest client, regtest node", types.Regtest, "regtest", false},
		{"mainnet client, mainnet node", types.Mainnet, "main", false},
		{"unset client, stagenet node", types.Network{}, "stagenet", false},
	}
	for _, c := range cases {
		evicted := 0
		cl, _ := rpcServer(t, func(method string, _ []interface{}) string {
			if method == "getblockchaininfo" {
				return ok(`{"chain":"` + c.chain + `","blocks":1000,"headers":1000,"initialblockdownload":false}`)
			}
			return ok(`null`) // every output "spent", should the check be skipped
		})
		cl.Network = c.network
		err := cl.RequireSynced()
		if !c.refuse {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrWrongChain) || !errors.Is(err, ErrPermanent) || errors.Is(err, ErrTransient) {
			t.Errorf("%s: got %v, want ErrWrongChain (permanent, not transient)", c.name, err)
		}
		_, verr := cl.VerifyAndFilterUTXOs([]types.UTXO{{TxID: someTxID, Vout: 0}},
			func(string, uint32) { evicted++ }, nil)
		if !errors.Is(verr, ErrWrongChain) {
			t.Errorf("%s: VerifyAndFilterUTXOs got %v, want ErrWrongChain", c.name, verr)
		}
		if evicted != 0 {
			t.Errorf("%s: %d outputs evicted on a wrong-chain node", c.name, evicted)
		}
	}
}

// RequireChain is what deposit.Monitor asks when the client's own Network was
// left unset; it must hold the node to the chain it is given and pass on an
// empty one.
func TestRequireChainHoldsTheNodeToTheGivenChain(t *testing.T) {
	cl, _ := rpcServer(t, func(method string, _ []interface{}) string {
		return ok(`{"chain":"regtest","blocks":1000,"headers":1000,"initialblockdownload":false}`)
	})
	if err := cl.RequireChain(types.Regtest.ChainID); err != nil {
		t.Errorf("regtest node, regtest wanted: %v", err)
	}
	if err := cl.RequireChain(""); err != nil {
		t.Errorf("empty chain id must pass: %v", err)
	}
	err := cl.RequireChain(types.Mainnet.ChainID)
	if !errors.Is(err, ErrWrongChain) || !errors.Is(err, ErrPermanent) {
		t.Errorf("regtest node, mainnet wanted: got %v, want ErrWrongChain", err)
	}
}
