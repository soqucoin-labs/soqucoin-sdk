package types

import "testing"

// The chain parameters the money paths read, pinned to the node's
// src/chainparams.cpp:
//
//	mainnet   nCoinbaseMaturity = 288 in the block-1 tier (digishieldConsensus)
//	stagenet  nCoinbaseMaturity = 288 from height 100,000 (maturityMirrorConsensus), 30 below it
//	regtest   nCoinbaseMaturity = 60
//
// A coinbase treated as spendable before consensus allows it is a payout that
// can be undone, so no network's value here may be below consensus at any height.
func TestNetworkChainParameters(t *testing.T) {
	cases := []struct {
		n        Network
		chainID  string
		maturity int64
	}{
		{Mainnet, "main", 288},
		{Stagenet, "stagenet", 288},
		{Regtest, "regtest", 60},
	}
	for _, c := range cases {
		if c.n.ChainID != c.chainID {
			t.Errorf("%s: ChainID %q, want %q", c.n.Name, c.n.ChainID, c.chainID)
		}
		if c.n.CoinbaseMaturity != c.maturity {
			t.Errorf("%s: CoinbaseMaturity %d, want %d", c.n.Name, c.n.CoinbaseMaturity, c.maturity)
		}
	}
}

// The deprecated constant is mainnet's value and the largest of the three, so
// code that still reads it never treats a coinbase as mature too early on any
// network. Every network carries a chain id and a maturity: a zero value in
// either would disable a check silently.
func TestDeprecatedCoinbaseMaturityIsMainnetAndTheMaximum(t *testing.T) {
	if CoinbaseMaturity != Mainnet.CoinbaseMaturity {
		t.Fatalf("CoinbaseMaturity %d, mainnet %d", CoinbaseMaturity, Mainnet.CoinbaseMaturity)
	}
	for _, n := range []Network{Mainnet, Stagenet, Regtest} {
		if n.CoinbaseMaturity > CoinbaseMaturity {
			t.Errorf("%s: maturity %d exceeds the constant %d", n.Name, n.CoinbaseMaturity, CoinbaseMaturity)
		}
		if n.CoinbaseMaturity <= 0 || n.ChainID == "" {
			t.Errorf("%s: incomplete chain parameters %+v", n.Name, n)
		}
	}
}

// On mainnet coinbase maturity is not below the finality horizon: a coinbase
// spendable inside the reorg window is the one output class whose reversal
// invalidates every descendant spend (the INVARIANT comment in chainparams.cpp).
func TestMainnetMaturityCoversTheHorizon(t *testing.T) {
	if Mainnet.CoinbaseMaturity < MaxReorgDepth {
		t.Fatalf("mainnet maturity %d is inside the %d-block horizon", Mainnet.CoinbaseMaturity, MaxReorgDepth)
	}
}
