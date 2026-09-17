package tx

import (
	"errors"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Both USDSOQ builders measured the fee over three zero-value outputs with nil
// scripts, which serialise as 9 bytes each where a witness v7 or v5 program
// serialises as 43. The fee was therefore sized over 27 bytes of outputs
// instead of 129, and a mint at the recommended rate paid under that rate for
// the transaction it produced: the node's relay floor is per vByte of the real
// serialization, so such a transaction is not relayed.
//
// The property is the one that matters to a caller: the fee the builder took
// buys at least the rate that was asked for, measured on the transaction the
// builder returned.
func TestUSDSOQBuildersSizeTheFeeOverTheOutputsTheyEmit(t *testing.T) {
	recipient := ScriptV7USDSOQHolding(hash32(0x07))
	assetChange := ScriptV7USDSOQHolding(hash32(0x08))
	native := ScriptP2WPKH(hash32(0x02))
	authority := hash32(0x05)
	rate := types.RecommendedFeeRate

	t.Run("mint", func(t *testing.T) {
		in := []types.UTXO{realUTXO(t, 0x10, 0, 100*types.ShorsPerSOQ)}
		tr, err := BuildMintUSDSOQTransaction(in, recipient, 5_000_000_000, native, authority, rate)
		if err != nil {
			t.Fatalf("mint at the recommended rate refused: %v", err)
		}
		// The fee is what the SOQ inputs paid: the asset amount is created
		// from nothing and the marker carries no value.
		fee := 100*types.ShorsPerSOQ - soqValueOf(tr, native)
		assertFeeBuysTheRate(t, tr, fee, rate)
	})

	t.Run("send", func(t *testing.T) {
		asset := []types.UTXO{realUTXO(t, 0x20, 0, 9_000_000_000)}
		soq := []types.UTXO{realUTXO(t, 0x21, 0, 100*types.ShorsPerSOQ)}
		tr, err := BuildSendUSDSOQTransaction(asset, soq, recipient, 5_000_000_000,
			assetChange, native, rate)
		if err != nil {
			t.Fatalf("send at the recommended rate refused: %v", err)
		}
		fee := 100*types.ShorsPerSOQ - soqValueOf(tr, native)
		assertFeeBuysTheRate(t, tr, fee, rate)
	})
}

// soqValueOf totals the outputs carrying the native script, which is the
// change the builder left: every other output is an asset output or the
// zero-value authority marker.
func soqValueOf(tr *Transaction, native []byte) int64 {
	var total int64
	for _, o := range tr.Outputs {
		if len(o.ScriptPubKey) == len(native) && string(o.ScriptPubKey) == string(native) {
			total += o.Value
		}
	}
	return total
}

func assertFeeBuysTheRate(t *testing.T, tr *Transaction, fee, rate int64) {
	t.Helper()
	vsize := tr.VSize()
	if vsize <= 0 {
		t.Fatalf("vsize %d", vsize)
	}
	if fee < vsize*rate {
		t.Fatalf("fee %d shors over %d vB is %d shors/vB, under the %d asked for: short by %d",
			fee, vsize, fee/vsize, rate, vsize*rate-fee)
	}
}

// Neither builder called checkFee, so neither the rate cap nor the absolute
// cap applied, while docs/SECURITY.md tells the reader the builders cap both.
func TestUSDSOQBuildersCapFees(t *testing.T) {
	recipient := ScriptV7USDSOQHolding(hash32(0x07))
	assetChange := ScriptV7USDSOQHolding(hash32(0x08))
	native := ScriptP2WPKH(hash32(0x02))
	authority := hash32(0x05)
	in := []types.UTXO{realUTXO(t, 0x10, 0, 100*types.ShorsPerSOQ)}
	asset := []types.UTXO{realUTXO(t, 0x20, 0, 9_000_000_000)}

	// A fee-rate typo above the rate cap.
	if _, err := BuildMintUSDSOQTransaction(in, recipient, 1_000_000_000, native, authority,
		MaxFeeRateShorsPerVB+1); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("mint accepted a rate above the cap: %v", err)
	}
	if _, err := BuildSendUSDSOQTransaction(asset, in, recipient, 1_000_000_000, assetChange,
		native, MaxFeeRateShorsPerVB+1); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("send accepted a rate above the cap: %v", err)
	}

	// A rate of zero or below is refused as well: it produces a transaction
	// no miner includes.
	for _, rate := range []int64{0, -1} {
		if _, err := BuildMintUSDSOQTransaction(in, recipient, 1_000_000_000, native, authority,
			rate); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("mint accepted the rate %d: %v", rate, err)
		}
		if _, err := BuildSendUSDSOQTransaction(asset, in, recipient, 1_000_000_000, assetChange,
			native, rate); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("send accepted the rate %d: %v", rate, err)
		}
	}

	// Under the rate cap and over the absolute cap: many inputs at a high
	// rate, which is the shape a consolidation takes.
	many := make([]types.UTXO, 0, 80)
	for i := 0; i < 80; i++ {
		many = append(many, realUTXO(t, 0x10, uint32(i), 1_000_000_000))
	}
	if _, err := BuildMintUSDSOQTransaction(many, recipient, 1_000_000_000, native, authority,
		MaxFeeRateShorsPerVB); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("mint accepted a fee above MaxFeeShors: %v", err)
	}
	if _, err := BuildSendUSDSOQTransaction(asset, many, recipient, 1_000_000_000, assetChange,
		native, MaxFeeRateShorsPerVB); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("send accepted a fee above MaxFeeShors: %v", err)
	}

	// The ordinary case still builds, so the caps are caps rather than a
	// refusal of the operation.
	if _, err := BuildMintUSDSOQTransaction(in, recipient, 1_000_000_000, native, authority,
		types.RecommendedFeeRate); err != nil {
		t.Errorf("mint at the recommended rate refused: %v", err)
	}
	if _, err := BuildSendUSDSOQTransaction(asset, in, recipient, 1_000_000_000, assetChange,
		native, types.RecommendedFeeRate); err != nil {
		t.Errorf("send at the recommended rate refused: %v", err)
	}
}
