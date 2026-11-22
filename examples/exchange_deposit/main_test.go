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

// The ledger lists for the ceiling every credited deposit that is not final,
// whatever its height, and none that is.
func TestTheLedgerListsWhatIsNotFinal(t *testing.T) {
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
	got, err := ledger.NotFinal(ctx)
	if err != nil || len(got) != 1 || got[0].TxID != "aa" {
		t.Errorf("not final: %+v (err %v), want the deposit at height 100 alone", got, err)
	}
	if pending, _ := ledger.Pending(ctx); len(pending) != 1 {
		t.Errorf("Pending lists %d, want the one that is not final", len(pending))
	}
}

// A page goes out when either hold begins or ends, also while the other
// lasts, and not when a hold only changes its wording.
func TestThePageFollowsEachHoldOnItsOwn(t *testing.T) {
	none := deposit.TiersStatus{}
	unsettled := deposit.TiersStatus{Unsettled: "a branch of 3 blocks"}
	both := deposit.TiersStatus{Unsettled: "a branch of 3 blocks", Ahead: "a branch with more work"}
	ahead := deposit.TiersStatus{Ahead: "a branch with more work"}
	for _, tc := range []struct {
		name       string
		paged, now deposit.TiersStatus
		want       bool
	}{
		{"nothing held, nothing held", none, none, false},
		{"the chain turns unsettled", none, unsettled, true},
		{"a branch with more work appears on an unsettled chain", unsettled, both, true},
		{"the branch is passed and the chain is still unsettled", both, unsettled, true},
		{"the chain settles while the branch is ahead", both, ahead, true},
		{"the branch is passed", ahead, none, true},
		{"another reason for the same hold", unsettled, deposit.TiersStatus{Unsettled: "the hashrate fell"}, false},
		{"a later reading of the same holds", both, deposit.TiersStatus{Unsettled: "x", Ahead: "y", Tip: 9, Exposed: 5}, false},
	} {
		if got := safeguardChanged(tc.paged, tc.now); got != tc.want {
			t.Errorf("%s: a page %v, want %v", tc.name, got, tc.want)
		}
	}
}
