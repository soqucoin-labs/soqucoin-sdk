package deposit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// *rpc.Client is the Node the guide names; a wrapper that forwards fewer
// methods than the interface asks for does not compile.
var _ Node = (*rpc.Client)(nil)

// A node that fails a gettxout inside the credit loop has not disagreed with
// the indexer; it has not answered. The pass ends there with the node's error,
// the deposits credited before it are returned, and no indexer_mismatch is
// raised: that alert routes to the runbook for a lying indexer, and a node
// restart is not one.
func TestNodeErrorInsideTheCreditLoopEndsThePassWithTheError(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{
		{TxID: txA, Vout: 0, Value: 100, Height: 900, Address: a},
		{TxID: txB, Vout: 0, Value: 100, Height: 900, Address: a},
	}
	node.outs[key(txA, 0)] = txout(t, a, 100, 101, false)
	node.outs[key(txB, 0)] = txout(t, a, 100, 101, false)
	m.Node = &failingNode{fakeNode: node, failKey: key(txB, 0)}

	got, err := m.Scan(context.Background())
	if err == nil || errors.Is(err, ErrPaused) {
		t.Fatalf("scan with a node error on the second output: err %v, want the node's error returned as it is", err)
	}
	if len(got) != 1 || got[0].TxID != txA || len(led.credited) != 1 {
		t.Fatalf("credited %+v, want txA alone returned with the error", got)
	}
	if len(al.kinds) != 0 {
		t.Fatalf("alerts %v, want none: the node did not disagree, it did not answer", al.kinds)
	}
	// The node is back: the failed outpoint is credited on the next pass.
	m.Node = node
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].TxID != txB || len(led.credited) != 2 {
		t.Fatalf("next pass: %v %+v", err, got)
	}
}

// The same error while the node is asked only for the depth of an outpoint
// the ledger already holds: nothing enters the final set, the credit stands,
// and the pass ends with the error as it does at the other two gettxout sites.
func TestNodeErrorOnTheDepthLookupEndsThePassAndAddsNothingToTheFinalSet(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cl := &countingLedger{fakeLedger: led}
	m.Ledger = cl
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 1000, types.MaxReorgDepth+1, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("credit: %v %+v", err, got)
	}
	// Final in the ledger by the recheck path, then forgotten by the Monitor,
	// as a restart would.
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.finalMu.Lock()
	m.final = nil
	m.finalMu.Unlock()
	m.Node = &failingNode{fakeNode: node, failKey: key(txA, 0)}
	asked := cl.isCredited
	got, err := m.Scan(context.Background())
	if err == nil || errors.Is(err, ErrPaused) || len(got) != 0 {
		t.Fatalf("scan with a failing depth lookup: err %v got %+v, want the node's error and nothing credited", err, got)
	}
	if m.finalCount() != 0 {
		t.Fatal("an outpoint entered the final set without the node's word")
	}
	if cl.isCredited != asked+1 {
		t.Fatalf("ledger asked %d times, want once", cl.isCredited-asked)
	}
	if len(al.kinds) != 0 {
		t.Fatalf("alerts on a failed depth lookup: %v", al.kinds)
	}
	m.Node = node
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.finalCount() != 1 {
		t.Fatal("the outpoint did not enter the set once the node answered")
	}
}

// A Monitor whose Network is unset applies mainnet's maturity, so it asks the
// node for mainnet: a regtest node under it is a deployment error, refused
// with one alert, not a Monitor that credits at 288 where consensus says 60.
func TestUnsetNetworkIsMainnetForTheChainCheckToo(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	node.chain = types.Regtest.ChainID
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 8_800_000_000, Height: 950, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 8_800_000_000, 60, true)

	got, err := m.Scan(context.Background())
	if len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("credited %+v on a regtest node under a Monitor with no Network", got)
	}
	if !errors.Is(err, rpc.ErrWrongChain) || errors.Is(err, ErrPaused) {
		t.Fatalf("got %v, want rpc.ErrWrongChain", err)
	}
	if len(al.kinds) != 1 || al.kinds[0] != AlertNodeWrongChain {
		t.Fatalf("alerts %v, want exactly one %s", al.kinds, AlertNodeWrongChain)
	}
	// Named as regtest, the same Monitor credits the mature coinbase.
	m.Network = types.Regtest
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("regtest Monitor over a regtest node: %v %+v", err, got)
	}
}

// cadenceCache is a cache that reports how often a quiet address's freshness
// advances, as *electrumx.Client does through its ping interval.
type cadenceCache struct {
	fakeCache
	every time.Duration
}

func (c *cadenceCache) FreshnessInterval() time.Duration { return c.every }

// The window must hold one missed ping. A cache that reports its cadence is
// refused with a MaxCacheAge shorter than two of them, before the node is
// asked anything; a cache that reports none is not checked.
func TestMaxCacheAgeMustHoldTwoPingsOfACacheThatReportsThem(t *testing.T) {
	m, cache, node, _, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 101, false)
	m.Cache = &cadenceCache{fakeCache: *cache, every: time.Minute}

	for _, age := range []time.Duration{time.Minute, 2*time.Minute - time.Second} {
		m.MaxCacheAge = age
		calls := node.calls
		got, err := m.Scan(context.Background())
		if !errors.Is(err, ErrCacheAgeBelowPings) || errors.Is(err, ErrPaused) || len(got) != 0 {
			t.Fatalf("MaxCacheAge %v over a 1m cadence: err %v got %+v, want ErrCacheAgeBelowPings", age, err, got)
		}
		if node.calls != calls {
			t.Fatalf("MaxCacheAge %v: the node was asked %d times before the refusal", age, node.calls-calls)
		}
		if len(al.kinds) != 0 {
			t.Fatalf("alerts %v on a configuration error", al.kinds)
		}
	}
	m.MaxCacheAge = 2 * time.Minute
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("MaxCacheAge of exactly two cadences: %v %+v", err, got)
	}
	// The defaults hold: 5 minutes over the client's 60 seconds.
	m.MaxCacheAge = 0
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	// A cache without a cadence is not checked, whatever the window.
	m.Cache = cache
	m.MaxCacheAge = time.Second
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatalf("cache without a cadence: %v", err)
	}
}
