//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
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

// fundThrough funds a contract with Fund as the bench's payer and returns the
// outpoint. On a default regtest node the three deployments are active from
// genesis, so the rule asks for a tip at the depth and no more.
func (b *swapBench) fundThrough(terms htlc.Terms) types.UTXO {
	b.t.Helper()
	payer, _ := b.party(b.payer)
	funded, _, err := htlc.Fund(context.Background(), b.n.rpc, payer, terms,
		htlc.Funding{Inputs: []types.UTXO{b.coin()}, ChangeAddress: b.hot, FeeRate: htlcFeeRate}, nil)
	if err != nil {
		b.t.Fatalf("Fund: %v", err)
	}
	return funded
}

// Audit against the node: a funding is refused in the mempool and one block
// below the depth and passes at it; terms that name another contract or a
// higher amount are refused on the same output; and once a claim of the
// output is in the mempool, and after it is mined, the output is refused.
func TestAuditAgainstTheNode(t *testing.T) {
	b := newSwapBench(t)
	ctx := context.Background()
	secret := sha256.Sum256([]byte("audit"))
	terms := htlc.Terms{Params: b.contract(secret[:], htlc.Height(b.n.height()+200)), Amount: htlcAmount}
	funded := b.fundThrough(terms)
	payee, signer := b.party(b.payee)
	audit := func(tm htlc.Terms, blocksToLock int64) (htlc.Funded, error) {
		return htlc.Audit(ctx, b.n.rpc, payee, tm, funded.TxID, funded.Vout, blocksToLock)
	}

	if _, err := audit(terms, 0); !errors.Is(err, htlc.ErrShallow) {
		t.Fatalf("a funding in the mempool: got %v, want ErrShallow", err)
	}
	b.n.mine(b.stranger, 29)
	if _, err := audit(terms, 0); !errors.Is(err, htlc.ErrShallow) {
		t.Fatalf("29 confirmations: got %v, want ErrShallow", err)
	}
	b.n.mine(b.stranger, 1)
	got, err := audit(terms, 0)
	if err != nil {
		t.Fatalf("30 confirmations: %v", err)
	}
	tip := b.n.height()
	if got.Output.TxID != funded.TxID || got.Output.Value != htlcAmount || got.Confirmations != 30 || got.Tip != tip || got.Output.Height != tip-29 {
		t.Fatalf("Audit returned %+v at tip %d", got, tip)
	}

	otherLock, otherKey, more := terms, terms, terms
	otherLock.Params.LockHeight++
	otherKey.Params.PayerKeyHash = b.keyHash(b.stranger)
	more.Amount++
	for name, tm := range map[string]htlc.Terms{"another lock height": otherLock, "another payer": otherKey, "one shor more": more} {
		if _, err := audit(tm, 0); !errors.Is(err, htlc.ErrFundedOutput) {
			t.Errorf("terms with %s: got %v, want ErrFundedOutput", name, err)
		}
	}
	left := int64(terms.Params.LockHeight) - tip
	if _, err := audit(terms, left); err != nil {
		t.Errorf("%d blocks asked with %d left: %v", left, left, err)
	}
	if _, err := audit(terms, left+1); !errors.Is(err, htlc.ErrLockTooNear) {
		t.Errorf("%d blocks asked with %d left: got %v, want ErrLockTooNear", left+1, left, err)
	}
	if stranger, _ := b.party(b.stranger); true {
		if _, err := htlc.Audit(ctx, b.n.rpc, stranger, terms, funded.TxID, funded.Vout, 0); !errors.Is(err, htlc.ErrWrongKey) {
			t.Errorf("a party the contract does not name: got %v, want ErrWrongKey", err)
		}
	}

	claim, err := htlc.BuildClaim(terms.Params, got.Output, secret, b.payee, htlcFeeRate, b.keys, b.payee)
	if err != nil {
		t.Fatal(err)
	}
	b.accepted("a claim", claim)
	if _, err := audit(terms, 0); !errors.Is(err, htlc.ErrNotFunded) {
		t.Errorf("with a claim in the mempool: got %v, want ErrNotFunded", err)
	}
	b.minedAt("the claim", claim)
	if _, err := audit(terms, 0); !errors.Is(err, htlc.ErrNotFunded) {
		t.Errorf("after the claim is mined: got %v, want ErrNotFunded", err)
	}
	if signer.signs != 0 {
		t.Errorf("Audit asked for %d signatures", signer.signs)
	}
}

// Claim against the node: refused while the funding is below the depth and
// when the tip is inside depth and inclusion of the lock height, with the
// signer not asked; then a claim through the function is mined, pays the
// destination, and FindSecret reads the secret from its block.
func TestClaimAndFindSecretAgainstTheNode(t *testing.T) {
	b := newSwapBench(t)
	ctx := context.Background()
	secret := sha256.Sum256([]byte("claim"))
	const inclusion = 6
	payee, signer := b.party(b.payee)
	spend := htlc.Spend{PayToAddress: b.stranger, FeeRate: htlcFeeRate}

	// A lock height that leaves one block too few once the funding has its depth.
	near := htlc.Terms{Params: b.contract(secret[:], htlc.Height(b.n.height()+30+30+inclusion-1)), Amount: htlcAmount}
	nearFunded := b.fundThrough(near)
	terms := htlc.Terms{Params: b.contract(secret[:], htlc.Height(b.n.height()+200)), Amount: htlcAmount}
	funded := b.fundThrough(terms)

	b.n.mine(b.stranger, 29)
	if tr, err := htlc.Claim(ctx, b.n.rpc, payee, terms, funded.TxID, funded.Vout, secret, spend, inclusion, nil); !errors.Is(err, htlc.ErrShallow) || tr != nil {
		t.Fatalf("29 confirmations: got %v, want ErrShallow", err)
	}
	b.n.mine(b.stranger, 1)
	if tr, err := htlc.Claim(ctx, b.n.rpc, payee, near, nearFunded.TxID, nearFunded.Vout, secret, spend, inclusion, nil); !errors.Is(err, htlc.ErrLockTooNear) || tr != nil {
		t.Fatalf("one block inside depth and inclusion of the lock height: got %v, want ErrLockTooNear", err)
	}
	if signer.signs != 0 || b.mempoolSize() != 0 {
		t.Fatalf("after two refused claims: %d signatures, %d transactions in the mempool", signer.signs, b.mempoolSize())
	}

	var recorded string
	claim, err := htlc.Claim(ctx, b.n.rpc, payee, terms, funded.TxID, funded.Vout, secret, spend, inclusion,
		func(txid, rawHex string) error { recorded = txid; return nil })
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if recorded != claim.TxID() || !b.inMempool(claim.TxID()) {
		t.Fatalf("recorded %q, in the mempool %v; want the claim %s recorded and accepted", recorded, b.inMempool(claim.TxID()), claim.TxID())
	}
	fundedAt := b.n.height() - 29
	if _, _, err := htlc.FindSecret(ctx, b.n.rpc, terms.Params, funded.TxID, funded.Vout, fundedAt); !errors.Is(err, htlc.ErrNoSpend) {
		t.Errorf("FindSecret with the claim in the mempool: got %v, want ErrNoSpend", err)
	}
	b.minedAt("the claim", claim)
	b.paysTo("the claim", claim, b.stranger)
	got, spender, err := htlc.FindSecret(ctx, b.n.rpc, terms.Params, funded.TxID, funded.Vout, fundedAt)
	if err != nil || got != secret || spender != claim.TxID() {
		t.Fatalf("FindSecret: got %x, %q and %v; want the secret from %s", got, spender, err, claim.TxID())
	}
	if _, _, err := htlc.FindSecret(ctx, b.n.rpc, terms.Params, nearFunded.TxID, nearFunded.Vout, fundedAt-1); !errors.Is(err, htlc.ErrNoSpend) {
		t.Errorf("FindSecret for the unspent contract: got %v, want ErrNoSpend", err)
	}
}

// Refund against the node: refused one block below the lock height with the
// signer not asked, accepted with the tip at it, mined in the next block, and
// FindSecret reports a spend that shows no secret.
func TestRefundAgainstTheNode(t *testing.T) {
	b := newSwapBench(t)
	ctx := context.Background()
	secret := sha256.Sum256([]byte("refund"))
	lock := b.n.height() + 40
	terms := htlc.Terms{Params: b.contract(secret[:], htlc.Height(lock)), Amount: htlcAmount}
	funded := b.fundThrough(terms)
	fundedAt := b.n.height() + 1
	payer, signer := b.party(b.payer)
	spend := htlc.Spend{PayToAddress: b.hot, FeeRate: htlcFeeRate}

	b.mineTo(lock - 1)
	if tr, err := htlc.Refund(ctx, b.n.rpc, payer, terms.Params, funded.TxID, funded.Vout, spend, nil); !errors.Is(err, htlc.ErrBeforeLockHeight) || tr != nil || signer.signs != 0 {
		t.Fatalf("tip one below the lock height: got %v and %d signatures, want ErrBeforeLockHeight and none", err, signer.signs)
	}
	if payee, _ := b.party(b.payee); true {
		if _, err := htlc.Refund(ctx, b.n.rpc, payee, terms.Params, funded.TxID, funded.Vout, spend, nil); !errors.Is(err, htlc.ErrWrongKey) {
			t.Errorf("the payee asks for the refund: got %v, want ErrWrongKey", err)
		}
	}
	b.mineTo(lock)
	refund, err := htlc.Refund(ctx, b.n.rpc, payer, terms.Params, funded.TxID, funded.Vout, spend, nil)
	if err != nil {
		t.Fatalf("tip at the lock height: %v", err)
	}
	if at := b.minedAt("the refund", refund); at != lock+1 {
		t.Fatalf("the refund is in block %d, want %d", at, lock+1)
	}
	b.paysTo("the refund", refund, b.hot)
	if _, spender, err := htlc.FindSecret(ctx, b.n.rpc, terms.Params, funded.TxID, funded.Vout, fundedAt); !errors.Is(err, htlc.ErrNoSecret) || spender != refund.TxID() {
		t.Errorf("FindSecret after the refund: got %q and %v, want ErrNoSecret with %s", spender, err, refund.TxID())
	}
	if _, err := htlc.Refund(ctx, b.n.rpc, payer, terms.Params, funded.TxID, funded.Vout, spend, nil); !errors.Is(err, htlc.ErrNotFunded) {
		t.Errorf("a second refund: got %v, want ErrNotFunded", err)
	}
}
