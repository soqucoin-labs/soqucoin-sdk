// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// party is one key of the fixture.
type party struct {
	addr string
	pub  []byte
	kh   [32]byte
}

// world is a contract between a payee and a payer on the "sq" prefix, a
// stranger's key on the same prefix, a key on the stagenet prefix, and one
// signer that holds all four.
type world struct {
	m                                 *keys.Manager
	payee, payer, stranger, elsewhere party
	preimage                          [32]byte
	p                                 Params
	funded                            types.UTXO
}

const (
	testFeeRate     = 1000
	testFundedValue = 5 * types.ShorsPerSOQ
	testLockHeight  = 70_000 // a three-byte script number, as a mainnet lock is
)

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{m: keys.NewManager(filepath.Join(t.TempDir(), "k.json"), "pw")}
	if err := w.m.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	mk := func(hrp string, label byte) party {
		// A seed whose key starts with the node's invalid-key marker derives
		// nothing; the next byte is tried.
		for n := byte(0); ; n++ {
			var seed [keys.SeedSize]byte
			for i := range seed {
				seed[i] = label
			}
			seed[0] = n
			kp, err := keys.FromSeed(hrp, seed)
			if errors.Is(err, keys.ErrInvalidPublicKey) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := w.m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
				t.Fatal(err)
			}
			return party{addr: kp.Address, pub: kp.PublicKey, kh: sha256.Sum256(kp.PublicKey)}
		}
	}
	w.payee, w.payer, w.stranger = mk("sq", 0xa1), mk("sq", 0xa2), mk("sq", 0xa3)
	w.elsewhere = mk("ssq", 0xa4)
	for i := range w.preimage {
		w.preimage[i] = byte(i + 1)
	}
	w.p = Params{
		Hash:         sha256.Sum256(w.preimage[:]),
		PayeeKeyHash: w.payee.kh,
		PayerKeyHash: w.payer.kh,
		LockHeight:   testLockHeight,
	}
	w.funded = types.UTXO{TxID: strings.Repeat("7a", 32), Vout: 1, Value: testFundedValue}
	return w
}

func (w *world) claim(t *testing.T) *tx.Transaction {
	t.Helper()
	tr, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, testFeeRate, w.m, w.payee.addr)
	if err != nil {
		t.Fatalf("BuildClaim: %v", err)
	}
	return tr
}

func (w *world) refund(t *testing.T) *tx.Transaction {
	t.Helper()
	tr, err := BuildRefund(w.p, w.funded, w.payer.addr, testFeeRate, w.m, w.payer.addr)
	if err != nil {
		t.Fatalf("BuildRefund: %v", err)
	}
	return tr
}

// counting wraps a signer and counts what it was asked to sign.
type counting struct {
	inner tx.Signer
	signs int
}

func (c *counting) Sign(a string, d []byte) ([]byte, error) { c.signs++; return c.inner.Sign(a, d) }
func (c *counting) PublicKeyFor(a string) ([]byte, error)   { return c.inner.PublicKeyFor(a) }

// clone copies a transaction deeply enough that a test may change any field.
func clone(t *tx.Transaction) *tx.Transaction {
	c := *t
	c.Inputs = make([]tx.TxInput, len(t.Inputs))
	for i, in := range t.Inputs {
		c.Inputs[i] = in
		c.Inputs[i].ScriptPubKey = append([]byte(nil), in.ScriptPubKey...)
		c.Inputs[i].WitnessData = make([][]byte, len(in.WitnessData))
		for j, item := range in.WitnessData {
			c.Inputs[i].WitnessData[j] = append([]byte{}, item...)
		}
	}
	c.Outputs = make([]tx.TxOutput, len(t.Outputs))
	for i, out := range t.Outputs {
		c.Outputs[i] = tx.TxOutput{Value: out.Value, ScriptPubKey: append([]byte(nil), out.ScriptPubKey...)}
	}
	return &c
}

// handSpend signs a spend of the contract as it stands in tr, with any key of
// the fixture and any witness shape, bypassing the builders. It is how the
// tests make the spends the builders refuse to make.
func (w *world) handSpend(t *testing.T, tr *tx.Transaction, who party, preimage []byte, selector []byte) {
	t.Helper()
	script := mustScript(t, w.p)
	digest, err := tr.ComputeSigHashWithScriptCode(0, script, tx.SigHashAll)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := w.m.Sign(who.addr, digest)
	if err != nil {
		t.Fatal(err)
	}
	items := [][]byte{append(sig, tx.SigHashAll), who.pub}
	if preimage != nil {
		items = append(items, preimage)
	}
	items = append(items, selector, script, append([]byte{0x00}, who.pub...))
	tr.Inputs[0].WitnessData = items
}

func TestClaimHasTheRuledShape(t *testing.T) {
	w := newWorld(t)
	tr := w.claim(t)
	script := mustScript(t, w.p)
	out, _ := w.p.OutputScript()

	if tr.Version != 2 || tr.LockTime != 0 || len(tr.Inputs) != 1 || len(tr.Outputs) != 1 {
		t.Fatalf("version %d, lock time %d, %d inputs, %d outputs", tr.Version, tr.LockTime, len(tr.Inputs), len(tr.Outputs))
	}
	in := tr.Inputs[0]
	if in.Sequence != 0xfffffffe {
		t.Errorf("claim sequence %#x, want 0xfffffffe", in.Sequence)
	}
	if !bytes.Equal(in.ScriptPubKey, out) || in.Value != testFundedValue || in.Vout != 1 {
		t.Errorf("input does not name the funded output")
	}
	if in.Address != w.payee.addr {
		t.Errorf("the input's address is %q, want the signer's", in.Address)
	}
	item := in.WitnessData
	if len(item) != 6 {
		t.Fatalf("%d witness items, want 6", len(item))
	}
	if len(item[0]) != 2421 || item[0][2420] != 0x01 {
		t.Errorf("signature item of %d bytes ending %#02x", len(item[0]), item[0][len(item[0])-1])
	}
	if !bytes.Equal(item[1], w.payee.pub) {
		t.Error("item 2 is not the payee's raw key")
	}
	if !bytes.Equal(item[2], w.preimage[:]) {
		t.Error("item 3 is not the preimage")
	}
	if !bytes.Equal(item[3], []byte{0x01}) {
		t.Errorf("selector %x, want 01", item[3])
	}
	if !bytes.Equal(item[4], script) {
		t.Error("item 5 is not the witnessScript")
	}
	if !bytes.Equal(item[5], append([]byte{0x00}, w.payee.pub...)) {
		t.Error("the trailer is not 0x00 and the payee's key")
	}
	payee := tx.ScriptP2WPKH(w.payee.kh[:])
	if !bytes.Equal(tr.Outputs[0].ScriptPubKey, payee) {
		t.Errorf("output script %x, want the payee's payment script", tr.Outputs[0].ScriptPubKey)
	}
	// The fee is the SDK's rule on the signed size, and the witness is the
	// size the template's item lengths give with a three-byte lock height.
	if fee := testFundedValue - tr.Outputs[0].Value; fee != (tr.VSize()+tx.FeeMarginVBytes)*testFeeRate {
		t.Errorf("fee %d, want (%d + %d) x %d", fee, tr.VSize(), tx.FeeMarginVBytes, testFeeRate)
	}
	if got := witnessBytes(in.WitnessData); got != 5210 {
		t.Errorf("serialized claim witness is %d bytes, want 5,210", got)
	}
	if err := VerifySpend(tr, w.p, testFundedValue); err != nil {
		t.Errorf("VerifySpend refuses the builder's claim: %v", err)
	}
	// The payment verifier keeps refusing the contract's spend.
	if err := tr.VerifyAll(); !errors.Is(err, tx.ErrWitnessForm) {
		t.Errorf("tx.VerifyAll on a claim gave %v, want ErrWitnessForm", err)
	}
}

func TestRefundHasTheRuledShape(t *testing.T) {
	w := newWorld(t)
	tr := w.refund(t)
	in := tr.Inputs[0]
	if tr.Version != 2 || tr.LockTime != testLockHeight || in.Sequence != 0xfffffffd {
		t.Errorf("version %d, lock time %d, sequence %#x; want 2, %d, 0xfffffffd", tr.Version, tr.LockTime, in.Sequence, testLockHeight)
	}
	item := in.WitnessData
	if len(item) != 5 {
		t.Fatalf("%d witness items, want 5", len(item))
	}
	if len(item[0]) != 2421 || item[0][2420] != 0x01 || !bytes.Equal(item[1], w.payer.pub) ||
		len(item[2]) != 0 || !bytes.Equal(item[3], mustScript(t, w.p)) ||
		!bytes.Equal(item[4], append([]byte{0x00}, w.payer.pub...)) {
		t.Error("the refund witness is not signature, the payer's key, the empty selector, the script, the trailer")
	}
	if !bytes.Equal(tr.Outputs[0].ScriptPubKey, tx.ScriptP2WPKH(w.payer.kh[:])) {
		t.Error("the refund does not pay the payer's address")
	}
	if fee := testFundedValue - tr.Outputs[0].Value; fee != (tr.VSize()+tx.FeeMarginVBytes)*testFeeRate {
		t.Errorf("fee %d, want (%d + %d) x %d", fee, tr.VSize(), tx.FeeMarginVBytes, testFeeRate)
	}
	if got := witnessBytes(item); got != 5176 {
		t.Errorf("serialized refund witness is %d bytes, want 5,176", got)
	}
	if err := VerifySpend(tr, w.p, testFundedValue); err != nil {
		t.Errorf("VerifySpend refuses the builder's refund: %v", err)
	}
}

// witnessBytes is the serialized size of one input's witness: the item count
// and each item with its length prefix.
func witnessBytes(items [][]byte) int {
	n := 1
	for _, it := range items {
		switch {
		case len(it) < 0xfd:
			n += 1 + len(it)
		default:
			n += 3 + len(it)
		}
	}
	return n
}

// A claim that the node would refuse must not be signed: once sent it has
// shown the secret. Each case is refused before the signer is asked.
func TestBuildersRefuseBeforeTheSignerIsAsked(t *testing.T) {
	w := newWorld(t)
	wrong := w.preimage
	wrong[0] ^= 1
	v6, err := w.p.Address("sq")
	if err != nil {
		t.Fatal(err)
	}
	bad := w.p
	bad.LockHeight = 0
	small := func(v int64) types.UTXO { u := w.funded; u.Value = v; return u }
	badTxid := w.funded
	badTxid.TxID = "zz"

	claimFee := w.claim(t)
	fee := testFundedValue - claimFee.Outputs[0].Value

	cases := []struct {
		name string
		run  func(s tx.Signer) error
		want error
	}{
		{"claim, wrong preimage", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, wrong, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, ErrPreimage},
		{"claim, the zero preimage", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, [32]byte{}, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, ErrPreimage},
		{"claim signed by the payer", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.payer.addr, testFeeRate, s, w.payer.addr)
			return err
		}, ErrWrongKey},
		{"claim signed by a stranger", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.stranger.addr, testFeeRate, s, w.stranger.addr)
			return err
		}, ErrWrongKey},
		{"refund signed by the payee", func(s tx.Signer) error {
			_, err := BuildRefund(w.p, w.funded, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, ErrWrongKey},
		{"refund signed by a stranger", func(s tx.Signer) error {
			_, err := BuildRefund(w.p, w.funded, w.stranger.addr, testFeeRate, s, w.stranger.addr)
			return err
		}, ErrWrongKey},
		{"claim paid to the contract's own address", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, v6, testFeeRate, s, w.payee.addr)
			return err
		}, ErrDestination},
		{"claim paid to another network", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.elsewhere.addr, testFeeRate, s, w.payee.addr)
			return err
		}, ErrDestination},
		{"claim paid to no address", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, "", testFeeRate, s, w.payee.addr)
			return err
		}, ErrDestination},
		{"refund paid to another network", func(s tx.Signer) error {
			_, err := BuildRefund(w.p, w.funded, w.elsewhere.addr, testFeeRate, s, w.payer.addr)
			return err
		}, ErrDestination},
		{"claim, lock height 0", func(s tx.Signer) error {
			_, err := BuildClaim(bad, w.funded, w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, ErrLockHeight},
		{"refund, lock height 0", func(s tx.Signer) error {
			_, err := BuildRefund(bad, w.funded, w.payer.addr, testFeeRate, s, w.payer.addr)
			return err
		}, ErrLockHeight},
		{"claim, fee rate 0", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, 0, s, w.payee.addr)
			return err
		}, tx.ErrInvalidAmount},
		{"claim, fee rate below 0", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, -1, s, w.payee.addr)
			return err
		}, tx.ErrInvalidAmount},
		{"claim, fee rate above the limit", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, tx.MaxFeeRateShorsPerVB+1, s, w.payee.addr)
			return err
		}, tx.ErrFeeTooHigh},
		{"claim, fee above a tightened limit", func(s tx.Signer) error {
			// At the default limit no allowed rate brings a spend of the
			// contract above it, so the limit is tightened to one shor under
			// this claim's fee, as an operator may set it.
			old := tx.MaxFeeShors
			tx.MaxFeeShors = fee - 1
			defer func() { tx.MaxFeeShors = old }()
			_, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, tx.ErrFeeTooHigh},
		{"claim, fee at a tightened limit, value too small", func(s tx.Signer) error {
			// The limit set to the fee itself lets the fee pass, and the
			// refusal is then the next check's.
			old := tx.MaxFeeShors
			tx.MaxFeeShors = fee
			defer func() { tx.MaxFeeShors = old }()
			_, err := BuildClaim(w.p, small(fee), w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, tx.ErrInsufficientFunds},
		{"claim, value equal to the fee", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, small(fee), w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, tx.ErrInsufficientFunds},
		{"claim, value one shor under the fee and the floor", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, small(fee+tx.DustThreshold-1), w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, tx.ErrBelowDust},
		{"claim, value 0", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, small(0), w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, tx.ErrInvalidAmount},
		{"claim, value above the money range", func(s tx.Signer) error {
			_, err := BuildClaim(w.p, small(tx.MaxMoney+1), w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
			return err
		}, tx.ErrInvalidAmount},
	}
	for _, c := range cases {
		s := &counting{inner: w.m}
		err := c.run(s)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if s.signs != 0 {
			t.Errorf("%s: the signer was asked to sign %d times", c.name, s.signs)
		}
	}

	s := &counting{inner: w.m}
	if tr, err := BuildClaim(w.p, badTxid, w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr); err == nil || tr != nil || s.signs != 0 {
		t.Errorf("a transaction id that is not hex: %v, %d signatures", err, s.signs)
	}

	// The floor itself is accepted: the value that leaves exactly the relay
	// floor after the fee.
	if tr, err := BuildClaim(w.p, small(fee+tx.DustThreshold), w.preimage, w.payee.addr, testFeeRate, w.m, w.payee.addr); err != nil || tr.Outputs[0].Value != tx.DustThreshold {
		t.Errorf("a value that leaves the relay floor: %v", err)
	}
}

// faulty is a signer that does one thing wrong.
type faulty struct {
	inner     tx.Signer
	otherKey  []byte // returned by PublicKeyFor when set
	signAs    string // signs with this address's key when set
	flipDig   bool   // signs another digest
	shortSig  bool
	signErr   error
	pubKeyErr error
}

func (f faulty) Sign(a string, d []byte) ([]byte, error) {
	if f.signErr != nil {
		return nil, f.signErr
	}
	if f.signAs != "" {
		a = f.signAs
	}
	if f.flipDig {
		d = append([]byte(nil), d...)
		d[0] ^= 1
	}
	sig, err := f.inner.Sign(a, d)
	if f.shortSig && err == nil {
		sig = sig[:len(sig)-1]
	}
	return sig, err
}

func (f faulty) PublicKeyFor(a string) ([]byte, error) {
	if f.pubKeyErr != nil {
		return nil, f.pubKeyErr
	}
	if f.otherKey != nil {
		return f.otherKey, nil
	}
	return f.inner.PublicKeyFor(a)
}

func TestBuildersRefuseWhatAFaultySignerReturns(t *testing.T) {
	w := newWorld(t)
	boom := errors.New("signer unavailable")
	cases := []struct {
		name string
		s    faulty
		want error
	}{
		{"a signature over another digest", faulty{inner: w.m, flipDig: true}, tx.ErrSignature},
		{"a signature by another key", faulty{inner: w.m, signAs: w.stranger.addr}, tx.ErrSignature},
		{"a short signature", faulty{inner: w.m, shortSig: true}, ErrSpendForm},
		{"the key with a leading zero", faulty{inner: w.m, otherKey: append([]byte{0x00}, w.payee.pub...)}, ErrKeySize},
		{"another party's key", faulty{inner: w.m, otherKey: w.stranger.pub}, ErrWrongKey},
		{"an error from Sign", faulty{inner: w.m, signErr: boom}, boom},
		{"an error from PublicKeyFor", faulty{inner: w.m, pubKeyErr: boom}, boom},
	}
	for _, c := range cases {
		tr, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, testFeeRate, c.s, w.payee.addr)
		if !errors.Is(err, c.want) || tr != nil {
			t.Errorf("claim, %s: got %v (transaction returned: %v), want %v", c.name, err, tr != nil, c.want)
		}
	}
	// A signer that holds the payee's key under a name that is no address:
	// the builder has no network to hold the destination to, and refuses.
	if tr, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, testFeeRate,
		faulty{inner: w.m, otherKey: w.payee.pub}, "hot-wallet-key-1"); !errors.Is(err, ErrDestination) || tr != nil {
		t.Errorf("a signer address that is no address: got %v, want ErrDestination", err)
	}
	tr, err := BuildRefund(w.p, w.funded, w.payer.addr, testFeeRate, faulty{inner: w.m, flipDig: true}, w.payer.addr)
	if !errors.Is(err, tx.ErrSignature) || tr != nil {
		t.Errorf("refund, a signature over another digest: got %v", err)
	}
}

// One change at a time to a spend the builder made. Each is a transaction the
// node refuses, or one it accepts in a form this package does not write.
func TestVerifySpendRefusesEachChange(t *testing.T) {
	w := newWorld(t)
	otherHeight := w.p
	otherHeight.LockHeight++
	otherScript := mustScript(t, otherHeight)
	item := func(i int, f func([]byte) []byte) func(*tx.Transaction) {
		return func(tr *tx.Transaction) { tr.Inputs[0].WitnessData[i] = f(tr.Inputs[0].WitnessData[i]) }
	}
	set := func(b ...byte) func([]byte) []byte { return func([]byte) []byte { return b } }
	hashType := func(b byte) func(*tx.Transaction) {
		return item(0, func(s []byte) []byte { s[len(s)-1] = b; return s })
	}
	both := []struct {
		name   string
		change func(*tx.Transaction)
		want   error
	}{
		{"an extra item under the signature", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData = append([][]byte{{0x01}}, tr.Inputs[0].WitnessData...)
		}, ErrSpendForm},
		{"an extra item above the selector", func(tr *tx.Transaction) {
			wd := tr.Inputs[0].WitnessData
			n := len(wd) - 2
			tr.Inputs[0].WitnessData = append(append(append([][]byte{}, wd[:n]...), []byte{0x01}), wd[n:]...)
		}, ErrSpendForm},
		{"no witness", func(tr *tx.Transaction) { tr.Inputs[0].WitnessData = nil }, ErrSpendForm},
		{"the trailer missing", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData = tr.Inputs[0].WitnessData[:len(tr.Inputs[0].WitnessData)-1]
		}, ErrSpendForm},
		{"the key with a leading zero", item(1, func(k []byte) []byte { return append([]byte{0x00}, k...) }), ErrSpendForm},
		{"a signature item one byte short", item(0, func(s []byte) []byte { return s[1:] }), ErrSpendForm},
		{"a signature item with no hash type", item(0, func(s []byte) []byte { return s[:len(s)-1] }), ErrSpendForm},
		{"a signature item one byte longer", item(0, func(s []byte) []byte { return append(s, 0x01) }), ErrSpendForm},
		{"the key with a leading zero, and a trailer made for it", func(tr *tx.Transaction) {
			wd := tr.Inputs[0].WitnessData
			wd[1] = append([]byte{0x00}, wd[1]...)
			wd[len(wd)-1] = append([]byte{0x00}, wd[1]...)
		}, ErrSpendForm},
		{"hash type 0x00", hashType(0x00), tx.ErrHashType},
		{"hash type NONE", hashType(0x02), tx.ErrHashType},
		{"hash type SINGLE", hashType(0x03), tx.ErrHashType},
		{"hash type ANYPREVOUT", hashType(0x41), tx.ErrHashType},
		{"hash type ANYPREVOUTANYSCRIPT", hashType(0x42), tx.ErrHashType},
		{"hash type ALL with ANYONECANPAY", hashType(0x81), tx.ErrHashType},
		{"one bit of the signature", item(0, func(s []byte) []byte { s[100] ^= 1; return s }), tx.ErrSignature},
		{"the witnessScript of another height", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData[len(tr.Inputs[0].WitnessData)-2] = otherScript
		}, ErrSpendForm},
		{"a trailer with another key", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData[len(tr.Inputs[0].WitnessData)-1] = append([]byte{0x00}, w.stranger.pub...)
		}, ErrSpendForm},
		{"a trailer that starts 0x01", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData[len(tr.Inputs[0].WitnessData)-1][0] = 0x01
		}, ErrSpendForm},
		{"a one-byte trailer", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData[len(tr.Inputs[0].WitnessData)-1] = []byte{0x00}
		}, ErrSpendForm},
		{"an empty trailer", func(tr *tx.Transaction) {
			tr.Inputs[0].WitnessData[len(tr.Inputs[0].WitnessData)-1] = []byte{}
		}, ErrSpendForm},
		{"a stranger's key and trailer", func(tr *tx.Transaction) {
			wd := tr.Inputs[0].WitnessData
			wd[1], wd[len(wd)-1] = w.stranger.pub, append([]byte{0x00}, w.stranger.pub...)
		}, ErrWrongKey},
		{"sequence 0xffffffff", func(tr *tx.Transaction) { tr.Inputs[0].Sequence = 0xffffffff }, ErrSpendForm},
		{"sequence 0", func(tr *tx.Transaction) { tr.Inputs[0].Sequence = 0 }, ErrSpendForm},
		{"version 1", func(tr *tx.Transaction) { tr.Version = 1 }, ErrSpendForm},
		{"version 3", func(tr *tx.Transaction) { tr.Version = 3 }, ErrSpendForm},
		{"a second input", func(tr *tx.Transaction) { tr.Inputs = append(tr.Inputs, tr.Inputs[0]) }, ErrSpendForm},
		{"no input", func(tr *tx.Transaction) { tr.Inputs = nil }, ErrSpendForm},
		{"an input that names a payment output", func(tr *tx.Transaction) {
			tr.Inputs[0].ScriptPubKey = tx.ScriptP2WPKH(w.payee.kh[:])
		}, ErrSpendForm},
		{"an input that names another contract", func(tr *tx.Transaction) {
			tr.Inputs[0].ScriptPubKey, _ = otherHeight.OutputScript()
		}, ErrSpendForm},
		{"another value in the input", func(tr *tx.Transaction) { tr.Inputs[0].Value++ }, ErrSpendForm},
		{"another outpoint index", func(tr *tx.Transaction) { tr.Inputs[0].Vout++ }, tx.ErrSignature},
		{"another outpoint transaction", func(tr *tx.Transaction) { tr.Inputs[0].TxID[0] ^= 1 }, tx.ErrSignature},
		{"the output's value changed", func(tr *tx.Transaction) { tr.Outputs[0].Value-- }, tx.ErrSignature},
		{"the output's script changed", func(tr *tx.Transaction) {
			tr.Outputs[0].ScriptPubKey = tx.ScriptP2WPKH(w.stranger.kh[:])
		}, tx.ErrSignature},
		{"a second output", func(tr *tx.Transaction) { tr.AddOutput(1, tx.ScriptP2WPKH(w.stranger.kh[:])) }, tx.ErrSignature},
		{"no output", func(tr *tx.Transaction) { tr.Outputs = nil }, tx.ErrSignature},
	}
	claimOnly := []struct {
		name   string
		change func(*tx.Transaction)
		want   error
	}{
		{"32 other bytes as the preimage", item(2, func(p []byte) []byte { p[0] ^= 1; return p }), ErrPreimage},
		{"a 33-byte preimage item", item(2, func(p []byte) []byte { return append(p, 0x00) }), ErrSpendForm},
		{"a 31-byte preimage item", item(2, func(p []byte) []byte { return p[:31] }), ErrSpendForm},
		{"an empty preimage item", item(2, set()), ErrSpendForm},
		{"selector 0x02", item(3, set(0x02)), ErrSpendForm},
		{"selector 0x01 0x00", item(3, set(0x01, 0x00)), ErrSpendForm},
		{"the empty selector", item(3, set()), ErrSpendForm},
		{"the refund's sequence", func(tr *tx.Transaction) { tr.Inputs[0].Sequence = RefundSequence }, ErrSpendForm},
		{"lock time 1", func(tr *tx.Transaction) { tr.LockTime = 1 }, ErrSpendForm},
		{"the contract's lock time", func(tr *tx.Transaction) { tr.LockTime = testLockHeight }, ErrSpendForm},
		{"the preimage item dropped", func(tr *tx.Transaction) {
			wd := tr.Inputs[0].WitnessData
			tr.Inputs[0].WitnessData = append(append([][]byte{}, wd[:2]...), wd[3:]...)
		}, ErrSpendForm},
	}
	refundOnly := []struct {
		name   string
		change func(*tx.Transaction)
		want   error
	}{
		{"selector 0x01", item(2, set(0x01)), ErrSpendForm},
		{"selector 0x00", item(2, set(0x00)), ErrSpendForm},
		{"the claim's sequence", func(tr *tx.Transaction) { tr.Inputs[0].Sequence = ClaimSequence }, ErrSpendForm},
		{"lock time one below the height", func(tr *tx.Transaction) { tr.LockTime = testLockHeight - 1 }, ErrSpendForm},
		{"lock time one above the height", func(tr *tx.Transaction) { tr.LockTime = testLockHeight + 1 }, ErrSpendForm},
		{"lock time 0", func(tr *tx.Transaction) { tr.LockTime = 0 }, ErrSpendForm},
	}

	run := func(kind string, good *tx.Transaction, name string, change func(*tx.Transaction), want error) {
		tr := clone(good)
		change(tr)
		err := VerifySpend(tr, w.p, testFundedValue)
		if !errors.Is(err, want) {
			t.Errorf("%s, %s: got %v, want %v", kind, name, err, want)
		}
		// The node takes several other hash types, so the refusal of one is
		// also a refusal of the form.
		if want == tx.ErrHashType && !errors.Is(err, ErrSpendForm) {
			t.Errorf("%s, %s: %v is not ErrSpendForm", kind, name, err)
		}
	}
	claim, refund := w.claim(t), w.refund(t)
	for _, c := range both {
		run("claim", claim, c.name, c.change, c.want)
		run("refund", refund, c.name, c.change, c.want)
	}
	for _, c := range claimOnly {
		run("claim", claim, c.name, c.change, c.want)
	}
	for _, c := range refundOnly {
		run("refund", refund, c.name, c.change, c.want)
	}

	// The unchanged spends still pass, so the table changed copies only.
	if err := VerifySpend(claim, w.p, testFundedValue); err != nil {
		t.Errorf("the claim after the table: %v", err)
	}
	if err := VerifySpend(refund, w.p, testFundedValue); err != nil {
		t.Errorf("the refund after the table: %v", err)
	}
	// The value is an argument: a caller that states another value is refused
	// whatever the transaction carries.
	if err := VerifySpend(claim, w.p, testFundedValue+1); !errors.Is(err, ErrSpendForm) {
		t.Errorf("another value as the argument: %v", err)
	}
	if err := VerifySpend(nil, w.p, testFundedValue); !errors.Is(err, ErrSpendForm) {
		t.Errorf("a nil transaction: %v", err)
	}
	// Another contract's parameters refuse the spend, and invalid ones are
	// reported as such.
	if err := VerifySpend(claim, otherHeight, testFundedValue); !errors.Is(err, ErrSpendForm) {
		t.Errorf("the claim against another contract: %v", err)
	}
	bad := w.p
	bad.LockHeight = 0
	if err := VerifySpend(claim, bad, testFundedValue); !errors.Is(err, ErrLockHeight) {
		t.Errorf("parameters with lock height 0: %v", err)
	}
}

// Spends signed correctly by the wrong party, in the right shape: the payer,
// who may know the secret, on the claim branch; the payee on the refund
// branch; a stranger on both. The signature is good and the key is not the
// branch's.
func TestVerifySpendRefusesTheOtherPartysBranch(t *testing.T) {
	w := newWorld(t)
	claim, refund := w.claim(t), w.refund(t)
	for _, who := range []party{w.payer, w.stranger} {
		tr := clone(claim)
		w.handSpend(t, tr, who, w.preimage[:], []byte{0x01})
		if err := VerifySpend(tr, w.p, testFundedValue); !errors.Is(err, ErrWrongKey) {
			t.Errorf("a claim signed by %s: %v, want ErrWrongKey", who.addr, err)
		}
	}
	for _, who := range []party{w.payee, w.stranger} {
		tr := clone(refund)
		w.handSpend(t, tr, who, nil, []byte{})
		if err := VerifySpend(tr, w.p, testFundedValue); !errors.Is(err, ErrWrongKey) {
			t.Errorf("a refund signed by %s: %v, want ErrWrongKey", who.addr, err)
		}
	}
	// The helper itself makes spends that pass when the party is the right
	// one, so the refusals above are about the key.
	tr := clone(claim)
	w.handSpend(t, tr, w.payee, w.preimage[:], []byte{0x01})
	if err := VerifySpend(tr, w.p, testFundedValue); err != nil {
		t.Errorf("a hand-made claim by the payee: %v", err)
	}
	tr = clone(refund)
	w.handSpend(t, tr, w.payer, nil, []byte{})
	if err := VerifySpend(tr, w.p, testFundedValue); err != nil {
		t.Errorf("a hand-made refund by the payer: %v", err)
	}
}

// A signature made over the output script, the script code of a payment
// spend, does not verify: the contract's spend signs over the witnessScript.
func TestVerifySpendRefusesASignatureOverTheOutputScript(t *testing.T) {
	w := newWorld(t)
	tr := clone(w.claim(t))
	digest, err := tr.ComputeSigHash(0, tx.SigHashAll)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := w.m.Sign(w.payee.addr, digest)
	if err != nil {
		t.Fatal(err)
	}
	tr.Inputs[0].WitnessData[0] = append(sig, tx.SigHashAll)
	if err := VerifySpend(tr, w.p, testFundedValue); !errors.Is(err, tx.ErrSignature) {
		t.Errorf("got %v, want tx.ErrSignature", err)
	}
}

// A contract that commits to a key the node treats as invalid (first byte
// 0xff) cannot be spent on that branch; the verifier reports the key.
func TestVerifySpendRefusesTheInvalidKeyMarker(t *testing.T) {
	w := newWorld(t)
	invalid := append([]byte{0xff}, w.payee.pub[1:]...)
	p := w.p
	p.PayeeKeyHash = sha256.Sum256(invalid)
	tr := clone(w.claim(t))
	tr.Inputs[0].ScriptPubKey, _ = p.OutputScript()
	wd := tr.Inputs[0].WitnessData
	wd[1], wd[4], wd[5] = invalid, mustScript(t, p), append([]byte{0x00}, invalid...)
	err := VerifySpend(tr, p, testFundedValue)
	if !errors.Is(err, ErrSpendForm) || !errors.Is(err, keys.ErrInvalidPublicKey) {
		t.Errorf("got %v, want ErrSpendForm wrapping keys.ErrInvalidPublicKey", err)
	}
}

// The output pays the destination named and the fee is taken at the rate
// asked: a third party's address, which is not the signer's, and two rates
// that are not the recommended one.
func TestSpendsPayTheDestinationAtTheRateAsked(t *testing.T) {
	w := newWorld(t)
	stranger := tx.ScriptP2WPKH(w.stranger.kh[:])

	claim, err := BuildClaim(w.p, w.funded, w.preimage, w.stranger.addr, 3*testFeeRate, w.m, w.payee.addr)
	if err != nil {
		t.Fatalf("BuildClaim: %v", err)
	}
	refund, err := BuildRefund(w.p, w.funded, w.stranger.addr, 7, w.m, w.payer.addr)
	if err != nil {
		t.Fatalf("BuildRefund: %v", err)
	}
	for _, c := range []struct {
		name string
		tr   *tx.Transaction
		rate int64
	}{{"claim", claim, 3 * testFeeRate}, {"refund", refund, 7}} {
		if len(c.tr.Outputs) != 1 || !bytes.Equal(c.tr.Outputs[0].ScriptPubKey, stranger) {
			t.Errorf("%s: the output does not pay the destination named", c.name)
		}
		want := (c.tr.VSize() + tx.FeeMarginVBytes) * c.rate
		if fee := testFundedValue - c.tr.Outputs[0].Value; fee != want {
			t.Errorf("%s at %d shors/vB: fee %d, want %d", c.name, c.rate, fee, want)
		}
		if err := VerifySpend(c.tr, w.p, testFundedValue); err != nil {
			t.Errorf("%s: VerifySpend: %v", c.name, err)
		}
	}
}

// A key that begins 0xff is the node's invalid-key marker, and the node
// refuses every spend under it. A builder whose signer returns such a key
// refuses before the signer is asked, whatever the contract commits to.
func TestBuildersRefuseTheInvalidKeyMarkerBeforeTheSignerIsAsked(t *testing.T) {
	w := newWorld(t)
	invalid := append([]byte{0xff}, w.payee.pub[1:]...)
	p := w.p
	p.PayeeKeyHash = sha256.Sum256(invalid)
	p.PayerKeyHash = p.PayeeKeyHash

	s := &counting{inner: faulty{inner: w.m, otherKey: invalid}}
	tr, err := BuildClaim(p, w.funded, w.preimage, w.payee.addr, testFeeRate, s, w.payee.addr)
	if !errors.Is(err, keys.ErrInvalidPublicKey) || tr != nil || s.signs != 0 {
		t.Errorf("claim: got %v (transaction returned: %v), %d signatures; want keys.ErrInvalidPublicKey and none", err, tr != nil, s.signs)
	}
	tr, err = BuildRefund(p, w.funded, w.payer.addr, testFeeRate, s, w.payer.addr)
	if !errors.Is(err, keys.ErrInvalidPublicKey) || tr != nil || s.signs != 0 {
		t.Errorf("refund: got %v (transaction returned: %v), %d signatures; want keys.ErrInvalidPublicKey and none", err, tr != nil, s.signs)
	}
}

// sharing is a signer that hands out one slice of its own for the key.
type sharing struct {
	inner tx.Signer
	key   []byte
}

func (s sharing) Sign(a string, d []byte) ([]byte, error) { return s.inner.Sign(a, d) }
func (s sharing) PublicKeyFor(string) ([]byte, error)     { return s.key, nil }

// The key item of a returned transaction is the transaction's own bytes: a
// change to the signer's slice does not change the transaction, and a change
// to the key item does not change the signer's key.
func TestSpendHoldsItsOwnCopyOfTheKey(t *testing.T) {
	w := newWorld(t)
	own := append([]byte(nil), w.payee.pub...)
	tr, err := BuildClaim(w.p, w.funded, w.preimage, w.payee.addr, testFeeRate, sharing{inner: w.m, key: own}, w.payee.addr)
	if err != nil {
		t.Fatalf("BuildClaim: %v", err)
	}
	own[5] ^= 1
	if !bytes.Equal(tr.Inputs[0].WitnessData[1], w.payee.pub) {
		t.Error("the key item changed with the signer's slice")
	}
	if err := VerifySpend(tr, w.p, testFundedValue); err != nil {
		t.Errorf("after the signer's slice changed: %v", err)
	}
	own[5] ^= 1
	tr.Inputs[0].WitnessData[1][7] ^= 1
	if !bytes.Equal(own, w.payee.pub) {
		t.Error("the signer's slice changed with the key item")
	}
}

// A refusal names a length or a field and never prints a witness item. Two
// hand-assembled shapes carry the secret where the selector is read: six
// items with the secret as items 3 and 4, and the refund's five with the
// secret as item 3. The whole text of the refusal is compared, so the item
// is in it in no encoding.
func TestVerifySpendErrorsDoNotPrintTheSecret(t *testing.T) {
	w := newWorld(t)
	secret := hex.EncodeToString(w.preimage[:])
	want := ErrSpendForm.Error() + ": selector of 32 bytes"

	six := clone(w.claim(t))
	six.Inputs[0].WitnessData[3] = append([]byte(nil), w.preimage[:]...)
	five := clone(w.refund(t))
	five.Inputs[0].WitnessData[2] = append([]byte(nil), w.preimage[:]...)
	for name, tr := range map[string]*tx.Transaction{"six items": six, "five items": five} {
		err := VerifySpend(tr, w.p, testFundedValue)
		if !errors.Is(err, ErrSpendForm) {
			t.Errorf("%s: got %v, want ErrSpendForm", name, err)
			continue
		}
		if strings.Contains(strings.ToLower(err.Error()), secret) {
			t.Errorf("%s: the error holds the secret: %v", name, err)
		}
		if err.Error() != want {
			t.Errorf("%s: the refusal reads %q, want %q", name, err, want)
		}
	}
}
