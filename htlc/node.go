// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
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
)

// deployments are the node's names of the three height-gated deployments the
// contract needs: the witness version 6 script path, the key hash opcode and
// the control flow opcodes (src/versionbits.cpp, src/validation.cpp).
var deployments = [...]string{"p2wsh_dilithium", "dilithium_keyhash", "v6_controlflow"}

// keyHash returns the hash of the party's own key after the checks every
// function below starts with: the address is of the party's network and the
// key the signer holds for it passes KeyHash, which refuses a key the node
// refuses in every spend.
func (party Party) keyHash() ([32]byte, error) {
	if party.Signer == nil || party.Network.ChainID == "" {
		return [32]byte{}, fmt.Errorf("%w: a party names its network and its signer", ErrNetwork)
	}
	hrp, err := address.HRPOf(party.Address)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	if hrp != party.Network.HRP {
		return [32]byte{}, fmt.Errorf("%w: prefix %q, want %q", ErrNetwork, hrp, party.Network.HRP)
	}
	publicKey, err := party.Signer.PublicKeyFor(party.Address)
	if err != nil {
		return [32]byte{}, fmt.Errorf("public key for %s: %w", party.Address, err)
	}
	return KeyHash(publicKey)
}

// check returns the depth of the terms and the contract's output script
// after refusing parameters Validate refuses, an amount outside the node's
// range and a depth below the floor.
func (t Terms) check() (depth int64, script []byte, err error) {
	if script, err = t.Params.OutputScript(); err != nil {
		return 0, nil, err
	}
	if t.Amount <= 0 || t.Amount > tx.MaxMoney {
		return 0, nil, fmt.Errorf("%w: amount %d", tx.ErrInvalidAmount, t.Amount)
	}
	floor := MinDepth(t.Amount)
	switch {
	case t.Depth == 0:
		return floor, script, nil
	case t.Depth < floor:
		return 0, nil, fmt.Errorf("%w: depth %d, floor %d for %d shors", ErrDepth, t.Depth, floor, t.Amount)
	}
	return t.Depth, script, nil
}

// chain asks the node for its state and refuses a node on another chain than
// the party's (rpc.ErrWrongChain) and one that is in initial block download
// or behind its headers (rpc.ErrNodeSyncing).
func chain(ctx context.Context, node Node, network types.Network) (*rpc.BlockchainInfo, error) {
	info, err := node.GetBlockchainInfo(ctx)
	if err != nil {
		return nil, err
	}
	if info.Chain != network.ChainID {
		return nil, fmt.Errorf("%w: node reports %q, the party is on %q", rpc.ErrWrongChain, info.Chain, network.ChainID)
	}
	if info.InitialSync || info.Headers > info.Blocks {
		return nil, fmt.Errorf("%w (blocks %d, headers %d, initialblockdownload %v)",
			rpc.ErrNodeSyncing, info.Blocks, info.Headers, info.InitialSync)
	}
	return info, nil
}

// active requires the tip to be at least depth blocks above the activation
// height of each deployment the contract needs. A node that does not report
// one of the three is refused: nothing else says whether it runs the script.
// A deployment with no height is reported as 2147483647 and fails the
// comparison like any height the tip has not reached.
func active(info *rpc.BlockchainInfo, depth int64) error {
	for _, name := range deployments {
		height, ok := info.ActivationHeights[name]
		if !ok || height < 0 {
			return fmt.Errorf("%w: %s", ErrNoActivationHeights, name)
		}
		if info.Blocks-depth < height {
			return fmt.Errorf("%w: %s is active from height %d, the tip is at %d and the depth is %d",
				ErrNotActive, name, height, info.Blocks, depth)
		}
	}
	return nil
}

// output returns what the node holds at the outpoint, its mempool counted,
// and refuses an absent or spent output (ErrNotFunded) and one whose script
// is not want, the contract's 34 bytes (ErrFundedOutput).
func output(ctx context.Context, node Node, want []byte, txid string, vout uint32) (*rpc.TxOut, error) {
	out, err := node.GetTxOut(ctx, txid, vout, true)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("%w: %s:%d", ErrNotFunded, txid, vout)
	}
	if out.ScriptPubKey.Hex != hex.EncodeToString(want) {
		return nil, fmt.Errorf("%w: the script at %s:%d is not the one derived from the parameters", ErrFundedOutput, txid, vout)
	}
	return out, nil
}

// send hands the signed transaction to the record and then to the node.
func send(ctx context.Context, node Node, t *tx.Transaction, record Record) error {
	rawHex, txid := t.SerializeHex(), t.TxID()
	if record != nil {
		if err := record(txid, rawHex); err != nil {
			return fmt.Errorf("%w: %w", ErrRecord, err)
		}
	}
	_, err := node.Broadcast(ctx, rawHex, txid)
	return err
}

// Fund builds, signs and broadcasts the transaction that funds the contract
// and returns its output. The party is the payer.
//
// Below the height at which the node starts to run the contract's script, an
// output of this form is spendable by anyone, and a funding that a node took
// above the height can confirm below it when the tip moves back. Fund
// therefore sends nothing unless the node reports the activation height of
// each of the three deployments the contract needs and its tip is at least
// the depth of the terms above every one of them (ErrNotActive,
// ErrNoActivationHeights).
//
// Refused before the node is asked: a party whose own key begins 0xff
// (keys.ErrInvalidPublicKey) or does not hash to Params.PayerKeyHash
// (ErrWrongKey), since the payer of such a contract can never refund; terms
// with parameters Validate refuses, an amount outside the node's range or a
// depth below MinDepth; a change address of another network. Refused on the
// node's reply: another chain, a node that is not synchronised, the
// activation rule above, and a lock height fewer than LockMargin blocks above
// the tip (ErrLockTooNear). Then BuildFunding, and every input is signed and
// verified.
//
// record, when set, receives the signed transaction before it is sent. The
// transaction is returned whenever one was signed, also beside an error of
// the record or of the broadcast. After rpc.ErrUnknownOutcome the caller
// sends those bytes again and does not call Fund again: a second call builds
// a second funding. Fund cannot see an earlier funding of the same contract.
func Fund(ctx context.Context, node Node, party Party, terms Terms, f Funding, record Record) (types.UTXO, *tx.Transaction, error) {
	own, err := party.keyHash()
	if err != nil {
		return types.UTXO{}, nil, err
	}
	if own != terms.Params.PayerKeyHash {
		return types.UTXO{}, nil, fmt.Errorf("%w: the payer of the contract is not the party", ErrWrongKey)
	}
	depth, _, err := terms.check()
	if err != nil {
		return types.UTXO{}, nil, err
	}
	if err := sameNetwork(f.ChangeAddress, party.Address); err != nil {
		return types.UTXO{}, nil, fmt.Errorf("change address: %w", err)
	}
	info, err := chain(ctx, node, party.Network)
	if err != nil {
		return types.UTXO{}, nil, err
	}
	if err := active(info, depth); err != nil {
		return types.UTXO{}, nil, err
	}
	if left := int64(terms.Params.LockHeight) - info.Blocks; left < max(f.LockMargin, 1) {
		return types.UTXO{}, nil, fmt.Errorf("%w: %d blocks from the tip at %d, want %d", ErrLockTooNear, left, info.Blocks, max(f.LockMargin, 1))
	}
	t, err := BuildFunding(terms.Params, f.Inputs, terms.Amount, f.ChangeAddress, f.FeeRate)
	if err != nil {
		return types.UTXO{}, nil, err
	}
	if err := t.SignAll(party.Signer); err != nil {
		return types.UTXO{}, nil, err
	}
	if err := t.VerifyAll(); err != nil {
		return types.UTXO{}, nil, err
	}
	if err := send(ctx, node, t, record); err != nil {
		return types.UTXO{}, t, err
	}
	return types.UTXO{TxID: t.TxID(), Vout: 0, Value: terms.Amount}, t, nil
}

// Audit checks the output at an outpoint against the terms, as the party that
// is about to act on it does: the payee before it claims or funds its own
// side, the payer after it funded. It returns the output with the value the
// node holds.
//
// The party's own key must hash to one of the two key hashes of the
// parameters (ErrWrongKey), under the 0xff rule of KeyHash. The node must be
// on the party's chain and synchronised, and must satisfy the activation rule
// of Fund for the depth of the terms. The output must be unspent with no
// spend in the node's mempool (ErrNotFunded), carry exactly the 34 bytes
// derived from the parameters and a value at or above the amount
// (ErrFundedOutput), and have at least the depth in confirmations, a
// coinbase output at least the network's maturity as well (ErrShallow). The
// tip must leave at least blocksToLock blocks before the lock height
// (ErrLockTooNear). ErrTipMoved means the node's two replies were given at
// different tips; ask again.
//
// An address string a counterparty sent takes no part: the script is derived
// from the parameters and compared with the bytes the node holds.
func Audit(ctx context.Context, node Node, party Party, terms Terms, txid string, vout uint32, blocksToLock int64) (Funded, error) {
	own, err := party.keyHash()
	if err != nil {
		return Funded{}, err
	}
	if own != terms.Params.PayeeKeyHash && own != terms.Params.PayerKeyHash {
		return Funded{}, fmt.Errorf("%w: the contract names no key of the party", ErrWrongKey)
	}
	depth, script, err := terms.check()
	if err != nil {
		return Funded{}, err
	}
	return audit(ctx, node, party.Network, terms, depth, script, txid, vout, blocksToLock)
}

func audit(ctx context.Context, node Node, network types.Network, terms Terms, depth int64, script []byte, txid string, vout uint32, blocksToLock int64) (Funded, error) {
	info, err := chain(ctx, node, network)
	if err != nil {
		return Funded{}, err
	}
	if err := active(info, depth); err != nil {
		return Funded{}, err
	}
	out, err := output(ctx, node, script, txid, vout)
	if err != nil {
		return Funded{}, err
	}
	if out.BestBlock != info.BestBlockHash {
		return Funded{}, ErrTipMoved
	}
	if out.Value < terms.Amount {
		return Funded{}, fmt.Errorf("%w: value %d, agreed %d", ErrFundedOutput, out.Value, terms.Amount)
	}
	need := depth
	if out.Coinbase {
		maturity := network.CoinbaseMaturity
		if maturity <= 0 {
			maturity = types.Mainnet.CoinbaseMaturity
		}
		need = max(need, maturity)
	}
	if out.Confirmations < need {
		return Funded{}, fmt.Errorf("%w: %d confirmations, want %d", ErrShallow, out.Confirmations, need)
	}
	if left := int64(terms.Params.LockHeight) - info.Blocks; left < blocksToLock {
		return Funded{}, fmt.Errorf("%w: %d blocks from the tip at %d, want %d", ErrLockTooNear, left, info.Blocks, blocksToLock)
	}
	return Funded{
		Output:        types.UTXO{TxID: txid, Vout: vout, Value: out.Value, Height: info.Blocks - out.Confirmations + 1},
		Confirmations: out.Confirmations,
		Tip:           info.Blocks,
	}, nil
}

// Claim audits the contract's output, then builds, signs and broadcasts the
// claim that takes it with the secret. The party is the payee.
//
// A claim shows the secret to every node it reaches, so each check comes
// before the signer is asked: the party's own key under the 0xff rule and
// against Params.PayeeKeyHash (ErrWrongKey); the preimage against Params.Hash
// (ErrPreimage); every check of Audit, with depth + inclusion as the blocks
// the tip must leave before the lock height. inclusion is the number of
// blocks allowed for the claim to get into a block, at least 1; the claim
// then lies at least the depth below the lock height, from which the payer's
// refund is valid. Past that point Claim refuses (ErrLockTooNear) and the
// secret stays private.
//
// The claim is built by BuildClaim on the value the node holds. record, when
// set, receives it before it is sent; its bytes hold the secret. The
// transaction is returned whenever one was signed. A claim does not signal
// replacement: its fee is raised by a transaction that spends its output.
func Claim(ctx context.Context, node Node, party Party, terms Terms, txid string, vout uint32, preimage [32]byte, s Spend, inclusion int64, record Record) (*tx.Transaction, error) {
	own, err := party.keyHash()
	if err != nil {
		return nil, err
	}
	if own != terms.Params.PayeeKeyHash {
		return nil, fmt.Errorf("%w: the payee of the contract is not the party", ErrWrongKey)
	}
	if sha256.Sum256(preimage[:]) != terms.Params.Hash {
		return nil, ErrPreimage
	}
	depth, script, err := terms.check()
	if err != nil {
		return nil, err
	}
	if inclusion < 1 {
		return nil, fmt.Errorf("%w: inclusion of %d blocks, want 1 or more", ErrLockTooNear, inclusion)
	}
	funded, err := audit(ctx, node, party.Network, terms, depth, script, txid, vout, depth+inclusion)
	if err != nil {
		return nil, err
	}
	t, err := BuildClaim(terms.Params, funded.Output, preimage, s.PayToAddress, s.FeeRate, party.Signer, party.Address)
	if err != nil {
		return nil, err
	}
	return t, send(ctx, node, t, record)
}

// Refund builds, signs and broadcasts the refund that takes the contract's
// output back. The party is the payer.
//
// Refused: a party whose own key is not the payer's (ErrWrongKey); a node on
// another chain or not synchronised; a tip below the lock height
// (ErrBeforeLockHeight), since a node takes a refund into its mempool from
// the lock height on and the first block that can hold it is the one after;
// an outpoint with no unspent output or with a spend in the mempool
// (ErrNotFunded); an output with another script (ErrFundedOutput). The refund
// is built by BuildRefund on the value the node holds. It shows no secret, so
// no depth and no activation rule is asked of it.
func Refund(ctx context.Context, node Node, party Party, p Params, txid string, vout uint32, s Spend, record Record) (*tx.Transaction, error) {
	own, err := party.keyHash()
	if err != nil {
		return nil, err
	}
	if own != p.PayerKeyHash {
		return nil, fmt.Errorf("%w: the payer of the contract is not the party", ErrWrongKey)
	}
	script, err := p.OutputScript()
	if err != nil {
		return nil, err
	}
	info, err := chain(ctx, node, party.Network)
	if err != nil {
		return nil, err
	}
	if info.Blocks < int64(p.LockHeight) {
		return nil, fmt.Errorf("%w: tip %d, lock height %d", ErrBeforeLockHeight, info.Blocks, p.LockHeight)
	}
	out, err := output(ctx, node, script, txid, vout)
	if err != nil {
		return nil, err
	}
	t, err := BuildRefund(p, types.UTXO{TxID: txid, Vout: vout, Value: out.Value}, s.PayToAddress, s.FeeRate, party.Signer, party.Address)
	if err != nil {
		return nil, err
	}
	return t, send(ctx, node, t, record)
}

// FindSecret reads the blocks from fromHeight to the node's tip for the
// transaction that spends the outpoint and returns the secret its witness
// shows (SecretFromWitness) with the transaction's id. A spend that shows no
// secret, a refund, gives ErrNoSecret with the id; no spend in the range
// gives ErrNoSpend. The node's mempool is not read.
//
// The secret is accepted by its hash alone, so a node cannot supply a wrong
// one. A node can leave a spend out: ErrNoSpend from a node that is behind
// says nothing about the chain.
func FindSecret(ctx context.Context, node Node, p Params, txid string, vout uint32, fromHeight int64) (secret [32]byte, spender string, err error) {
	info, err := node.GetBlockchainInfo(ctx)
	if err != nil {
		return [32]byte{}, "", err
	}
	for height := fromHeight; height <= info.Blocks; height++ {
		hash, err := node.GetBlockHash(ctx, height)
		if err != nil {
			return [32]byte{}, "", err
		}
		raw, err := node.GetBlock(ctx, hash, 2)
		if err != nil {
			return [32]byte{}, "", err
		}
		var block struct {
			Tx []struct {
				TxID string `json:"txid"`
				Vin  []struct {
					TxID    string   `json:"txid"`
					Vout    uint32   `json:"vout"`
					Witness []string `json:"txinwitness"`
				} `json:"vin"`
			} `json:"tx"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return [32]byte{}, "", fmt.Errorf("block %d: %w", height, err)
		}
		for _, t := range block.Tx {
			for _, in := range t.Vin {
				if in.TxID != txid || in.Vout != vout {
					continue
				}
				items := make([][]byte, len(in.Witness))
				for i, item := range in.Witness {
					if items[i], err = hex.DecodeString(item); err != nil {
						return [32]byte{}, t.TxID, fmt.Errorf("witness item %d of %s: %w", i, t.TxID, err)
					}
				}
				secret, err := SecretFromWitness(items, p)
				return secret, t.TxID, err
			}
		}
	}
	return [32]byte{}, "", fmt.Errorf("%w: %s:%d from height %d to %d", ErrNoSpend, txid, vout, fromHeight, info.Blocks)
}
