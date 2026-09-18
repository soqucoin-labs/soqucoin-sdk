package electrumx

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The listunspent reply is the one input to the cache the balance, the
// selector's candidates and the reconciler read. A server that answers
// garbage is refused as a whole: the cache keeps the set it had, the address
// records the error so deposit.Monitor skips and alarms it, and the balance
// does not move. Before this, tx_hash "zz", a value of -500 and a height of
// -7 entered the cache and GetBalance reported a negative figure.

// unspentServer answers listunspent for one address with whatever the test
// has stored last.
func unspentServer(t *testing.T, sh string) (*scriptedStub, *atomic.Value) {
	t.Helper()
	var answer atomic.Value
	answer.Store(oneUTXO)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "blockchain.scripthash.listunspent" && firstParam(req) == sh {
			return []string{reply(req.ID, answer.Load().(string))}
		}
		return []string{reply(req.ID, `null`)}
	})
	return stub, &answer
}

func sameSet(a, b []types.UTXO) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TxID != b[i].TxID || a[i].Vout != b[i].Vout || a[i].Value != b[i].Value || a[i].Height != b[i].Height {
			return false
		}
	}
	return true
}

func TestAMalformedListunspentReplyIsRefusedAndTheCacheKept(t *testing.T) {
	a := craftAddr(t, 0x33)
	sh := scripthashOf(t, a)
	stub, answer := unspentServer(t, sh)
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatalf("the good reply was refused: %v", err)
	}
	good := c.GetUTXOs(a)
	if len(good) != 1 {
		t.Fatalf("expected one cached output after the good reply, got %d", len(good))
	}
	conf0, unconf0 := c.GetBalance(1, 100)

	entry := func(txid string, vout int, value string, height int) string {
		return `{"tx_hash":"` + txid + `","tx_pos":` + strconv.Itoa(vout) + `,"value":` + value + `,"height":` + strconv.Itoa(height) + `}`
	}
	ceiling := strconv.FormatInt(types.MaxMoney, 10)
	over := strconv.FormatInt(types.MaxMoney+1, 10)
	// Five outputs at the ceiling sum past an int64, which no honest chain
	// state reaches: the supply is above one ceiling and far below five.
	var atCeiling []string
	for v := 0; v < 5; v++ {
		atCeiling = append(atCeiling, entry(txA, v, ceiling, 3))
	}
	for _, tc := range []struct{ name, reply string }{
		{"null", `null`},
		{"an-object", `{"tx_hash":"` + txA + `","tx_pos":0,"value":5,"height":3}`},
		{"a-short-txid", `[` + entry(txA[:62], 0, "5", 3) + `]`}, // valid hex, wrong length
		{"a-non-hex-txid", `[` + entry(strings.Repeat("z", 64), 0, "5", 3) + `]`},
		{"a-negative-value", `[` + entry(txA, 0, "-500", 3) + `]`},
		{"a-value-above-the-ceiling", `[` + entry(txA, 0, over, 3) + `]`},
		{"values-overflowing-an-int64", `[` + strings.Join(atCeiling, ",") + `]`},
		{"a-negative-height", `[` + entry(txA, 0, "5", -7) + `]`},
		{"a-duplicate-outpoint", `[` + entry(txA, 0, "5", 3) + `,` + entry(txA, 0, "7", 3) + `]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer.Store(tc.reply)
			err := c.RefreshAll(context.Background())
			if err == nil {
				t.Fatalf("the reply %s was accepted", tc.reply)
			}
			if got := c.GetUTXOs(a); !sameSet(got, good) {
				t.Fatalf("the cache moved on a refused reply: %+v", got)
			}
			if _, rerr := c.LastRefreshOf(a); rerr == nil {
				t.Fatal("the address records no error after a refused reply, so the Monitor would credit against it")
			}
			if conf, unconf := c.GetBalance(1, 100); conf != conf0 || unconf != unconf0 {
				t.Fatalf("the balance moved on a refused reply: %d/%d, was %d/%d", conf, unconf, conf0, unconf0)
			}
		})
	}
}

// The bounds are the protocol's, so what the protocol permits is accepted: a
// mempool output has height 0, an output may carry no value, a server that
// prints its ids in upper case is read with the id stored in the form the node
// prints and the cache compares, and an address whose outputs sum past the
// node's per-output ceiling is an address, since the supply is above that
// ceiling.
func TestAWellFormedListunspentReplyIsAcceptedAsTheProtocolPermits(t *testing.T) {
	a := craftAddr(t, 0x34)
	sh := scripthashOf(t, a)
	stub, answer := unspentServer(t, sh)
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(strings.ReplaceAll(txA, "a", "b"))
	answer.Store(`[{"tx_hash":"` + upper + `","tx_pos":0,"value":0,"height":0}]`)
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatalf("a reply the protocol permits was refused: %v", err)
	}
	got := c.GetUTXOs(a)
	if len(got) != 1 || got[0].TxID != strings.ToLower(upper) || got[0].Value != 0 || got[0].Height != 0 {
		t.Fatalf("cached %+v", got)
	}
	if _, err := c.LastRefreshOf(a); err != nil {
		t.Fatalf("the address records %v after an accepted reply", err)
	}

	ceiling := strconv.FormatInt(types.MaxMoney, 10)
	answer.Store(`[{"tx_hash":"` + txA + `","tx_pos":0,"value":` + ceiling + `,"height":3},{"tx_hash":"` + txA + `","tx_pos":1,"value":` + ceiling + `,"height":4}]`)
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatalf("an address holding two outputs at the per-output ceiling was refused: %v", err)
	}
	if conf, _ := c.GetBalance(1, 100); conf != 2*types.MaxMoney {
		t.Fatalf("balance after two outputs at the ceiling: %d, want %d", conf, 2*types.MaxMoney)
	}
}
