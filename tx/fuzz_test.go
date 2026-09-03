package tx

import (
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// FuzzWeightMatchesSerialization: for any number of inputs and outputs and any
// values the builder accepts, the weight charged for before signing must equal
// the weight of the signed bytes. Run with:
//
//	go test ./tx -run '^$' -fuzz FuzzWeightMatchesSerialization -fuzztime 60s
func FuzzWeightMatchesSerialization(f *testing.F) {
	f.Add(uint8(1), uint8(2), int64(1_000_000_000), uint32(0))
	f.Add(uint8(80), uint8(2), int64(5_000_000), uint32(7))
	f.Add(uint8(3), uint8(1), int64(300_000), uint32(0xffffffff))
	f.Fuzz(func(t *testing.T, nIn, nOut uint8, value int64, seq uint32) {
		if nIn == 0 || nIn > 253 || nOut == 0 || nOut > 16 || value <= 0 || value > MaxMoney/300 {
			return
		}
		tr := NewTransaction()
		for i := 0; i < int(nIn); i++ {
			u := types.UTXO{TxID: displayTxID, Vout: uint32(i), Value: value, Address: "ssq1ptest"}
			if err := tr.AddInput(u, ScriptP2WPKH(hash32(byte(i)))); err != nil {
				t.Fatal(err)
			}
			tr.Inputs[i].Sequence = seq
		}
		for i := 0; i < int(nOut); i++ {
			tr.AddOutput(value, ScriptP2WPKH(hash32(byte(0x80+i))))
		}
		before := tr.EstimateWeight()
		if err := tr.SignAll(fakeSigner{sigLen: DilithiumSigSize, pubLen: DilithiumPubKeySize}); err != nil {
			t.Fatal(err)
		}
		measured := 3*len(tr.serializeNoWitness()) + len(tr.Serialize())
		if before != measured || tr.EstimateWeight() != measured {
			t.Fatalf("%d in / %d out: estimate %d, after signing %d, measured %d", nIn, nOut, before, tr.EstimateWeight(), measured)
		}
		if tr.TxID() == "" {
			t.Fatal("empty txid")
		}
	})
}

// FuzzBuildSendNeverPanicsAndNeverOverpays: whatever amounts and fee rate a
// caller passes, BuildSendTransaction either refuses with a typed error or
// produces a transaction whose outputs plus fee equal its inputs exactly, with
// every output at or above the node's floor and the fee within the caps.
func FuzzBuildSendNeverPanicsAndNeverOverpays(f *testing.F) {
	f.Add(int64(5_000_000_000), int64(1_000_000_000), int64(1000))
	f.Add(int64(300_000), int64(279_500), int64(1000))
	f.Add(int64(-1), int64(1), int64(1000))
	f.Add(int64(100*types.ShorsPerSOQ), int64(1_000_000_000), int64(9_000_000))
	f.Fuzz(func(t *testing.T, inputValue, amount, feeRate int64) {
		spk := ScriptP2WPKH(hash32(0x02))
		in := []types.UTXO{realUTXO(t, 0x10, 0, inputValue)}
		tr, err := BuildSendTransaction(in, spk, amount, ScriptP2WPKH(hash32(0x03)), feeRate)
		if err != nil {
			return
		}
		var outSum int64
		for _, o := range tr.Outputs {
			if o.Value < MinOutputValue(o.ScriptPubKey) {
				t.Fatalf("output below the node's floor emitted: %d", o.Value)
			}
			outSum += o.Value
		}
		fee := inputValue - outSum
		if fee <= 0 || fee > MaxFeeShors || feeRate > MaxFeeRateShorsPerVB {
			t.Fatalf("fee %d at %d shors/vB escaped the caps (inputs %d, outputs %d)", fee, feeRate, inputValue, outSum)
		}
		if tr.Outputs[0].Value != amount {
			t.Fatalf("recipient got %d, asked %d", tr.Outputs[0].Value, amount)
		}
	})
}
