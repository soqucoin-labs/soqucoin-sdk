// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Node is what the functions of this file ask of a node. *rpc.Client
// satisfies it. The node is the party's own: its answers on the state of the
// chain are taken as given once it reports the party's network and that it is
// synchronised.
type Node interface {
	GetBlockchainInfo(ctx context.Context) (*rpc.BlockchainInfo, error)
	GetTxOut(ctx context.Context, txid string, vout uint32, includeMempool bool) (*rpc.TxOut, error)
	Broadcast(ctx context.Context, rawTxHex, txid string) (string, error)
	GetBlockHash(ctx context.Context, height int64) (string, error)
	GetBlock(ctx context.Context, hash string, verbosity int) (json.RawMessage, error)
}

// Party is the caller of a function of this file: its network, its signer,
// and the payment address of its own key in that signer.
type Party struct {
	Network types.Network
	Signer  tx.Signer
	Address string
}

// Terms are what the two parties agreed about one contract on Soqucoin.
type Terms struct {
	Params Params
	// Amount is the value of the contract's output in shors.
	Amount int64
	// Depth is the number of confirmations the party waits for. Zero selects
	// MinDepth(Amount), and a depth below it is refused with ErrDepth.
	Depth int64
}

// Funding is what Fund needs beyond the terms.
type Funding struct {
	// Inputs are payment outputs of keys the party's signer holds.
	Inputs []types.UTXO
	// ChangeAddress receives what the inputs hold above the amount and the fee.
	ChangeAddress string
	// FeeRate is in shors per virtual byte.
	FeeRate int64
	// LockMargin is the least number of blocks between the node's tip and
	// the lock height. Below 1 it is read as 1.
	LockMargin int64
}

// Spend is the destination and the fee rate of a claim or a refund.
type Spend struct {
	PayToAddress string
	FeeRate      int64
}

// Funded is the contract's output as the node holds it.
type Funded struct {
	// Output names the outpoint with the value the node holds.
	Output types.UTXO
	// Confirmations is the node's count for the output, and Tip the height
	// of the block it was counted at.
	Confirmations int64
	Tip           int64
}

// Record receives a signed transaction before it is broadcast: its id and its
// raw bytes in hexadecimal. An error stops the broadcast. The bytes of a claim
// hold the secret.
type Record func(txid, rawHex string) error

// Errors of the functions that ask a node.
var (
	// ErrNoActivationHeights marks a node whose getblockchaininfo does not
	// print the activation height of each deployment the contract needs.
	ErrNoActivationHeights = errors.New("htlc: the node does not report the activation heights the contract needs")

	// ErrNotActive marks a tip that is not yet the swap's depth above the
	// activation height of each deployment the contract needs.
	ErrNotActive = errors.New("htlc: the contract's rules have not been active for the swap's depth")

	// ErrDepth marks a depth below MinDepth of the amount.
	ErrDepth = errors.New("htlc: depth is below the floor for the amount")

	// ErrNetwork marks an address that is not of the party's network.
	ErrNetwork = errors.New("htlc: address is not of the party's network")

	// ErrNotFunded marks an outpoint that holds no unspent output, a spend
	// in the node's mempool counted.
	ErrNotFunded = errors.New("htlc: no unspent output at the outpoint")

	// ErrFundedOutput marks an output that is not the agreed contract: another
	// script, or a value below the amount.
	ErrFundedOutput = errors.New("htlc: the output is not the agreed contract")

	// ErrShallow marks a funding with fewer confirmations than the depth.
	ErrShallow = errors.New("htlc: the funding is below the depth")

	// ErrLockTooNear marks a lock height with too few blocks left above the
	// tip for the step asked.
	ErrLockTooNear = errors.New("htlc: the lock height is too near the tip")

	// ErrBeforeLockHeight marks a refund asked while the tip is below the
	// lock height.
	ErrBeforeLockHeight = errors.New("htlc: the tip is below the lock height")

	// ErrTipMoved marks two replies of the node that were given at different
	// tips. It is transient: ask again.
	ErrTipMoved = fmt.Errorf("%w: the tip moved between two replies", rpc.ErrTransient)

	// ErrRecord marks a transaction the record refused. Nothing was sent.
	ErrRecord = errors.New("htlc: the record refused the transaction")

	// ErrNoSpend marks a range of blocks in which nothing spends the output.
	ErrNoSpend = errors.New("htlc: no block in the range spends the output")

	// ErrNoSecret marks a witness that holds no 32-byte item hashing to the
	// contract's hash: the spend of a refund.
	ErrNoSecret = errors.New("htlc: the witness shows no secret of this contract")
)

var errDeclared = errors.New("htlc: declared, not built")

// MinDepth returns the least confirmation depth of a swap of the given
// amount on Soqucoin.
func MinDepth(amount int64) int64 { return 0 }

// BuildFunding returns the unsigned transaction that funds the contract.
func BuildFunding(p Params, inputs []types.UTXO, amount int64, changeAddress string, feeRate int64) (*tx.Transaction, error) {
	return nil, errDeclared
}

// Fund funds the contract.
func Fund(ctx context.Context, node Node, party Party, terms Terms, f Funding, record Record) (types.UTXO, *tx.Transaction, error) {
	return types.UTXO{}, nil, errDeclared
}

// Audit checks the funded output against the terms.
func Audit(ctx context.Context, node Node, party Party, terms Terms, txid string, vout uint32, blocksToLock int64) (Funded, error) {
	return Funded{}, errDeclared
}

// Claim takes the contract's output with the secret.
func Claim(ctx context.Context, node Node, party Party, terms Terms, txid string, vout uint32, preimage [32]byte, s Spend, inclusion int64, record Record) (*tx.Transaction, error) {
	return nil, errDeclared
}

// Refund takes the contract's output back for the payer.
func Refund(ctx context.Context, node Node, party Party, p Params, txid string, vout uint32, s Spend, record Record) (*tx.Transaction, error) {
	return nil, errDeclared
}

// SecretFromWitness returns the secret a claim's witness shows.
func SecretFromWitness(items [][]byte, p Params) ([32]byte, error) {
	return [32]byte{}, errDeclared
}

// FindSecret reads the secret from the mined spend of the contract's output.
func FindSecret(ctx context.Context, node Node, p Params, txid string, vout uint32, fromHeight int64) (secret [32]byte, spender string, err error) {
	return [32]byte{}, "", errDeclared
}
