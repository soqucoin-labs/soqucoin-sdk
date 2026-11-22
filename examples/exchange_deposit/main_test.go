package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The example's three figures make a Tiers, and the tier of a deposit is
// decided at the two value lines. A deposit that is exactly at a line belongs
// to the tier above it, so the amount at the second line waits for the
// finality horizon and is never credited at 30.
func TestTheExampleLinesGiveTheThreeTiers(t *testing.T) {
	ledger := &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{}}
	// The node is not asked: the tier of a value reads the lines alone.
	tiers, err := deposit.NewTiers(rpc.NewClient("http://127.0.0.1:1", "", "", nil), ledger, lines)
	if err != nil {
		t.Fatalf("the example's lines are refused: %v", err)
	}
	for _, tc := range []struct {
		value int64
		want  int64
	}{
		{1, 10},
		{lines.SmallBelow - 1, 10},
		{lines.SmallBelow, 30},
		{lines.LargeFrom - 1, 30},
		{lines.LargeFrom, 288},
		{types.MaxMoney, 288},
	} {
		if got := tiers.Required(tc.value); got != tc.want {
			t.Errorf("a deposit of %d shors is credited at %d confirmations, want %d", tc.value, got, tc.want)
		}
	}
	if types.MaxReorgDepth != 288 {
		t.Fatalf("types.MaxReorgDepth is %d, and the top tier is stated as 288", types.MaxReorgDepth)
	}
	if lines.Ceiling < lines.LargeFrom {
		t.Errorf("the ceiling %d is below the second value line %d: a deposit just below the line could never be credited at 30",
			lines.Ceiling, lines.LargeFrom)
	}
}

// The ledger lists for the ceiling every deposit credited in a block at or
// above a height, the final ones among them, and none from below it.
func TestTheLedgerListsWhatWasCreditedSinceAHeight(t *testing.T) {
	ctx := context.Background()
	ledger := &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, d := range []deposit.Deposit{
		{TxID: "aa", Value: 5, Height: 100, Confirmations: 10},
		{TxID: "bb", Value: 7, Height: 200, Confirmations: 10},
	} {
		if err := ledger.Credit(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.MarkFinal(ctx, "bb", 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		height int64
		want   int
	}{{100, 2}, {101, 1}, {200, 1}, {201, 0}} {
		got, err := ledger.CreditedSince(ctx, tc.height)
		if err != nil || len(got) != tc.want {
			t.Errorf("credited since height %d: %d deposits (err %v), want %d", tc.height, len(got), err, tc.want)
		}
	}
	if pending, _ := ledger.Pending(ctx); len(pending) != 1 {
		t.Errorf("Pending lists %d, want the one that is not final", len(pending))
	}
}
