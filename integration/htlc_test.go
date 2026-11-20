//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/htlc"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The swap contract against a real node. The cases are the attacks the
// template answers, each sent to a regtest node and refused by it, and the two
// spends package htlc builds, each accepted and mined. A spend the builders
// refuse to make is written by hand here (handSpend), because the question
// is what the node does with it.
//
// On regtest the three deployments of witness version 6 are active from
// genesis, so the node runs the script for every spend below. The cases fund
// through tx.BuildSignedTransaction and Params.OutputScript.
//
// Not reachable from here: a refund placed in the block at the lock height
// itself. The node's RPC builds blocks from its own mempool, which refuses
// that refund as non-final, so the block rule on lock time is exercised only
// through the mempool's reading of it.

const (
	htlcFeeRate = types.RecommendedFeeRate
	htlcAmount  = 10 * types.ShorsPerSOQ
)

// signers routes a signing request to the manager that holds the address.
type signers map[string]*keys.Manager

func (s signers) Sign(addr string, digest []byte) ([]byte, error) {
	m, ok := s[addr]
	if !ok {
		return nil, fmt.Errorf("no key for %s", addr)
	}
	return m.Sign(addr, digest)
}

func (s signers) PublicKeyFor(addr string) ([]byte, error) {
	m, ok := s[addr]
	if !ok {
		return nil, fmt.Errorf("no key for %s", addr)
	}
	return m.PublicKeyFor(addr)
}

// swapBench is a node, a funded wallet key and the three parties.
type swapBench struct {
	t                           *testing.T
	n                           *node
	keys                        signers
	hot, payee, payer, stranger string
	scan                        *scanner
	used                        map[string]bool
}

func newSwapBench(t *testing.T, nodeArgs ...string) *swapBench {
	t.Helper()
	b := &swapBench{t: t, n: startNode(t, nodeArgs...), keys: signers{}, used: map[string]bool{}}
	add := func() string {
		m, addr := newKey(t)
		b.keys[addr] = m
		return addr
	}
	b.hot, b.payee, b.payer, b.stranger = add(), add(), add(), add()
	b.n.mine(b.hot, 8)
	b.n.mine(b.stranger, int(types.Regtest.CoinbaseMaturity)+2)
	b.scan = newScanner(b.n, b.hot)
	return b
}

func (b *swapBench) keyHash(addr string) [32]byte {
	b.t.Helper()
	pub, err := b.keys.PublicKeyFor(addr)
	if err != nil {
		b.t.Fatal(err)
	}
	kh, err := htlc.KeyHash(pub)
	if err != nil {
		b.t.Fatal(err)
	}
	return kh
}

// contract returns parameters between the bench's payee and payer for the
// SHA-256 of secret, which need not be 32 bytes: a test may commit to the
// hash of a secret the template refuses.
func (b *swapBench) contract(secret []byte, lock htlc.Height) htlc.Params {
	return htlc.Params{
		Hash:         sha256.Sum256(secret),
		PayeeKeyHash: b.keyHash(b.payee),
		PayerKeyHash: b.keyHash(b.payer),
		LockHeight:   lock,
	}
}

// fund pays htlcAmount to the contract's output from a mature coinbase of the
// wallet key, mines it, and returns the funded output after reading it back
// from the node: its 34 bytes are the ones derived here and the node shows
// the address derived here.
func (b *swapBench) fund(p htlc.Params) types.UTXO {
	b.t.Helper()
	ctx := context.Background()
	if err := b.scan.RefreshAll(ctx); err != nil {
		b.t.Fatal(err)
	}
	var coin *types.UTXO
	for _, u := range b.scan.GetUTXOs(b.hot) {
		u := u
		if !b.used[okey(u.TxID, u.Vout)] && b.n.height()-u.Height >= types.Regtest.CoinbaseMaturity {
			coin = &u
			break
		}
	}
	if coin == nil {
		b.t.Fatal("no mature coin left for the wallet key")
	}
	b.used[okey(coin.TxID, coin.Vout)] = true
	out, err := p.OutputScript()
	if err != nil {
		b.t.Fatal(err)
	}
	change, err := address.ScriptFor(b.hot)
	if err != nil {
		b.t.Fatal(err)
	}
	funding, err := tx.BuildSignedTransaction([]types.UTXO{*coin}, out, htlcAmount, change, htlcFeeRate, b.keys)
	if err != nil {
		b.t.Fatalf("funding transaction: %v", err)
	}
	if _, err := b.n.rpc.SendRawTransaction(ctx, funding.SerializeHex()); err != nil {
		b.t.Fatalf("the node refuses the funding transaction: %v", err)
	}
	b.n.mine(b.stranger, 1)

	raw, err := b.n.rpc.Call(ctx, "gettxout", funding.TxID(), 0, false)
	if err != nil || string(raw) == "null" {
		b.t.Fatalf("the funded output is not in the chain: %s, %v", raw, err)
	}
	var seen struct {
		Value        json.Number `json:"value"`
		ScriptPubKey struct {
			Hex       string   `json:"hex"`
			Type      string   `json:"type"`
			Addresses []string `json:"addresses"`
		} `json:"scriptPubKey"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		b.t.Fatal(err)
	}
	if seen.ScriptPubKey.Hex != hex.EncodeToString(out) {
		b.t.Fatalf("funded output script %s, derived %x", seen.ScriptPubKey.Hex, out)
	}
	want, err := p.Address(types.Regtest.HRP)
	if err != nil {
		b.t.Fatal(err)
	}
	if len(seen.ScriptPubKey.Addresses) != 1 || seen.ScriptPubKey.Addresses[0] != want {
		b.t.Fatalf("the node shows %v (%s) for the funded output, derived %s", seen.ScriptPubKey.Addresses, seen.ScriptPubKey.Type, want)
	}
	if v, err := types.ParseSOQ(seen.Value.String()); err != nil || v != htlcAmount {
		b.t.Fatalf("funded value %s, want %d shors", seen.Value, htlcAmount)
	}
	return types.UTXO{TxID: funding.TxID(), Vout: 0, Value: htlcAmount}
}

// hand describes a spend written without the builders.
type hand struct {
	who      string   // the address whose key signs and is shown
	secret   []byte   // nil for the refund branch
	selector []byte   // the selector item
	extra    [][]byte // items placed under the signature
	sequence uint32
	lockTime uint32
}

// handSpend signs a spend of the contract's output as described, paying the
// signer's address, with the fee the builders would take.
func (b *swapBench) handSpend(p htlc.Params, funded types.UTXO, h hand) *tx.Transaction {
	b.t.Helper()
	script, err := p.Script()
	if err != nil {
		b.t.Fatal(err)
	}
	out, _ := p.OutputScript()
	pay, err := address.ScriptFor(h.who)
	if err != nil {
		b.t.Fatal(err)
	}
	pub, err := b.keys.PublicKeyFor(h.who)
	if err != nil {
		b.t.Fatal(err)
	}
	tr := tx.NewTransaction()
	in := funded
	in.Address = h.who
	if err := tr.AddInput(in, out); err != nil {
		b.t.Fatal(err)
	}
	tr.Inputs[0].Sequence = h.sequence
	tr.LockTime = h.lockTime
	tr.AddOutput(0, pay)
	assemble := func(sig []byte) [][]byte {
		items := append([][]byte{}, h.extra...)
		items = append(items, sig, pub)
		if h.secret != nil {
			items = append(items, h.secret)
		}
		return append(items, h.selector, script, append([]byte{0x00}, pub...))
	}
	tr.Inputs[0].WitnessData = assemble(make([]byte, types.SignatureSize+1))
	tr.Outputs[0].Value = funded.Value - tr.EstimateFee(htlcFeeRate)
	digest, err := tr.ComputeSigHashWithScriptCode(0, script, tx.SigHashAll)
	if err != nil {
		b.t.Fatal(err)
	}
	sig, err := b.keys.Sign(h.who, digest)
	if err != nil {
		b.t.Fatal(err)
	}
	tr.Inputs[0].WitnessData = assemble(append(sig, tx.SigHashAll))
	return tr
}

// refused sends the transaction and requires the node to refuse it with a
// message that holds the fragment, and to keep it out of its mempool.
func (b *swapBench) refused(name string, tr *tx.Transaction, fragment string) {
	b.t.Helper()
	_, err := b.n.rpc.SendRawTransaction(context.Background(), tr.SerializeHex())
	if err == nil {
		b.t.Errorf("%s: the node accepted the transaction %s", name, tr.TxID())
		return
	}
	if !strings.Contains(err.Error(), fragment) {
		b.t.Errorf("%s: refused with %q, want a message holding %q", name, err, fragment)
	}
	if b.inMempool(tr.TxID()) {
		b.t.Errorf("%s: the refused transaction is in the mempool", name)
	}
}

func (b *swapBench) accepted(name string, tr *tx.Transaction) {
	b.t.Helper()
	if _, err := b.n.rpc.Broadcast(context.Background(), tr.SerializeHex(), tr.TxID()); err != nil {
		b.t.Fatalf("%s: the node refuses the transaction: %v", name, err)
	}
	if !b.inMempool(tr.TxID()) {
		b.t.Fatalf("%s: the accepted transaction is not in the mempool", name)
	}
}

func (b *swapBench) inMempool(txid string) bool {
	b.t.Helper()
	raw, err := b.n.rpc.Call(context.Background(), "getrawmempool")
	if err != nil {
		b.t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		b.t.Fatal(err)
	}
	for _, id := range ids {
		if id == txid {
			return true
		}
	}
	return false
}

// minedAt mines one block and requires the transaction's first output to be
// in it: unspent, with one confirmation, at the height returned.
func (b *swapBench) minedAt(name string, tr *tx.Transaction) int64 {
	b.t.Helper()
	b.n.mine(b.stranger, 1)
	out, err := b.n.rpc.GetTxOut(context.Background(), tr.TxID(), 0, false)
	if err != nil || out == nil || out.Confirmations != 1 {
		b.t.Fatalf("%s: not in the block just mined: %+v, %v", name, out, err)
	}
	if out.Value != tr.Outputs[0].Value {
		b.t.Errorf("%s: mined output worth %d, built %d", name, out.Value, tr.Outputs[0].Value)
	}
	return b.n.height()
}

// paysTo requires the node's first output of a mined transaction to carry the
// payment script of addr.
func (b *swapBench) paysTo(name string, tr *tx.Transaction, addr string) {
	b.t.Helper()
	want, err := address.ScriptFor(addr)
	if err != nil {
		b.t.Fatal(err)
	}
	out, err := b.n.rpc.GetTxOut(context.Background(), tr.TxID(), 0, false)
	if err != nil || out == nil {
		b.t.Fatalf("%s: no unspent first output: %+v, %v", name, out, err)
	}
	if out.ScriptPubKey.Hex != hex.EncodeToString(want) {
		b.t.Errorf("%s: the node holds output script %s, want %x, the script of %s", name, out.ScriptPubKey.Hex, want, addr)
	}
}

func (b *swapBench) mineTo(height int64) {
	b.t.Helper()
	if at := b.n.height(); at < height {
		b.n.mine(b.stranger, int(height-at))
	}
	if at := b.n.height(); at != height {
		b.t.Fatalf("tip at %d, want %d", at, height)
	}
}

func (b *swapBench) spent(funded types.UTXO) bool {
	b.t.Helper()
	out, err := b.n.rpc.GetTxOut(context.Background(), funded.TxID, funded.Vout, true)
	if err != nil {
		b.t.Fatal(err)
	}
	return out == nil
}

// recorded is a spend the node mined. record writes the spends of a run to
// the file named by HTLC_VECTORS_OUT, when it is set, so the offline tests of
// package htlc can hold the verifier to transactions a node accepted.
type recorded struct {
	Kind       string   `json:"kind"`
	Hash       string   `json:"hash"`
	PayeeHash  string   `json:"payee_hash"`
	PayerHash  string   `json:"payer_hash"`
	LockHeight uint32   `json:"lock_height"`
	FundedTxID string   `json:"funded_txid"`
	FundedVout uint32   `json:"funded_vout"`
	Value      int64    `json:"funded_value"`
	OutValue   int64    `json:"output_value"`
	OutScript  string   `json:"output_script"`
	Witness    []string `json:"witness"`
	TxID       string   `json:"txid"`
	Raw        string   `json:"raw"`
	MinedAt    int64    `json:"mined_at_height"`
}

// recordedFile is the vectors file: the node build the spends were mined by,
// as HTLC_VECTORS_SOURCE states it, and the spends.
type recordedFile struct {
	Source string     `json:"source"`
	Spends []recorded `json:"spends"`
}

var recordedSpends []recorded

func (b *swapBench) record(kind string, p htlc.Params, funded types.UTXO, tr *tx.Transaction, height int64) {
	r := recorded{Kind: kind, Hash: hex.EncodeToString(p.Hash[:]),
		PayeeHash: hex.EncodeToString(p.PayeeKeyHash[:]), PayerHash: hex.EncodeToString(p.PayerKeyHash[:]),
		LockHeight: uint32(p.LockHeight), FundedTxID: funded.TxID, FundedVout: funded.Vout, Value: funded.Value,
		OutValue: tr.Outputs[0].Value, OutScript: hex.EncodeToString(tr.Outputs[0].ScriptPubKey),
		TxID: tr.TxID(), Raw: tr.SerializeHex(), MinedAt: height}
	for _, item := range tr.Inputs[0].WitnessData {
		r.Witness = append(r.Witness, hex.EncodeToString(item))
	}
	recordedSpends = append(recordedSpends, r)
	path := os.Getenv("HTLC_VECTORS_OUT")
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(recordedFile{Source: os.Getenv("HTLC_VECTORS_SOURCE"), Spends: recordedSpends}, "", "  ")
	if err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// The claim branch: every spend that is not the payee's, with the secret, in
// the template's form is refused by the node, and the builder's claim is
// mined.
func TestSwapContractClaimAgainstTheNode(t *testing.T) {
	b := newSwapBench(t)
	secret := sha256.Sum256([]byte("the secret of the claim case"))
	lock := htlc.Height(b.n.height() + 40)
	p := b.contract(secret[:], lock)
	funded := b.fund(p)

	wrong := secret
	wrong[31] ^= 1
	claim := hand{who: b.payee, secret: secret[:], selector: []byte{0x01}, sequence: htlc.ClaimSequence}
	with := func(f func(*hand)) hand { h := claim; f(&h); return h }

	b.refused("a claim with 32 wrong bytes",
		b.handSpend(p, funded, with(func(h *hand) { h.secret = wrong[:] })), "Script failed an OP_EQUALVERIFY operation")
	b.refused("a claim with the secret under a stranger's key",
		b.handSpend(p, funded, with(func(h *hand) { h.who = b.stranger })), "OP_CHECKDILITHIUMKEYHASH verification failed")
	b.refused("a claim with the secret under the payer's key",
		b.handSpend(p, funded, with(func(h *hand) { h.who = b.payer })), "OP_CHECKDILITHIUMKEYHASH verification failed")
	b.refused("a claim with one more item under the signature",
		b.handSpend(p, funded, with(func(h *hand) { h.extra = [][]byte{{0x01}} })), "finished with a false/empty top stack element")
	b.refused("a claim with the selector 0x02",
		b.handSpend(p, funded, with(func(h *hand) { h.selector = []byte{0x02} })), "non-mandatory-script-verify-flag (OP_IF/NOTIF argument must be minimal)")
	b.refused("a claim with a 31-byte secret item",
		b.handSpend(p, funded, with(func(h *hand) { h.secret = secret[:31] })), "Script failed an OP_EQUALVERIFY operation")

	// The refund is not yet valid: the tip is below the lock height.
	refund, err := htlc.BuildRefund(p, funded, b.payer, htlcFeeRate, b.keys, b.payer)
	if err != nil {
		t.Fatal(err)
	}
	b.refused("the refund before the lock height", refund, "non-final")
	b.refused("a refund with a lock time below the script's height",
		b.handSpend(p, funded, hand{who: b.payer, selector: []byte{}, sequence: htlc.RefundSequence,
			lockTime: uint32(b.n.height())}), "Locktime requirement not satisfied")

	if b.spent(funded) {
		t.Fatal("a refused spend took the funded output")
	}
	built, err := htlc.BuildClaim(p, funded, secret, b.payee, htlcFeeRate, b.keys, b.payee)
	if err != nil {
		t.Fatal(err)
	}
	b.accepted("the builder's claim", built)
	height := b.minedAt("the builder's claim", built)
	b.record("claim", p, funded, built, height)

	// After the claim is mined the refund has nothing to spend, at the lock
	// height as before it.
	b.mineTo(int64(lock))
	b.refused("the refund after the claim is mined", refund, "bad-txns-inputs-spent")
}

// A secret of 33 bytes: the contract commits to its hash, so only the length
// check stands between the claim and the output. On another chain a contract
// with the same hash could not take that secret.
func TestSwapContractRefusesALongSecretAgainstTheNode(t *testing.T) {
	b := newSwapBench(t)
	long := make([]byte, 33)
	for i := range long {
		long[i] = byte(0x40 + i)
	}
	p := b.contract(long, htlc.Height(b.n.height()+40))
	funded := b.fund(p)
	b.refused("a claim with the 33-byte secret the hash commits to",
		b.handSpend(p, funded, hand{who: b.payee, secret: long, selector: []byte{0x01}, sequence: htlc.ClaimSequence}),
		"Script failed an OP_EQUALVERIFY operation")
	if b.spent(funded) {
		t.Fatal("the refused claim took the funded output")
	}
}

// The refund branch, at a lock height from 128 to 255: the height's script
// number is then one byte with the top bit set and carries the added zero, so
// the node checks that form of the push on a refund it mines.
func TestSwapContractRefundAgainstTheNode(t *testing.T) {
	b := newSwapBench(t)
	secret := sha256.Sum256([]byte("the secret of the refund case"))
	lock := htlc.Height(b.n.height() + 12)
	if lock < 128 {
		lock = 130
	}
	if lock > 255 {
		t.Fatalf("lock height %d: the case needs a height from 128 to 255", lock)
	}
	p := b.contract(secret[:], lock)
	funded := b.fund(p)
	refund, err := htlc.BuildRefund(p, funded, b.payer, htlcFeeRate, b.keys, b.payer)
	if err != nil {
		t.Fatal(err)
	}
	T := int64(lock)

	// One block short: the node's mempool judges the refund for block T.
	b.mineTo(T - 1)
	b.refused("the refund with the tip one block below the lock height", refund, "non-final")

	b.mineTo(T)
	b.refused("a refund with sequence 0xffffffff at the lock height",
		b.handSpend(p, funded, hand{who: b.payer, selector: []byte{}, sequence: 0xffffffff, lockTime: uint32(T)}),
		"Locktime requirement not satisfied")
	b.refused("a refund with lock time one below the script's height",
		b.handSpend(p, funded, hand{who: b.payer, selector: []byte{}, sequence: htlc.RefundSequence, lockTime: uint32(T - 1)}),
		"Locktime requirement not satisfied")
	b.refused("a refund under the payee's key",
		b.handSpend(p, funded, hand{who: b.payee, selector: []byte{}, sequence: htlc.RefundSequence, lockTime: uint32(T)}),
		"OP_CHECKDILITHIUMKEYHASH verification failed")
	b.refused("a refund under a stranger's key",
		b.handSpend(p, funded, hand{who: b.stranger, selector: []byte{}, sequence: htlc.RefundSequence, lockTime: uint32(T)}),
		"OP_CHECKDILITHIUMKEYHASH verification failed")
	b.refused("a refund with the selector 0x01 and no secret",
		b.handSpend(p, funded, hand{who: b.payer, selector: []byte{0x01}, sequence: htlc.RefundSequence, lockTime: uint32(T)}),
		"Script failed an OP_EQUALVERIFY operation")

	if b.spent(funded) {
		t.Fatal("a refused spend took the funded output")
	}
	b.accepted("the builder's refund with the tip at the lock height", refund)
	if height := b.minedAt("the builder's refund", refund); height != T+1 {
		t.Errorf("the refund is in block %d, want %d", height, T+1)
	} else {
		b.record("refund", p, funded, refund, height)
	}

	claim, err := htlc.BuildClaim(p, funded, secret, b.payee, htlcFeeRate, b.keys, b.payee)
	if err != nil {
		t.Fatal(err)
	}
	b.refused("the claim after the refund is mined", claim, "bad-txns-inputs-spent")
}

// After the lock height both spends are valid. The two sequence numbers
// decide what a mempool does with the second to arrive: a claim that pays
// more takes the place of a refund, and a refund does not take the place of
// a claim, whatever it pays.
func TestSwapContractClaimAndRefundMeetInTheMempool(t *testing.T) {
	b := newSwapBench(t)
	secret := sha256.Sum256([]byte("the secret of the mempool case"))
	lock := htlc.Height(b.n.height() + 6)
	p := b.contract(secret[:], lock)
	first, second := b.fund(p), b.fund(p)
	b.mineTo(int64(lock))

	// The refund arrives first, then a claim at three times its fee rate.
	// The claim pays the wallet key, which is not the key that signs it.
	refund, err := htlc.BuildRefund(p, first, b.payer, htlcFeeRate, b.keys, b.payer)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := htlc.BuildClaim(p, first, secret, b.hot, 3*htlcFeeRate, b.keys, b.payee)
	if err != nil {
		t.Fatal(err)
	}
	b.accepted("the refund, first to arrive", refund)
	b.accepted("the claim that pays more than the refund it meets", claim)
	if b.inMempool(refund.TxID()) {
		t.Error("the refund is still in the mempool beside the claim that replaced it")
	}
	b.minedAt("the claim that replaced the refund", claim)
	b.paysTo("the claim that replaced the refund", claim, b.hot)

	// The claim arrives first, then a refund at three times its fee rate.
	claim, err = htlc.BuildClaim(p, second, secret, b.payee, htlcFeeRate, b.keys, b.payee)
	if err != nil {
		t.Fatal(err)
	}
	refund, err = htlc.BuildRefund(p, second, b.payer, 3*htlcFeeRate, b.keys, b.payer)
	if err != nil {
		t.Fatal(err)
	}
	b.accepted("the claim, first to arrive", claim)
	b.refused("a refund that pays more than the claim it meets", refund, "txn-mempool-conflict")
	if !b.inMempool(claim.TxID()) {
		t.Error("the claim left the mempool")
	}
	b.minedAt("the claim the refund could not replace", claim)
}
