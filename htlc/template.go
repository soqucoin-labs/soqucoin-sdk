// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

// Package htlc builds the hashed time-lock contract of a cross-chain swap on
// Soqucoin: the witnessScript, its witness version 6 output, and the two
// transactions that spend it, the claim with the secret and the refund after
// the lock height.
//
// There is one template. Its bytes are those the node asserts in
// src/test/lightning_script_tests.cpp (b1_script_vectors):
//
//	OP_IF
//	  OP_SIZE 32 OP_EQUALVERIFY OP_SHA256 <hash> OP_EQUALVERIFY
//	  <payee key hash> OP_CHECKDILITHIUMKEYHASH OP_1
//	OP_ELSE
//	  <lock height> OP_CHECKLOCKTIMEVERIFY OP_DROP
//	  <payer key hash> OP_CHECKDILITHIUMKEYHASH OP_1
//	OP_ENDIF
//
// The payee claims with a 32-byte secret whose SHA-256 is the hash. The payer
// funded the output and takes it back from the lock height on. A key hash is
// the SHA-256 of a raw 1,312-byte ML-DSA-44 public key.
//
// The template, BuildFunding, the two builders of a spend and VerifySpend
// read no node, no clock and no file, so none of them knows whether the chain
// enforces witness version 6 yet, whether an output exists, or how far the
// tip is from the lock height. Fund, Audit, Claim, Refund and FindSecret ask
// a node for those and refuse on its answers; a caller that uses the others
// alone makes the same checks before it funds, claims or refunds. The
// builders read the fee limits of package tx (MaxFeeRateShorsPerVB,
// MaxFeeShors), and the two builders of a spend ask the signer, whose
// signatures may differ from call to call.
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

// Height is a block height on Soqucoin. The contract's lock is a height and
// never a time.
type Height uint32

// MaxLockHeight is the largest lock height. From 500,000,000 on the node
// reads a lock time as a time (src/script/script.h, LOCKTIME_THRESHOLD), and
// a refund whose lock time is a height would then fail the script's
// comparison.
const MaxLockHeight Height = 499_999_999

// The sequence number of the one input of each spend. Neither is 0xffffffff,
// which switches a transaction's lock time off and fails the refund's lock
// time check. The claim's value does not signal replacement and the refund's
// does (src/policy/rbf.cpp), so a node's mempool does not replace a claim it
// holds and can replace a refund with a claim that pays more. The payee
// cannot replace its own claim either: a claim's fee is not raised by sending
// the claim again at a higher rate.
const (
	ClaimSequence  uint32 = 0xfffffffe
	RefundSequence uint32 = 0xfffffffd
)

// Errors of the template.
var (
	// ErrLockHeight marks a lock height outside 1 to MaxLockHeight.
	ErrLockHeight = errors.New("htlc: lock height must be a block height from 1 to 499,999,999")

	// ErrNotTemplate marks a script that is not, byte for byte, the template
	// for the parameters read from it.
	ErrNotTemplate = errors.New("htlc: script is not the swap contract template")

	// ErrKeySize marks a public key that is not 1,312 bytes.
	ErrKeySize = errors.New("htlc: public key must be the raw 1,312-byte ML-DSA-44 key")
)

// The opcodes of the template (src/script/script.h).
const (
	op1                     = 0x51
	opIf                    = 0x63
	opElse                  = 0x67
	opEndIf                 = 0x68
	opDrop                  = 0x75
	opSize                  = 0x82
	opEqualVerify           = 0x88
	opSHA256                = 0xa8
	opCheckLockTimeVerify   = 0xb1
	opCheckDilithiumKeyHash = 0xb6
	push32                  = 0x20
)

// The template is 115 bytes and the lock height's script number, which is one
// to four bytes for a height up to MaxLockHeight.
const (
	scriptFixedBytes = 115
	minScriptBytes   = scriptFixedBytes + 1
	maxScriptBytes   = scriptFixedBytes + 4
	lockPushOffset   = 76
)

// Params are the four values a contract is made of. Both parties derive the
// same script from the same four, so each can compare the funded output with
// bytes it computed itself.
type Params struct {
	// Hash is the SHA-256 of the 32-byte secret.
	Hash [32]byte
	// PayeeKeyHash is the SHA-256 of the raw public key that claims.
	PayeeKeyHash [32]byte
	// PayerKeyHash is the SHA-256 of the raw public key that refunds.
	PayerKeyHash [32]byte
	// LockHeight is the height from which the refund is valid: a refund is
	// accepted by a node whose tip is at this height and can be in the next
	// block.
	LockHeight Height
}

// Validate refuses a lock height of 0 or above MaxLockHeight with
// ErrLockHeight. Whether a height is far enough ahead of the tip is a rule of
// the swap and is not checked here.
func (p Params) Validate() error {
	if p.LockHeight < 1 || p.LockHeight > MaxLockHeight {
		return fmt.Errorf("%w: got %d", ErrLockHeight, p.LockHeight)
	}
	return nil
}

// lockHeightNumber writes a height as the node's script number: little-endian,
// no leading zero bytes, and one more zero byte when the top bit of the last
// byte is set, because that bit is the sign. 128 is 80 00. Without the added
// byte the node reads another number (src/script/script.h, CScriptNum). When
// only the top bit is set (128, 32,768, 8,388,608) it reads zero: relay
// refuses the refund as a non-minimal number and a block takes it at any
// height. Otherwise it reads a negative number and every refund fails. The
// claim passes in both cases. The caller has validated the height, so it is
// at least 1.
func lockHeightNumber(h Height) []byte {
	var n []byte
	for v := uint32(h); v > 0; v >>= 8 {
		n = append(n, byte(v))
	}
	if n[len(n)-1]&0x80 != 0 {
		n = append(n, 0x00)
	}
	return n
}

// Script returns the witnessScript of the contract. The lock height is
// written as a data push of its script number at every value, 1 to 16
// included, as the node's own builder writes it; a script that uses OP_1 to
// OP_16 there is another script with another output.
func (p Params) Script() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	n := lockHeightNumber(p.LockHeight)
	s := make([]byte, 0, scriptFixedBytes+len(n))
	s = append(s, opIf, opSize, 0x01, 0x20, opEqualVerify, opSHA256, push32)
	s = append(s, p.Hash[:]...)
	s = append(s, opEqualVerify, push32)
	s = append(s, p.PayeeKeyHash[:]...)
	s = append(s, opCheckDilithiumKeyHash, op1, opElse, byte(len(n)))
	s = append(s, n...)
	s = append(s, opCheckLockTimeVerify, opDrop, push32)
	s = append(s, p.PayerKeyHash[:]...)
	s = append(s, opCheckDilithiumKeyHash, op1, opEndIf)
	return s, nil
}

// program is the witness program of the contract: one SHA-256 of the script.
func (p Params) program() ([]byte, error) {
	script, err := p.Script()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(script)
	return sum[:], nil
}

// OutputScript returns the 34-byte output script of the contract: OP_6 and
// the SHA-256 of the witnessScript. These bytes, and no address string, are
// what a party compares with the output it was shown.
//
// The function does not know the chain's state. The node enforces the
// contract through three deployments: the witness version 6 script path, the
// key hash opcode and the control flow opcodes (src/validation.cpp,
// ConnectBlock). Below the height at which the last of them is active
// consensus does not check all of the script the output commits to, so an
// output of this form is funded only after that height is buried; see
// tx.ScriptWitnessV6.
func (p Params) OutputScript() ([]byte, error) {
	program, err := p.program()
	if err != nil {
		return nil, err
	}
	return tx.ScriptWitnessV6(program)
}

// Address returns the display form of the output under the prefix of one of
// the SDK's networks. It is for showing and for comparing: an address of
// another script passes every checksum, so the script is always derived from
// the parameters and never from an address. The rule of OutputScript on the
// chain's state applies.
func (p Params) Address(hrp string) (string, error) {
	program, err := p.program()
	if err != nil {
		return "", err
	}
	return address.EncodeWitnessV6(hrp, program)
}

// ParseScript reads the four parameters from a witnessScript and returns them
// only if the script is the template for exactly those parameters: it builds
// the script again from what it read and compares the two byte for byte.
// Every other script is refused with ErrNotTemplate, among them a contract
// with another hash function, another signature opcode, a relative lock, a
// key in place of a key hash, no length check on the secret, a lock height
// written in another form or as a time, and one with a byte more or less.
func ParseScript(script []byte) (Params, error) {
	if len(script) < minScriptBytes || len(script) > maxScriptBytes {
		return Params{}, fmt.Errorf("%w: %d bytes", ErrNotTemplate, len(script))
	}
	n := int(script[lockPushOffset])
	if len(script) != scriptFixedBytes+n {
		return Params{}, fmt.Errorf("%w: lock height push of %d bytes in a script of %d", ErrNotTemplate, n, len(script))
	}
	var p Params
	copy(p.Hash[:], script[7:39])
	copy(p.PayeeKeyHash[:], script[41:73])
	number := script[lockPushOffset+1 : lockPushOffset+1+n]
	var height uint32
	for i := n - 1; i >= 0; i-- {
		height = height<<8 | uint32(number[i])
	}
	p.LockHeight = Height(height)
	payer := lockPushOffset + 1 + n + 3
	copy(p.PayerKeyHash[:], script[payer:payer+32])

	want, err := p.Script()
	if err != nil {
		return Params{}, fmt.Errorf("%w: %w", ErrNotTemplate, err)
	}
	if !bytes.Equal(want, script) {
		return Params{}, ErrNotTemplate
	}
	return p, nil
}

// KeyHash returns the SHA-256 of a raw 1,312-byte ML-DSA-44 public key, the
// value the template commits to for each party. It is also the witness
// program of that key's payment address. A key of any other length, the
// 1,313-byte form with a leading zero included, is refused with ErrKeySize.
//
// A key whose first byte is 0xff is refused with keys.ErrInvalidPublicKey, as
// keys.AddressFor refuses it: the node reads that byte as its invalid-key
// marker (src/pubkey.h) and refuses every spend under the key, so a branch
// that commits to its hash can never be spent. The check holds only for a
// key hash derived here: keys.PubKeyHash and a plain SHA-256 give the same 32
// bytes with no refusal, and the other party's key hash arrives as 32 bytes
// that cannot be checked.
func KeyHash(publicKey []byte) ([32]byte, error) {
	if len(publicKey) != types.PublicKeySize {
		return [32]byte{}, fmt.Errorf("%w: got %d", ErrKeySize, len(publicKey))
	}
	if publicKey[0] == 0xff {
		return [32]byte{}, keys.ErrInvalidPublicKey
	}
	return sha256.Sum256(publicKey), nil
}
