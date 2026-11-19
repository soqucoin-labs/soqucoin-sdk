//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/htlc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The functions of package htlc that ask a node, against a real one.

// counted is a signer that counts what it is asked to sign.
type counted struct {
	tx.Signer
	signs int
}

func (c *counted) Sign(addr string, digest []byte) ([]byte, error) {
	c.signs++
	return c.Signer.Sign(addr, digest)
}

// party returns the bench's key at addr as a party of a swap, with a signer
// that counts.
func (b *swapBench) party(addr string) (htlc.Party, *counted) {
	c := &counted{Signer: b.keys}
	return htlc.Party{Network: types.Regtest, Signer: c, Address: addr}, c
}

// coin returns a mature coinbase output of the wallet key that no case has
// used.
func (b *swapBench) coin() types.UTXO {
	b.t.Helper()
	if err := b.scan.RefreshAll(context.Background()); err != nil {
		b.t.Fatal(err)
	}
	for _, u := range b.scan.GetUTXOs(b.hot) {
		if !b.used[okey(u.TxID, u.Vout)] && b.n.height()-u.Height >= types.Regtest.CoinbaseMaturity {
			b.used[okey(u.TxID, u.Vout)] = true
			return u
		}
	}
	b.t.Fatal("no mature coin left for the wallet key")
	return types.UTXO{}
}

func (b *swapBench) mempoolSize() int {
	b.t.Helper()
	raw, err := b.n.rpc.Call(context.Background(), "getrawmempool")
	if err != nil {
		b.t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		b.t.Fatal(err)
	}
	return len(ids)
}

// Fund refuses until the tip is the swap's depth above the height at which
// the node starts to run the contract's script. The node of this case is
// started with that height above its tip for each of the three deployments.
// Below the height an output of this form is spendable by anyone who gets a
// block; a regtest node has no relay rule against funding one, so the refusal
// here is Fund's alone, and the case shows the node taking what Fund refused.
func TestFundRefusesUntilTheContractHasBeenActiveForTheDepth(t *testing.T) {
	const activation = 80
	var args []string
	for _, name := range []string{"p2wsh_dilithium", "dilithium_keyhash", "v6_controlflow"} {
		args = append(args, fmt.Sprintf("-activationheight=%s:%d", name, activation))
	}
	b := newSwapBench(t, args...)
	ctx := context.Background()
	if at := b.n.height(); at >= activation {
		t.Fatalf("the bench starts at height %d, at or above the activation height %d", at, activation)
	}
	info, err := b.n.rpc.GetBlockchainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"p2wsh_dilithium", "dilithium_keyhash", "v6_controlflow"} {
		if got, ok := info.ActivationHeights[name]; !ok || got != activation {
			t.Fatalf("the node reports %s at %d (present %v), started with %d", name, got, ok, activation)
		}
	}

	const depth = 30
	if got := htlc.MinDepth(htlcAmount); got != depth {
		t.Errorf("MinDepth of %d shors is %d, want %d", htlcAmount, got, depth)
	}
	first := int64(activation) + depth
	terms := htlc.Terms{Params: b.contract(make([]byte, 32), htlc.Height(first+100)), Amount: htlcAmount}
	payer, signer := b.party(b.payer)
	records := 0
	record := func(string, string) error { records++; return nil }
	funding := func() htlc.Funding {
		return htlc.Funding{Inputs: []types.UTXO{b.coin()}, ChangeAddress: b.hot, FeeRate: htlcFeeRate}
	}

	for _, tip := range []int64{b.n.height(), activation - 1, activation, first - 1} {
		b.mineTo(tip)
		_, tr, err := htlc.Fund(ctx, b.n.rpc, payer, terms, funding(), record)
		if !errors.Is(err, htlc.ErrNotActive) {
			t.Fatalf("tip %d: Fund returned %v, want ErrNotActive until the tip is at %d", tip, err, first)
		}
		if tr != nil || records != 0 || signer.signs != 0 {
			t.Fatalf("tip %d: a refused Fund returned a transaction (%v), recorded %d and asked for %d signatures", tip, tr != nil, records, signer.signs)
		}
		if n := b.mempoolSize(); n != 0 {
			t.Fatalf("tip %d: %d transactions in the mempool after a refused Fund", tip, n)
		}
	}

	b.mineTo(first)
	funded, tr, err := htlc.Fund(ctx, b.n.rpc, payer, terms, funding(), record)
	if err != nil {
		t.Fatalf("tip %d: Fund: %v", first, err)
	}
	if records != 1 || signer.signs != 1 {
		t.Fatalf("an accepted Fund recorded %d times and asked for %d signatures, want 1 and 1", records, signer.signs)
	}
	if funded.TxID != tr.TxID() || funded.Vout != 0 || funded.Value != htlcAmount {
		t.Fatalf("Fund returned the outpoint %s:%d worth %d, built %s:0 worth %d", funded.TxID, funded.Vout, funded.Value, tr.TxID(), htlcAmount)
	}
	b.minedAt("the funding", tr)
	want, err := terms.Params.OutputScript()
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.n.rpc.GetTxOut(ctx, funded.TxID, funded.Vout, false)
	if err != nil || out == nil {
		t.Fatalf("the funded output is not in the chain: %+v, %v", out, err)
	}
	if out.ScriptPubKey.Hex != hex.EncodeToString(want) || out.Value != htlcAmount {
		t.Fatalf("the node holds script %s worth %d, derived %x worth %d", out.ScriptPubKey.Hex, out.Value, want, htlcAmount)
	}
}

// What Fund refuses below the height, the node of the case above takes: a
// funding built by hand at a tip below the activation height enters the
// mempool of a regtest node.
func TestTheNodeTakesAFundingBelowTheActivationHeight(t *testing.T) {
	const activation = 200
	var args []string
	for _, name := range []string{"p2wsh_dilithium", "dilithium_keyhash", "v6_controlflow"} {
		args = append(args, fmt.Sprintf("-activationheight=%s:%d", name, activation))
	}
	b := newSwapBench(t, args...)
	p := b.contract(make([]byte, 32), 400)
	unsigned, err := htlc.BuildFunding(p, []types.UTXO{b.coin()}, htlcAmount, b.hot, htlcFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if err := unsigned.SignAll(b.keys); err != nil {
		t.Fatal(err)
	}
	b.accepted("a funding below the activation height", unsigned)
}
