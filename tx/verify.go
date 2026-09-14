// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Verification errors. Each means the transaction must not be broadcast: the
// node would refuse it, or it would spend from a key other than the one the
// output names. VerifyInput wraps them with the input index.
var (
	// ErrUnsigned marks an input with no witness.
	ErrUnsigned = errors.New("tx: input is not signed")

	// ErrWitnessForm marks a witness that is not the two-item stack the node's
	// single-key Dilithium path requires: signature||hashtype (2421 bytes) and
	// 0x00||public key (1313 bytes), spending a witness v1 32-byte program.
	ErrWitnessForm = errors.New("tx: witness is not [signature||hashtype, 0x00||pubkey] over a v1 program")

	// ErrHashType marks a hashtype byte other than SIGHASH_ALL. The node reads
	// that byte, refuses the ANYPREVOUT types outright on this path, and
	// computes the sighash for the others under BIP 143 rules this SDK does not
	// implement; it signs SIGHASH_ALL only.
	ErrHashType = errors.New("tx: witness hashtype is not SIGHASH_ALL (0x01)")

	// ErrWrongKey marks a public key whose SHA-256 is not the witness program
	// of the output being spent.
	ErrWrongKey = errors.New("tx: witness public key does not hash to the output's witness program")

	// ErrSignature marks a signature that does not verify over the sighash
	// recomputed from the transaction as it stands.
	ErrSignature = errors.New("tx: signature does not verify over the recomputed sighash")
)

// VerifyInput checks input i exactly as the node's single-key Dilithium path
// does (src/script/interpreter.cpp, VerifyWitnessProgram and
// TransactionSignatureChecker::CheckSig): the witness is two items of the
// consensus sizes, the public key carries the 0x00 prefix, its SHA-256 is the
// 32-byte program in the input's scriptPubKey, the hashtype byte at the end of
// the signature is SIGHASH_ALL, and the signature verifies over the BIP 143
// sighash recomputed from the transaction with that hashtype.
//
// The hashtype is read from the witness, not supplied by the caller. keys.Verify
// takes a digest the caller computed and so cannot tell whether the byte the
// node will read agrees with it; this method recomputes the digest from the
// byte, so a witness that verifies here is one the node verifies the same way.
// Every error is one of the sentinels above, wrapped with the input index.
func (tx *Transaction) VerifyInput(i int) error {
	if i < 0 || i >= len(tx.Inputs) {
		return fmt.Errorf("input index %d out of range [0, %d)", i, len(tx.Inputs))
	}
	in := tx.Inputs[i]
	if len(in.WitnessData) == 0 {
		return fmt.Errorf("input %d: %w", i, ErrUnsigned)
	}
	if len(in.WitnessData) != 2 {
		return fmt.Errorf("input %d: %d witness items: %w", i, len(in.WitnessData), ErrWitnessForm)
	}
	sig, pk := in.WitnessData[0], in.WitnessData[1]
	if len(sig) != types.SignatureSize+1 {
		return fmt.Errorf("input %d: signature item is %d bytes, want %d: %w",
			i, len(sig), types.SignatureSize+1, ErrWitnessForm)
	}
	if len(pk) != types.PublicKeySize+1 {
		return fmt.Errorf("input %d: public key item is %d bytes, want %d: %w",
			i, len(pk), types.PublicKeySize+1, ErrWitnessForm)
	}
	if pk[0] != 0x00 {
		return fmt.Errorf("input %d: public key item begins with %#02x, want 0x00: %w", i, pk[0], ErrWitnessForm)
	}
	// OP_1 <32 bytes>: the only output shape this SDK signs for, and the shape
	// the node routes to the single-key Dilithium check.
	spk := in.ScriptPubKey
	if len(spk) != 34 || spk[0] != 0x51 || spk[1] != 0x20 {
		return fmt.Errorf("input %d: scriptPubKey is not a witness v1 32-byte program: %w", i, ErrWitnessForm)
	}
	hashType := sig[types.SignatureSize]
	if hashType != SigHashAll {
		return fmt.Errorf("input %d: hashtype %#02x: %w", i, hashType, ErrHashType)
	}
	program := sha256.Sum256(pk[1:])
	if !bytes.Equal(program[:], spk[2:]) {
		return fmt.Errorf("input %d: %w", i, ErrWrongKey)
	}
	digest, err := tx.ComputeSigHash(i, uint32(hashType))
	if err != nil {
		return fmt.Errorf("input %d: %w", i, err)
	}
	ok, err := keys.Verify(pk[1:], digest, sig[:types.SignatureSize])
	if err != nil {
		return fmt.Errorf("input %d: %w", i, err)
	}
	if !ok {
		return fmt.Errorf("input %d: %w", i, ErrSignature)
	}
	return nil
}

// VerifyAll runs VerifyInput on every input and returns the first failure.
// BuildSignedTransaction and BuildAndSign call it before returning, so a
// transaction those return has been verified; call it yourself after signing
// by hand, and before broadcasting anything that was stored and reloaded.
func (tx *Transaction) VerifyAll() error {
	for i := range tx.Inputs {
		if err := tx.VerifyInput(i); err != nil {
			return err
		}
	}
	return nil
}
