// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// The witness version 6 output script against the witness address vectors
// shared with the node (address/testdata/witness_address_vectors.json, whose
// source commit and digest address/witness_address_vectors_test.go states).
// The file carries no script field: its description gives the script of a
// program as the version opcode (OP_1 = 0x51, OP_6 = 0x56), the push 0x20 and
// the program, and these tests derive it that way.

package tx

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

type addressVector struct {
	Prefix  string `json:"prefix"`
	Version byte   `json:"version"`
	Program string `json:"program"`
	Address string `json:"address"`
}

func validAddressVectors(t *testing.T) []addressVector {
	t.Helper()
	raw, err := os.ReadFile("../address/testdata/witness_address_vectors.json")
	if err != nil {
		t.Fatalf("read the vectors: %v", err)
	}
	var set struct {
		Valid []addressVector `json:"valid"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("parse the vectors: %v", err)
	}
	if len(set.Valid) != 32 {
		t.Fatalf("%d valid vectors, want 32", len(set.Valid))
	}
	return set.Valid
}

func (v addressVector) program(t *testing.T) []byte {
	t.Helper()
	p, err := hex.DecodeString(v.Program)
	if err != nil {
		t.Fatalf("vector %s: program is not hex: %v", v.Address, err)
	}
	return p
}

func TestScriptWitnessV6MatchesTheVectors(t *testing.T) {
	seen := 0
	for _, v := range validAddressVectors(t) {
		if v.Version != 6 {
			continue
		}
		seen++
		program := v.program(t)
		want := append([]byte{0x56, 0x20}, program...)

		got, err := ScriptWitnessV6(program)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("ScriptWitnessV6(%x) = %x, %v; want %x", program, got, err, want)
			continue
		}

		// The address of the vector decodes to the program this script holds.
		decoded, err := soqaddr.DecodeWitnessV6(v.Prefix, v.Address)
		if err != nil || !bytes.Equal(decoded, got[2:]) {
			t.Errorf("DecodeWitnessV6(%q) = %x, %v; the script holds %x", v.Address, decoded, err, got[2:])
		}

		// The script is its own copy of the program.
		program[0] ^= 0xff
		if !bytes.Equal(got, want) {
			t.Errorf("ScriptWitnessV6 returned a script that shares memory with its argument")
		}

		// A 34-byte script has the relay floor of a version 1 output.
		if floor := MinOutputValue(got); floor != DustThreshold {
			t.Errorf("MinOutputValue = %d, want %d", floor, DustThreshold)
		}
	}
	if seen != 16 {
		t.Fatalf("%d version 6 vectors, want 16", seen)
	}
}

// An OP_6 output whose program is not 32 bytes can never be spent.
func TestScriptWitnessV6Takes32BytesOnly(t *testing.T) {
	for size := 0; size <= 40; size++ {
		got, err := ScriptWitnessV6(make([]byte, size))
		if size == 32 {
			if err != nil || len(got) != 34 {
				t.Errorf("32 bytes: %x, %v", got, err)
			}
			continue
		}
		if got != nil || !errors.Is(err, ErrWitnessProgramSize) {
			t.Errorf("%d bytes: ScriptWitnessV6 = %x, %v; want ErrWitnessProgramSize", size, got, err)
		}
	}
	if got, err := ScriptWitnessV6(nil); got != nil || !errors.Is(err, ErrWitnessProgramSize) {
		t.Errorf("nil program: ScriptWitnessV6 = %x, %v; want ErrWitnessProgramSize", got, err)
	}
}

// A transaction that pays a version 6 script carries the 34 bytes as the
// output's script, behind the length byte 0x22.
func TestSendToAWitnessV6ScriptSerializesTheScript(t *testing.T) {
	spk, err := ScriptWitnessV6(hash32(0x6a))
	if err != nil {
		t.Fatalf("ScriptWitnessV6: %v", err)
	}
	in := []types.UTXO{{TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: addrFor(t, types.Regtest.HRP, 0x11)}}
	tr, err := BuildSendTransaction(in, spk, 1_000_000_000, ScriptP2WPKH(hash32(0x33)), 10)
	if err != nil {
		t.Fatalf("BuildSendTransaction: %v", err)
	}
	if !bytes.Equal(tr.Outputs[0].ScriptPubKey, spk) || tr.Outputs[0].Value != 1_000_000_000 {
		t.Fatalf("output 0 = %d shors to %x", tr.Outputs[0].Value, tr.Outputs[0].ScriptPubKey)
	}
	want := append([]byte{0x00, 0xca, 0x9a, 0x3b, 0x00, 0x00, 0x00, 0x00, 0x22, 0x56, 0x20}, hash32(0x6a)...)
	if !bytes.Contains(tr.Serialize(), want) {
		t.Errorf("the serialized transaction does not hold the output %x", want)
	}
}

// The existing version 1 path, against the same file. Every assertion here
// holds on the tree before the version 6 output exists and on the tree after.
func TestVersion1ScriptsAndInputsAgainstTheVectors(t *testing.T) {
	for _, v := range validAddressVectors(t) {
		program := v.program(t)
		in := []types.UTXO{{TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: v.Address}}
		tr, err := BuildSendTransaction(in, ScriptP2WPKH(hash32(0x22)), 1_000_000_000, ScriptP2WPKH(hash32(0x33)), 10)

		if v.Version == 6 {
			// The builders derive each input's script from its address through
			// the payment address decoder, which refuses a version 6 string.
			if tr != nil || !errors.Is(err, soqaddr.ErrUnsupportedWitnessVersion) {
				t.Errorf("an input at %q: BuildSendTransaction = %v, %v; want ErrUnsupportedWitnessVersion", v.Address, tr, err)
			}
			continue
		}

		want := append([]byte{0x51, 0x20}, program...)
		if got := ScriptP2WPKH(program); !bytes.Equal(got, want) {
			t.Errorf("ScriptP2WPKH(%x) = %x, want %x", program, got, want)
		}
		if err != nil {
			t.Errorf("an input at %q: BuildSendTransaction: %v", v.Address, err)
			continue
		}
		if got := tr.Inputs[0].ScriptPubKey; !bytes.Equal(got, want) {
			t.Errorf("an input at %q: script %x, want %x", v.Address, got, want)
		}
	}
}
