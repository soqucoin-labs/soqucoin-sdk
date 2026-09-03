//go:build integration

package integration

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// memLedger is the harness's book.
type memLedger struct {
	credited map[string]deposit.Deposit
	final    map[string]bool
}

func newLedger() *memLedger {
	return &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{}}
}
func (l *memLedger) Credit(d deposit.Deposit) error { l.credited[okey(d.TxID, d.Vout)] = d; return nil }
func (l *memLedger) IsCredited(txid string, vout uint32) (bool, error) {
	_, ok := l.credited[okey(txid, vout)]
	return ok, nil
}
func (l *memLedger) Pending() ([]deposit.Deposit, error) {
	var out []deposit.Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}
func (l *memLedger) MarkFinal(txid string, vout uint32) error {
	l.final[okey(txid, vout)] = true
	return nil
}

// fixture: a node with a funded hot wallet (mature coinbase) and a deposit
// address, plus the scanner tracking both.
type fixture struct {
	n       *node
	hotKeys interface {
		Sign(string, []byte) ([]byte, error)
		PublicKeyFor(string) ([]byte, error)
	}
	hot, dep  string
	scan      *scanner
	alerts    []deposit.AlertKind
	alertMsgs []string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	n := startNode(t)
	hotKeys, hot := newKey(t)
	_, dep := newKey(t)
	f := &fixture{n: n, hotKeys: hotKeys, hot: hot, dep: dep}
	// Fund the hot wallet with coinbase and bury it past coinbase maturity.
	n.mine(hot, 5)
	n.mine(dep, 1) // one deposit worth of coinbase to the deposit address, buried below
	n.mine(hot, int(types.CoinbaseMaturity)+5)
	f.scan = newScanner(n, hot, dep)
	if err := f.scan.RefreshAll(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) monitor(t *testing.T, led *memLedger, required int64) *deposit.Monitor {
	return &deposit.Monitor{
		Cache: f.scan, Node: f.n.rpc, Ledger: led,
		Addresses: func() []string { return []string{f.dep} },
		Required:  func(int64) int64 { return required },
		OnAlert: func(k deposit.AlertKind, m string) {
			f.alerts = append(f.alerts, k)
			f.alertMsgs = append(f.alertMsgs, m)
		},
	}
}

func (f *fixture) engine(t *testing.T, store withdraw.Store, spent *utxo.SpentSet, bc withdraw.Broadcaster) *withdraw.Engine {
	t.Helper()
	changeSPK, err := address.ScriptFor(f.hot)
	if err != nil {
		t.Fatal(err)
	}
	sel := utxo.NewCoinSelector(spent)
	return &withdraw.Engine{
		Store: store, Spent: spent, Broadcaster: bc,
		Confirmer:             withdraw.RPCConfirmer{Client: f.n.rpc},
		RequiredConfirmations: 3, ReservationTTL: time.Minute,
		Select: func(amount, feeRate int64) ([]types.UTXO, error) {
			if err := f.n.rpc.RequireSynced(); err != nil {
				return nil, err
			}
			if err := f.scan.RefreshAll(); err != nil {
				return nil, err
			}
			// Every coin in the harness is a coinbase, so require maturity here;
			// VerifyAndFilterUTXOs would otherwise drop an immature pick and the
			// selection would come back empty.
			selected, _, err := sel.SelectUTXOs(f.scan.GetAllUTXOs(), amount+1100*feeRate, int(types.CoinbaseMaturity)+1, f.n.height(), []string{f.hot})
			if err != nil {
				return nil, err
			}
			return f.n.rpc.VerifyAndFilterUTXOs(selected, nil, nil)
		},
		BuildSign: func(inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
			spk, err := address.ScriptFor(to)
			if err != nil {
				return "", "", err
			}
			return tx.BuildAndSign(inputs, spk, amount, changeSPK, feeRate, f.hotKeys)
		},
	}
}

// 1. A deposit is credited only after the node confirms it, and exactly once.
func TestDepositCreditedAfterNodeCrossCheck(t *testing.T) {
	f := setup(t)
	led := newLedger()
	m := f.monitor(t, led, 30)
	got, err := m.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Address != f.dep {
		t.Fatalf("credited %+v, want the one coinbase deposit to %s", got, f.dep)
	}
	if got[0].Value != 500_000*types.ShorsPerSOQ {
		t.Errorf("value %d, want the regtest coinbase reward", got[0].Value)
	}
	if again, _ := m.Scan(); len(again) != 0 {
		t.Fatal("deposit credited twice")
	}
	if len(f.alerts) != 0 {
		t.Errorf("alerts on a clean credit: %v", f.alertMsgs)
	}
}

// 2. A withdrawal built, signed and broadcast by the SDK is accepted by the
// node, mined, and confirmed; its change comes back to the hot wallet.
func TestWithdrawalEndToEnd(t *testing.T) {
	f := setup(t)
	_, recipient := newKey(t)
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent.json"))
	e := f.engine(t, withdraw.NewMemStore(), spent, f.n.rpc)
	amount := 1_000 * types.ShorsPerSOQ
	if _, _, err := e.Submit("wd-1", recipient, amount, types.RecommendedFeeRate); err != nil {
		t.Fatal(err)
	}
	in, err := e.Process("wd-1")
	if err != nil {
		t.Fatalf("process: %v (state %s, last error %s)", err, in.State, in.LastError)
	}
	if in.State != withdraw.StateBroadcast {
		t.Fatalf("state %s", in.State)
	}
	// The node has it in its mempool under the txid the SDK computed.
	if _, err := f.n.rpc.Call("getmempoolentry", in.TxID); err != nil {
		t.Fatalf("node does not have %s in its mempool: %v", in.TxID, err)
	}
	f.n.mine(f.hot, 3)
	if err := e.UpdateConfirmations(in); err != nil {
		t.Fatal(err)
	}
	if in.State != withdraw.StateConfirmed || in.Confirmations < 3 {
		t.Fatalf("after 3 blocks: %+v", in)
	}
	// Recipient sees exactly the amount; the scanner sees the change.
	rs := newScanner(f.n, recipient)
	if err := rs.RefreshAll(); err != nil {
		t.Fatal(err)
	}
	got := rs.GetUTXOs(recipient)
	if len(got) != 1 || got[0].Value != amount {
		t.Fatalf("recipient UTXOs %+v, want exactly %d", got, amount)
	}
}

// 3. The reply to the broadcast is lost. The intent stays Built, the retry
// sends the same bytes, and the chain ends up with ONE transaction.
type lossyBroadcaster struct {
	inner *rpc.Client
	drop  bool
	sent  int
}

func (l *lossyBroadcaster) Broadcast(raw, txid string) (string, error) {
	l.sent++
	got, err := l.inner.Broadcast(raw, txid)
	if l.drop {
		l.drop = false
		return "", errors.Join(rpc.ErrUnknownOutcome, errors.New("harness dropped the reply"))
	}
	return got, err
}

func TestLostBroadcastReplyNeverPaysTwice(t *testing.T) {
	f := setup(t)
	_, recipient := newKey(t)
	dir := t.TempDir()
	store, _ := withdraw.NewFileStore(filepath.Join(dir, "intents.json"))
	spent := utxo.NewSpentSet(filepath.Join(dir, "spent.json"))
	lb := &lossyBroadcaster{inner: f.n.rpc, drop: true}
	e := f.engine(t, store, spent, lb)
	amount := 700 * types.ShorsPerSOQ
	e.Submit("wd-lost", recipient, amount, types.RecommendedFeeRate)
	in, err := e.Process("wd-lost")
	if !errors.Is(err, rpc.ErrUnknownOutcome) || in.State != withdraw.StateBuilt {
		t.Fatalf("lost reply: err=%v state=%s", err, in.State)
	}
	// "Restart": a new engine over the same files recovers and re-sends the
	// SAME bytes; the node reports the duplicate as already known.
	store2, _ := withdraw.NewFileStore(filepath.Join(dir, "intents.json"))
	e2 := f.engine(t, store2, utxo.NewSpentSet(filepath.Join(dir, "spent.json")), f.n.rpc)
	e2.BuildSign = func([]types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("recovery rebuilt a transaction")
		return "", "", nil
	}
	if err := e2.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	after, _, _ := store2.Get("wd-lost")
	if after.State != withdraw.StateBroadcast || after.TxID != in.TxID {
		t.Fatalf("recovered %+v", after)
	}
	f.n.mine(f.hot, 2)
	rs := newScanner(f.n, recipient)
	rs.RefreshAll()
	if got := rs.GetUTXOs(recipient); len(got) != 1 || got[0].Value != amount {
		t.Fatalf("recipient has %+v; a lost reply must never produce a second payment", got)
	}
}

// 4. Two withdrawals cannot spend the same input, and a permanent rejection
// releases the inputs for the next one.
func TestConcurrentWithdrawalsNeverShareInputs(t *testing.T) {
	f := setup(t)
	_, r1 := newKey(t)
	_, r2 := newKey(t)
	spent := utxo.NewSpentSet(filepath.Join(t.TempDir(), "spent.json"))
	e := f.engine(t, withdraw.NewMemStore(), spent, f.n.rpc)
	e.Submit("a", r1, 400_000*types.ShorsPerSOQ, types.RecommendedFeeRate)
	e.Submit("b", r2, 400_000*types.ShorsPerSOQ, types.RecommendedFeeRate)
	a, _, _ := e.Store.Get("a")
	b, _, _ := e.Store.Get("b")
	if err := e.Build(a); err != nil {
		t.Fatal(err)
	}
	if err := e.Build(b); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range append(a.Inputs, b.Inputs...) {
		k := okey(o.TxID, o.Vout)
		if seen[k] {
			t.Fatalf("input %s selected by both withdrawals", k)
		}
		seen[k] = true
	}
	if err := e.Broadcast(a); err != nil {
		t.Fatal(err)
	}
	if err := e.Broadcast(b); err != nil {
		t.Fatalf("second withdrawal rejected although it holds different inputs: %v", err)
	}
	f.n.mine(f.hot, 1)
	if h := f.n.height(); h == 0 {
		t.Fatal("no block")
	}
}

// 5. Inputs the node would reject are refused before anything is signed.
func TestRefusedInputsNeverReachTheNode(t *testing.T) {
	f := setup(t)
	_, recipient := newKey(t)
	e := f.engine(t, withdraw.NewMemStore(), utxo.NewSpentSet(""), f.n.rpc)

	// A v5 (USDSOQ authority) address is not a payment destination.
	prog := make([]byte, 32)
	v5, _ := address.Encode(types.Regtest.HRP, 5, prog)
	e.Submit("v5", v5, types.ShorsPerSOQ, types.RecommendedFeeRate)
	if in, err := e.Process("v5"); err == nil || in.State != withdraw.StateFailed {
		t.Fatalf("v5 destination accepted: %v %+v", err, in)
	}
	// Below the node's relay floor.
	e.Submit("dust", recipient, 100_000, types.RecommendedFeeRate)
	if in, err := e.Process("dust"); !errors.Is(err, tx.ErrBelowDust) || in.State != withdraw.StateFailed {
		t.Fatalf("sub-floor amount accepted: %v %+v", err, in)
	}
	// A fee-rate typo.
	e.Submit("typo", recipient, types.ShorsPerSOQ, 9_000_000)
	if in, err := e.Process("typo"); !errors.Is(err, tx.ErrFeeTooHigh) || in.State != withdraw.StateFailed {
		t.Fatalf("fee-rate typo accepted: %v %+v", err, in)
	}
	if n, _ := f.n.rpc.Call("getmempoolinfo"); n == nil {
		t.Fatal("node unreachable")
	}
	raw, _ := f.n.rpc.Call("getrawmempool")
	if string(raw) != "[]" {
		t.Fatalf("something reached the node's mempool: %s", raw)
	}
}

// 6. A reorganisation that removes a credited deposit is alarmed. The deposit
// credited in setup is the coinbase of block 6; a fork from block 5 is far
// inside the horizon (the chain is ~250 blocks tall, the horizon is 288), so
// the node accepts it, and a coinbase cannot return to the mempool, so the
// deposit is simply gone.
func TestReorgRemovingACreditedDepositIsAlarmed(t *testing.T) {
	f := setup(t)
	led := newLedger()
	m := f.monitor(t, led, 30)
	got, err := m.Scan()
	if err != nil || len(got) != 1 {
		t.Fatalf("initial credit: %v %+v", err, got)
	}
	depositBlock, err := f.n.rpc.GetBlockHash(got[0].Height)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.rpc.Call("invalidateblock", depositBlock); err != nil {
		t.Fatalf("invalidateblock: %v", err)
	}
	f.n.mine(f.hot, int(types.CoinbaseMaturity)+10) // a longer chain without the deposit
	if err := f.scan.RefreshAll(); err != nil {
		t.Fatal(err)
	}
	if left := f.scan.GetUTXOs(f.dep); len(left) != 0 {
		t.Fatalf("scanner still shows the reorganised deposit: %+v", left)
	}
	f.alerts = nil
	if _, err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range f.alerts {
		if k == deposit.AlertDepositVanished {
			found = true
		}
	}
	if !found {
		t.Fatalf("credited deposit reorganised away without an alarm; alerts %v", f.alertMsgs)
	}
	// And the book is not credited a second time for anything.
	if again, _ := m.Scan(); len(again) != 0 {
		t.Fatalf("credited after the reorg: %+v", again)
	}
}
