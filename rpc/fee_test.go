package rpc

import (
	"errors"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func feeServer(t *testing.T, reply string) (*Client, *[]interface{}) {
	t.Helper()
	var params []interface{}
	c, _ := rpcServer(t, func(method string, p []interface{}) string {
		if method != "estimatesmartfee" {
			t.Errorf("method = %q, want estimatesmartfee", method)
		}
		params = p
		return ok(reply)
	})
	return c, &params
}

// The node's SOQ-per-kilobyte figure becomes shors per virtual byte, rounded
// up, and is clamped to the builders' range. Every case names the figure the
// node printed and the rate a caller gets.
func TestFeeRateShorsPerVBConvertsAndClamps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  FeeEstimate
	}{
		{"within range", `{"feerate":0.02000000,"blocks":6}`,
			FeeEstimate{Rate: 2000, NodeRate: 2000, Blocks: 6}},
		{"exactly the floor", `{"feerate":0.01000000,"blocks":6}`,
			FeeEstimate{Rate: 1000, NodeRate: 1000, Blocks: 6}},
		{"exactly the ceiling", `{"feerate":1.00000000,"blocks":2}`,
			FeeEstimate{Rate: 100_000, NodeRate: 100_000, Blocks: 2}},
		{"below the floor, rounded up, clamped up", `{"feerate":0.00012345,"blocks":6}`,
			FeeEstimate{Rate: 1000, NodeRate: 13, Blocks: 6, Clamped: true}},
		{"one shor over a whole vbyte rounds up", `{"feerate":0.01000001,"blocks":6}`,
			FeeEstimate{Rate: 1001, NodeRate: 1001, Blocks: 6}},
		{"above the ceiling, clamped down", `{"feerate":1.50000000,"blocks":1}`,
			FeeEstimate{Rate: 100_000, NodeRate: 150_000, Blocks: 1, Clamped: true}},
		{"no estimate: the node prints a negative rate", `{"feerate":-1,"blocks":6}`,
			FeeEstimate{Rate: 1000, Fallback: true}},
		{"no estimate, float form", `{"feerate":-1.0,"blocks":-1}`,
			FeeEstimate{Rate: 1000, Fallback: true}},
	} {
		c, params := feeServer(t, tc.reply)
		got, err := c.FeeRateShorsPerVB(6)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
		if len(*params) != 1 || (*params)[0] != float64(6) {
			t.Errorf("%s: params %v, want [6]", tc.name, *params)
		}
		if got.Rate < types.RecommendedFeeRate || got.Rate > tx.MaxFeeRateShorsPerVB {
			t.Errorf("%s: rate %d outside the builders' range", tc.name, got.Rate)
		}
	}
}

// The ceiling is the builders' live cap, so an operator who tightens
// tx.MaxFeeRateShorsPerVB tightens the estimate too; a cap below the floor is
// a configuration error reported before the node is asked.
func TestFeeRateShorsPerVBHonoursTheOperatorsCap(t *testing.T) {
	saved := tx.MaxFeeRateShorsPerVB
	t.Cleanup(func() { tx.MaxFeeRateShorsPerVB = saved })

	tx.MaxFeeRateShorsPerVB = 5000
	c, _ := feeServer(t, `{"feerate":0.20000000,"blocks":6}`)
	got, err := c.FeeRateShorsPerVB(6)
	if err != nil || got != (FeeEstimate{Rate: 5000, NodeRate: 20_000, Blocks: 6, Clamped: true}) {
		t.Fatalf("tightened cap: %+v, %v", got, err)
	}

	tx.MaxFeeRateShorsPerVB = types.RecommendedFeeRate - 1
	c, params := feeServer(t, `{"feerate":0.02000000,"blocks":6}`)
	_, err = c.FeeRateShorsPerVB(6)
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("cap below floor: %v, want ErrPermanent", err)
	}
	if *params != nil {
		t.Fatal("the node was asked although the range was empty")
	}
}

// A node error keeps its kind, and a reply without a numeric fee rate is an
// error rather than a fallback: the fallback is for a node that answered "no
// estimate", not for one that answered something else.
func TestFeeRateShorsPerVBErrors(t *testing.T) {
	c, _ := rpcServer(t, func(string, []interface{}) string { return rpcErr(CodeInWarmup, "Loading block index...") })
	if _, err := c.FeeRateShorsPerVB(6); !errors.Is(err, ErrTransient) {
		t.Errorf("warmup: %v, want ErrTransient", err)
	}
	for _, reply := range []string{`{"blocks":6}`, `{"feerate":"abc","blocks":6}`, `{"feerate":1e-4,"blocks":6}`, `{"feerate":0.000000001,"blocks":6}`} {
		c, _ := feeServer(t, reply)
		got, err := c.FeeRateShorsPerVB(6)
		if err == nil || got.Fallback {
			t.Errorf("%s: %+v, %v; want an error and no fallback", reply, got, err)
		}
	}
}
