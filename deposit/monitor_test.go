package deposit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const (
	txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func addr(t *testing.T, fill byte) string {
	t.Helper()
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = fill
	}
	a, err := address.Encode("sq", 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func scriptHex(t *testing.T, a string) string {
	t.Helper()
	spk, err := address.ScriptFor(a)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(spk)
}

// ── fakes ──────────────────────────────────────────────────────────────────

type fakeCache struct {
	utxos map[string][]types.UTXO
	at    time.Time
	err   error
}

func (c *fakeCache) GetUTXOs(a string) []types.UTXO  { return c.utxos[a] }
func (c *fakeCache) LastRefresh() (time.Time, error) { return c.at, c.err }

type fakeNode struct {
	synced  bool
	syncErr error  // returned by RequireSynced when set, in place of ErrNodeSyncing
	chain   string // what getblockchaininfo would report
	tip     int64
	outs    map[string]*rpc.TxOut // "txid:vout"
	calls   int
}

func (n *fakeNode) RequireChain(_ context.Context, want string) error {
	if want != "" && n.chain != want {
		return fmt.Errorf("%w: node reports %q, configured for %q", rpc.ErrWrongChain, n.chain, want)
	}
	return nil
}

func key(txid string, vout uint32) string { return txid + ":" + string(rune('0'+vout)) }

func (n *fakeNode) RequireSynced(_ context.Context) error {
	if n.syncErr != nil {
		return n.syncErr
	}
	if !n.synced {
		return rpc.ErrNodeSyncing
	}
	return nil
}
func (n *fakeNode) GetBlockCount(_ context.Context) (int64, error) { return n.tip, nil }
func (n *fakeNode) GetTxOut(_ context.Context, txid string, vout uint32, _ bool) (*rpc.TxOut, error) {
	n.calls++
	return n.outs[key(txid, vout)], nil
}

type fakeLedger struct {
	credited map[string]Deposit
	final    map[string]bool
}

func newLedger() *fakeLedger {
	return &fakeLedger{credited: map[string]Deposit{}, final: map[string]bool{}}
}
func (l *fakeLedger) Credit(_ context.Context, d Deposit) error {
	l.credited[key(d.TxID, d.Vout)] = d
	return nil
}
func (l *fakeLedger) IsCredited(_ context.Context, txid string, vout uint32) (bool, error) {
	_, ok := l.credited[key(txid, vout)]
	return ok, nil
}
func (l *fakeLedger) Pending(_ context.Context) ([]Deposit, error) {
	var out []Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}
func (l *fakeLedger) MarkFinal(_ context.Context, txid string, vout uint32) error {
	l.final[key(txid, vout)] = true
	return nil
}

// fakeCachePerAddr is a cache that also reports freshness per address, as
// *electrumx.Client does.
type fakeCachePerAddr struct {
	fakeCache
	ats  map[string]time.Time
	errs map[string]error
}

func (c *fakeCachePerAddr) LastRefreshOf(a string) (time.Time, error) { return c.ats[a], c.errs[a] }

type alerts struct{ kinds []AlertKind }

func (a *alerts) fn(k AlertKind, _ string) { a.kinds = append(a.kinds, k) }
func (a *alerts) has(k AlertKind) bool {
	for _, x := range a.kinds {
		if x == k {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*Monitor, *fakeCache, *fakeNode, *fakeLedger, *alerts, string) {
	t.Helper()
	a := addr(t, 0x11)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	cache := &fakeCache{utxos: map[string][]types.UTXO{}, at: now}
	node := &fakeNode{synced: true, chain: types.Mainnet.ChainID, tip: 1000, outs: map[string]*rpc.TxOut{}}
	led := newLedger()
	al := &alerts{}
	m := &Monitor{
		Cache: cache, Node: node, Ledger: led,
		Addresses: func(context.Context) []string { return []string{a} },
		Required:  func(int64) int64 { return 30 },
		OnAlert:   al.fn,
		now:       func() time.Time { return now },
	}
	return m, cache, node, led, al, a
}

// txout builds the node's view of an output paying `a`.
func txout(t *testing.T, a string, shors int64, confs int64, coinbase bool) *rpc.TxOut {
	t.Helper()
	return &rpc.TxOut{Value: shors, Confirmations: confs, Coinbase: coinbase,
		ScriptPubKey: rpc.ScriptPubKey{Hex: scriptHex(t, a)}}
}

// ── tests ──────────────────────────────────────────────────────────────────

// The happy path: indexer and node agree, the deposit is credited once.
func TestCreditsWhenNodeAgrees(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)

	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].TxID != txA || got[0].Value != 150_000_000 {
		t.Fatalf("scan: %v %+v", err, got)
	}
	// Second scan: already credited, not credited again.
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("second scan credited again: %v %+v", err, got)
	}
	if len(led.credited) != 1 || len(al.kinds) != 0 {
		t.Errorf("ledger %d entries, alerts %v", len(led.credited), al.kinds)
	}
}

// The threat model: a compromised indexer invents or alters a deposit. The
// node is the authority; nothing is credited and the operator is alerted.
func TestRefusesWhatTheNodeDoesNotConfirm(t *testing.T) {
	cases := []struct {
		name string
		utxo types.UTXO
		out  *rpc.TxOut
	}{
		{"output does not exist on the node",
			types.UTXO{TxID: txA, Vout: 0, Value: 100, Height: 900}, nil},
		{"indexer inflates the value",
			types.UTXO{TxID: txA, Vout: 0, Value: 200_000_000, Height: 900}, nil}, // set below with real value 150_000_000
		{"indexer attributes another address's output to ours",
			types.UTXO{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900}, nil}, // set below with other script
		{"indexer claims depth the node has not seen",
			types.UTXO{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900}, nil}, // node confs 5
	}
	for i, tc := range cases {
		m, cache, node, led, al, a := setup(t)
		tc.utxo.Address = a
		cache.utxos[a] = []types.UTXO{tc.utxo}
		switch i {
		case 1:
			node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)
		case 2:
			node.outs[key(txA, 0)] = txout(t, addr(t, 0x22), 150_000_000, 101, false)
		case 3:
			node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 5, false)
		}
		got, err := m.Scan(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 0 || len(led.credited) != 0 {
			t.Errorf("%s: CREDITED %+v", tc.name, got)
		}
		if !al.has(AlertIndexerMismatch) {
			t.Errorf("%s: no mismatch alert raised", tc.name)
		}
	}
}

// A negative height from a lying indexer used to compute confirmations as
// tip+2 and pass the "unconfirmed" guard.
func TestNegativeOrZeroHeightIsNeverConfirmed(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	cache.utxos[a] = []types.UTXO{
		{TxID: txA, Vout: 0, Value: 150_000_000, Height: -1, Address: a},
		{TxID: txB, Vout: 0, Value: 150_000_000, Height: 0, Address: a},
	}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 1002, false)
	node.outs[key(txB, 0)] = txout(t, a, 150_000_000, 0, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("credited on non-positive height: %v %+v", err, got)
	}
}

// Nothing is credited while the node is syncing or the indexer is stale.
func TestPausesWhenNodeSyncingOrCacheStale(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)

	node.synced = false
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrPaused) || !al.has(AlertNodeSyncing) {
		t.Fatalf("syncing node: err=%v alerts=%v", err, al.kinds)
	}
	node.synced = true

	cache.at = m.now().Add(-time.Hour)
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrPaused) || !al.has(AlertCacheStale) {
		t.Fatalf("stale cache: err=%v alerts=%v", err, al.kinds)
	}
	cache.at = m.now()
	cache.err = errors.New("refresh failed")
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrPaused) {
		t.Fatalf("errored cache: %v", err)
	}
	if len(led.credited) != 0 {
		t.Fatal("credited while paused")
	}
	cache.err = nil
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("after recovery: %v %+v", err, got)
	}
}

// A coinbase deposit is real and waits for the network's maturity without an
// alarm; it is credited at exactly that depth and not one block earlier. 240,
// the upstream value the SDK carried through v0.3.4, is immature on mainnet.
// Mainnet is the default when Network is unset; regtest matures at 60.
func TestImmatureCoinbaseWaitsWithoutAlarm(t *testing.T) {
	cases := []struct {
		name     string
		network  types.Network
		immature []int64
		mature   int64
	}{
		{"mainnet by default", types.Network{}, []int64{51, 240, 287}, 288},
		{"mainnet", types.Mainnet, []int64{240}, 288},
		{"stagenet", types.Stagenet, []int64{30, 287}, 288},
		{"regtest", types.Regtest, []int64{59}, 60},
		{"hand-built network without a maturity", types.Network{ChainID: "main"}, []int64{287}, 288},
	}
	for _, c := range cases {
		m, cache, node, led, al, a := setup(t)
		m.Network = c.network
		if c.network.ChainID != "" {
			node.chain = c.network.ChainID
		}
		cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 8_800_000_000, Height: 950, Address: a}}
		for _, confs := range c.immature {
			node.outs[key(txA, 0)] = txout(t, a, 8_800_000_000, confs, true)
			if got, _ := m.Scan(context.Background()); len(got) != 0 || len(led.credited) != 0 {
				t.Fatalf("%s: coinbase at %d confirmations credited", c.name, confs)
			}
			if len(al.kinds) != 0 {
				t.Errorf("%s: an immature coinbase is not an alarm: %v", c.name, al.kinds)
			}
		}
		node.outs[key(txA, 0)] = txout(t, a, 8_800_000_000, c.mature, true)
		if got, _ := m.Scan(context.Background()); len(got) != 1 {
			t.Fatalf("%s: coinbase at %d confirmations not credited", c.name, c.mature)
		}
	}
}

// A credited deposit that leaves the node's UTXO set before finality is
// alarmed; one that passes the horizon is marked final and no longer checked.
func TestRecheckPendingAlarmsVanishedAndMarksFinal(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{
		{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a},
		{TxID: txB, Vout: 0, Value: 150_000_000, Height: 900, Address: a},
	}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)
	node.outs[key(txB, 0)] = txout(t, a, 150_000_000, 101, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 2 {
		t.Fatalf("initial credit: %v %+v", err, got)
	}
	// A reorg removes A; B is buried past the horizon.
	delete(node.outs, key(txA, 0))
	node.outs[key(txB, 0)] = txout(t, a, 150_000_000, types.MaxReorgDepth+1, false)
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !al.has(AlertDepositVanished) {
		t.Fatal("vanished credited deposit not alarmed")
	}
	if !led.final[key(txB, 0)] {
		t.Error("deposit past the horizon not marked final")
	}
	if led.final[key(txA, 0)] {
		t.Error("vanished deposit marked final")
	}
	// Final deposits are not re-queried.
	before := node.calls
	m.Scan(context.Background())
	if node.calls-before > 1 { // only A (still pending) is re-checked
		t.Errorf("final deposits still re-queried: %d calls", node.calls-before)
	}
}

// Confirmation policy scales with value and is applied from the node's depth.
func TestPolicyAppliedFromNodeDepth(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	m.Required = func(v int64) int64 {
		if v > 1_000_000_000 {
			return 120
		}
		return 30
	}
	cache.utxos[a] = []types.UTXO{
		{TxID: txA, Vout: 0, Value: 5_000_000_000, Height: 950, Address: a}, // 51 confs, needs 120
		{TxID: txB, Vout: 0, Value: 100_000_000, Height: 950, Address: a},   // 51 confs, needs 30
	}
	node.outs[key(txA, 0)] = txout(t, a, 5_000_000_000, 51, false)
	node.outs[key(txB, 0)] = txout(t, a, 100_000_000, 51, false)
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].TxID != txB {
		t.Fatalf("policy: %v %+v", err, got)
	}
	if _, ok := led.credited[key(txA, 0)]; ok {
		t.Error("large deposit credited before its required depth")
	}
}

// A Monitor configured for regtest whose node serves mainnet must credit
// nothing: with the regtest maturity of 60 it would otherwise credit a mainnet
// coinbase 228 blocks before consensus lets it be spent. The refusal is a
// permanent deployment error with its own alert, not a syncing pause, whether
// the node itself reports the mismatch (an rpc.Client with Network set) or the
// Monitor asks (an rpc.Client whose Network was left unset). A Monitor with no
// Network asks for mainnet; see TestUnsetNetworkIsMainnetForTheChainCheckToo.
func TestMonitorRefusesANodeOnAnotherChain(t *testing.T) {
	deposit := func(m *Monitor, cache *fakeCache, node *fakeNode, a string) {
		cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 8_800_000_000, Height: 950, Address: a}}
		node.outs[key(txA, 0)] = txout(t, a, 8_800_000_000, 60, true) // mature on regtest, immature on mainnet
	}
	check := func(name string, m *Monitor, led *fakeLedger, al *alerts) {
		got, err := m.Scan(context.Background())
		if len(got) != 0 || len(led.credited) != 0 {
			t.Fatalf("%s: credited %+v on a node serving another chain", name, got)
		}
		if !errors.Is(err, rpc.ErrWrongChain) || !errors.Is(err, rpc.ErrPermanent) || errors.Is(err, ErrPaused) {
			t.Errorf("%s: got %v, want rpc.ErrWrongChain (permanent, not a pause)", name, err)
		}
		if len(al.kinds) != 1 || al.kinds[0] != AlertNodeWrongChain {
			t.Errorf("%s: alerts %v, want exactly one %s", name, al.kinds, AlertNodeWrongChain)
		}
	}

	// The Monitor asks: node reports mainnet, Monitor is regtest.
	m, cache, node, led, al, a := setup(t)
	m.Network = types.Regtest
	deposit(m, cache, node, a)
	check("monitor asks", m, led, al)

	// The node reports it itself from RequireSynced.
	m, cache, node, led, al, a = setup(t)
	m.Network = types.Regtest
	node.syncErr = fmt.Errorf("%w: node reports %q, configured for %q", rpc.ErrWrongChain, "main", "regtest")
	deposit(m, cache, node, a)
	check("node reports", m, led, al)
}

// With per-address freshness, one address the indexer has not refreshed is
// skipped and alarmed while the others are credited; only when every address
// is stale does the scan pause. The global LastRefresh is not consulted.
func TestStaleAddressesAreSkippedNotAllOfThem(t *testing.T) {
	m, _, node, led, al, a1 := setup(t)
	a2 := addr(t, 0x22)
	now := m.now()
	cache := &fakeCachePerAddr{
		fakeCache: fakeCache{utxos: map[string][]types.UTXO{}, at: time.Time{}, err: errors.New("one address failed")},
		ats:       map[string]time.Time{a1: now, a2: now.Add(-time.Hour)},
		errs:      map[string]error{a2: errors.New("boom")},
	}
	m.Cache = cache
	m.Addresses = func(context.Context) []string { return []string{a1, a2} }
	cache.utxos[a1] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a1}}
	cache.utxos[a2] = []types.UTXO{{TxID: txB, Vout: 0, Value: 150_000_000, Height: 900, Address: a2}}
	node.outs[key(txA, 0)] = txout(t, a1, 150_000_000, 101, false)
	node.outs[key(txB, 0)] = txout(t, a2, 150_000_000, 101, false)

	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("one stale address must not pause the scan: %v", err)
	}
	if len(got) != 1 || got[0].TxID != txA {
		t.Fatalf("credited %+v, want only the fresh address's deposit", got)
	}
	if _, ok := led.credited[key(txB, 0)]; ok {
		t.Fatal("a deposit at a stale address was credited")
	}
	if !al.has(AlertCacheStale) {
		t.Fatal("the stale address was not alarmed")
	}

	// Every address stale: paused, nothing credited.
	cache.ats[a1] = now.Add(-time.Hour)
	al.kinds = nil
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrPaused) || !al.has(AlertCacheStale) {
		t.Fatalf("all stale: err=%v alerts=%v", err, al.kinds)
	}
	if _, ok := led.credited[key(txB, 0)]; ok {
		t.Fatal("credited while every address was stale")
	}

	// A never-refreshed address is stale too; a refreshed one is credited.
	cache.ats[a1] = now
	delete(cache.ats, a2)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("a1 already credited, a2 never refreshed: %v %+v", err, got)
	}
	cache.ats[a2] = now
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 || got[0].TxID != txB {
		t.Fatalf("after a2 refreshed: %v %+v", err, got)
	}
}
