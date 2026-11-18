// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// independentDigest is a second writing of the BIP 143 digest in its
// SIGHASH_ALL form, assembled field by field from the transaction's public
// fields with no call into the builder's serializers, so the tests below
// compare two implementations and not one with itself. The script code's
// length is one byte under 253 bytes and 0xfd with two little-endian bytes
// from 253 to 65,535; the tests use none longer. hashType is the last field
// of the preimage.
func independentDigest(t *testing.T, tr *Transaction, index int, scriptCode []byte, hashType uint32) []byte {
	t.Helper()
	if len(scriptCode) > 0xffff {
		t.Fatalf("independentDigest: script code of %d bytes needs a longer length prefix", len(scriptCode))
	}
	dsha := func(b []byte) []byte {
		first := sha256.Sum256(b)
		second := sha256.Sum256(first[:])
		return second[:]
	}
	le32 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	le64 := func(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }

	var prevouts, sequences, outputs []byte
	for _, in := range tr.Inputs {
		prevouts = append(prevouts, in.TxID[:]...)
		prevouts = append(prevouts, le32(in.Vout)...)
		sequences = append(sequences, le32(in.Sequence)...)
	}
	for _, out := range tr.Outputs {
		if len(out.ScriptPubKey) >= 0xfd {
			t.Fatalf("independentDigest: output script of %d bytes", len(out.ScriptPubKey))
		}
		outputs = append(outputs, le64(uint64(out.Value))...)
		outputs = append(outputs, byte(len(out.ScriptPubKey)))
		outputs = append(outputs, out.ScriptPubKey...)
	}
	in := tr.Inputs[index]
	var pre []byte
	pre = append(pre, le32(tr.Version)...)
	pre = append(pre, dsha(prevouts)...)
	pre = append(pre, dsha(sequences)...)
	pre = append(pre, in.TxID[:]...)
	pre = append(pre, le32(in.Vout)...)
	if len(scriptCode) < 0xfd {
		pre = append(pre, byte(len(scriptCode)))
	} else {
		pre = append(pre, 0xfd, byte(len(scriptCode)), byte(len(scriptCode)>>8))
	}
	pre = append(pre, scriptCode...)
	pre = append(pre, le64(uint64(in.Value))...)
	pre = append(pre, le32(in.Sequence)...)
	pre = append(pre, dsha(outputs)...)
	pre = append(pre, le32(tr.LockTime)...)
	pre = append(pre, le32(hashType)...)
	return dsha(pre)
}

// The digest over a named script code, given the input's own output script,
// is the digest the node handed its signer for that input. This is the path
// every version 1 spend signs through once ComputeSigHash calls the function.
func TestScriptCodeDigestOverTheOutputScriptIsTheNodeDigest(t *testing.T) {
	v := loadNodeVector(t)
	tr := buildNodeVector(t, v)
	for i, want := range v.Digests {
		got, err := tr.ComputeSigHashWithScriptCode(i, tr.Inputs[i].ScriptPubKey, SigHashAll)
		if err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
		if hex.EncodeToString(got) != want {
			t.Errorf("input %d: digest over the output script %x, node computed %s", i, got, want)
		}
		viaInput, err := tr.ComputeSigHash(i, SigHashAll)
		if err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
		if !bytes.Equal(got, viaInput) {
			t.Errorf("input %d: ComputeSigHash %x differs from the digest over the output script %x", i, viaInput, got)
		}
	}
}

// The script code the caller names is the one committed to: a witnessScript
// of 116 to 119 bytes (the lengths of the swap contract) gives the digest of
// the independent writing, and no two script codes below give one digest.
// 252 and 253 bytes are the two sides of the length prefix's change from one
// byte to three, and 300 is a length whose low byte alone is another length.
func TestScriptCodeDigestCommitsToTheNamedScriptCode(t *testing.T) {
	v := loadNodeVector(t)
	tr := buildNodeVector(t, v)

	codes := [][]byte{
		tr.Inputs[0].ScriptPubKey,
		{},
		{0x51},
		bytes.Repeat([]byte{0x63}, 116),
		bytes.Repeat([]byte{0x63}, 117),
		bytes.Repeat([]byte{0x63}, 119),
		append(bytes.Repeat([]byte{0x63}, 116), 0x68),
		bytes.Repeat([]byte{0x63}, 252),
		bytes.Repeat([]byte{0x63}, 253),
		bytes.Repeat([]byte{0x63}, 300),
	}
	seen := map[string]int{}
	for n, code := range codes {
		for i := range tr.Inputs {
			got, err := tr.ComputeSigHashWithScriptCode(i, code, SigHashAll)
			if err != nil {
				t.Fatalf("script code %d, input %d: %v", n, i, err)
			}
			if want := independentDigest(t, tr, i, code, SigHashAll); !bytes.Equal(got, want) {
				t.Errorf("script code %d (%d bytes), input %d: digest %x, independent writing %x",
					n, len(code), i, got, want)
			}
			if i == 0 {
				if prev, dup := seen[string(got)]; dup {
					t.Errorf("script codes %d and %d give one digest %x", prev, n, got)
				}
				seen[string(got)] = n
			}
		}
	}
}

// The hash type the caller names is the last field of the preimage: the
// digest at 0x00 and at 0x04 is the independent writing's for that value and
// is not the digest at SIGHASH_ALL. The preimage keeps the SIGHASH_ALL form
// at every value, which is the node's form for these two.
func TestScriptCodeDigestCommitsToTheHashType(t *testing.T) {
	v := loadNodeVector(t)
	tr := buildNodeVector(t, v)
	code := bytes.Repeat([]byte{0x63}, 117)
	all, err := tr.ComputeSigHashWithScriptCode(0, code, SigHashAll)
	if err != nil {
		t.Fatal(err)
	}
	for _, hashType := range []uint32{0x00, 0x04} {
		got, err := tr.ComputeSigHashWithScriptCode(0, code, hashType)
		if err != nil {
			t.Fatalf("hash type %#02x: %v", hashType, err)
		}
		if want := independentDigest(t, tr, 0, code, hashType); !bytes.Equal(got, want) {
			t.Errorf("hash type %#02x: digest %x, independent writing %x", hashType, got, want)
		}
		if bytes.Equal(got, all) {
			t.Errorf("hash type %#02x gives the digest of SIGHASH_ALL", hashType)
		}
		viaInput, err := tr.ComputeSigHash(0, hashType)
		if err != nil {
			t.Fatalf("hash type %#02x: %v", hashType, err)
		}
		if want := independentDigest(t, tr, 0, tr.Inputs[0].ScriptPubKey, hashType); !bytes.Equal(viaInput, want) {
			t.Errorf("hash type %#02x: ComputeSigHash %x, independent writing %x", hashType, viaInput, want)
		}
	}
}

// The digest reads the script code and leaves the transaction as it was.
func TestScriptCodeDigestDoesNotChangeTheTransaction(t *testing.T) {
	v := loadNodeVector(t)
	tr := buildNodeVector(t, v)
	before := tr.SerializeHex()
	spk := append([]byte(nil), tr.Inputs[0].ScriptPubKey...)
	if _, err := tr.ComputeSigHashWithScriptCode(0, bytes.Repeat([]byte{0x63}, 117), SigHashAll); err != nil {
		t.Fatal(err)
	}
	if tr.SerializeHex() != before || !bytes.Equal(tr.Inputs[0].ScriptPubKey, spk) {
		t.Error("the transaction changed under ComputeSigHashWithScriptCode")
	}
}

func TestScriptCodeDigestRefusesAnIndexOutOfRange(t *testing.T) {
	v := loadNodeVector(t)
	tr := buildNodeVector(t, v)
	for _, i := range []int{-1, len(tr.Inputs), len(tr.Inputs) + 1} {
		if d, err := tr.ComputeSigHashWithScriptCode(i, []byte{0x51}, SigHashAll); err == nil || d != nil {
			t.Errorf("index %d: digest %x, err %v; want an error and no digest", i, d, err)
		}
	}
}
