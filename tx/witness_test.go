// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// The witness format is not the obvious one, and the obvious one is rejected.
// Consensus requires:
//
//	stack[0] = signature || sighash-type byte   (2421 bytes)
//	stack[1] = 0x00 || public key               (1313 bytes)
//
// CTransaction::HasDilithiumSignatures checks pk_blob[0] == 0x00, because NIST
// FIPS 204 Table 3 specifies ML-DSA-44 public keys begin with that byte.
//
// Before SignInput existed, the only signing example in the SDK assembled
// [][]byte{sig, pubKey} with neither the trailing sighash byte nor the leading
// 0x00, so every transaction it produced was rejected by the node with
// "bad-txns-requires-dilithium". Proven on stagenet: the wrong format was rejected,
// the corrected format confirmed as
// 99fd147aaa4d575ee8f6266acfda4b09a5b0dc730d964294efded2cf3cd2eae7.

package tx

import (
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// fakeSigner returns fixed-size material so the format can be asserted without
// running Dilithium.
type fakeSigner struct{ sigLen, pubLen int }

func (f fakeSigner) Sign(string, []byte) ([]byte, error) {
	s := make([]byte, f.sigLen)
	for i := range s {
		s[i] = 0xAA
	}
	return s, nil
}
func (f fakeSigner) PublicKeyFor(string) ([]byte, error) {
	p := make([]byte, f.pubLen)
	for i := range p {
		p[i] = 0xBB
	}
	return p, nil
}

func signable(t *testing.T) *Transaction {
	t.Helper()
	tr := NewTransaction()
	u := types.UTXO{TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: "ssq1ptest"}
	if err := tr.AddInput(u, ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatal(err)
	}
	tr.AddOutput(4_000_000_000, ScriptP2WPKH(hash32(0x02)))
	return tr
}

func TestSignInputProducesConsensusWitnessFormat(t *testing.T) {
	tr := signable(t)
	if err := tr.SignInput(0, fakeSigner{types.SignatureSize, types.PublicKeySize}, SigHashAll); err != nil {
		t.Fatalf("SignInput: %v", err)
	}
	w := tr.Inputs[0].WitnessData
	if len(w) != 2 {
		t.Fatalf("witness stack has %d items, want 2", len(w))
	}
	if len(w[0]) != types.SignatureSize+1 {
		t.Errorf("stack[0] is %d bytes, want %d (signature + sighash byte)",
			len(w[0]), types.SignatureSize+1)
	}
	if got := w[0][len(w[0])-1]; got != byte(SigHashAll) {
		t.Errorf("trailing sighash byte = %#x, want %#x", got, SigHashAll)
	}
	if len(w[1]) != types.PublicKeySize+1 {
		t.Errorf("stack[1] is %d bytes, want %d (0x00 + public key)",
			len(w[1]), types.PublicKeySize+1)
	}
	// The check that consensus actually performs.
	if w[1][0] != 0x00 {
		t.Errorf("stack[1][0] = %#x, want 0x00; consensus rejects anything else "+
			"with bad-txns-requires-dilithium", w[1][0])
	}
}

// The historical bug, pinned so it cannot return: bare sig and bare pubkey.
func TestBareWitnessIsNotWhatWeProduce(t *testing.T) {
	tr := signable(t)
	if err := tr.SignInput(0, fakeSigner{types.SignatureSize, types.PublicKeySize}, SigHashAll); err != nil {
		t.Fatal(err)
	}
	w := tr.Inputs[0].WitnessData
	if len(w[0]) == types.SignatureSize {
		t.Error("stack[0] is a bare signature with no sighash byte (the old bug)")
	}
	if len(w[1]) == types.PublicKeySize {
		t.Error("stack[1] is a bare public key with no 0x00 prefix (the old bug)")
	}
}

// A signer returning the wrong sizes must be refused rather than silently
// producing a transaction the network rejects.
func TestSignInputRejectsWrongSizedMaterial(t *testing.T) {
	for name, s := range map[string]fakeSigner{
		"short signature":  {types.SignatureSize - 1, types.PublicKeySize},
		"long signature":   {types.SignatureSize + 1, types.PublicKeySize},
		"short public key": {types.SignatureSize, types.PublicKeySize - 1},
		"long public key":  {types.SignatureSize, types.PublicKeySize + 1},
	} {
		t.Run(name, func(t *testing.T) {
			tr := signable(t)
			if err := tr.SignInput(0, s, SigHashAll); err == nil {
				t.Errorf("accepted %s", name)
			}
		})
	}
}

func TestSignInputBoundsAndMissingAddress(t *testing.T) {
	tr := signable(t)
	f := fakeSigner{types.SignatureSize, types.PublicKeySize}
	for _, i := range []int{-1, 1, 99} {
		if err := tr.SignInput(i, f, SigHashAll); err == nil {
			t.Errorf("index %d accepted", i)
		}
	}
	tr.Inputs[0].Address = ""
	if err := tr.SignInput(0, f, SigHashAll); err == nil {
		t.Error("signed an input with no address")
	}
}

func TestSignAllSignsEveryInput(t *testing.T) {
	tr := NewTransaction()
	for i := 0; i < 3; i++ {
		u := types.UTXO{TxID: displayTxID, Vout: uint32(i), Value: 1_000_000_000, Address: "ssq1ptest"}
		if err := tr.AddInput(u, ScriptP2WPKH(hash32(0x01))); err != nil {
			t.Fatal(err)
		}
	}
	tr.AddOutput(2_500_000_000, ScriptP2WPKH(hash32(0x02)))
	if err := tr.SignAll(fakeSigner{types.SignatureSize, types.PublicKeySize}); err != nil {
		t.Fatalf("SignAll: %v", err)
	}
	for i, in := range tr.Inputs {
		if len(in.WitnessData) != 2 {
			t.Errorf("input %d not signed", i)
		}
	}
}
