// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const (
	// A txid in display order, as an RPC or block explorer would present it.
	displayTxID = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	// The same value in internal order, which is what the wire format carries.
	internalTxID = "201f1e1d1c1b1a191817161514131211100f0e0d0c0b0a090807060504030201"
)

func testUTXO(txid string, vout uint32, value int64) types.UTXO {
	return types.UTXO{TxID: txid, Vout: vout, Value: value, Address: "ssq1ptest"}
}

func hash32(fill byte) []byte {
	h := make([]byte, 32)
	for i := range h {
		h[i] = fill
	}
	return h
}

// ── AddInput: byte-order reversal ──────────────────────────────────────────
//
// Transaction IDs are displayed in the reverse of their serialized order. Getting
// this backwards produces a transaction that references a nonexistent output: it
// is accepted locally, rejected by the network, and the mistake is invisible in
// any log that prints the txid, because the printed value looks right.

func TestAddInputReversesToInternalByteOrder(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 1000), nil); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	got := hex.EncodeToString(tr.Inputs[0].TxID[:])
	if got != internalTxID {
		t.Errorf("stored txid = %s\n            want %s (display order reversed)", got, internalTxID)
	}
}

func TestAddInputRejectsMalformedTxID(t *testing.T) {
	cases := map[string]string{
		"too short":  "0102030405",
		"too long":   displayTxID + "ff",
		"not hex":    strings.Repeat("zz", 32),
		"odd length": displayTxID[:63],
		"empty":      "",
	}
	for name, txid := range cases {
		t.Run(name, func(t *testing.T) {
			tr := NewTransaction()
			if err := tr.AddInput(testUTXO(txid, 0, 1000), nil); err == nil {
				t.Errorf("accepted malformed txid %q", txid)
			}
			if len(tr.Inputs) != 0 {
				t.Errorf("a rejected input was still appended: %d inputs", len(tr.Inputs))
			}
		})
	}
}

func TestAddInputPreservesUTXOFields(t *testing.T) {
	tr := NewTransaction()
	spk := ScriptP2WPKH(hash32(0xab))
	if err := tr.AddInput(testUTXO(displayTxID, 7, 42_000), spk); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	in := tr.Inputs[0]
	if in.Vout != 7 {
		t.Errorf("Vout = %d, want 7", in.Vout)
	}
	if in.Value != 42_000 {
		t.Errorf("Value = %d, want 42000 (sighash depends on it)", in.Value)
	}
	if in.Sequence != DefaultSequence {
		t.Errorf("Sequence = %#x, want %#x", in.Sequence, DefaultSequence)
	}
	if string(in.ScriptPubKey) != string(spk) {
		t.Error("ScriptPubKey not carried through; BIP143 sighash needs it")
	}
}

// ── TxID ───────────────────────────────────────────────────────────────────

func TestTxIDIsDeterministic(t *testing.T) {
	build := func() *Transaction {
		tr := NewTransaction()
		if err := tr.AddInput(testUTXO(displayTxID, 1, 500_000), ScriptP2WPKH(hash32(0x01))); err != nil {
			t.Fatalf("AddInput: %v", err)
		}
		tr.AddOutput(400_000, ScriptP2WPKH(hash32(0x02)))
		return tr
	}
	a, b := build().TxID(), build().TxID()
	if a != b {
		t.Errorf("TxID not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("TxID length = %d, want 64 hex chars", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("TxID is not hex: %v", err)
	}
}

// The witness is excluded from the txid. That is what makes a signature
// non-malleating with respect to the identifier, and it is the property every
// downstream idempotency key depends on.
func TestTxIDIgnoresWitnessData(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 500_000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	tr.AddOutput(400_000, ScriptP2WPKH(hash32(0x02)))

	before := tr.TxID()
	tr.Inputs[0].WitnessData = [][]byte{hash32(0xde), hash32(0xad)}
	if after := tr.TxID(); after != before {
		t.Errorf("witness data changed the txid: %s -> %s", before, after)
	}
}

func TestTxIDChangesWithOutputValue(t *testing.T) {
	mk := func(value int64) string {
		tr := NewTransaction()
		if err := tr.AddInput(testUTXO(displayTxID, 0, 500_000), nil); err != nil {
			t.Fatalf("AddInput: %v", err)
		}
		tr.AddOutput(value, ScriptP2WPKH(hash32(0x02)))
		return tr.TxID()
	}
	if mk(100_000) == mk(100_001) {
		t.Error("txid did not change when an output value changed")
	}
}

// ── BIP143 sighash ─────────────────────────────────────────────────────────

func TestComputeSigHashRejectsOutOfRangeIndex(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 1000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	for _, idx := range []int{-1, 1, 99} {
		if _, err := tr.ComputeSigHash(idx, SigHashAll); err == nil {
			t.Errorf("index %d accepted; want out-of-range error", idx)
		}
	}
}

// Each input must commit to its own index. If two inputs produced the same
// sighash, a signature authorising one input would authorise the other, so a
// single valid signature could move a second UTXO.
func TestComputeSigHashIsPerInput(t *testing.T) {
	tr := NewTransaction()
	other := "ff" + displayTxID[2:]
	if err := tr.AddInput(testUTXO(displayTxID, 0, 100_000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := tr.AddInput(testUTXO(other, 1, 200_000), ScriptP2WPKH(hash32(0x02))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	tr.AddOutput(250_000, ScriptP2WPKH(hash32(0x03)))

	h0, err := tr.ComputeSigHash(0, SigHashAll)
	if err != nil {
		t.Fatalf("sighash 0: %v", err)
	}
	h1, err := tr.ComputeSigHash(1, SigHashAll)
	if err != nil {
		t.Fatalf("sighash 1: %v", err)
	}
	if len(h0) != 32 {
		t.Fatalf("sighash length = %d, want 32", len(h0))
	}
	if string(h0) == string(h1) {
		t.Error("inputs 0 and 1 share a sighash; one signature would authorise both")
	}
}

func TestComputeSigHashIsDeterministic(t *testing.T) {
	build := func() *Transaction {
		tr := NewTransaction()
		if err := tr.AddInput(testUTXO(displayTxID, 3, 777_000), ScriptP2WPKH(hash32(0x05))); err != nil {
			t.Fatalf("AddInput: %v", err)
		}
		tr.AddOutput(700_000, ScriptP2WPKH(hash32(0x06)))
		return tr
	}
	a, err := build().ComputeSigHash(0, SigHashAll)
	if err != nil {
		t.Fatalf("sighash: %v", err)
	}
	b, err := build().ComputeSigHash(0, SigHashAll)
	if err != nil {
		t.Fatalf("sighash: %v", err)
	}
	if string(a) != string(b) {
		t.Error("sighash not deterministic for identical transactions")
	}
}

// BIP143 commits to the input's value. A sighash that ignored it would let an
// attacker present the same signature against a different-valued prevout.
func TestComputeSigHashCommitsToInputValue(t *testing.T) {
	mk := func(value int64) []byte {
		tr := NewTransaction()
		if err := tr.AddInput(testUTXO(displayTxID, 0, value), ScriptP2WPKH(hash32(0x01))); err != nil {
			t.Fatalf("AddInput: %v", err)
		}
		tr.AddOutput(1_000, ScriptP2WPKH(hash32(0x02)))
		h, err := tr.ComputeSigHash(0, SigHashAll)
		if err != nil {
			t.Fatalf("sighash: %v", err)
		}
		return h
	}
	if string(mk(100_000)) == string(mk(900_000)) {
		t.Error("sighash ignores the input value")
	}
}

// ── Script builders ────────────────────────────────────────────────────────

func TestScriptBuildersProduceCorrectWitnessPrograms(t *testing.T) {
	cases := []struct {
		name    string
		script  []byte
		wantVer byte
	}{
		{"P2WPKH is witness v1", ScriptP2WPKH(hash32(0x11)), 0x51},
		{"authority marker is witness v5", ScriptWitnessV5(hash32(0x22)), 0x55},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.script) != 34 {
				t.Fatalf("length = %d, want 34", len(c.script))
			}
			if c.script[0] != c.wantVer {
				t.Errorf("version byte = %#x, want %#x", c.script[0], c.wantVer)
			}
			if c.script[1] != 0x20 {
				t.Errorf("push opcode = %#x, want 0x20 (32 bytes)", c.script[1])
			}
		})
	}
}

func TestScriptBuildersPanicOnWrongHashLength(t *testing.T) {
	for name, fn := range map[string]func([]byte) []byte{
		"ScriptP2WPKH":    ScriptP2WPKH,
		"ScriptWitnessV5": ScriptWitnessV5,
	} {
		for _, bad := range [][]byte{make([]byte, 20), make([]byte, 31), make([]byte, 33), {}} {
			t.Run(name, func(t *testing.T) {
				defer func() {
					if recover() == nil {
						t.Errorf("%s accepted a %d-byte hash", name, len(bad))
					}
				}()
				_ = fn(bad)
			})
		}
	}
}

// ── Asset typing ───────────────────────────────────────────────────────────

// Asset type is carried by the WITNESS VERSION, not by an output field.
//
// This test previously asserted on TxOutput.AssetType and TxOutput.Visibility, and
// it passed — because it was checking fields the SDK set and then serialized as two
// extra bytes that consensus had already stopped reading. Asserting on a field the
// chain does not see is how a self-consistently wrong format survives a green
// suite. The assertions now look at the script, which is what consensus reads.
func TestOutputAssetTypingFollowsWitnessVersion(t *testing.T) {
	tr := NewTransaction()
	tr.AddOutput(1_000, ScriptP2WPKH(hash32(0x01)))                // native SOQ, v1
	tr.AddOutputUSDSOQ(2_000, ScriptV7USDSOQHolding(hash32(0x02))) // USDSOQ, v7
	tr.AddOutputWitnessV5(hash32(0x03))                            // authority marker, v5

	wantVersionByte := []byte{0x51, 0x57, 0x55} // OP_1, OP_7, OP_5
	for i, want := range wantVersionByte {
		spk := tr.Outputs[i].ScriptPubKey
		if len(spk) != 34 {
			t.Fatalf("output %d script length = %d, want 34", i, len(spk))
		}
		if spk[0] != want {
			t.Errorf("output %d version byte = %#x, want %#x", i, spk[0], want)
		}
	}

	// The authority marker carries no value by design.
	if got := tr.Outputs[2].Value; got != 0 {
		t.Errorf("authority marker Value = %d, want 0", got)
	}
}

// ── Weight and fee ─────────────────────────────────────────────────────────

func TestEstimateWeightGrowsWithInputsAndOutputs(t *testing.T) {
	base := NewTransaction().EstimateWeight()
	if base != TxOverheadWeight {
		t.Errorf("empty weight = %d, want %d", base, TxOverheadWeight)
	}

	oneIn := NewTransaction()
	if err := oneIn.AddInput(testUTXO(displayTxID, 0, 1), nil); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if got, want := oneIn.EstimateWeight(), TxOverheadWeight+EstimatedInputWeight; got != want {
		t.Errorf("one-input weight = %d, want %d", got, want)
	}

	oneOut := NewTransaction()
	oneOut.AddOutput(1, ScriptP2WPKH(hash32(0x01)))
	if got, want := oneOut.EstimateWeight(), TxOverheadWeight+EstimatedOutputWeight; got != want {
		t.Errorf("one-output weight = %d, want %d", got, want)
	}

	// A Dilithium input dominates the weight: signature 2,420 + pubkey 1,312
	// bytes means an input costs far more than an output.
	if EstimatedInputWeight <= EstimatedOutputWeight {
		t.Error("input weight should exceed output weight for post-quantum signatures")
	}
}

func TestEstimateFeeRoundsVsizeUp(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 1), nil); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	vsize := int64((tr.EstimateWeight() + 3) / 4)

	if got := tr.EstimateFee(1); got != vsize {
		t.Errorf("fee at 1 sat/vB = %d, want %d", got, vsize)
	}
	if got := tr.EstimateFee(10); got != vsize*10 {
		t.Errorf("fee at 10 sat/vB = %d, want %d", got, vsize*10)
	}
	if got := tr.EstimateFee(0); got != 0 {
		t.Errorf("fee at 0 sat/vB = %d, want 0", got)
	}
}

// ── Serialization ──────────────────────────────────────────────────────────

func TestSerializeIsStableAndHexMatches(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 500_000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	tr.AddOutput(400_000, ScriptP2WPKH(hash32(0x02)))
	tr.Inputs[0].WitnessData = [][]byte{hash32(0x03), hash32(0x04)}

	raw := tr.Serialize()
	if len(raw) == 0 {
		t.Fatal("Serialize returned no bytes")
	}
	if got := tr.SerializeHex(); got != hex.EncodeToString(raw) {
		t.Error("SerializeHex does not match Serialize")
	}
	if string(tr.Serialize()) != string(raw) {
		t.Error("Serialize is not stable across calls")
	}
	// Version is the first 4 bytes, little-endian.
	if raw[0] != TxVersion || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
		t.Errorf("version prefix = % x, want %02x 00 00 00", raw[:4], TxVersion)
	}
}

func TestNewTransactionDefaults(t *testing.T) {
	tr := NewTransaction()
	if tr.Version != TxVersion {
		t.Errorf("Version = %d, want %d", tr.Version, TxVersion)
	}
	if tr.LockTime != 0 {
		t.Errorf("LockTime = %d, want 0", tr.LockTime)
	}
	if len(tr.Inputs) != 0 || len(tr.Outputs) != 0 {
		t.Error("a new transaction should have no inputs or outputs")
	}
}
