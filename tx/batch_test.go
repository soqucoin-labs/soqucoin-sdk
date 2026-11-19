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

func batchOutputs(n int, value int64) []TxOutput {
	out := make([]TxOutput, n)
	for i := range out {
		out[i] = TxOutput{Value: value, ScriptPubKey: ScriptP2WPKH(hash32(byte(0x40 + i%64)))}
	}
	return out
}

// What a batch must never do: pay a recipient less than asked, leave one out,
// or build something the node refuses. Each case is refused whole.
func TestBatchRefusals(t *testing.T) {
	change := ScriptP2WPKH(hash32(0x03))
	one := []types.UTXO{realUTXO(t, 0x10, 0, 50_000_000_000)}
	floor := MinOutputValue(ScriptP2WPKH(hash32(0x40)))

	dust := batchOutputs(3, 1_000_000_000)
	dust[1].Value = floor - 1
	zero := batchOutputs(3, 1_000_000_000)
	zero[2].Value = 0
	negative := batchOutputs(2, 1_000_000_000)
	negative[0].Value = -1
	overMoney := batchOutputs(2, MaxMoney)

	cases := []struct {
		name    string
		inputs  []types.UTXO
		outputs []TxOutput
		rate    int64
		want    error
	}{
		{"no recipient", one, nil, 1000, ErrNoOutputs},
		{"one more recipient than the cap", one, batchOutputs(MaxBatchOutputs+1, 1_000_000), 1000, ErrNoOutputs},
		{"one amount under the relay floor", one, dust, 1000, ErrBelowDust},
		{"one amount of zero", one, zero, 1000, ErrInvalidAmount},
		{"one negative amount", one, negative, 1000, ErrInvalidAmount},
		{"total above MaxMoney", one, overMoney, 1000, ErrInvalidAmount},
		{"inputs short of the total by the fee", []types.UTXO{realUTXO(t, 0x10, 0, 3_000_000_000)}, batchOutputs(3, 1_000_000_000), 1000, ErrInsufficientFunds},
		{"no inputs", nil, batchOutputs(2, 1_000_000_000), 1000, ErrInsufficientFunds},
		{"zero rate", one, batchOutputs(2, 1_000_000_000), 0, ErrInvalidAmount},
		{"rate above the cap", one, batchOutputs(2, 1_000_000_000), MaxFeeRateShorsPerVB + 1, ErrFeeTooHigh},
		{"a recipient with no script", one, []TxOutput{{Value: 1_000_000_000}}, 1000, ErrEmptyScript},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, err := BuildBatchTransaction(c.inputs, c.outputs, change, c.rate)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
			if tr != nil {
				t.Error("a refused batch returned a transaction")
			}
		})
	}

	if _, err := BuildBatchTransaction(one, batchOutputs(2, 1_000_000_000), nil, 1000); !errors.Is(err, ErrEmptyScript) {
		t.Errorf("no change script: err = %v, want ErrEmptyScript", err)
	}

	sq, err := address.Encode(types.Mainnet.HRP, 1, hash32(0x20))
	if err != nil {
		t.Fatal(err)
	}
	mixed := []types.UTXO{one[0], {TxID: displayTxID, Vout: 1, Value: 1_000_000_000, Address: sq}}
	if _, err := BuildBatchTransaction(mixed, batchOutputs(2, 1_000_000_000), change, 1000); err == nil {
		t.Error("inputs on two networks were accepted")
	}
}

// Output i pays recipient i the amount asked, the change is last, and the
// inputs equal the outputs plus the fee charged for the bytes built.
func TestBatchPaysEveryRecipientExactlyInOrder(t *testing.T) {
	const rate = int64(1000)
	change := ScriptP2WPKH(hash32(0x03))
	inputs := []types.UTXO{
		realUTXO(t, 0x10, 0, 40_000_000_000),
		realUTXO(t, 0x11, 1, 30_000_000_000),
	}
	outputs := batchOutputs(25, 0)
	var total int64
	for i := range outputs {
		outputs[i].Value = int64(1_000_000_000 + i*7_000_003)
		total += outputs[i].Value
	}

	tr, err := BuildBatchTransaction(inputs, outputs, change, rate)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Outputs) != len(outputs)+1 {
		t.Fatalf("outputs = %d, want %d recipients and the change", len(tr.Outputs), len(outputs))
	}
	for i, o := range outputs {
		if tr.Outputs[i].Value != o.Value || string(tr.Outputs[i].ScriptPubKey) != string(o.ScriptPubKey) {
			t.Errorf("output %d is not recipient %d as given", i, i)
		}
	}
	last := tr.Outputs[len(outputs)]
	if string(last.ScriptPubKey) != string(change) {
		t.Error("the last output is not the change")
	}
	fee := tr.EstimateFee(rate)
	if got := int64(70_000_000_000) - total - last.Value; got != fee {
		t.Errorf("fee paid %d, fee measured on the built bytes %d", got, fee)
	}
	if fee != (tr.VSize()+FeeMarginVBytes)*rate {
		t.Errorf("fee %d is not (vsize %d + %d) x %d", fee, tr.VSize(), FeeMarginVBytes, rate)
	}
}

// A remainder under the relay floor is left as fee and no output carries it;
// a remainder at the floor is the change output.
func TestBatchChangeAtTheRelayFloor(t *testing.T) {
	const rate = int64(1000)
	change := ScriptP2WPKH(hash32(0x03))
	outputs := batchOutputs(4, 1_000_000_000)
	probe, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, 10_000_000_000)}, outputs, change, rate)
	if err != nil {
		t.Fatal(err)
	}
	fee := probe.EstimateFee(rate)
	floor := MinOutputValue(change)

	at, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, 4_000_000_000+fee+floor)}, outputs, change, rate)
	if err != nil {
		t.Fatal(err)
	}
	if len(at.Outputs) != 5 || at.Outputs[4].Value != floor {
		t.Errorf("change at the floor: %d outputs, want 5 with the last worth %d", len(at.Outputs), floor)
	}
	under, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, 4_000_000_000+fee+floor-1)}, outputs, change, rate)
	if err != nil {
		t.Fatal(err)
	}
	if len(under.Outputs) != 4 {
		t.Errorf("change under the floor: %d outputs, want the 4 recipients only", len(under.Outputs))
	}
	exact, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, 4_000_000_000+fee)}, outputs, change, rate)
	if err != nil {
		t.Fatalf("inputs worth exactly the total and the fee were refused: %v", err)
	}
	if len(exact.Outputs) != 4 {
		t.Errorf("no remainder: %d outputs, want 4", len(exact.Outputs))
	}
	// One shor short of the total and the fee is refused, not paid from the fee.
	if _, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, 4_000_000_000+fee-1)}, outputs, change, rate); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("inputs one shor short: err = %v, want ErrInsufficientFunds", err)
	}
}

// The largest batch the limits allow, 80 inputs and MaxBatchOutputs
// recipients, is under the node's weight limit and under the fee limit at the
// recommended rate, so neither refusal can be reached through the selector's
// input cap. BatchFeeAllowance is the fee the further outputs add.
func TestLargestBatchIsStandardAndTheAllowanceIsExact(t *testing.T) {
	change := ScriptP2WPKH(hash32(0x03))
	var inputs []types.UTXO
	for i := 0; i < 80; i++ {
		inputs = append(inputs, realUTXO(t, 0x10, uint32(i), 1_000_000_000))
	}
	outputs := batchOutputs(MaxBatchOutputs, 100_000_000)
	tr, err := BuildBatchTransaction(inputs, outputs, change, types.RecommendedFeeRate)
	if err != nil {
		t.Fatalf("the largest batch was refused: %v", err)
	}
	if w := tr.EstimateWeight(); w != 80*EstimatedInputWeight+(MaxBatchOutputs+1)*EstimatedOutputWeight+TxOverheadWeight+2*4 {
		// 2 further bytes: the output count is a three-byte varint above 252.
		t.Errorf("weight %d is not the documented sum", w)
	}
	if w := tr.EstimateWeight(); w > MaxStandardTxWeight/2 {
		t.Errorf("weight %d is over half the relay limit %d", w, MaxStandardTxWeight)
	}
	if fee := tr.EstimateFee(types.RecommendedFeeRate); fee > MaxFeeShors {
		t.Errorf("fee %d exceeds MaxFeeShors %d", fee, MaxFeeShors)
	}

	one := []types.UTXO{realUTXO(t, 0x10, 0, 50_000_000_000)}
	single, err := BuildBatchTransaction(one, batchOutputs(1, 1_000_000_000), change, 1000)
	if err != nil {
		t.Fatal(err)
	}
	many, err := BuildBatchTransaction(one, batchOutputs(40, 1_000_000_000), change, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := many.EstimateFee(1000)-single.EstimateFee(1000), BatchFeeAllowance(40, 1000); got != want {
		t.Errorf("40 recipients cost %d more than one, BatchFeeAllowance says %d", got, want)
	}
	if BatchFeeAllowance(1, 1000) != 0 || BatchFeeAllowance(0, 1000) != 0 || BatchFeeAllowance(5, 0) != 0 {
		t.Error("BatchFeeAllowance is not zero for one recipient, none, or no rate")
	}

	// A one-recipient batch is the same bytes as the one-recipient builder.
	send, err := BuildSendTransaction(one, outputs[0].ScriptPubKey, 1_000_000_000, change, 1000)
	if err != nil {
		t.Fatal(err)
	}
	same, err := BuildBatchTransaction(one, []TxOutput{{Value: 1_000_000_000, ScriptPubKey: outputs[0].ScriptPubKey}}, change, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if send.SerializeHex() != same.SerializeHex() {
		t.Error("a one-recipient batch differs from BuildSendTransaction on the same request")
	}
}

// BuildSignedBatch returns a transaction every input of which verifies as the
// node verifies it, and BuildAndSignBatch returns its bytes and id.
func TestBuildSignedBatchVerifiesEveryInput(t *testing.T) {
	m := keys.NewManager(filepath.Join(t.TempDir(), "k.enc"), "pw")
	kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	var inputs []types.UTXO
	for i := 0; i < 3; i++ {
		inputs = append(inputs, types.UTXO{TxID: displayTxID, Vout: uint32(i), Value: 10_000_000_000, Address: kp.Address})
	}
	change, _ := address.ScriptFor(kp.Address)
	outputs := batchOutputs(12, 1_500_000_000)

	tr, err := BuildSignedBatch(inputs, outputs, change, types.RecommendedFeeRate, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.VerifyAll(); err != nil {
		t.Errorf("VerifyAll on the returned batch: %v", err)
	}
	if got, want := 3*len(tr.serializeNoWitness())+len(tr.Serialize()), tr.EstimateWeight(); got != want {
		t.Errorf("serialized weight %d, estimate %d: the fee was measured on other bytes", got, want)
	}
	rawHex, txid, err := BuildAndSignBatch(inputs, outputs, change, types.RecommendedFeeRate, m)
	if err != nil {
		t.Fatal(err)
	}
	if rawHex == "" || txid != tr.TxID() {
		t.Errorf("BuildAndSignBatch txid %s, BuildSignedBatch txid %s", txid, tr.TxID())
	}

	// A signature or a key the node would refuse is an error here.
	if _, err := BuildSignedBatch(inputs, outputs, change, types.RecommendedFeeRate, badSigner{Signer: m, flipSig: true}); !errors.Is(err, ErrSignature) {
		t.Errorf("corrupt signature: err = %v, want ErrSignature", err)
	}
	other, _ := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if _, err := BuildSignedBatch(inputs, outputs, change, types.RecommendedFeeRate, badSigner{Signer: m, otherKey: other.PublicKey}); !errors.Is(err, ErrWrongKey) {
		t.Errorf("other key: err = %v, want ErrWrongKey", err)
	}

	// A signer that holds no key for the inputs is an error, not a transaction.
	empty := keys.NewManager(filepath.Join(t.TempDir(), "e.enc"), "pw")
	if _, err := BuildSignedBatch(inputs, outputs, change, types.RecommendedFeeRate, empty); err == nil {
		t.Error("a signer without the key produced a batch")
	}
}

// A wallet that holds the batch total, the fee of a one-recipient payment and
// BatchFeeAllowance funds the batch at every size. From 252 recipients the
// output count takes three bytes, and the allowance carries those two bytes.
func TestBatchFeeAllowanceFundsTheBatchAtEverySize(t *testing.T) {
	change := ScriptP2WPKH(hash32(0x03))
	const amount, rate = 1_000_000, 1000
	single, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, 50_000_000_000)}, batchOutputs(1, amount), change, rate)
	if err != nil {
		t.Fatal(err)
	}
	oneFee := single.EstimateFee(rate)
	for _, n := range []int{2, 251, 252, 253, MaxBatchOutputs} {
		funds := int64(n)*amount + oneFee + BatchFeeAllowance(n, rate)
		tr, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, funds)}, batchOutputs(n, amount), change, rate)
		if err != nil {
			t.Errorf("%d recipients with the total, the one-recipient fee and the allowance: %v", n, err)
			continue
		}
		if len(tr.Outputs) != n {
			t.Errorf("%d recipients: %d outputs, want no change from exact funds", n, len(tr.Outputs))
		}
		if _, err := BuildBatchTransaction([]types.UTXO{realUTXO(t, 0x10, 0, funds-1)}, batchOutputs(n, amount), change, rate); !errors.Is(err, ErrInsufficientFunds) {
			t.Errorf("%d recipients, one shor under: err = %v, want ErrInsufficientFunds (the allowance is not exact)", n, err)
		}
	}
}
