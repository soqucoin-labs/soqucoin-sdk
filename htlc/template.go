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
// Every function here is a function of its arguments. None reads a node, a
// clock or a file, so none knows whether the chain enforces witness version 6
// yet, whether an output exists, or how far the tip is from the lock height.
// A caller checks those against a node before it funds, claims or refunds.
package htlc

import (
	"errors"

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
// holds and can replace a refund with a claim that pays more.
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

// The declarations below are placed ahead of their bodies so that the tests
// of the package compile and fail on their assertions.

var errNotBuilt = errors.New("htlc: not built yet")

type Params struct {
	Hash         [32]byte
	PayeeKeyHash [32]byte
	PayerKeyHash [32]byte
	LockHeight   Height
}

func (p Params) Validate() error                { return errNotBuilt }
func (p Params) Script() ([]byte, error)        { return nil, errNotBuilt }
func (p Params) OutputScript() ([]byte, error)  { return nil, errNotBuilt }
func (p Params) Address(string) (string, error) { return "", errNotBuilt }
func ParseScript([]byte) (Params, error)        { return Params{}, errNotBuilt }
func KeyHash([]byte) ([32]byte, error)          { return [32]byte{}, errNotBuilt }

var (
	ErrPreimage    = errors.New("htlc: preimage does not hash to the contract's hash")
	ErrWrongKey    = errors.New("htlc: public key does not hash to the key hash of this branch")
	ErrDestination = errors.New("htlc: destination is not a payment address on the signer's network")
	ErrSpendForm   = errors.New("htlc: not a claim or a refund of this contract in the form this package writes")
)

func BuildClaim(Params, types.UTXO, [32]byte, string, int64, tx.Signer, string) (*tx.Transaction, error) {
	return nil, errNotBuilt
}

func BuildRefund(Params, types.UTXO, string, int64, tx.Signer, string) (*tx.Transaction, error) {
	return nil, errNotBuilt
}

func VerifySpend(*tx.Transaction, Params, int64) error { return errNotBuilt }
