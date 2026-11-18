// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Errors of the two spends. The builders and VerifySpend also return
// tx.ErrHashType, tx.ErrSignature, tx.ErrInvalidAmount, tx.ErrFeeTooHigh,
// tx.ErrInsufficientFunds and tx.ErrBelowDust with their meaning in package
// tx.
var (
	// ErrPreimage marks a secret whose SHA-256 is not the contract's hash.
	ErrPreimage = errors.New("htlc: preimage does not hash to the contract's hash")

	// ErrWrongKey marks a public key whose SHA-256 is not the key hash of the
	// branch being spent: the payee's for a claim, the payer's for a refund.
	ErrWrongKey = errors.New("htlc: public key does not hash to the key hash of this branch")

	// ErrDestination marks a destination that is not a payment address of
	// the signer's network.
	ErrDestination = errors.New("htlc: destination is not a payment address on the signer's network")

	// ErrSpendForm marks a transaction that is not a claim or a refund of the
	// contract in the form this package writes.
	ErrSpendForm = errors.New("htlc: not a claim or a refund of this contract in the form this package writes")
)

// The witness of a spend, first item deepest on the stack. The node reads the
// last item as the trailer every input carries and the one before it as the
// witnessScript (src/script/interpreter.cpp, the witness version 6 branch).
//
//	claim:  signature, key, preimage, selector 0x01, witnessScript, trailer
//	refund: signature, key, selector (empty),        witnessScript, trailer
const (
	claimItems  = 6
	refundItems = 5

	// A signature item is the 2,420-byte signature and the hash type byte.
	signatureItemSize = types.SignatureSize + 1

	// trailerPrefix starts the trailer: 0x00 and the signer's raw key, 1,313
	// bytes. Consensus requires the leading byte of the last witness item of
	// every input; relay takes this length.
	trailerPrefix = 0x00
)

// BuildClaim returns the signed transaction that takes the contract's output
// with the secret: one input, the funded output; one output, the funded value
// less the fee, to payToAddress. Version 2, lock time 0, sequence
// ClaimSequence, SIGHASH_ALL.
//
// A claim shows the secret to every node it reaches, so nothing is signed and
// nothing is returned unless the claim is one the script accepts. Refused
// before the signer is asked: a preimage that does not hash to p.Hash
// (ErrPreimage); a signer whose key begins 0xff, which the node refuses in
// every spend (keys.ErrInvalidPublicKey), or does not hash to p.PayeeKeyHash
// (ErrWrongKey); a destination that is not a payment address of the signer's
// network (ErrDestination); a fee rate or a fee outside the limits of package
// tx; a value that leaves less than the relay floor. The signed transaction
// is then checked with VerifySpend.
//
// funded names the output: its transaction id, index and value. The function
// cannot see whether that output exists, is confirmed or is already spent.
// signerAddress selects the key in signer.
func BuildClaim(p Params, funded types.UTXO, preimage [32]byte, payToAddress string, feeRate int64, signer tx.Signer, signerAddress string) (*tx.Transaction, error) {
	script, err := p.Script()
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(preimage[:]) != p.Hash {
		return nil, ErrPreimage
	}
	return buildSpend(p, script, funded, &preimage, payToAddress, feeRate, signer, signerAddress)
}

// BuildRefund returns the signed transaction that takes the contract's output
// back for the payer. It has the shape of a claim with no preimage, lock time
// p.LockHeight and sequence RefundSequence. The refusals are those of
// BuildClaim, with p.PayerKeyHash as the key hash.
//
// The function builds at any tip. A node refuses the refund as non-final
// until its tip is at the lock height; the first block that can hold it is
// the one after.
func BuildRefund(p Params, funded types.UTXO, payToAddress string, feeRate int64, signer tx.Signer, signerAddress string) (*tx.Transaction, error) {
	script, err := p.Script()
	if err != nil {
		return nil, err
	}
	return buildSpend(p, script, funded, nil, payToAddress, feeRate, signer, signerAddress)
}

// buildSpend builds a claim when preimage is set and a refund when it is nil.
// script is p.Script(), and for a claim the caller has checked the preimage.
func buildSpend(p Params, script []byte, funded types.UTXO, preimage *[32]byte, payToAddress string, feeRate int64, signer tx.Signer, signerAddress string) (*tx.Transaction, error) {
	outputScript, err := p.OutputScript()
	if err != nil {
		return nil, err
	}

	// The key first: a spend under a key the branch does not name fails at
	// the node, and a failed claim has still shown the secret.
	keyHash := p.PayerKeyHash
	if preimage != nil {
		keyHash = p.PayeeKeyHash
	}
	publicKey, err := signer.PublicKeyFor(signerAddress)
	if err != nil {
		return nil, fmt.Errorf("public key for %s: %w", signerAddress, err)
	}
	// The transaction holds its own copy: the signer may keep the slice.
	publicKey = bytes.Clone(publicKey)
	got, err := KeyHash(publicKey)
	if err != nil {
		return nil, err
	}
	if got != keyHash {
		return nil, ErrWrongKey
	}

	payScript, err := destinationScript(payToAddress, signerAddress)
	if err != nil {
		return nil, err
	}
	if funded.Value <= 0 || funded.Value > tx.MaxMoney {
		return nil, fmt.Errorf("%w: funded value %d", tx.ErrInvalidAmount, funded.Value)
	}

	t := tx.NewTransaction()
	t.Version = tx.TxVersion
	in := funded
	in.Address = signerAddress
	if err := t.AddInput(in, outputScript); err != nil {
		return nil, fmt.Errorf("funded output: %w", err)
	}
	if preimage != nil {
		t.Inputs[0].Sequence = ClaimSequence
		t.LockTime = 0
	} else {
		t.Inputs[0].Sequence = RefundSequence
		t.LockTime = uint32(p.LockHeight)
	}
	t.AddOutput(0, payScript)

	// The fee is measured with a witness of the final item sizes in place.
	// An input without one is counted as a two-item payment witness, which is
	// smaller than either spend of the contract.
	t.Inputs[0].WitnessData = witness(make([]byte, signatureItemSize), publicKey, preimage, script)
	fee := t.EstimateFee(feeRate)
	if feeRate <= 0 {
		return nil, fmt.Errorf("%w: fee rate %d shors/vB", tx.ErrInvalidAmount, feeRate)
	}
	if feeRate > tx.MaxFeeRateShorsPerVB {
		return nil, fmt.Errorf("%w: fee rate %d shors/vB exceeds MaxFeeRateShorsPerVB %d", tx.ErrFeeTooHigh, feeRate, tx.MaxFeeRateShorsPerVB)
	}
	if fee > tx.MaxFeeShors {
		return nil, fmt.Errorf("%w: fee %d shors exceeds MaxFeeShors %d", tx.ErrFeeTooHigh, fee, tx.MaxFeeShors)
	}
	value := funded.Value - fee
	if value <= 0 {
		return nil, fmt.Errorf("%w: funded value %d, fee %d", tx.ErrInsufficientFunds, funded.Value, fee)
	}
	if floor := tx.MinOutputValue(payScript); value < floor {
		return nil, fmt.Errorf("%w: %d shors left after a fee of %d, floor %d", tx.ErrBelowDust, value, fee, floor)
	}
	t.Outputs[0].Value = value

	// The node signs a witness version 6 spend over the witnessScript, not
	// over the output script.
	digest, err := t.ComputeSigHashWithScriptCode(0, script, tx.SigHashAll)
	if err != nil {
		return nil, err
	}
	signature, err := signer.Sign(signerAddress, digest)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	signatureItem := make([]byte, 0, signatureItemSize)
	signatureItem = append(signatureItem, signature...)
	signatureItem = append(signatureItem, tx.SigHashAll)
	t.Inputs[0].WitnessData = witness(signatureItem, publicKey, preimage, script)

	if err := VerifySpend(t, p, funded.Value); err != nil {
		return nil, err
	}
	return t, nil
}

// witness assembles the items of a claim (preimage set) or a refund (nil).
func witness(signatureItem, publicKey []byte, preimage *[32]byte, script []byte) [][]byte {
	trailer := make([]byte, 0, 1+len(publicKey))
	trailer = append(trailer, trailerPrefix)
	trailer = append(trailer, publicKey...)
	if preimage != nil {
		return [][]byte{signatureItem, publicKey, preimage[:], {0x01}, script, trailer}
	}
	return [][]byte{signatureItem, publicKey, {}, script, trailer}
}

// destinationScript returns the output script of a payment address. The
// address goes through the SDK's payment decoder, which takes witness version
// 1 and the prefixes of the SDK's networks only, and it must carry the prefix
// of the signer's address.
func destinationScript(payToAddress, signerAddress string) ([]byte, error) {
	payHRP, err := address.HRPOf(payToAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDestination, err)
	}
	signerHRP, err := address.HRPOf(signerAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: signer address: %w", ErrDestination, err)
	}
	if payHRP != signerHRP {
		return nil, fmt.Errorf("%w: destination prefix %q, signer prefix %q", ErrDestination, payHRP, signerHRP)
	}
	script, err := address.ScriptFor(payToAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDestination, err)
	}
	return script, nil
}

// VerifySpend checks that t is a claim or a refund of the contract p, spending
// an output of the given value, in the form this package writes. It repeats
// what the node's script check reads and holds each field the script leaves
// open to one value:
//
//   - one input, whose output script is p.OutputScript() and whose value is
//     the value given; transaction version 2;
//   - six witness items for a claim, five for a refund, in the order of the
//     package: a 2,421-byte signature item ending in SIGHASH_ALL, the raw
//     1,312-byte key, for a claim the 32-byte preimage that hashes to p.Hash,
//     the selector (the byte 0x01 for a claim, the empty item for a refund),
//     the witnessScript equal to p.Script(), and the trailer, 0x00 and the
//     same key;
//   - a claim has lock time 0 and sequence ClaimSequence, a refund lock time
//     p.LockHeight and sequence RefundSequence;
//   - the key hashes to p.PayeeKeyHash for a claim and to p.PayerKeyHash for a
//     refund, and the signature verifies under it over the digest with the
//     witnessScript as script code.
//
// It is stricter than the node, which also accepts the key with a leading
// zero, any true or false selector in a block, a refund with a later lock
// time or another non-final sequence, a claim with another sequence or
// another lock time it holds final, any trailer that starts with 0x00, other
// hash types (the anyprevout ones excepted), transaction version 1 and a
// transaction with further inputs. What passes here passes the script; what
// the node accepts in another form is refused here with ErrSpendForm, and a
// hash type that is not SIGHASH_ALL with tx.ErrHashType as well.
//
// The key item and the trailer are outside the signature. Anyone who holds a
// spend this package built can rewrite them into a form the node accepts, and
// the transaction id stays the same while the size changes.
//
// The outputs, the fee and the state of the chain are not checked: whether
// the output exists and is unspent, whether witness version 6 is enforced,
// and whether the tip has reached the lock height.
func VerifySpend(t *tx.Transaction, p Params, value int64) error {
	script, err := p.Script()
	if err != nil {
		return err
	}
	outputScript, err := p.OutputScript()
	if err != nil {
		return err
	}
	if t == nil || len(t.Inputs) != 1 {
		return fmt.Errorf("%w: want one input", ErrSpendForm)
	}
	if t.Version != tx.TxVersion {
		return fmt.Errorf("%w: transaction version %d, want %d", ErrSpendForm, t.Version, tx.TxVersion)
	}
	in := t.Inputs[0]
	if !bytes.Equal(in.ScriptPubKey, outputScript) {
		return fmt.Errorf("%w: the input does not spend the contract's output script", ErrSpendForm)
	}
	if in.Value != value {
		return fmt.Errorf("%w: the input carries value %d, want %d", ErrSpendForm, in.Value, value)
	}

	w := in.WitnessData
	var (
		claim    bool
		keyHash  [32]byte
		selector []byte
	)
	switch len(w) {
	case claimItems:
		claim, keyHash, selector = true, p.PayeeKeyHash, []byte{0x01}
	case refundItems:
		claim, keyHash, selector = false, p.PayerKeyHash, []byte{}
	default:
		return fmt.Errorf("%w: %d witness items", ErrSpendForm, len(w))
	}
	signatureItem, publicKey := w[0], w[1]
	gotSelector, gotScript, trailer := w[len(w)-3], w[len(w)-2], w[len(w)-1]

	if len(signatureItem) != signatureItemSize {
		return fmt.Errorf("%w: signature item is %d bytes, want %d", ErrSpendForm, len(signatureItem), signatureItemSize)
	}
	if len(publicKey) != types.PublicKeySize {
		return fmt.Errorf("%w: key item is %d bytes, want %d", ErrSpendForm, len(publicKey), types.PublicKeySize)
	}
	if claim {
		preimage := w[2]
		if len(preimage) != sha256.Size {
			return fmt.Errorf("%w: preimage item is %d bytes, want %d", ErrSpendForm, len(preimage), sha256.Size)
		}
		if sha256.Sum256(preimage) != p.Hash {
			return ErrPreimage
		}
		if t.LockTime != 0 || in.Sequence != ClaimSequence {
			return fmt.Errorf("%w: claim with lock time %d and sequence %#x, want 0 and %#x",
				ErrSpendForm, t.LockTime, in.Sequence, ClaimSequence)
		}
	} else if t.LockTime != uint32(p.LockHeight) || in.Sequence != RefundSequence {
		return fmt.Errorf("%w: refund with lock time %d and sequence %#x, want %d and %#x",
			ErrSpendForm, t.LockTime, in.Sequence, p.LockHeight, RefundSequence)
	}
	if !bytes.Equal(gotSelector, selector) {
		return fmt.Errorf("%w: selector of %d bytes", ErrSpendForm, len(gotSelector))
	}
	if !bytes.Equal(gotScript, script) {
		return fmt.Errorf("%w: the witnessScript item is not the contract's script", ErrSpendForm)
	}
	if len(trailer) != 1+len(publicKey) || trailer[0] != trailerPrefix || !bytes.Equal(trailer[1:], publicKey) {
		return fmt.Errorf("%w: the trailer is not 0x00 and the key item", ErrSpendForm)
	}
	hashType := signatureItem[types.SignatureSize]
	if hashType != tx.SigHashAll {
		return fmt.Errorf("%w: hashtype %#02x: %w", ErrSpendForm, hashType, tx.ErrHashType)
	}
	if sha256.Sum256(publicKey) != keyHash {
		return ErrWrongKey
	}
	digest, err := t.ComputeSigHashWithScriptCode(0, script, uint32(hashType))
	if err != nil {
		return err
	}
	ok, err := keys.Verify(publicKey, digest, signatureItem[:types.SignatureSize])
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSpendForm, err)
	}
	if !ok {
		return tx.ErrSignature
	}
	return nil
}
