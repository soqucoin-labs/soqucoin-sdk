package tx

import (
	"errors"
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Limits of a batch payment.
const (
	// MaxStandardTxWeight is the node's relay limit on one transaction
	// (src/policy/policy.h, MAX_STANDARD_TX_WEIGHT). A heavier transaction is
	// refused by every node with default policy.
	MaxStandardTxWeight = 800_000

	// MaxBatchOutputs is the most recipient outputs one batch payment carries.
	// With the selector's 80-input cap (utxo.MaxInputsPerTX) a full batch
	// weighs 80 x 3,905 + 501 x 172 + 50 = 398,622 WU, half the relay limit,
	// and costs about 1 SOQ at the recommended rate, half of MaxFeeShors.
	MaxBatchOutputs = 500
)

// Batch errors. Both are permanent: the request itself is wrong.
var (
	// ErrNoOutputs is returned for a batch with no recipient, or with more
	// than MaxBatchOutputs.
	ErrNoOutputs = errors.New("tx: a batch pays between 1 and MaxBatchOutputs recipients")
	// ErrTooHeavy is returned for a transaction above MaxStandardTxWeight.
	ErrTooHeavy = errors.New("tx: transaction exceeds the node's standard weight limit")
)

// BatchFeeAllowance is the fee the recipient outputs of a batch add over a
// one-recipient payment, at feeRate (shors per vByte): each further output is
// 43 bytes for a witness version 1 address. A coin selector that budgets the
// fee of a one-recipient payment is asked for the batch total plus this.
func BatchFeeAllowance(recipients int, feeRate int64) int64 {
	if recipients <= 1 || feeRate <= 0 {
		return 0
	}
	return int64(recipients-1) * (EstimatedOutputWeight / 4) * feeRate
}

// BuildBatchTransaction constructs an unsigned transaction that pays every
// recipient in outputs, in the order given, and returns the remainder to
// changeScriptPubKey as the last output. One transaction pays the whole
// batch or nothing is built: no amount is reduced and no recipient is left
// out.
//
// The fee is measured on the real serialization with the change output
// present, at feeRate (shors per vByte). A remainder below the relay floor
// for the change script is left to the miner as fee, as in
// BuildSendTransaction.
//
// Refused: no recipient or more than MaxBatchOutputs (ErrNoOutputs); an
// amount outside (0, MaxMoney] or a total above MaxMoney (ErrInvalidAmount);
// an amount below the relay floor for its script (ErrBelowDust, naming the
// output); inputs on different networks; a fee outside the sanity limits
// (ErrFeeTooHigh); inputs worth less than the total plus the fee
// (ErrInsufficientFunds); a transaction above MaxStandardTxWeight
// (ErrTooHeavy).
func BuildBatchTransaction(
	inputs []types.UTXO,
	outputs []TxOutput,
	changeScriptPubKey []byte,
	feeRate int64,
) (*Transaction, error) {
	if len(outputs) == 0 || len(outputs) > MaxBatchOutputs {
		return nil, fmt.Errorf("%w: %d given", ErrNoOutputs, len(outputs))
	}
	if err := requireSameNetwork(inputs); err != nil {
		return nil, err
	}
	var total int64
	for i, o := range outputs {
		if err := checkAmount(o.Value); err != nil {
			return nil, fmt.Errorf("output %d: %w", i, err)
		}
		if floor := MinOutputValue(o.ScriptPubKey); o.Value < floor {
			return nil, fmt.Errorf("%w: output %d: %d shors, floor %d", ErrBelowDust, i, o.Value, floor)
		}
		if total > MaxMoney-o.Value {
			return nil, fmt.Errorf("%w: the batch total exceeds MaxMoney at output %d", ErrInvalidAmount, i)
		}
		total += o.Value
	}
	totalInput, err := sumInputs(inputs)
	if err != nil {
		return nil, err
	}

	tx := NewTransaction()
	for _, u := range inputs {
		inputSPK, err := inputScriptPubKey(u.Address)
		if err != nil {
			return nil, err
		}
		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add input: %w", err)
		}
	}
	for _, o := range outputs {
		tx.AddOutput(o.Value, o.ScriptPubKey)
	}

	// Measured with the change output present; if the change is later folded
	// away the transaction only gets lighter and the fee paid only grows.
	tx.AddOutput(0, changeScriptPubKey)
	if w := tx.EstimateWeight(); w > MaxStandardTxWeight {
		return nil, fmt.Errorf("%w: %d WU with %d inputs and %d outputs, limit %d",
			ErrTooHeavy, w, len(inputs), len(outputs), MaxStandardTxWeight)
	}
	fee := tx.EstimateFee(feeRate)
	tx.Outputs = tx.Outputs[:len(outputs)]
	if err := checkFee(fee, feeRate); err != nil {
		return nil, err
	}

	change := totalInput - total - fee
	if change < 0 {
		return nil, fmt.Errorf("%w: inputs=%d, batch total=%d, fee=%d",
			ErrInsufficientFunds, totalInput, total, fee)
	}
	if change >= MinOutputValue(changeScriptPubKey) {
		tx.AddOutput(change, changeScriptPubKey)
	}
	return tx, nil
}

// BuildSignedBatch is BuildBatchTransaction followed by SignAll and
// VerifyAll: every input is signed with SIGHASH_ALL and verified as the node
// verifies it before the transaction is returned. Output i pays outputs[i];
// the change output, when there is one, is the last.
func BuildSignedBatch(
	inputs []types.UTXO,
	outputs []TxOutput,
	changeScriptPubKey []byte,
	feeRate int64,
	signer Signer,
) (*Transaction, error) {
	t, err := BuildBatchTransaction(inputs, outputs, changeScriptPubKey, feeRate)
	if err != nil {
		return nil, err
	}
	if err := t.SignAll(signer); err != nil {
		return nil, err
	}
	if err := t.VerifyAll(); err != nil {
		return nil, err
	}
	return t, nil
}

// BuildAndSignBatch builds, signs and serializes a batch payment in one call,
// returning the raw hex ready for sendrawtransaction and the transaction id.
// It is BuildSignedBatch followed by SerializeHex and TxID.
func BuildAndSignBatch(
	inputs []types.UTXO,
	outputs []TxOutput,
	changeScriptPubKey []byte,
	feeRate int64,
	signer Signer,
) (rawHex string, txid string, err error) {
	t, err := BuildSignedBatch(inputs, outputs, changeScriptPubKey, feeRate, signer)
	if err != nil {
		return "", "", err
	}
	return t.SerializeHex(), t.TxID(), nil
}
