package tx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// realAddr returns a decodable stagenet v1 address for a distinct program, so
// the builders (which derive scriptCode from the address) can be driven.
func realAddr(t *testing.T, fill byte) string {
	t.Helper()
	a, err := address.Encode(types.Stagenet.HRP, 1, hash32(fill))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func realUTXO(t *testing.T, fill byte, vout uint32, value int64) types.UTXO {
	t.Helper()
	return types.UTXO{TxID: displayTxID, Vout: vout, Value: value, Address: realAddr(t, fill)}
}

// The weight the builder charges for MUST be the weight of the bytes it
// broadcasts. The miner's default floor compares fee against the node's
// vsize, so an estimate 2.25 vB per input short (the previous constants) puts
// every multi-input transaction below the floor at the documented rate.
func TestEstimateWeightEqualsSerializedWeightOnceSigned(t *testing.T) {
	for _, n := range []int{1, 2, 10, 80, 253} {
		tr := NewTransaction()
		for i := 0; i < n; i++ {
			if err := tr.AddInput(realUTXO(t, 0x10, uint32(i), 1_000_000_000), ScriptP2WPKH(hash32(0x01))); err != nil {
				t.Fatal(err)
			}
		}
		tr.AddOutput(500_000_000, ScriptP2WPKH(hash32(0x02)))
		tr.AddOutput(400_000_000, ScriptP2WPKH(hash32(0x03)))
		before := tr.EstimateWeight()

		if err := tr.SignAll(fakeSigner{sigLen: DilithiumSigSize, pubLen: DilithiumPubKeySize}); err != nil {
			t.Fatal(err)
		}
		measured := 3*len(tr.serializeNoWitness()) + len(tr.Serialize())
		if before != measured || tr.EstimateWeight() != measured {
			t.Errorf("%d inputs: estimate before signing %d, after %d, serialized %d WU",
				n, before, tr.EstimateWeight(), measured)
		}
	}
}

// The node reported vsize 1073 for the real one-input two-output stagenet
// transaction recorded in docs/VERIFICATION.md; the SDK used to say 1072.
func TestSingleInputTwoOutputVSizeMatchesNode(t *testing.T) {
	tr := NewTransaction()
	if err := tr.AddInput(realUTXO(t, 0x10, 0, 5_000_000_000), ScriptP2WPKH(hash32(0x01))); err != nil {
		t.Fatal(err)
	}
	tr.AddOutput(4_000_000_000, ScriptP2WPKH(hash32(0x02)))
	tr.AddOutput(999_000_000, ScriptP2WPKH(hash32(0x03)))
	if got := tr.VSize(); got != 1073 {
		t.Fatalf("vsize = %d, node says 1073", got)
	}
	// At the recommended rate the fee clears the miner floor with the margin.
	if fee := tr.EstimateFee(types.RecommendedFeeRate); fee < 1073*types.RecommendedFeeRate {
		t.Errorf("fee %d is below the floor for 1073 vB", fee)
	}
}

func TestMinOutputValueFollowsScriptSize(t *testing.T) {
	if got := MinOutputValue(ScriptP2WPKH(hash32(0))); got != 279_500 {
		t.Errorf("34-byte script floor = %d, want 279500 (6500 x 43)", got)
	}
	if DustThreshold != 279_500 {
		t.Errorf("DustThreshold = %d, want 279500", DustThreshold)
	}
	if got := MinOutputValue(make([]byte, 22)); got != 6500*31 {
		t.Errorf("22-byte script floor = %d, want %d", got, 6500*31)
	}
}

func TestBuildSendRefusesRecipientBelowRelayFloor(t *testing.T) {
	in := []types.UTXO{realUTXO(t, 0x10, 0, 5_000_000_000)}
	spk := ScriptP2WPKH(hash32(0x02))
	for _, amt := range []int64{1, 100_000, 279_499} {
		_, err := BuildSendTransaction(in, spk, amt, spk, types.RecommendedFeeRate)
		if !errors.Is(err, ErrBelowDust) {
			t.Errorf("amount %d accepted: %v (the node would reject the output as non-standard)", amt, err)
		}
	}
	if _, err := BuildSendTransaction(in, spk, 279_500, spk, types.RecommendedFeeRate); err != nil {
		t.Errorf("amount at the floor refused: %v", err)
	}
}

func TestBuildSendFoldsSubFloorChangeIntoFee(t *testing.T) {
	spk := ScriptP2WPKH(hash32(0x02))
	// Fee for 1 input, 2 outputs at 1000 sat/vB is (1073+1)*1000.
	fee := int64(1074) * types.RecommendedFeeRate
	amount := int64(1_000_000_000)
	for _, change := range []int64{0, 100_000, 279_499} {
		in := []types.UTXO{realUTXO(t, 0x10, 0, amount+fee+change)}
		tr, err := BuildSendTransaction(in, spk, amount, ScriptP2WPKH(hash32(0x03)), types.RecommendedFeeRate)
		if err != nil {
			t.Fatalf("change %d: %v", change, err)
		}
		if len(tr.Outputs) != 1 {
			t.Errorf("change %d: %d outputs, want the sub-floor change folded into fee", change, len(tr.Outputs))
		}
	}
	in := []types.UTXO{realUTXO(t, 0x10, 0, amount+fee+279_500)}
	tr, err := BuildSendTransaction(in, spk, amount, ScriptP2WPKH(hash32(0x03)), types.RecommendedFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Outputs) != 2 || tr.Outputs[1].Value != 279_500 {
		t.Errorf("change at the floor should be emitted: %+v", tr.Outputs)
	}
}

func TestBuildSendRefusesBadAmounts(t *testing.T) {
	in := []types.UTXO{realUTXO(t, 0x10, 0, 5_000_000_000)}
	spk := ScriptP2WPKH(hash32(0x02))
	for _, amt := range []int64{0, -1, -5_000_000_000, MaxMoney + 1} {
		if _, err := BuildSendTransaction(in, spk, amt, spk, types.RecommendedFeeRate); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("amount %d accepted: %v", amt, err)
		}
	}
	// Input values are checked too, and their sum cannot wrap.
	bad := []types.UTXO{realUTXO(t, 0x10, 0, -1)}
	if _, err := BuildSendTransaction(bad, spk, 1_000_000_000, spk, types.RecommendedFeeRate); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("negative input accepted: %v", err)
	}
	huge := []types.UTXO{realUTXO(t, 0x10, 0, MaxMoney), realUTXO(t, 0x10, 1, MaxMoney)}
	if _, err := BuildSendTransaction(huge, spk, 1_000_000_000, spk, types.RecommendedFeeRate); !errors.Is(err, ErrInputOverflow) {
		t.Errorf("overflowing inputs accepted: %v", err)
	}
	if _, err := BuildSendTransaction(in, spk, 6_000_000_000, spk, types.RecommendedFeeRate); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("insufficient funds not typed: %v", err)
	}
}

func TestBuildSendCapsFees(t *testing.T) {
	spk := ScriptP2WPKH(hash32(0x02))
	in := []types.UTXO{realUTXO(t, 0x10, 0, 100*types.SatoshisPerSOQ)}
	// A fee-rate typo: 9,000,000 sat/vB would burn ~96 SOQ of a 100 SOQ input.
	if _, err := BuildSendTransaction(in, spk, 1_000_000_000, spk, 9_000_000); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("fee-rate typo accepted: %v", err)
	}
	if _, err := BuildSendTransaction(in, spk, 1_000_000_000, spk, 0); err == nil {
		t.Error("zero fee rate accepted")
	}
	// Under the rate cap but over the absolute cap: many inputs at a high rate.
	many := make([]types.UTXO, 0, 80)
	for i := 0; i < 80; i++ {
		many = append(many, realUTXO(t, 0x10, uint32(i), 1_000_000_000))
	}
	if _, err := BuildSendTransaction(many, spk, 1_000_000_000, spk, MaxFeeRateSatPerVB); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("fee above MaxFeeSat accepted: %v", err)
	}
	// A maximal standard payout at the recommended rate is fine.
	tr, err := BuildSendTransaction(many, spk, 1_000_000_000, spk, types.RecommendedFeeRate)
	if err != nil {
		t.Fatalf("80-input payout at the recommended rate refused: %v", err)
	}
	fee := int64(80)*1_000_000_000 - tr.Outputs[0].Value - tr.Outputs[1].Value
	if fee > MaxFeeSat {
		t.Errorf("fee %d exceeds the cap the builder should have enforced", fee)
	}
	t.Log(fmt.Sprintf("80-input fee at %d sat/vB: %d sat", types.RecommendedFeeRate, fee))
}
