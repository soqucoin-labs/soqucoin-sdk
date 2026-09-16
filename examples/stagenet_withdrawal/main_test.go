package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

const (
	tTxA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tHot = "ssq1photwallet"
)

// The verification transaction is run against a freshly funded address, so
// one output holding everything is the ordinary case, and the payment is
// most of it. Budgeting the fee against the whole candidate list rather than
// the selection charged a further 976 vB per candidate and added the first
// input's 976 vB twice, so a one-output wallet was asked for 2,076 vB of
// headroom against a transaction of about 1,073, and the selector refused a
// payment the wallet could pay.
func TestSelectForFeeLetsOneFundingOutputPayNearlyAllOfIt(t *testing.T) {
	const value = 2_500_000_000
	const amount = value - 1_500_000
	funding := []types.UTXO{{TxID: tTxA, Vout: 0, Value: value, Height: 900, Address: tHot}}
	selector := utxo.NewCoinSelector(utxo.NewSpentSet(filepath.Join(t.TempDir(), "s.json"), nil))

	// The fixture has to sit between the two targets, or it proves nothing.
	took := amount + vsizeFor(1)*types.RecommendedFeeRate
	every := amount + (1100+976*int64(len(funding)))*types.RecommendedFeeRate
	if took > value || every <= value {
		t.Fatalf("fixture: value %d, target for what it takes %d, target for every candidate %d", value, took, every)
	}

	selected, err := selectForFee(selector, funding, tHot, amount, types.RecommendedFeeRate, 1000)
	if err != nil {
		t.Fatalf("a payment of %d from an output of %d was refused: %v", amount, value, err)
	}
	if len(selected) != 1 || selected[0].TxID != tTxA {
		t.Fatalf("selected %+v, want the one funding output", selected)
	}
	if got := selected[0].Value - amount; got < vsizeFor(1)*types.RecommendedFeeRate {
		t.Fatalf("headroom over the payment is %d shors, under the %d the fee needs", got, vsizeFor(1)*types.RecommendedFeeRate)
	}
}

// vsizeFor is a fee target, so it must never fall under the vsize of the
// transaction the selection it sizes will actually produce. A target under
// the real figure makes the selector accept a selection that cannot pay its
// own fee; BuildSendTransaction then computes a negative change and returns
// ErrInsufficientFunds, which the engine reads as permanent and which fails
// the withdrawal for good.
func TestVsizeForNeverUndersizesTheTransactionItBudgets(t *testing.T) {
	recipient := tx.ScriptP2WPKH(make([]byte, 32))
	change := tx.ScriptP2WPKH(bytes.Repeat([]byte{1}, 32))
	for _, n := range []int{1, 2, 3, 5, 10, 40, utxo.MaxInputsPerTX} {
		inputs := make([]types.UTXO, n)
		for i := range inputs {
			inputs[i] = types.UTXO{
				TxID:   fmt.Sprintf("%060d%04d", 0, i),
				Vout:   uint32(i),
				Value:  10_000_000_000,
				Height: 900, Address: "sq1pa3n373z2lgva3m53nssuwm7jl0dz697uzul7wh55ct7maf00xe4s2m80fs",
			}
		}
		tr, err := tx.BuildSendTransaction(inputs, recipient, 1_000_000_000, change, types.RecommendedFeeRate)
		if err != nil {
			t.Fatalf("n=%d: build: %v", n, err)
		}
		if budget, real := vsizeFor(n), tr.VSize(); budget < real {
			t.Errorf("n=%d: fee target %d vB, transaction %d vB, short by %d", n, budget, real, real-budget)
		}
	}
}
