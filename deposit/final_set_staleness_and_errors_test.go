package deposit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Pruning is per scanned address. With one address fresh and one stale, the
// stale address keeps its final entries and the fresh address loses the
// entries the cache no longer lists.
func TestPruneKeepsTheEntriesOfAStaleAddress(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	b := addr(t, 0x22)
	m.Addresses = func(context.Context) []string { return []string{a, b} }
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	cache.utxos[b] = []types.UTXO{{TxID: "bb", Vout: 0, Value: 1000, Height: node.tip - 40, Address: b}}
	node.outs[key("aa", 0)] = txout(t, a, 1000, types.MaxReorgDepth+1, false)
	node.outs[key("bb", 0)] = txout(t, b, 1000, types.MaxReorgDepth+1, false)
	for i := 0; i < 2; i++ {
		if _, err := m.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !led.final[key("aa", 0)] || !led.final[key("bb", 0)] || m.finalCount() != 2 {
		t.Fatalf("setup: set holds %d", m.finalCount())
	}

	// b is stale, a is fresh, and both outputs leave the cache.
	per := &fakeCachePerAddr{fakeCache: *cache, ats: map[string]time.Time{a: m.now()}, errs: map[string]error{}}
	per.utxos[a] = nil
	per.utxos[b] = nil
	m.Cache = per
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatalf("scan with one fresh address paused: %v", err)
	}
	if m.finalCount() != 1 {
		t.Fatalf("set holds %d after the prune, want b's entry alone", m.finalCount())
	}
	if !m.isFinal("bb", 0) || m.isFinal("aa", 0) {
		t.Fatal("the wrong address's entry was pruned")
	}
}

// failingNode answers gettxout with an error for one outpoint.
type failingNode struct {
	*fakeNode
	failKey string
}

func (n *failingNode) GetTxOut(ctx context.Context, txid string, vout uint32, mem bool) (*rpc.TxOut, error) {
	if key(txid, vout) == n.failKey {
		return nil, errors.New("node: connection refused")
	}
	return n.fakeNode.GetTxOut(ctx, txid, vout, mem)
}

// A node error on the depth lookup adds nothing to the final set: the node's
// word was not obtained. The credit stands, no alert is raised, and the
// ledger is asked again next scan.
func TestNodeErrorOnTheDepthLookupAddsNothingToTheFinalSet(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cl := &countingLedger{fakeLedger: led}
	m.Ledger = cl
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	node.outs[key("aa", 0)] = txout(t, a, 1000, types.MaxReorgDepth+1, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("credit: %v %+v", err, got)
	}
	// Mark it final in the ledger by the recheck path, then forget it in the
	// Monitor, as a restart would.
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.finalMu.Lock()
	m.final = nil
	m.finalMu.Unlock()
	m.Node = &failingNode{fakeNode: node, failKey: key("aa", 0)}
	asked := cl.isCredited
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatalf("scan with a failing depth lookup: %v", err)
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
