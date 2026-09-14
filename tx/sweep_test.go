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

// A sweep has one output worth the inputs less the fee, and the fee is the
// one charged for the bytes actually built: (vsize + margin) x rate on the
// one-output form, not on a two-output form with a change output it never has.
func TestSweepOutputIsTotalLessTheMeasuredFee(t *testing.T) {
	const rate = int64(1000)
	inputs := []types.UTXO{
		realUTXO(t, 0x10, 0, 400_000_000),
		realUTXO(t, 0x10, 1, 300_000_000),
		realUTXO(t, 0x11, 2, 200_000_000),
	}
	dest := ScriptP2WPKH(hash32(0x02))
	tr, err := BuildSweepTransaction(inputs, dest, rate)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(tr.Outputs))
	}
	fee := tr.EstimateFee(rate) // the same shape it was measured on
	if want := int64(900_000_000) - fee; tr.Outputs[0].Value != want {
		t.Errorf("output = %d, want total - fee = %d", tr.Outputs[0].Value, want)
	}
	if fee != (tr.VSize()+FeeMarginVBytes)*rate {
		t.Errorf("fee %d is not (vsize %d + %d) x %d", fee, tr.VSize(), FeeMarginVBytes, rate)
	}

	// Against the two-output builder on the same inputs: the sweep's fee is
	// smaller by exactly one 43-byte output (172 WU, 43 vB).
	send, err := BuildSendTransaction(inputs, dest, 100_000_000, ScriptP2WPKH(hash32(0x03)), rate)
	if err != nil {
		t.Fatal(err)
	}
	sendFee := int64(900_000_000) - send.Outputs[0].Value - send.Outputs[1].Value
	if sendFee-fee != 43*rate {
		t.Errorf("sweep fee %d, two-output fee %d: difference %d, want one output, %d", fee, sendFee, sendFee-fee, 43*rate)
	}
}

func TestSweepRefusals(t *testing.T) {
	dest := ScriptP2WPKH(hash32(0x02))
	one := []types.UTXO{realUTXO(t, 0x10, 0, 5_000_000_000)}
	// A one-input sweep at 1000 shors/vB costs (1030 + 1) x 1000 shors; the
	// figure is read off the builder so the cases below track it.
	probe, err := BuildSweepTransaction(one, dest, 1000)
	if err != nil {
		t.Fatal(err)
	}
	fee := int64(5_000_000_000) - probe.Outputs[0].Value

	cases := []struct {
		name   string
		inputs []types.UTXO
		rate   int64
		want   error
	}{
		{"no inputs", nil, 1000, ErrInsufficientFunds},
		{"worth exactly the fee", []types.UTXO{realUTXO(t, 0x10, 0, fee)}, 1000, ErrInsufficientFunds},
		{"worth less than the fee", []types.UTXO{realUTXO(t, 0x10, 0, fee-1)}, 1000, ErrInsufficientFunds},
		{"remainder under the relay floor", []types.UTXO{realUTXO(t, 0x10, 0, fee+MinOutputValue(dest)-1)}, 1000, ErrBelowDust},
		{"zero rate", one, 0, ErrInvalidAmount},
		{"rate above the cap", one, MaxFeeRateShorsPerVB + 1, ErrFeeTooHigh},
	}
	// 80 inputs at 3,000 shors/vB is about 2.35 SOQ of fee, over MaxFeeShors.
	var eighty []types.UTXO
	for i := 0; i < 80; i++ {
		eighty = append(eighty, realUTXO(t, 0x10, uint32(i), 1_000_000_000))
	}
	cases = append(cases, struct {
		name   string
		inputs []types.UTXO
		rate   int64
		want   error
	}{"fee above MaxFeeShors", eighty, 3000, ErrFeeTooHigh})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildSweepTransaction(c.inputs, dest, c.rate)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}

	// The remainder exactly at the floor is accepted.
	if _, err := BuildSweepTransaction([]types.UTXO{realUTXO(t, 0x10, 0, fee+MinOutputValue(dest))}, dest, 1000); err != nil {
		t.Errorf("remainder at the floor refused: %v", err)
	}

	// Inputs on two networks are refused before anything is built.
	sq, err := address.Encode(types.Mainnet.HRP, 1, hash32(0x20))
	if err != nil {
		t.Fatal(err)
	}
	mixed := []types.UTXO{one[0], {TxID: displayTxID, Vout: 1, Value: 1_000_000_000, Address: sq}}
	if _, err := BuildSweepTransaction(mixed, dest, 1000); err == nil {
		t.Error("inputs on two networks were accepted")
	}
}

// BuildSignedSweep returns a transaction the node would accept, and refuses a
// signer whose signature or key the node would refuse.
func TestBuildSignedSweepVerifiesEveryInput(t *testing.T) {
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
		inputs = append(inputs, types.UTXO{TxID: displayTxID, Vout: uint32(i), Value: 1_000_000_000, Address: kp.Address})
	}
	dest, _ := address.ScriptFor(kp.Address)

	tr, err := BuildSignedSweep(inputs, dest, types.RecommendedFeeRate, m)
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range tr.Inputs {
		if len(in.WitnessData) != 2 {
			t.Fatalf("input %d: witness items = %d, want 2", i, len(in.WitnessData))
		}
	}
	if err := tr.VerifyAll(); err != nil {
		t.Errorf("VerifyAll on the returned sweep: %v", err)
	}
	if got, want := 3*len(tr.serializeNoWitness())+len(tr.Serialize()), tr.EstimateWeight(); got != want {
		t.Errorf("serialized weight %d, estimate %d: the fee was measured on other bytes", got, want)
	}

	if _, err := BuildSignedSweep(inputs, dest, types.RecommendedFeeRate, badSigner{Signer: m, flipSig: true}); !errors.Is(err, ErrSignature) {
		t.Errorf("corrupt signature: err = %v, want ErrSignature", err)
	}
	other, _ := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if _, err := BuildSignedSweep(inputs, dest, types.RecommendedFeeRate, badSigner{Signer: m, otherKey: other.PublicKey}); !errors.Is(err, ErrWrongKey) {
		t.Errorf("other key: err = %v, want ErrWrongKey", err)
	}
}
