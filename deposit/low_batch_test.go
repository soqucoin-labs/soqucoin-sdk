package deposit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// gatedLedger is goroutine-safe, counts Credit calls per outpoint, and can
// hold the first IsCredited caller until the test releases it.
type gatedLedger struct {
	mu          sync.Mutex
	credited    map[string]Deposit
	final       map[string]bool
	creditCalls map[string]int
	hold        chan struct{} // when non-nil, the first IsCredited blocks until it is closed
	entered     chan struct{} // closed when that first caller has arrived
	held        bool
}

func newGatedLedger() *gatedLedger {
	return &gatedLedger{credited: map[string]Deposit{}, final: map[string]bool{}, creditCalls: map[string]int{},
		entered: make(chan struct{})}
}

func (l *gatedLedger) Credit(_ context.Context, d Deposit) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.creditCalls[key(d.TxID, d.Vout)]++
	l.credited[key(d.TxID, d.Vout)] = d
	return nil
}

func (l *gatedLedger) IsCredited(_ context.Context, txid string, vout uint32) (bool, error) {
	l.mu.Lock()
	_, ok := l.credited[key(txid, vout)]
	hold := l.hold
	first := hold != nil && !l.held
	if first {
		l.held = true
		close(l.entered)
	}
	l.mu.Unlock()
	if first {
		<-hold
	}
	return ok, nil
}

func (l *gatedLedger) Pending(context.Context) ([]Deposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}

func (l *gatedLedger) MarkFinal(_ context.Context, txid string, vout uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.final[key(txid, vout)] = true
	return nil
}

func (l *gatedLedger) calls(txid string, vout uint32) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.creditCalls[key(txid, vout)]
}

// quietLedger is a fakeLedger whose Pending leaves every outpoint out, as a
// book does for a deposit it has swept: the credited outpoint is then asked
// about in Scan's own loop rather than in recheckPending.
type quietLedger struct{ *fakeLedger }

func (quietLedger) Pending(context.Context) ([]Deposit, error) { return nil, nil }

// countingNode counts the passes that reached the node, which is how a test
// sees whether a second Scan ran while the first was held.
type countingNode struct {
	fakeNode
	passes atomic.Int32
}

func (n *countingNode) RequireSynced(ctx context.Context) error {
	n.passes.Add(1)
	return n.fakeNode.RequireSynced(ctx)
}

// recordingAlerts keeps kinds and messages, goroutine-safe.
type recordingAlerts struct {
	mu    sync.Mutex
	kinds []AlertKind
	msgs  []string
}

func (a *recordingAlerts) fn(k AlertKind, m string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kinds = append(a.kinds, k)
	a.msgs = append(a.msgs, m)
}

func (a *recordingAlerts) count(k AlertKind) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, x := range a.kinds {
		if x == k {
			n++
		}
	}
	return n
}

// A credited deposit the node now reports shallower than the policy requires
// is a reorganisation that re-included it lower down. It is alarmed on every
// pass, the credit stands, and it is not marked final. This is the pending
// site: the ledger still lists the outpoint.
func TestDepthRegressionOfAPendingDepositIsAlarmed(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	al := &recordingAlerts{}
	m.OnAlert = al.fn
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 960, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 41, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("credit: %v %+v", err, got)
	}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 998, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 3, false)
	for i := 0; i < 3; i++ {
		if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 {
			t.Fatalf("scan %d: %v %+v", i, err, got)
		}
	}
	if al.count(AlertDepositRegressed) != 3 {
		t.Fatalf("want one deposit_regressed alert per pass, got %v", al.kinds)
	}
	if !strings.Contains(al.msgs[0], "depth 3") || !strings.Contains(al.msgs[0], "required 30") {
		t.Fatalf("the alert names neither the node's depth nor the policy: %s", al.msgs[0])
	}
	if led.final[key(txA, 0)] {
		t.Fatal("marked final at 3 confirmations")
	}
	if _, ok := led.credited[key(txA, 0)]; !ok {
		t.Fatal("the credit did not stand")
	}
}

// The same regression at the other site that asks the node about a credited
// output: one the ledger leaves out of Pending.
func TestDepthRegressionOfASweptDepositIsAlarmed(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	al := &recordingAlerts{}
	m.OnAlert = al.fn
	m.Ledger = quietLedger{led}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 960, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 41, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("credit: %v %+v", err, got)
	}
	node.outs[key(txA, 0)] = txout(t, a, 100, 3, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("scan: %v %+v", err, got)
	}
	if al.count(AlertDepositRegressed) != 1 {
		t.Fatalf("want one deposit_regressed alert, got %v", al.kinds)
	}
	if m.isFinal(txA, 0) {
		t.Fatal("entered the final set at 3 confirmations")
	}
}

// Two callers of Scan run one pass at a time, so an outpoint both would
// credit reaches Credit once. The first pass is held inside IsCredited; the
// second must not have reached the node before the first is released.
func TestConcurrentScansRunOneAtATimeAndCreditOnce(t *testing.T) {
	m, cache, _, _, _, a := setup(t)
	node := &countingNode{fakeNode: fakeNode{synced: true, chain: types.Mainnet.ChainID, tip: 1000, outs: map[string]*rpc.TxOut{}}}
	m.Node = node
	led := newGatedLedger()
	led.hold = make(chan struct{})
	m.Ledger = led
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 101, false)

	var wg sync.WaitGroup
	var credited atomic.Int32
	scan := func() {
		defer wg.Done()
		got, err := m.Scan(context.Background())
		if err != nil {
			t.Errorf("scan: %v", err)
		}
		credited.Add(int32(len(got)))
	}
	wg.Add(2)
	go scan()
	<-led.entered // the first pass is inside IsCredited
	go scan()
	time.Sleep(50 * time.Millisecond)
	if n := node.passes.Load(); n != 1 {
		t.Fatalf("the second Scan reached the node while the first was in progress: %d passes", n)
	}
	close(led.hold)
	wg.Wait()
	if got := led.calls(txA, 0); got != 1 {
		t.Fatalf("Credit called %d times for one outpoint", got)
	}
	if credited.Load() != 1 {
		t.Fatalf("the two passes returned %d credits together", credited.Load())
	}
}

// Deposit.Height is the node's word: the tip the pass read less the depth
// the node reported, plus one. The indexer's height decides only whether the
// output is asked about.
func TestDepositHeightIsTheNodes(t *testing.T) {
	m, cache, node, _, _, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 500, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 101, false)
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if got[0].Height != 900 || got[0].Confirmations != 101 {
		t.Fatalf("Height %d Confirmations %d; want the node's 900 and 101", got[0].Height, got[0].Confirmations)
	}
}

// An address the indexer has not yet answered for is awaiting its first
// reply, not stale: quiet for one MaxCacheAge from the pass that first listed
// it, then stale as any other. A pass whose every address is awaiting credits
// nothing and returns no error.
func TestAddressesAwaitingTheFirstReplyAreQuietForOneWindow(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	per := &fakeCachePerAddr{fakeCache: *cache, ats: map[string]time.Time{}, errs: map[string]error{}}
	m.Cache = per
	start := m.now()
	clock := start
	m.now = func() time.Time { return clock }
	per.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 100, 101, false)

	for i := 0; i < 3; i++ {
		clock = start.Add(time.Duration(i) * time.Minute)
		if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 {
			t.Fatalf("scan %d while awaiting: %v %+v", i, err, got)
		}
	}
	if al.count(AlertCacheStale) != 0 {
		t.Fatalf("alarmed while awaiting the first reply: %v", al.kinds)
	}
	clock = start.Add(m.maxCacheAge() + time.Second)
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrPaused) {
		t.Fatalf("past the window with no reply: %v, want ErrPaused", err)
	}
	if al.count(AlertCacheStale) != 1 {
		t.Fatalf("past the window: %v", al.kinds)
	}
	per.ats[a] = clock
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 || len(led.credited) != 1 {
		t.Fatalf("after the first reply: %v %+v", err, got)
	}
}

// The asset gate is the node's script. An indexer that reports a USDSOQ
// output as SOQ under a v1 address is refused because the node's script is a
// v7 holding script, whatever the indexer's asset flag says.
func TestUSDSOQOutputIsRefusedByTheNodeScript(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	v1 := scriptHex(t, a)
	v7 := "57" + v1[2:]
	node.outs[key(txA, 0)] = &rpc.TxOut{Value: 5_000_000_000, Confirmations: 101,
		ScriptPubKey: rpc.ScriptPubKey{Hex: v7}}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 5_000_000_000, Height: 900, Address: a, AssetType: types.AssetTypeSOQ}}
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("USDSOQ credited as SOQ: %v %+v", err, got)
	}
	if al.count(AlertIndexerMismatch) != 1 {
		t.Fatalf("alerts %v", al.kinds)
	}
}
