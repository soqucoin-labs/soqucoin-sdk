package deposit

import (
	"encoding/hex"
	"errors"
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
	synced bool
	tip    int64
	outs   map[string]*rpc.TxOut // "txid:vout"
	calls  int
}

func key(txid string, vout uint32) string { return txid + ":" + string(rune('0'+vout)) }

func (n *fakeNode) RequireSynced() error {
	if !n.synced {
		return rpc.ErrNodeSyncing
	}
	return nil
}
func (n *fakeNode) GetBlockCount() (int64, error) { return n.tip, nil }
func (n *fakeNode) GetTxOut(txid string, vout uint32, _ bool) (*rpc.TxOut, error) {
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
func (l *fakeLedger) Credit(d Deposit) error {
	l.credited[key(d.TxID, d.Vout)] = d
	return nil
}
func (l *fakeLedger) IsCredited(txid string, vout uint32) (bool, error) {
	_, ok := l.credited[key(txid, vout)]
	return ok, nil
}
func (l *fakeLedger) Pending() ([]Deposit, error) {
	var out []Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}
func (l *fakeLedger) MarkFinal(txid string, vout uint32) error {
	l.final[key(txid, vout)] = true
	return nil
}

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
	node := &fakeNode{synced: true, tip: 1000, outs: map[string]*rpc.TxOut{}}
	led := newLedger()
	al := &alerts{}
	m := &Monitor{
		Cache: cache, Node: node, Ledger: led,
		Addresses: func() []string { return []string{a} },
		Required:  func(int64) int64 { return 30 },
		OnAlert:   al.fn,
		now:       func() time.Time { return now },
	}
	return m, cache, node, led, al, a
}

// txout builds the node's view of an output paying `a`.
func txout(t *testing.T, a string, soq float64, confs int64, coinbase bool) *rpc.TxOut {
	t.Helper()
	return &rpc.TxOut{Value: soq, Confirmations: confs, Coinbase: coinbase,
		ScriptPubKey: rpc.ScriptPubKey{Hex: scriptHex(t, a)}}
}

// ── tests ──────────────────────────────────────────────────────────────────

// The happy path: indexer and node agree, the deposit is credited once.
func TestCreditsWhenNodeAgrees(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 1.5, 101, false)

	got, err := m.Scan()
	if err != nil || len(got) != 1 || got[0].TxID != txA || got[0].Value != 150_000_000 {
		t.Fatalf("scan: %v %+v", err, got)
	}
	// Second scan: already credited, not credited again.
	got, err = m.Scan()
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
			types.UTXO{TxID: txA, Vout: 0, Value: 200_000_000, Height: 900}, nil}, // set below with real value 1.5
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
			node.outs[key(txA, 0)] = txout(t, a, 1.5, 101, false)
		case 2:
			node.outs[key(txA, 0)] = txout(t, addr(t, 0x22), 1.5, 101, false)
		case 3:
			node.outs[key(txA, 0)] = txout(t, a, 1.5, 5, false)
		}
		got, err := m.Scan()
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
	node.outs[key(txA, 0)] = txout(t, a, 1.5, 1002, false)
	node.outs[key(txB, 0)] = txout(t, a, 1.5, 0, false)
	if got, err := m.Scan(); err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("credited on non-positive height: %v %+v", err, got)
	}
}

// Nothing is credited while the node is syncing or the indexer is stale.
func TestPausesWhenNodeSyncingOrCacheStale(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 1.5, 101, false)

	node.synced = false
	if _, err := m.Scan(); !errors.Is(err, ErrPaused) || !al.has(AlertNodeSyncing) {
		t.Fatalf("syncing node: err=%v alerts=%v", err, al.kinds)
	}
	node.synced = true

	cache.at = m.now().Add(-time.Hour)
	if _, err := m.Scan(); !errors.Is(err, ErrPaused) || !al.has(AlertCacheStale) {
		t.Fatalf("stale cache: err=%v alerts=%v", err, al.kinds)
	}
	cache.at = m.now()
	cache.err = errors.New("refresh failed")
	if _, err := m.Scan(); !errors.Is(err, ErrPaused) {
		t.Fatalf("errored cache: %v", err)
	}
	if len(led.credited) != 0 {
		t.Fatal("credited while paused")
	}
	cache.err = nil
	if got, err := m.Scan(); err != nil || len(got) != 1 {
		t.Fatalf("after recovery: %v %+v", err, got)
	}
}

func TestImmatureCoinbaseWaitsWithoutAlarm(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 8_800_000_000, Height: 950, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 88, 51, true)
	if got, _ := m.Scan(); len(got) != 0 || len(led.credited) != 0 {
		t.Fatal("immature coinbase credited")
	}
	if len(al.kinds) != 0 {
		t.Errorf("immature coinbase is not an alarm: %v", al.kinds)
	}
	node.outs[key(txA, 0)] = txout(t, a, 88, types.CoinbaseMaturity, true)
	if got, _ := m.Scan(); len(got) != 1 {
		t.Fatal("mature coinbase not credited")
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
	node.outs[key(txA, 0)] = txout(t, a, 1.5, 101, false)
	node.outs[key(txB, 0)] = txout(t, a, 1.5, 101, false)
	if got, err := m.Scan(); err != nil || len(got) != 2 {
		t.Fatalf("initial credit: %v %+v", err, got)
	}
	// A reorg removes A; B is buried past the horizon.
	delete(node.outs, key(txA, 0))
	node.outs[key(txB, 0)] = txout(t, a, 1.5, types.MaxReorgDepth+1, false)
	if _, err := m.Scan(); err != nil {
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
	m.Scan()
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
	node.outs[key(txA, 0)] = txout(t, a, 50, 51, false)
	node.outs[key(txB, 0)] = txout(t, a, 1, 51, false)
	got, err := m.Scan()
	if err != nil || len(got) != 1 || got[0].TxID != txB {
		t.Fatalf("policy: %v %+v", err, got)
	}
	if _, ok := led.credited[key(txA, 0)]; ok {
		t.Error("large deposit credited before its required depth")
	}
}
