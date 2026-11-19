// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// ErrNoSecret marks a witness that holds no 32-byte item hashing to the
// contract's hash, as the witness of a refund does not.
var ErrNoSecret = errors.New("htlc: the witness shows no secret of this contract")

// MinDepth returns the least number of confirmations a swap of the given
// amount waits for on Soqucoin: 30 up to 100 SOQ, 120 up to 10,000 SOQ and
// 288, the chain's reorganisation depth limit, above. A party may wait longer
// and never less.
func MinDepth(amount int64) int64 {
	switch {
	case amount <= 100*types.ShorsPerSOQ:
		return 30
	case amount <= 10_000*types.ShorsPerSOQ:
		return 120
	default:
		return types.MaxReorgDepth
	}
}

// BuildFunding returns the unsigned transaction that pays amount to the
// contract's output from inputs, with the remainder above the fee to
// changeAddress. The contract is output 0. The inputs are payment outputs and
// are signed with Transaction.SignAll.
//
// Refused: parameters Validate refuses; a change address that is not a
// payment address (ErrDestination) or is of another network than the first
// input (ErrNetwork); and what tx.BuildSendTransaction refuses, among it
// inputs of more than one network, an amount below the relay floor of the
// output, a fee outside the limits of package tx and inputs worth less than
// the amount and the fee.
//
// The function builds in every state of the chain, as Params.OutputScript
// does, and cannot see whether the node runs the contract's script yet. Fund
// carries that check; a caller that broadcasts this transaction itself has
// made the check of Fund first.
func BuildFunding(p Params, inputs []types.UTXO, amount int64, changeAddress string, feeRate int64) (*tx.Transaction, error) {
	contract, err := p.OutputScript()
	if err != nil {
		return nil, err
	}
	change, err := address.ScriptFor(changeAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: change address: %w", ErrDestination, err)
	}
	if len(inputs) > 0 {
		if err := sameNetwork(changeAddress, inputs[0].Address); err != nil {
			return nil, fmt.Errorf("change address: %w", err)
		}
	}
	return tx.BuildSendTransaction(inputs, contract, amount, change, feeRate)
}

// sameNetwork refuses two addresses with different prefixes.
func sameNetwork(a, b string) error {
	ha, err := address.HRPOf(a)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	hb, err := address.HRPOf(b)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	if ha != hb {
		return fmt.Errorf("%w: prefix %q, want %q", ErrNetwork, ha, hb)
	}
	return nil
}

// SecretFromWitness returns the secret that the witness of a claim shows:
// the item of exactly 32 bytes whose SHA-256 is p.Hash, wherever it stands
// among the items. That comparison is the whole acceptance, so the result is
// the secret whoever supplied the items. A witness with no such item, as a
// refund's, gives ErrNoSecret.
func SecretFromWitness(items [][]byte, p Params) ([32]byte, error) {
	for _, item := range items {
		if len(item) == sha256.Size && sha256.Sum256(item) == p.Hash {
			return [32]byte(item), nil
		}
	}
	return [32]byte{}, ErrNoSecret
}
