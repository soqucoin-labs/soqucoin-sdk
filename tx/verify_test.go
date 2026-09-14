// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// realKeys returns a manager holding n freshly generated stagenet keys, in
// order. The manager is the Signer; the pairs give the tests the public keys.
func realKeys(t *testing.T, n int) (*keys.Manager, []*keys.KeyPair) {
	t.Helper()
	m := keys.NewManager(filepath.Join(t.TempDir(), "k.json"), "pw")
	var pairs []*keys.KeyPair
	for i := 0; i < n; i++ {
		kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, kp)
	}
	return m, pairs
}

// signedByRealKeys builds a two-input send, one input per key, and signs it.
func signedByRealKeys(t *testing.T) (*Transaction, *keys.Manager, []*keys.KeyPair) {
	t.Helper()
	m, pairs := realKeys(t, 2)
	inputs := []types.UTXO{
		{TxID: displayTxID, Vout: 0, Value: 3_000_000_000, Address: pairs[0].Address},
		{TxID: displayTxID, Vout: 1, Value: 2_000_000_000, Address: pairs[1].Address},
	}
	change, err := address.ScriptFor(pairs[0].Address)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := BuildSendTransaction(inputs, ScriptP2WPKH(hash32(0x02)), 4_000_000_000, change, types.RecommendedFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SignAll(m); err != nil {
		t.Fatal(err)
	}
	return tr, m, pairs
}

func TestVerifyAllAcceptsWhatSignAllProduces(t *testing.T) {
	tr, _, _ := signedByRealKeys(t)
	if err := tr.VerifyAll(); err != nil {
		t.Fatalf("VerifyAll on the SDK's own signed transaction: %v", err)
	}
	for i := range tr.Inputs {
		if err := tr.VerifyInput(i); err != nil {
			t.Fatalf("VerifyInput(%d): %v", i, err)
		}
	}
}

// The hashtype the node uses is the byte at the end of the witness signature.
// A caller-digest check (keys.Verify with a SIGHASH_ALL digest) said "valid"
// for a witness carrying any other byte, and the node then either refused the
// type (the ANYPREVOUT pair) or verified against a different message.
// VerifyInput reads the byte and refuses everything but SIGHASH_ALL.
func TestVerifyInputReadsHashTypeFromWitness(t *testing.T) {
	tr, _, _ := signedByRealKeys(t)
	for _, b := range []byte{SigHashNone, SigHashSingle, 0x41, 0x42, SigHashAll | SigHashAnyoneCanPay, 0x00} {
		sig := append([]byte(nil), tr.Inputs[0].WitnessData[0]...)
		sig[types.SignatureSize] = b
		mutated := *tr
		mutated.Inputs = append([]TxInput(nil), tr.Inputs...)
		mutated.Inputs[0].WitnessData = [][]byte{sig, tr.Inputs[0].WitnessData[1]}
		if err := mutated.VerifyInput(0); !errors.Is(err, ErrHashType) {
			t.Errorf("hashtype %#02x: got %v, want ErrHashType", b, err)
		}
		if err := mutated.VerifyAll(); !errors.Is(err, ErrHashType) {
			t.Errorf("hashtype %#02x via VerifyAll: got %v, want ErrHashType", b, err)
		}
	}
}

// The sighash is recomputed from the transaction as it stands, so a change to
// anything the signature commits to is caught even though the witness bytes
// are untouched.
func TestVerifyInputRecomputesTheSighash(t *testing.T) {
	tr, _, _ := signedByRealKeys(t)
	tr.Outputs[0].Value++
	if err := tr.VerifyInput(0); !errors.Is(err, ErrSignature) {
		t.Fatalf("output changed after signing: got %v, want ErrSignature", err)
	}
	tr.Outputs[0].Value--
	if err := tr.VerifyAll(); err != nil {
		t.Fatalf("restored transaction: %v", err)
	}
	tr.Inputs[1].Value++
	if err := tr.VerifyInput(1); !errors.Is(err, ErrSignature) {
		t.Fatalf("input value changed after signing: got %v, want ErrSignature", err)
	}
}

func TestVerifyInputDetectsTamperedSignature(t *testing.T) {
	tr, _, _ := signedByRealKeys(t)
	tr.Inputs[0].WitnessData[0][100] ^= 0x01
	if err := tr.VerifyInput(0); !errors.Is(err, ErrSignature) {
		t.Fatalf("flipped signature bit: got %v, want ErrSignature", err)
	}
}

// A valid signature by a key that does not own the output is refused before
// the signature is checked: the key's hash must be the output's program.
func TestVerifyInputDetectsWrongKey(t *testing.T) {
	tr, _, _ := signedByRealKeys(t)
	tr.Inputs[0].WitnessData[1], tr.Inputs[1].WitnessData[1] = tr.Inputs[1].WitnessData[1], tr.Inputs[0].WitnessData[1]
	for i := range tr.Inputs {
		if err := tr.VerifyInput(i); !errors.Is(err, ErrWrongKey) {
			t.Errorf("input %d with the other input's key: got %v, want ErrWrongKey", i, err)
		}
	}
}

func TestVerifyInputWitnessForm(t *testing.T) {
	tr, _, _ := signedByRealKeys(t)
	sig, pk := tr.Inputs[0].WitnessData[0], tr.Inputs[0].WitnessData[1]
	cases := map[string]struct {
		witness [][]byte
		spk     []byte
		want    error
	}{
		"unsigned":               {nil, tr.Inputs[0].ScriptPubKey, ErrUnsigned},
		"three items":            {[][]byte{sig, pk, {0x01}}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"one item":               {[][]byte{sig}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"signature without byte": {[][]byte{sig[:types.SignatureSize], pk}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"pubkey without prefix":  {[][]byte{sig, pk[1:]}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"empty pubkey item":      {[][]byte{sig, {}}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"empty signature item":   {[][]byte{{}, pk}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"pubkey prefix not zero": {[][]byte{sig, append([]byte{0x01}, pk[1:]...)}, tr.Inputs[0].ScriptPubKey, ErrWitnessForm},
		"v0 program":             {[][]byte{sig, pk}, append([]byte{0x00, 0x20}, tr.Inputs[0].ScriptPubKey[2:]...), ErrWitnessForm},
		"v7 program":             {[][]byte{sig, pk}, append([]byte{0x57, 0x20}, tr.Inputs[0].ScriptPubKey[2:]...), ErrWitnessForm},
		"short script":           {[][]byte{sig, pk}, tr.Inputs[0].ScriptPubKey[:33], ErrWitnessForm},
	}
	for name, c := range cases {
		mutated := *tr
		mutated.Inputs = append([]TxInput(nil), tr.Inputs...)
		mutated.Inputs[0].WitnessData = c.witness
		mutated.Inputs[0].ScriptPubKey = c.spk
		if err := mutated.VerifyInput(0); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	if err := tr.VerifyInput(2); err == nil {
		t.Error("index out of range accepted")
	}
	if err := tr.VerifyInput(-1); err == nil {
		t.Error("negative index accepted")
	}
}

// badSigner wraps a real signer and corrupts what it returns, standing in for
// a signer that signs the wrong digest or reports the wrong key.
type badSigner struct {
	Signer
	flipSig  bool
	otherKey []byte
}

func (b badSigner) Sign(addr string, digest []byte) ([]byte, error) {
	sig, err := b.Signer.Sign(addr, digest)
	if err == nil && b.flipSig {
		sig[0] ^= 0x01
	}
	return sig, err
}

func (b badSigner) PublicKeyFor(addr string) ([]byte, error) {
	if b.otherKey != nil {
		return b.otherKey, nil
	}
	return b.Signer.PublicKeyFor(addr)
}

// BuildSignedTransaction and BuildAndSign verify before they return, so a
// signer that would produce a rejected transaction is an error at build time.
func TestBuildSignedTransactionVerifiesEveryInput(t *testing.T) {
	m, pairs := realKeys(t, 2)
	inputs := []types.UTXO{{TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: pairs[0].Address}}
	recipient, change := ScriptP2WPKH(hash32(0x02)), ScriptP2WPKH(hash32(0x03))

	if _, err := BuildSignedTransaction(inputs, recipient, 1_000_000_000, change, 1000, m); err != nil {
		t.Fatalf("real signer: %v", err)
	}
	if _, err := BuildSignedTransaction(inputs, recipient, 1_000_000_000, change, 1000, badSigner{Signer: m, flipSig: true}); !errors.Is(err, ErrSignature) {
		t.Fatalf("signer returning a corrupt signature: got %v, want ErrSignature", err)
	}
	if _, _, err := BuildAndSign(inputs, recipient, 1_000_000_000, change, 1000, badSigner{Signer: m, flipSig: true}); !errors.Is(err, ErrSignature) {
		t.Fatalf("BuildAndSign with a corrupt signature: got %v, want ErrSignature", err)
	}
	if _, err := BuildSignedTransaction(inputs, recipient, 1_000_000_000, change, 1000, badSigner{Signer: m, otherKey: pairs[1].PublicKey}); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("signer reporting another key: got %v, want ErrWrongKey", err)
	}
}

// The fixed-content material the format tests sign with is not a signature,
// so verification must say so rather than accept it on shape alone.
func TestVerifyInputRefusesFixedMaterial(t *testing.T) {
	tr := signable(t)
	if err := tr.SignInput(0, fakeSigner{types.SignatureSize, types.PublicKeySize}, SigHashAll); err != nil {
		t.Fatal(err)
	}
	if err := tr.VerifyInput(0); err == nil {
		t.Fatal("fixed bytes of the right sizes verified")
	}
}

func BenchmarkVerifyAll80Inputs(b *testing.B) {
	m := keys.NewManager(filepath.Join(b.TempDir(), "k.json"), "pw")
	kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		b.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		b.Fatal(err)
	}
	var inputs []types.UTXO
	for i := 0; i < 80; i++ {
		inputs = append(inputs, types.UTXO{TxID: displayTxID, Vout: uint32(i), Value: 1_000_000_000, Address: kp.Address})
	}
	change, _ := address.ScriptFor(kp.Address)
	tr, err := BuildSendTransaction(inputs, ScriptP2WPKH(hash32(0x02)), 70_000_000_000, change, types.RecommendedFeeRate)
	if err != nil {
		b.Fatal(err)
	}
	if err := tr.SignAll(m); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tr.VerifyAll(); err != nil {
			b.Fatal(err)
		}
	}
}
