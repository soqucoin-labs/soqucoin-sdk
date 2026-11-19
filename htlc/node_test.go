// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// replies is a node that answers from recorded values and keeps what it was
// asked and sent.
type replies struct {
	info         rpc.BlockchainInfo
	infoErr      error
	out          *rpc.TxOut
	outErr       error
	broadcastErr error
	blocks       map[int64]string
	blockErr     error
	// prev names, by height, a block whose previousblockhash is not the
	// hash of the block below it; moved names a height whose hash differs
	// from the second time it is asked for.
	prev  map[int64]string
	moved map[int64]string
	asked map[int64]int
	// againErr answers a height asked for a second time.
	againErr error

	calls       int
	withMempool []bool
	sent        []string
	ids         []string
	read        []int64
}

func (r *replies) GetBlockchainInfo(context.Context) (*rpc.BlockchainInfo, error) {
	r.calls++
	if r.infoErr != nil {
		return nil, r.infoErr
	}
	info := r.info
	return &info, nil
}

func (r *replies) GetTxOut(_ context.Context, _ string, _ uint32, includeMempool bool) (*rpc.TxOut, error) {
	r.calls++
	r.withMempool = append(r.withMempool, includeMempool)
	if r.outErr != nil || r.out == nil {
		return nil, r.outErr
	}
	out := *r.out
	return &out, nil
}

func (r *replies) Broadcast(_ context.Context, rawTxHex, txid string) (string, error) {
	r.calls++
	if r.broadcastErr != nil {
		return "", r.broadcastErr
	}
	r.sent = append(r.sent, rawTxHex)
	r.ids = append(r.ids, txid)
	return txid, nil
}

func (r *replies) GetBlockHash(_ context.Context, height int64) (string, error) {
	r.calls++
	if _, ok := r.blocks[height]; !ok {
		return "", fmt.Errorf("%w: no block at %d", rpc.ErrPermanent, height)
	}
	if r.asked == nil {
		r.asked = map[int64]int{}
	}
	r.asked[height]++
	if r.againErr != nil && r.asked[height] > 1 {
		return "", r.againErr
	}
	if hash, ok := r.moved[height]; ok && r.asked[height] > 1 {
		return hash, nil
	}
	return fmt.Sprintf("hash-%d", height), nil
}

func (r *replies) GetBlock(_ context.Context, hash string, verbosity int) (json.RawMessage, error) {
	r.calls++
	var height int64
	if _, err := fmt.Sscanf(hash, "hash-%d", &height); err != nil || verbosity != 2 {
		return nil, fmt.Errorf("%w: block %q at verbosity %d", rpc.ErrPermanent, hash, verbosity)
	}
	r.read = append(r.read, height)
	block := r.blocks[height]
	if strings.HasPrefix(block, `{"`) {
		prev, ok := r.prev[height]
		if !ok {
			prev = fmt.Sprintf("hash-%d", height-1)
		}
		block = fmt.Sprintf(`{"previousblockhash":%q,`, prev) + block[1:]
	}
	return json.RawMessage(block), r.blockErr
}

const (
	testTip        = 1_000
	testActivation = testTip - 30 // the depth of testFundedValue below the tip
	testDepth      = 30
)

// bench is the fixture of spend_test.go on a node whose tip is exactly the
// depth above the three activation heights, with the contract funded at the
// outpoint w.funded and confirmed to the depth.
type bench struct {
	*world
	node   *replies
	signer *counting
	terms  Terms
	record []string
}

func newBench(t *testing.T) *bench {
	t.Helper()
	w := newWorld(t)
	script, err := w.p.OutputScript()
	if err != nil {
		t.Fatal(err)
	}
	b := &bench{world: w, signer: &counting{inner: w.m}, terms: Terms{Params: w.p, Amount: testFundedValue}}
	b.node = &replies{
		info: rpc.BlockchainInfo{
			Chain: types.Mainnet.ChainID, Blocks: testTip, Headers: testTip, BestBlockHash: "tip",
			ActivationHeights: map[string]int64{
				"p2wsh_dilithium": testActivation, "dilithium_keyhash": testActivation, "v6_controlflow": testActivation,
			},
		},
		out: &rpc.TxOut{BestBlock: "tip", Confirmations: testDepth, Value: testFundedValue,
			ScriptPubKey: rpc.ScriptPubKey{Hex: hex.EncodeToString(script)}},
	}
	return b
}

func (b *bench) party(p party) Party {
	return Party{Network: types.Mainnet, Signer: b.signer, Address: p.addr}
}

func (b *bench) keep(txid, rawHex string) error {
	b.record = append(b.record, txid+" "+rawHex)
	return nil
}

func (b *bench) funding() Funding {
	return Funding{
		Inputs:        []types.UTXO{{TxID: strings.Repeat("3c", 32), Vout: 0, Value: 2 * testFundedValue, Address: b.payer.addr}},
		ChangeAddress: b.payer.addr,
		FeeRate:       testFeeRate,
	}
}

// quiet requires that a refused call signed nothing, recorded nothing and
// sent nothing.
func (b *bench) quiet(t *testing.T, name string) {
	t.Helper()
	if b.signer.signs != 0 || len(b.record) != 0 || len(b.node.sent) != 0 {
		t.Errorf("%s: %d signatures, %d records, %d broadcasts after a refusal", name, b.signer.signs, len(b.record), len(b.node.sent))
	}
}

// none reports whether an outpoint is the zero value. types.UTXO holds a
// slice, so it is compared by its fields.
func none(u types.UTXO) bool {
	return u.TxID == "" && u.Vout == 0 && u.Value == 0 && u.Height == 0 && u.Address == "" && u.ScriptPubKey == nil
}

func sameOutput(a, b types.UTXO) bool {
	return a.TxID == b.TxID && a.Vout == b.Vout && a.Value == b.Value && a.Height == b.Height && a.Address == b.Address
}

func TestMinDepthAtTheAmountLines(t *testing.T) {
	for _, c := range []struct{ amount, want int64 }{
		{1, 30},
		{100 * types.ShorsPerSOQ, 30},
		{100*types.ShorsPerSOQ + 1, 120},
		{10_000 * types.ShorsPerSOQ, 120},
		{10_000*types.ShorsPerSOQ + 1, 288},
		{tx.MaxMoney, 288},
	} {
		if got := MinDepth(c.amount); got != c.want {
			t.Errorf("MinDepth(%d) = %d, want %d", c.amount, got, c.want)
		}
	}
}

// A reorganisation while the blocks are read can put the spend into a height
// already read. FindSecret gives a negative answer only for one chain: each
// block read names the one before it, and the last is still at its height.
func TestFindSecretGivesNoNegativeAnswerForAChainThatMoved(t *testing.T) {
	b := newBench(t)
	claim, refund := b.claim(t), b.refund(t)
	b.node.info.Blocks, b.node.info.Headers = 8, 8
	ctx := context.Background()
	find := func() ([32]byte, string, error) {
		b.node.asked = nil
		return FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5)
	}
	empty := func() { b.node.blocks = map[int64]string{5: block(), 6: block(), 7: block(), 8: block()} }

	empty()
	if _, _, err := find(); !errors.Is(err, ErrNoSpend) {
		t.Fatalf("one chain with no spend: got %v, want ErrNoSpend", err)
	}
	b.node.prev = map[int64]string{7: "hash-of-another-6"}
	if _, spender, err := find(); !errors.Is(err, ErrTipMoved) || errors.Is(err, ErrNoSpend) || spender != "" {
		t.Errorf("block 7 does not name the block read at 6: got %q and %v, want ErrTipMoved", spender, err)
	}
	b.node.prev = nil
	b.node.moved = map[int64]string{8: "hash-of-another-8"}
	if _, spender, err := find(); !errors.Is(err, ErrTipMoved) || errors.Is(err, ErrNoSpend) || spender != "" {
		t.Errorf("the last block read is no longer at its height: got %q and %v, want ErrTipMoved", spender, err)
	}

	// A refund in a block that has left the chain says nothing: the chain
	// that replaced it may hold the claim.
	b.node.blocks[7] = block([3]any{refund.TxID(), b.funded, refund.Inputs[0].WitnessData})
	b.node.moved = map[int64]string{7: "hash-of-another-7"}
	if secret, spender, err := find(); !errors.Is(err, ErrTipMoved) || errors.Is(err, ErrNoSecret) || spender != "" || secret != ([32]byte{}) {
		t.Errorf("a refund in a block no longer at its height: got %x, %q and %v; want ErrTipMoved", secret, spender, err)
	}
	b.node.moved = nil
	b.node.prev = map[int64]string{7: "hash-of-another-6"}
	if _, spender, err := find(); !errors.Is(err, ErrTipMoved) || errors.Is(err, ErrNoSecret) || spender != "" {
		t.Errorf("a refund in a block that does not name the block read below it: got %q and %v, want ErrTipMoved", spender, err)
	}
	b.node.prev = nil
	if _, spender, err := find(); !errors.Is(err, ErrNoSecret) || spender != refund.TxID() {
		t.Errorf("a refund on one chain: got %q and %v, want ErrNoSecret with its id", spender, err)
	}

	// The node's error for the height asked again is the answer, for a
	// refund and for no spend.
	b.node.againErr = fmt.Errorf("%w: asked again", rpc.ErrTransient)
	if _, spender, err := find(); !errors.Is(err, b.node.againErr) || errors.Is(err, ErrNoSecret) || spender != "" {
		t.Errorf("a refund, and the node fails on its height asked again: got %q and %v, want the node's error", spender, err)
	}
	empty()
	if _, _, err := find(); !errors.Is(err, b.node.againErr) || errors.Is(err, ErrNoSpend) {
		t.Errorf("no spend, and the node fails on the last height asked again: got %v, want the node's error", err)
	}
	b.node.againErr = nil

	// The secret is accepted by its hash, so a claim is returned from any
	// block, one that has moved included.
	b.node.blocks[7] = block([3]any{claim.TxID(), b.funded, claim.Inputs[0].WitnessData})
	b.node.prev, b.node.moved = map[int64]string{7: "hash-of-another-6"}, map[int64]string{7: "hash-of-another-7"}
	if secret, spender, err := find(); err != nil || secret != b.preimage || spender != claim.TxID() {
		t.Errorf("a claim in a block that moved: got %x, %q and %v; want the secret", secret, spender, err)
	}
}

func TestBuildFundingPaysTheContractFirst(t *testing.T) {
	b := newBench(t)
	f := b.funding()
	tr, err := BuildFunding(b.p, f.Inputs, testFundedValue, b.stranger.addr, testFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	contract, _ := b.p.OutputScript()
	if len(tr.Outputs) != 2 || !bytes.Equal(tr.Outputs[0].ScriptPubKey, contract) || tr.Outputs[0].Value != testFundedValue {
		t.Fatalf("outputs %+v: want the contract's script worth %d first, then change", tr.Outputs, testFundedValue)
	}
	change := append([]byte{0x51, 0x20}, b.stranger.kh[:]...)
	if !bytes.Equal(tr.Outputs[1].ScriptPubKey, change) {
		t.Errorf("change script %x, want the payment script of the change address %x", tr.Outputs[1].ScriptPubKey, change)
	}
	if fee := f.Inputs[0].Value - tr.Outputs[0].Value - tr.Outputs[1].Value; fee != tr.EstimateFee(testFeeRate) {
		t.Errorf("fee %d, want %d at the rate asked", fee, tr.EstimateFee(testFeeRate))
	}
	if len(tr.Inputs[0].WitnessData) != 0 {
		t.Error("BuildFunding returned a signed input")
	}

	contractAddress, _ := b.p.Address("sq")
	zeroLock := b.p
	zeroLock.LockHeight = 0
	for name, c := range map[string]struct {
		p      Params
		amount int64
		change string
		want   error
	}{
		"a lock height of zero":                 {zeroLock, testFundedValue, b.payer.addr, ErrLockHeight},
		"change to a script-output address":     {b.p, testFundedValue, contractAddress, ErrDestination},
		"change on another network":             {b.p, testFundedValue, b.elsewhere.addr, ErrNetwork},
		"an amount below the relay floor":       {b.p, 1, b.payer.addr, tx.ErrBelowDust},
		"an amount the inputs do not hold":      {b.p, 2 * testFundedValue, b.payer.addr, tx.ErrInsufficientFunds},
		"an amount above the node's range":      {b.p, tx.MaxMoney + 1, b.payer.addr, tx.ErrInvalidAmount},
		"an amount of zero":                     {b.p, 0, b.payer.addr, tx.ErrInvalidAmount},
		"a change address that is not a string": {b.p, testFundedValue, "", ErrDestination},
	} {
		if tr, err := BuildFunding(c.p, f.Inputs, c.amount, c.change, testFeeRate); !errors.Is(err, c.want) || tr != nil {
			t.Errorf("%s: got %v (transaction returned: %v), want %v", name, err, tr != nil, c.want)
		}
	}
}

// Every refusal of Fund, one change at a time on a call that funds. None asks
// the signer to sign, calls the record or sends, and a refusal that needs no
// reply of the node is made before the node is asked.
func TestFundRefusesBeforeTheSignerIsAsked(t *testing.T) {
	type call struct {
		b     *bench
		party Party
		terms Terms
		f     Funding
	}
	invalid := func(c *call) {
		key := append([]byte{0xff}, c.b.payer.pub[1:]...)
		c.party.Signer = &counting{inner: faulty{inner: c.b.m, otherKey: key}}
		c.terms.Params.PayerKeyHash = sha256.Sum256(key)
	}
	height := func(name string, h int64) func(*call) {
		return func(c *call) { c.b.node.info.ActivationHeights[name] = h }
	}
	cases := []struct {
		name       string
		change     func(*call)
		want       error
		beforeNode bool
	}{
		{"the party is the payee", func(c *call) { c.party.Address = c.b.payee.addr }, ErrWrongKey, true},
		{"the party is a stranger", func(c *call) { c.party.Address = c.b.stranger.addr }, ErrWrongKey, true},
		{"the party's key begins 0xff and the contract commits to it", invalid, keys.ErrInvalidPublicKey, true},
		{"the party's address is of another network", func(c *call) { c.party.Address = c.b.elsewhere.addr }, ErrNetwork, true},
		{"the party names no network", func(c *call) { c.party.Network = types.Network{} }, ErrNetwork, true},
		{"the party names a prefix and no chain", func(c *call) { c.party.Network = types.Network{HRP: types.Mainnet.HRP} }, ErrNetwork, true},
		{"the party names no signer", func(c *call) { c.party.Signer = nil }, ErrNetwork, true},
		{"the party's address is no address", func(c *call) { c.party.Address = "payer" }, ErrNetwork, true},
		{"a depth one below the floor", func(c *call) { c.terms.Depth = testDepth - 1 }, ErrDepth, true},
		{"a negative depth", func(c *call) { c.terms.Depth = -1 }, ErrDepth, true},
		{"a lock height of zero", func(c *call) { c.terms.Params.LockHeight = 0 }, ErrLockHeight, true},
		{"an amount of zero", func(c *call) { c.terms.Amount = 0 }, tx.ErrInvalidAmount, true},
		{"an amount above the node's range", func(c *call) { c.terms.Amount = tx.MaxMoney + 1 }, tx.ErrInvalidAmount, true},
		{"change on another network", func(c *call) { c.f.ChangeAddress = c.b.elsewhere.addr }, ErrNetwork, true},
		{"a change address that is no address", func(c *call) { c.f.ChangeAddress = "change" }, ErrNetwork, true},
		{"the signer holds no key for the party's address", func(c *call) {
			c.party.Signer = &counting{inner: faulty{inner: c.b.m, pubKeyErr: keys.ErrNoKey}}
		}, keys.ErrNoKey, true},

		{"the node fails", func(c *call) { c.b.node.infoErr = rpc.ErrTransient }, rpc.ErrTransient, false},
		{"the node is on another chain", func(c *call) { c.b.node.info.Chain = "stagenet" }, rpc.ErrWrongChain, false},
		{"the node is in initial block download", func(c *call) { c.b.node.info.InitialSync = true }, rpc.ErrNodeSyncing, false},
		{"the node is one block behind its headers", func(c *call) { c.b.node.info.Headers++ }, rpc.ErrNodeSyncing, false},
		{"the node prints no activation heights", func(c *call) { c.b.node.info.ActivationHeights = nil }, ErrNoActivationHeights, false},
		{"the lock height is the tip", func(c *call) { c.terms.Params.LockHeight = testTip }, ErrLockTooNear, false},
		{"the lock height is one block short of the margin", func(c *call) { c.terms.Params.LockHeight, c.f.LockMargin = testTip+9, 10 }, ErrLockTooNear, false},
		{"inputs worth less than the amount", func(c *call) { c.f.Inputs[0].Value = testFundedValue }, tx.ErrInsufficientFunds, false},
		{"no inputs", func(c *call) { c.f.Inputs = nil }, tx.ErrInsufficientFunds, false},
		{"an input of another network than the change", func(c *call) { c.f.Inputs[0].Address = c.b.elsewhere.addr }, ErrNetwork, false},
		{"an input whose address is no address", func(c *call) { c.f.Inputs[0].Address = "input" }, ErrNetwork, false},
		{"a fee rate of zero", func(c *call) { c.f.FeeRate = 0 }, tx.ErrInvalidAmount, false},
		{"a depth of 31 with the tip 30 above the heights", func(c *call) { c.terms.Depth = testDepth + 1 }, ErrNotActive, false},
	}
	for _, name := range deployments {
		cases = append(cases,
			struct {
				name       string
				change     func(*call)
				want       error
				beforeNode bool
			}{name + " is not reported", func(c *call) { delete(c.b.node.info.ActivationHeights, name) }, ErrNoActivationHeights, false},
			struct {
				name       string
				change     func(*call)
				want       error
				beforeNode bool
			}{name + " is reported below zero", height(name, -1), ErrNoActivationHeights, false},
			struct {
				name       string
				change     func(*call)
				want       error
				beforeNode bool
			}{name + " is active one block short of the depth", height(name, testActivation+1), ErrNotActive, false},
			struct {
				name       string
				change     func(*call)
				want       error
				beforeNode bool
			}{name + " has no height", height(name, 2147483647), ErrNotActive, false},
			struct {
				name       string
				change     func(*call)
				want       error
				beforeNode bool
			}{name + " is reported at the largest number", height(name, math.MaxInt64), ErrNotActive, false},
		)
	}
	for _, tc := range cases {
		b := newBench(t)
		c := &call{b: b, party: b.party(b.payer), terms: b.terms, f: b.funding()}
		tc.change(c)
		funded, tr, err := Fund(context.Background(), b.node, c.party, c.terms, c.f, b.keep)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if tr != nil || !none(funded) {
			t.Errorf("%s: a refused Fund returned %+v and a transaction (%v)", tc.name, funded, tr != nil)
		}
		b.quiet(t, tc.name)
		if s, ok := c.party.Signer.(*counting); ok && s.signs != 0 {
			t.Errorf("%s: the signer was asked for %d signatures", tc.name, s.signs)
		}
		if tc.beforeNode && b.node.calls != 0 {
			t.Errorf("%s: the node was asked %d times before the refusal", tc.name, b.node.calls)
		}
	}
}

// Fund at the first tip the rule allows: the record receives the bytes before
// the node does, and the outpoint returned is output 0 of what was sent.
func TestFundRecordsThenSends(t *testing.T) {
	b := newBench(t)
	var sentAtRecord int
	record := func(txid, rawHex string) error {
		sentAtRecord = len(b.node.sent)
		return b.keep(txid, rawHex)
	}
	terms := b.terms
	terms.Params.LockHeight = testTip + 1
	funded, tr, err := Fund(context.Background(), b.node, b.party(b.payer), terms, b.funding(), record)
	if err != nil {
		t.Fatal(err)
	}
	contract, _ := terms.Params.OutputScript()
	if !bytes.Equal(tr.Outputs[0].ScriptPubKey, contract) || tr.Outputs[0].Value != testFundedValue {
		t.Errorf("output 0 is %x worth %d, want the contract worth %d", tr.Outputs[0].ScriptPubKey, tr.Outputs[0].Value, testFundedValue)
	}
	if err := tr.VerifyAll(); err != nil {
		t.Errorf("the funding does not verify: %v", err)
	}
	if want := (types.UTXO{TxID: tr.TxID(), Vout: 0, Value: testFundedValue}); !sameOutput(funded, want) {
		t.Errorf("Fund returned %+v, want %+v", funded, want)
	}
	if len(b.record) != 1 || b.record[0] != tr.TxID()+" "+tr.SerializeHex() || sentAtRecord != 0 {
		t.Errorf("%d records, %d broadcasts before the record; want the id and the bytes recorded once, before any broadcast", len(b.record), sentAtRecord)
	}
	if len(b.node.sent) != 1 || b.node.sent[0] != tr.SerializeHex() || b.node.ids[0] != tr.TxID() {
		t.Errorf("the node was sent %d transactions under the ids %v, want the one returned under its id, by which a lost reply is resolved", len(b.node.sent), b.node.ids)
	}

	// A lock margin holds at its own figure, and a depth above the floor
	// holds when the tip is that far above the heights.
	b = newBench(t)
	f := b.funding()
	f.LockMargin = 10
	terms.Params.LockHeight = testTip + 10
	terms.Depth = 100
	for name := range b.node.info.ActivationHeights {
		b.node.info.ActivationHeights[name] = testTip - 100
	}
	if _, _, err := Fund(context.Background(), b.node, b.party(b.payer), terms, f, nil); err != nil {
		t.Errorf("a margin of 10 with 10 blocks left, a depth of 100 with the tip 100 above, no record: %v", err)
	}
	b.node.info.ActivationHeights["v6_controlflow"]++
	if _, _, err := Fund(context.Background(), b.node, b.party(b.payer), terms, f, nil); !errors.Is(err, ErrNotActive) {
		t.Errorf("a depth of 100 with one deployment 99 below the tip: got %v, want ErrNotActive", err)
	}
}

// A record that refuses stops the broadcast, and a broadcast that fails
// passes the node's error on. In both the signed transaction is returned:
// after an unknown outcome the caller sends those bytes again.
func TestFundReturnsTheTransactionBesideARecordOrBroadcastError(t *testing.T) {
	b := newBench(t)
	full := errors.New("disk full")
	_, tr, err := Fund(context.Background(), b.node, b.party(b.payer), b.terms, b.funding(), func(string, string) error { return full })
	if !errors.Is(err, ErrRecord) || !errors.Is(err, full) || tr == nil || len(b.node.sent) != 0 {
		t.Errorf("a refusing record: got %v, transaction returned %v, %d broadcasts; want ErrRecord with the cause, the transaction and none", err, tr != nil, len(b.node.sent))
	}

	b = newBench(t)
	b.node.broadcastErr = rpc.ErrUnknownOutcome
	funded, tr, err := Fund(context.Background(), b.node, b.party(b.payer), b.terms, b.funding(), b.keep)
	if !errors.Is(err, rpc.ErrUnknownOutcome) || tr == nil || !none(funded) || len(b.record) != 1 {
		t.Errorf("an unknown outcome: got %v, transaction returned %v, outpoint %+v, %d records; want the node's error, the transaction, no outpoint and one record", err, tr != nil, funded, len(b.record))
	}
}

// A funding whose inputs do not verify is neither recorded nor sent: a signer
// that signs another digest, and inputs of a key the signer does not hold.
func TestFundSendsNothingAnInputDoesNotVerifyIn(t *testing.T) {
	b := newBench(t)
	party := b.party(b.payer)
	party.Signer = faulty{inner: b.m, flipDig: true}
	funded, tr, err := Fund(context.Background(), b.node, party, b.terms, b.funding(), b.keep)
	if !errors.Is(err, tx.ErrSignature) || tr != nil || !none(funded) || len(b.record) != 0 || len(b.node.sent) != 0 {
		t.Errorf("a signature over another digest: got %v, transaction returned %v, %d records, %d broadcasts; want tx.ErrSignature and nothing", err, tr != nil, len(b.record), len(b.node.sent))
	}

	b = newBench(t)
	unheld, err := keys.GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	f := b.funding()
	f.Inputs[0].Address = unheld.Address
	funded, tr, err = Fund(context.Background(), b.node, b.party(b.payer), b.terms, f, b.keep)
	if !errors.Is(err, keys.ErrNoKey) || tr != nil || !none(funded) || len(b.record) != 0 || len(b.node.sent) != 0 {
		t.Errorf("an input of a key the signer does not hold: got %v, transaction returned %v, %d records, %d broadcasts; want keys.ErrNoKey and nothing", err, tr != nil, len(b.record), len(b.node.sent))
	}
}

// Every refusal of Audit, one change at a time on an output that passes.
func TestAuditRefusesAnOutputThatIsNotTheAgreedOne(t *testing.T) {
	type call struct {
		b            *bench
		party        Party
		terms        Terms
		blocksToLock int64
	}
	otherLock := func(c *call) { c.terms.Params.LockHeight++ }
	cases := []struct {
		name   string
		change func(*call)
		want   error
	}{
		{"the party is a stranger", func(c *call) { c.party.Address = c.b.stranger.addr }, ErrWrongKey},
		{"the party's address is of another network", func(c *call) { c.party.Address = c.b.elsewhere.addr }, ErrNetwork},
		{"a depth below the floor", func(c *call) { c.terms.Depth = testDepth - 1 }, ErrDepth},
		{"an amount of zero", func(c *call) { c.terms.Amount = 0 }, tx.ErrInvalidAmount},
		{"the node is on another chain", func(c *call) { c.b.node.info.Chain = "regtest" }, rpc.ErrWrongChain},
		{"the node is behind its headers", func(c *call) { c.b.node.info.Headers++ }, rpc.ErrNodeSyncing},
		{"the node prints no activation heights", func(c *call) { c.b.node.info.ActivationHeights = nil }, ErrNoActivationHeights},
		{"one deployment is a block short of the depth", func(c *call) { c.b.node.info.ActivationHeights["dilithium_keyhash"]++ }, ErrNotActive},
		{"no unspent output at the outpoint", func(c *call) { c.b.node.out = nil }, ErrNotFunded},
		{"the node fails on the output", func(c *call) { c.b.node.outErr = rpc.ErrTransient }, rpc.ErrTransient},
		{"the terms name another lock height than the output", otherLock, ErrFundedOutput},
		{"the terms name another hash than the output", func(c *call) { c.terms.Params.Hash[0] ^= 1 }, ErrFundedOutput},
		{"the output's script in upper case", func(c *call) { c.b.node.out.ScriptPubKey.Hex = strings.ToUpper(c.b.node.out.ScriptPubKey.Hex) }, ErrFundedOutput},
		{"a value one shor below the amount", func(c *call) { c.b.node.out.Value-- }, ErrFundedOutput},
		{"one confirmation below the depth", func(c *call) { c.b.node.out.Confirmations-- }, ErrShallow},
		{"in the mempool", func(c *call) { c.b.node.out.Confirmations = 0 }, ErrShallow},
		{"a coinbase output at the depth and below maturity", func(c *call) { c.b.node.out.Coinbase = true }, ErrShallow},
		{"a coinbase output one below maturity", func(c *call) { c.b.node.out.Coinbase, c.b.node.out.Confirmations = true, 287 }, ErrShallow},
		{"a coinbase output one below maturity on a network that names none", func(c *call) {
			c.party.Network = types.Network{ChainID: types.Mainnet.ChainID, HRP: types.Mainnet.HRP}
			c.b.node.out.Coinbase, c.b.node.out.Confirmations = true, 287
		}, ErrShallow},
		{"the output was read at another tip", func(c *call) { c.b.node.out.BestBlock = "earlier" }, ErrTipMoved},
		{"one block short of the blocks asked before the lock height", func(c *call) { c.blocksToLock = testLockHeight - testTip + 1 }, ErrLockTooNear},
	}
	for _, tc := range cases {
		b := newBench(t)
		c := &call{b: b, party: b.party(b.payee), terms: b.terms}
		tc.change(c)
		got, err := Audit(context.Background(), b.node, c.party, c.terms, b.funded.TxID, b.funded.Vout, c.blocksToLock)
		if !errors.Is(err, tc.want) || !none(got.Output) || got.Confirmations != 0 || got.Tip != 0 {
			t.Errorf("%s: got %+v and %v, want %v", tc.name, got, err, tc.want)
		}
	}
	if !errors.Is(ErrTipMoved, rpc.ErrTransient) {
		t.Error("ErrTipMoved is not transient")
	}
}

// Audit passes for either party of the contract and returns what the node
// holds: its value where that is above the amount, and the funding height.
func TestAuditReturnsTheOutputAsTheNodeHoldsIt(t *testing.T) {
	for _, who := range []string{"payee", "payer"} {
		b := newBench(t)
		p := b.payee
		if who == "payer" {
			p = b.payer
		}
		b.node.out.Value = testFundedValue + 7
		b.node.out.Confirmations = 41
		got, err := Audit(context.Background(), b.node, b.party(p), b.terms, b.funded.TxID, b.funded.Vout, testLockHeight-testTip)
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		want := Funded{
			Output:        types.UTXO{TxID: b.funded.TxID, Vout: b.funded.Vout, Value: testFundedValue + 7, Height: testTip - 40},
			Confirmations: 41, Tip: testTip,
		}
		if !sameOutput(got.Output, want.Output) || got.Confirmations != want.Confirmations || got.Tip != want.Tip {
			t.Errorf("%s: got %+v, want %+v", who, got, want)
		}
		if len(b.node.withMempool) != 1 || !b.node.withMempool[0] {
			t.Errorf("%s: the output was read with the mempool %v, want once with it", who, b.node.withMempool)
		}
		b.quiet(t, who)
	}

	b := newBench(t)
	b.node.out.Coinbase, b.node.out.Confirmations = true, 288
	if _, err := Audit(context.Background(), b.node, b.party(b.payee), b.terms, b.funded.TxID, b.funded.Vout, 0); err != nil {
		t.Errorf("a coinbase output at maturity: %v", err)
	}
}

// Claim sends a claim of the output as the node holds it, recorded first.
func TestClaimBuildsOnTheNodesValue(t *testing.T) {
	b := newBench(t)
	b.node.out.Value = testFundedValue + 7
	terms := b.terms
	terms.Params.LockHeight = testTip + testDepth + 6 // exactly depth + inclusion above the tip
	script, _ := terms.Params.OutputScript()
	b.node.out.ScriptPubKey.Hex = hex.EncodeToString(script)
	tr, err := Claim(context.Background(), b.node, b.party(b.payee), terms, b.funded.TxID, b.funded.Vout, b.preimage,
		Spend{PayToAddress: b.stranger.addr, FeeRate: testFeeRate}, 6, b.keep)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySpend(tr, terms.Params, testFundedValue+7); err != nil {
		t.Errorf("the claim does not verify against the node's value: %v", err)
	}
	pay := append([]byte{0x51, 0x20}, b.stranger.kh[:]...)
	if !bytes.Equal(tr.Outputs[0].ScriptPubKey, pay) {
		t.Errorf("the claim pays %x, want the destination %x", tr.Outputs[0].ScriptPubKey, pay)
	}
	if fee := testFundedValue + 7 - tr.Outputs[0].Value; fee != tr.EstimateFee(testFeeRate) {
		t.Errorf("the claim pays a fee of %d, want %d at the rate asked", fee, tr.EstimateFee(testFeeRate))
	}
	if len(b.record) != 1 || b.record[0] != tr.TxID()+" "+tr.SerializeHex() || len(b.node.sent) != 1 || b.node.sent[0] != tr.SerializeHex() {
		t.Errorf("%d records and %d broadcasts, want the claim recorded once and sent once", len(b.record), len(b.node.sent))
	}
	if len(b.node.withMempool) != 1 || !b.node.withMempool[0] {
		t.Errorf("the output was read with the mempool %v, want once with it", b.node.withMempool)
	}
}

// Every refusal of Claim comes before the signer is asked, so no refused
// claim shows the secret, and no refusal prints it.
func TestClaimRefusesBeforeTheSignerIsAsked(t *testing.T) {
	type call struct {
		b         *bench
		party     Party
		terms     Terms
		preimage  [32]byte
		s         Spend
		inclusion int64
	}
	cases := []struct {
		name       string
		change     func(*call)
		want       error
		beforeNode bool
	}{
		{"the party is the payer", func(c *call) { c.party.Address = c.b.payer.addr }, ErrWrongKey, true},
		{"the party is a stranger", func(c *call) { c.party.Address = c.b.stranger.addr }, ErrWrongKey, true},
		{"the party's address is of another network", func(c *call) { c.party.Address = c.b.elsewhere.addr }, ErrNetwork, true},
		{"another preimage", func(c *call) { c.preimage[31] ^= 1 }, ErrPreimage, true},
		{"a depth below the floor", func(c *call) { c.terms.Depth = 1 }, ErrDepth, true},
		{"no block allowed for inclusion", func(c *call) { c.inclusion = 0 }, ErrLockTooNear, true},
		{"an inclusion that leaves int64 when the depth is added", func(c *call) { c.inclusion = math.MaxInt64 }, ErrLockTooNear, true},
		{"the largest inclusion that does not", func(c *call) { c.inclusion = math.MaxInt64 - testDepth }, ErrLockTooNear, false},
		{"the node is on another chain", func(c *call) { c.b.node.info.Chain = "stagenet" }, rpc.ErrWrongChain, false},
		{"the node prints no activation heights", func(c *call) { c.b.node.info.ActivationHeights = nil }, ErrNoActivationHeights, false},
		{"one deployment is a block short of the depth", func(c *call) { c.b.node.info.ActivationHeights["p2wsh_dilithium"]++ }, ErrNotActive, false},
		{"the output is spent or absent", func(c *call) { c.b.node.out = nil }, ErrNotFunded, false},
		{"the output is another contract", func(c *call) { c.b.node.out.ScriptPubKey.Hex = "5620" + strings.Repeat("00", 32) }, ErrFundedOutput, false},
		{"the output holds less than the amount", func(c *call) { c.b.node.out.Value-- }, ErrFundedOutput, false},
		{"the funding is one confirmation below the depth", func(c *call) { c.b.node.out.Confirmations-- }, ErrShallow, false},
		{"the tip is one block inside depth and inclusion of the lock height", func(c *call) {
			c.b.node.info.Blocks, c.b.node.info.Headers = testLockHeight-testDepth-5, testLockHeight-testDepth-5
		}, ErrLockTooNear, false},
		{"the destination is of another network", func(c *call) { c.s.PayToAddress = c.b.elsewhere.addr }, ErrDestination, false},
		{"a fee rate of zero", func(c *call) { c.s.FeeRate = 0 }, tx.ErrInvalidAmount, false},
	}
	for _, tc := range cases {
		b := newBench(t)
		c := &call{b: b, party: b.party(b.payee), terms: b.terms, preimage: b.preimage,
			s: Spend{PayToAddress: b.payee.addr, FeeRate: testFeeRate}, inclusion: 6}
		tc.change(c)
		tr, err := Claim(context.Background(), b.node, c.party, c.terms, b.funded.TxID, b.funded.Vout, c.preimage, c.s, c.inclusion, b.keep)
		if !errors.Is(err, tc.want) || tr != nil {
			t.Errorf("%s: got %v (transaction returned: %v), want %v", tc.name, err, tr != nil, tc.want)
		}
		b.quiet(t, tc.name)
		if tc.beforeNode && b.node.calls != 0 {
			t.Errorf("%s: the node was asked %d times before the refusal", tc.name, b.node.calls)
		}
		if err != nil {
			holdsNoItem(t, tc.name, err, [][]byte{b.preimage[:]})
		}
	}

	// A record that refuses keeps the claim from the node. The claim is
	// returned; its bytes hold the secret and the caller chose the record.
	b := newBench(t)
	tr, err := Claim(context.Background(), b.node, b.party(b.payee), b.terms, b.funded.TxID, b.funded.Vout, b.preimage,
		Spend{PayToAddress: b.payee.addr, FeeRate: testFeeRate}, 6, func(string, string) error { return errors.New("no") })
	if !errors.Is(err, ErrRecord) || tr == nil || len(b.node.sent) != 0 {
		t.Errorf("a refusing record: got %v, transaction returned %v, %d broadcasts", err, tr != nil, len(b.node.sent))
	}
}

// slow is a signer during whose signature something else happens.
type slow struct {
	inner     tx.Signer
	meanwhile func()
}

func (s slow) Sign(a string, d []byte) ([]byte, error) {
	s.meanwhile()
	return s.inner.Sign(a, d)
}
func (s slow) PublicKeyFor(a string) ([]byte, error) { return s.inner.PublicKeyFor(a) }

// Claim audits, then asks the signer and the record, and neither is bounded
// in time. It reads the tip a second time after the record and sends only
// while the margin still holds, so a slow signer or a slow record does not
// put the secret on the network at a tip from which the refund can win. The
// claim is returned beside the refusal: it is signed and the record holds it.
func TestClaimReadsTheTipAgainBeforeItSends(t *testing.T) {
	const inclusion = 6
	const edge = testLockHeight - testDepth - inclusion // the last tip that leaves the margin
	at := func(tip int64) func(*bench) {
		return func(b *bench) { b.node.info.Blocks, b.node.info.Headers = tip, tip }
	}
	down := fmt.Errorf("%w: connection refused", rpc.ErrTransient)
	cases := []struct {
		name      string
		meanwhile func(*bench)
		cause     error
		sent      bool
	}{
		{"the tip reaches the last block that leaves the margin", at(edge), nil, true},
		{"the tip moves one block inside the margin", at(edge + 1), nil, false},
		{"the tip passes the lock height", at(testLockHeight + 1), nil, false},
		{"the node does not answer", func(b *bench) { b.node.infoErr = down }, down, false},
		{"the node is on another chain", func(b *bench) { b.node.info.Chain = "stagenet" }, rpc.ErrWrongChain, false},
		{"the node is behind its headers", func(b *bench) { b.node.info.Headers++ }, rpc.ErrNodeSyncing, false},
		{"the node is in initial block download", func(b *bench) { b.node.info.InitialSync = true }, rpc.ErrNodeSyncing, false},
	}
	for _, tc := range cases {
		for _, during := range []string{"the record", "the signature"} {
			name := tc.name + " during " + during
			b := newBench(t)
			party, record := b.party(b.payee), b.keep
			if during == "the record" {
				record = func(txid, rawHex string) error { tc.meanwhile(b); return b.keep(txid, rawHex) }
			} else {
				party.Signer = slow{inner: b.signer, meanwhile: func() { tc.meanwhile(b) }}
			}
			tr, err := Claim(context.Background(), b.node, party, b.terms, b.funded.TxID, b.funded.Vout, b.preimage,
				Spend{PayToAddress: b.payee.addr, FeeRate: testFeeRate}, inclusion, record)
			if tr == nil || b.signer.signs != 1 || len(b.record) != 1 {
				t.Fatalf("%s: transaction returned %v, %d signatures, %d records; want the claim signed and recorded once", name, tr != nil, b.signer.signs, len(b.record))
			}
			if tc.sent {
				if err != nil || len(b.node.sent) != 1 || b.node.sent[0] != tr.SerializeHex() || b.node.calls != 4 {
					t.Errorf("%s: got %v, %d broadcasts and %d calls of the node; want the claim sent after one more read", name, err, len(b.node.sent), b.node.calls)
				}
				continue
			}
			if !errors.Is(err, ErrLockTooNear) || len(b.node.sent) != 0 {
				t.Errorf("%s: got %v and %d broadcasts, want ErrLockTooNear and none", name, err, len(b.node.sent))
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("%s: got %v, want the cause %v kept", name, err, tc.cause)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "nothing was sent") {
					t.Errorf("%s: the refusal does not say that nothing was sent: %v", name, err)
				}
				holdsNoItem(t, name, err, [][]byte{b.preimage[:]})
			}
		}
	}

	// A record that refuses ends the call there: the node is not asked again.
	b := newBench(t)
	_, err := Claim(context.Background(), b.node, b.party(b.payee), b.terms, b.funded.TxID, b.funded.Vout, b.preimage,
		Spend{PayToAddress: b.payee.addr, FeeRate: testFeeRate}, inclusion, func(string, string) error { return errors.New("no") })
	if !errors.Is(err, ErrRecord) || errors.Is(err, ErrLockTooNear) || b.node.calls != 2 {
		t.Errorf("a refusing record: got %v after %d calls of the node, want ErrRecord after the two of the audit", err, b.node.calls)
	}
}

// Refund sends from the lock height on and refuses below it.
func TestRefundFromTheLockHeightOn(t *testing.T) {
	spend := func(b *bench) Spend { return Spend{PayToAddress: b.stranger.addr, FeeRate: testFeeRate} }
	at := func(b *bench, tip int64) { b.node.info.Blocks, b.node.info.Headers = tip, tip }

	b := newBench(t)
	at(b, testLockHeight-1)
	tr, err := Refund(context.Background(), b.node, b.party(b.payer), b.p, b.funded.TxID, b.funded.Vout, spend(b), b.keep)
	if !errors.Is(err, ErrBeforeLockHeight) || tr != nil {
		t.Errorf("one block below the lock height: got %v (transaction returned: %v), want ErrBeforeLockHeight", err, tr != nil)
	}
	b.quiet(t, "one block below the lock height")

	// At the lock height, on a node that prints no activation heights: the
	// refund asks for no activation rule and no depth.
	b = newBench(t)
	at(b, testLockHeight)
	b.node.info.ActivationHeights = nil
	b.node.out.Value, b.node.out.Confirmations = testFundedValue+7, 0
	tr, err = Refund(context.Background(), b.node, b.party(b.payer), b.p, b.funded.TxID, b.funded.Vout, spend(b), b.keep)
	if err != nil {
		t.Fatalf("at the lock height: %v", err)
	}
	if err := VerifySpend(tr, b.p, testFundedValue+7); err != nil {
		t.Errorf("the refund does not verify against the node's value: %v", err)
	}
	if fee := testFundedValue + 7 - tr.Outputs[0].Value; fee != tr.EstimateFee(testFeeRate) {
		t.Errorf("the refund pays a fee of %d, want %d at the rate asked", fee, tr.EstimateFee(testFeeRate))
	}
	if len(tr.Inputs[0].WitnessData) != refundItems {
		t.Errorf("%d witness items, want a refund's %d", len(tr.Inputs[0].WitnessData), refundItems)
	}
	if len(b.record) != 1 || len(b.node.sent) != 1 || b.node.sent[0] != tr.SerializeHex() || !b.node.withMempool[0] {
		t.Errorf("%d records, %d broadcasts, mempool read %v; want the refund recorded once, sent once, the output read with the mempool", len(b.record), len(b.node.sent), b.node.withMempool)
	}

	b = newBench(t)
	at(b, testLockHeight)
	tr, err = Refund(context.Background(), b.node, b.party(b.payer), b.p, b.funded.TxID, b.funded.Vout,
		Spend{PayToAddress: b.elsewhere.addr, FeeRate: testFeeRate}, b.keep)
	if !errors.Is(err, ErrDestination) || tr != nil {
		t.Errorf("a destination of another network: got %v (transaction returned: %v), want ErrDestination", err, tr != nil)
	}
	b.quiet(t, "a destination of another network")

	otherScript := func(b *bench) { b.node.out.ScriptPubKey.Hex = "5620" + strings.Repeat("11", 32) }
	for name, c := range map[string]struct {
		change     func(*bench) (Party, Params)
		want       error
		beforeNode bool
	}{
		"the party is the payee":                    {func(b *bench) (Party, Params) { return b.party(b.payee), b.p }, ErrWrongKey, true},
		"the party's address is of another network": {func(b *bench) (Party, Params) { return b.party(b.elsewhere), b.p }, ErrNetwork, true},
		"a lock height of zero":                     {func(b *bench) (Party, Params) { p := b.p; p.LockHeight = 0; return b.party(b.payer), p }, ErrLockHeight, true},
		"the node is on another chain":              {func(b *bench) (Party, Params) { b.node.info.Chain = "stagenet"; return b.party(b.payer), b.p }, rpc.ErrWrongChain, false},
		"the node is in initial sync":               {func(b *bench) (Party, Params) { b.node.info.InitialSync = true; return b.party(b.payer), b.p }, rpc.ErrNodeSyncing, false},
		"the output is spent or absent":             {func(b *bench) (Party, Params) { b.node.out = nil; return b.party(b.payer), b.p }, ErrNotFunded, false},
		"the output is another contract":            {func(b *bench) (Party, Params) { otherScript(b); return b.party(b.payer), b.p }, ErrFundedOutput, false},
	} {
		b := newBench(t)
		at(b, testLockHeight)
		party, p := c.change(b)
		tr, err := Refund(context.Background(), b.node, party, p, b.funded.TxID, b.funded.Vout, spend(b), b.keep)
		if !errors.Is(err, c.want) || tr != nil {
			t.Errorf("%s: got %v (transaction returned: %v), want %v", name, err, tr != nil, c.want)
		}
		if c.beforeNode && b.node.calls != 0 {
			t.Errorf("%s: the node was asked %d times before the refusal", name, b.node.calls)
		}
		b.quiet(t, name)
	}
}

func TestSecretFromWitnessTakesTheItemThatHashesToTheHash(t *testing.T) {
	w := newWorld(t)
	claim, refund := w.claim(t).Inputs[0].WitnessData, w.refund(t).Inputs[0].WitnessData
	if got, err := SecretFromWitness(claim, w.p); err != nil || got != w.preimage {
		t.Errorf("a claim's witness: got %x and %v, want the secret", got, err)
	}
	if got, err := SecretFromWitness([][]byte{{0x01}, w.preimage[:]}, w.p); err != nil || got != w.preimage {
		t.Errorf("the secret as the last of two items: got %x and %v", got, err)
	}
	long := append(w.preimage[:], 0x00)
	forLong := w.p
	forLong.Hash = sha256.Sum256(long)
	other := w.preimage
	other[0] ^= 1
	for name, c := range map[string]struct {
		items [][]byte
		p     Params
	}{
		"a refund's witness": {refund, w.p},
		"no items":           {nil, w.p},
		"32 other bytes":     {[][]byte{other[:]}, w.p},
		"33 bytes that hash to the contract's hash": {[][]byte{long}, forLong},
		"the secret's first 31 bytes":               {[][]byte{w.preimage[:31]}, w.p},
	} {
		if got, err := SecretFromWitness(c.items, c.p); !errors.Is(err, ErrNoSecret) || got != ([32]byte{}) {
			t.Errorf("%s: got %x and %v, want ErrNoSecret", name, got, err)
		}
	}
}

// block writes the node's verbose form of a block with one coinbase and the
// given spends, each a transaction id, the outpoint it spends and its witness.
func block(spends ...[3]any) string {
	type vin struct {
		Coinbase string   `json:"coinbase,omitempty"`
		TxID     string   `json:"txid,omitempty"`
		Vout     uint32   `json:"vout"`
		Witness  []string `json:"txinwitness,omitempty"`
	}
	type txn struct {
		TxID string `json:"txid"`
		Vin  []vin  `json:"vin"`
	}
	txs := []txn{{TxID: strings.Repeat("c0", 32), Vin: []vin{{Coinbase: "01"}}}}
	for _, s := range spends {
		out := s[1].(types.UTXO)
		var items []string
		for _, item := range s[2].([][]byte) {
			items = append(items, hex.EncodeToString(item))
		}
		txs = append(txs, txn{TxID: s[0].(string), Vin: []vin{
			{TxID: strings.Repeat("ee", 32), Vout: out.Vout},
			{TxID: out.TxID, Vout: out.Vout, Witness: items},
		}})
	}
	raw, _ := json.Marshal(map[string]any{"tx": txs})
	return string(raw)
}

func TestFindSecretReadsTheMinedSpendOfTheOutpoint(t *testing.T) {
	b := newBench(t)
	claim, refund := b.claim(t), b.refund(t)
	other := b.funded
	other.Vout++
	b.node.info.Blocks, b.node.info.Headers = 8, 8
	b.node.blocks = map[int64]string{
		5: block(),
		6: block([3]any{"spend-of-another-output", other, claim.Inputs[0].WitnessData}),
		7: block([3]any{claim.TxID(), b.funded, claim.Inputs[0].WitnessData}),
		8: block(),
	}
	ctx := context.Background()
	secret, spender, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5)
	if err != nil || secret != b.preimage || spender != claim.TxID() {
		t.Fatalf("got %x, %q and %v; want the secret from the claim %s", secret, spender, err, claim.TxID())
	}
	if fmt.Sprint(b.node.read) != "[5 6 7]" {
		t.Errorf("blocks read: %v, want 5, 6 and 7", b.node.read)
	}

	// A node takes a transaction id in either case, so Audit passes with one
	// in upper case. FindSecret finds the same spend under it.
	if secret, spender, err := FindSecret(ctx, b.node, b.p, strings.ToUpper(b.funded.TxID), b.funded.Vout, 5); err != nil || secret != b.preimage || spender != claim.TxID() {
		t.Errorf("the outpoint's id in upper case: got %x, %q and %v; want the secret from the claim", secret, spender, err)
	}
	// An id that is not 64 hexadecimal digits names no output, and the node
	// is not asked. The empty id equals the absent id of a coinbase input, so
	// without the refusal output 0 of it reads as refunded by the coinbase.
	for _, id := range []string{"", b.funded.TxID[:62], b.funded.TxID + "7a", "zz" + b.funded.TxID[2:]} {
		calls := b.node.calls
		if _, spender, err := FindSecret(ctx, b.node, b.p, id, 0, 5); !errors.Is(err, ErrOutpoint) || spender != "" || b.node.calls != calls {
			t.Errorf("the id %q: got %q and %v after %d calls of the node; want ErrOutpoint, no spender and no call", id, spender, err, b.node.calls-calls)
		}
	}

	if _, spender, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 8); !errors.Is(err, ErrNoSpend) || spender != "" {
		t.Errorf("from above the claim's block: got %q and %v, want ErrNoSpend", spender, err)
	}
	if _, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 9); !errors.Is(err, ErrNoSpend) {
		t.Errorf("from above the tip: got %v, want ErrNoSpend", err)
	}
	if _, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 4); !errors.Is(err, rpc.ErrPermanent) {
		t.Errorf("from a height the node has no block for: got %v, want the node's error", err)
	}

	// A node that is behind its headers or in initial block download has not
	// read the chain to its end: where it finds no spend it says so in place
	// of ErrNoSpend, and a spend it does hold is returned all the same.
	for name, behind := range map[string]func(){
		"behind its headers":        func() { b.node.info.Headers = 9 },
		"in initial block download": func() { b.node.info.Headers, b.node.info.InitialSync = 8, true },
	} {
		behind()
		if _, spender, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 8); !errors.Is(err, rpc.ErrNodeSyncing) || errors.Is(err, ErrNoSpend) || spender != "" {
			t.Errorf("no spend on a node %s: got %q and %v, want rpc.ErrNodeSyncing", name, spender, err)
		}
		if secret, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5); err != nil || secret != b.preimage {
			t.Errorf("the claim on a node %s: got %x and %v, want the secret", name, secret, err)
		}
	}
	b.node.info.Headers, b.node.info.InitialSync = 8, false

	// The block at the tip is read.
	b.node.blocks[7], b.node.blocks[8] = block(), block([3]any{claim.TxID(), b.funded, claim.Inputs[0].WitnessData})
	if secret, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5); err != nil || secret != b.preimage {
		t.Errorf("the claim in the block at the tip: got %x and %v, want the secret", secret, err)
	}
	b.node.blocks[8] = block()

	b.node.blocks[7] = block([3]any{refund.TxID(), b.funded, refund.Inputs[0].WitnessData})
	secret, spender, err = FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5)
	if !errors.Is(err, ErrNoSecret) || spender != refund.TxID() || secret != ([32]byte{}) {
		t.Errorf("a refund: got %x, %q and %v; want ErrNoSecret with the refund's id", secret, spender, err)
	}

	b.node.blocks[7] = strings.Replace(block([3]any{claim.TxID(), b.funded, [][]byte{{0xab}}}), `"ab"`, `"zz"`, 1)
	if _, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 7); err == nil || errors.Is(err, ErrNoSecret) || errors.Is(err, ErrNoSpend) {
		t.Errorf("a witness item that is not hexadecimal: got %v, want an error of its own", err)
	}
	b.node.blocks[7] = "{"
	if _, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 7); err == nil || errors.Is(err, ErrNoSpend) {
		t.Errorf("a block that is not JSON: got %v, want an error of its own", err)
	}
	b.node.blockErr = rpc.ErrUnknownOutcome
	if _, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5); !errors.Is(err, rpc.ErrUnknownOutcome) {
		t.Errorf("a node that fails on a block: got %v, want its error", err)
	}
	b.node.infoErr = rpc.ErrTransient
	if _, _, err := FindSecret(ctx, b.node, b.p, b.funded.TxID, b.funded.Vout, 5); !errors.Is(err, rpc.ErrTransient) {
		t.Errorf("a node that fails: got %v, want its error", err)
	}
}

// *rpc.Client is a Node.
var _ Node = (*rpc.Client)(nil)
