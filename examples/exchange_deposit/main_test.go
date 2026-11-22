package main

import (
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The tier of a deposit is decided at the two value lines. A deposit that is
// exactly at a line belongs to the tier above it, so the amount at the second
// line waits for the finality horizon and is never credited at 30.
func TestRequiredConfirmationsChangesAtTheTwoValueLines(t *testing.T) {
	for _, tc := range []struct {
		value int64
		want  int64
	}{
		{1, 10},
		{smallDeposit - 1, 10},
		{smallDeposit, 30},
		{largeDeposit - 1, 30},
		{largeDeposit, 288},
		{types.MaxMoney, 288},
	} {
		if got := requiredConfirmations(tc.value); got != tc.want {
			t.Errorf("a deposit of %d shors is credited at %d confirmations, want %d", tc.value, got, tc.want)
		}
	}
	if types.MaxReorgDepth != 288 {
		t.Fatalf("types.MaxReorgDepth is %d, and the top tier is stated as 288", types.MaxReorgDepth)
	}
}
