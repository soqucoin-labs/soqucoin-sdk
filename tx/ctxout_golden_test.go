// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// CTxOut serialization golden vectors, pinned byte-identically to the C++ node.
//
// WHY THIS FILE EXISTS
//
// Until v0.3.1 this SDK serialized two extra bytes after every scriptPubKey
// (nVisibility, nAssetType). CTxOut migration Phase 4 removed those bytes from
// consensus — CTxOut is now standard Bitcoin, and asset and visibility follow the
// WITNESS VERSION instead. So every transaction this package produced was
// malformed, every txid it computed was wrong, and because the same mistake was in
// serializeAllOutputs, every BIP143 sighash was computed over the wrong preimage.
//
// The package had 60% statement coverage and a green suite at the time. Those
// tests checked the serializer against ITSELF — determinism, byte-order reversal,
// per-input sighash separation — which cannot detect a format that is
// self-consistently wrong.
//
// The fix for that class of blind spot is an EXTERNAL reference. The vectors below
// come from the production reference implementation
// (soq-signer/internal/txbuilder/ctxout_matrix_test.go), which is itself pinned to
// the node's own ctxout_format_matrix_tests.cpp. If consensus changes shape again,
// these fail.

package tx

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// serializeOneOutput mirrors the single-output encoding used inside Serialize,
// TxID and serializeAllOutputs. All three must agree with this and with the node.
func serializeOneOutput(o TxOutput) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.Value)
	writeVarInt(&buf, uint64(len(o.ScriptPubKey)))
	buf.Write(o.ScriptPubKey)
	return buf.Bytes()
}

// The canonical fixture shared with the node and with soq-signer.
const (
	goldenValue     = int64(12345678)
	goldenScriptHex = "51" // OP_TRUE
	// value(8 LE) + varint(1) + 0x51
	goldenTxOutHex = "4e61bc00000000000151"
	// The pre-Phase-4 extended form. If this ever reappears the bug is back.
	prePhase4TxOutHex = "4e61bc000000000001510101"
)

func TestCTxOutGoldenVector(t *testing.T) {
	out := TxOutput{Value: goldenValue, ScriptPubKey: []byte{0x51}}
	got := hex.EncodeToString(serializeOneOutput(out))

	if got != goldenTxOutHex {
		t.Errorf("CTxOut encoding = %s\n                 want %s", got, goldenTxOutHex)
	}
	if got == prePhase4TxOutHex {
		t.Error("REGRESSION: the pre-Phase-4 extension bytes are back; every transaction " +
			"this package builds would be malformed and every txid wrong")
	}
	if n := len(serializeOneOutput(out)); n != 10 {
		t.Errorf("CTxOut length = %d bytes, want 10 (8 value + 1 varint + 1 script)", n)
	}
}

// A whole-transaction serialization must contain no trailing per-output bytes.
// Checked by arithmetic rather than by eye, so it cannot drift.
func TestSerializeHasNoPerOutputExtensionBytes(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 500_000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}

	oneOut := len(tr.Serialize())
	tr.AddOutput(400_000, ScriptP2WPKH(hash32(0x02)))
	twoOut := len(tr.Serialize())

	// An added output costs exactly 8 (value) + 1 (varint) + 34 (script) = 43 bytes.
	// 45 would mean the two extension bytes are being written again.
	const wantDelta = 8 + 1 + 34
	if delta := twoOut - oneOut; delta != wantDelta {
		t.Errorf("adding one output grew the transaction by %d bytes, want %d%s",
			delta, wantDelta,
			map[bool]string{true: " (extension bytes are back)"}[delta == wantDelta+2])
	}
}

// The three sites that serialize outputs must agree. They drifted independently
// before, and serializeAllOutputs is the one that feeds BIP143 — so a mismatch
// there produces signatures the network rejects while everything looks locally
// consistent.
func TestAllThreeOutputSerializersAgree(t *testing.T) {
	outs := []TxOutput{
		{Value: goldenValue, ScriptPubKey: []byte{0x51}},
		{Value: 999, ScriptPubKey: ScriptP2WPKH(hash32(0x07))},
	}

	// serializeAllOutputs is the BIP143 hashOutputs input.
	var expected bytes.Buffer
	for _, o := range outs {
		expected.Write(serializeOneOutput(o))
	}
	if got := serializeAllOutputs(outs); !bytes.Equal(got, expected.Bytes()) {
		t.Errorf("serializeAllOutputs disagrees with the golden encoding\n got  %x\n want %x",
			got, expected.Bytes())
	}

	// Serialize and TxID must embed the same output bytes. Locate them by
	// searching for the golden encoding rather than by offset arithmetic.
	tr := NewTransaction()
	if err := tr.AddInput(testUTXO(displayTxID, 0, 1_000_000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	tr.Outputs = outs

	golden := expected.Bytes()
	if !bytes.Contains(tr.Serialize(), golden) {
		t.Error("Serialize does not contain the golden output encoding")
	}
}

// USDSOQ is a witness version now, not a byte. Passing a non-v7 script must fail
// loudly: silently producing a native SOQ output where the caller asked for USDSOQ
// is a value-misattribution bug that no local test would notice.
func TestUSDSOQIsExpressedAsWitnessV7(t *testing.T) {
	script := ScriptV7USDSOQHolding(hash32(0x33))
	if len(script) != 34 {
		t.Fatalf("v7 script length = %d, want 34", len(script))
	}
	if script[0] != 0x57 {
		t.Errorf("version byte = %#x, want 0x57 (OP_7)", script[0])
	}
	if script[1] != 0x20 {
		t.Errorf("push opcode = %#x, want 0x20", script[1])
	}

	tr := NewTransaction()
	tr.AddOutputUSDSOQ(5_000, script)
	if len(tr.Outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(tr.Outputs))
	}
	// Encoding must be the plain byte-less form; the asset lives in the script.
	enc := serializeOneOutput(tr.Outputs[0])
	if !bytes.Equal(enc[9:], script) {
		t.Errorf("USDSOQ output script = %x, want %x", enc[9:], script)
	}

	t.Run("rejects a non-v7 script", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("AddOutputUSDSOQ accepted a native-SOQ script; the output would " +
					"have been native SOQ while the caller believed it was USDSOQ")
			}
		}()
		tr2 := NewTransaction()
		tr2.AddOutputUSDSOQ(5_000, ScriptP2WPKH(hash32(0x44)))
	})
}

func TestScriptV7PanicsOnWrongHashLength(t *testing.T) {
	for _, bad := range [][]byte{make([]byte, 20), make([]byte, 31), make([]byte, 33), {}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ScriptV7USDSOQHolding accepted a %d-byte hash", len(bad))
				}
			}()
			_ = ScriptV7USDSOQHolding(bad)
		}()
	}
}
