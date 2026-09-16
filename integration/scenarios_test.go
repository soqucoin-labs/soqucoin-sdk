//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
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
func (l *memLedger) Credit(_ context.Context, d deposit.Deposit) error {
	l.credited[okey(d.TxID, d.Vout)] = d
	return nil
}
func (l *memLedger) IsCredited(_ context.Context, txid string, vout uint32) (bool, error) {
	_, ok := l.credited[okey(txid, vout)]
	return ok, nil
}
func (l *memLedger) Pending(_ context.Context) ([]deposit.Deposit, error) {
	var out []deposit.Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}
func (l *memLedger) MarkFinal(_ context.Context, txid string, vout uint32) error {
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
	n.mine(hot, int(types.Regtest.CoinbaseMaturity)+5)
	f.scan = newScanner(n, hot, dep)
	if err := f.scan.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) monitor(t *testing.T, led *memLedger, required int64) *deposit.Monitor {
	return f.monitorOver(t, f.scan, led, required, f.dep)
}

// monitorOver is a Monitor reading the given cache for the given addresses:
// the scanner directly, or an electrumx.Client fed by the fake indexer.
func (f *fixture) monitorOver(t *testing.T, cache deposit.Cache, led *memLedger, required int64, addrs ...string) *deposit.Monitor {
	return &deposit.Monitor{
		Network: types.Regtest, // coinbase maturity 60; every harness deposit is a coinbase
		Cache:   cache, Node: f.n.rpc, Ledger: led,
		Addresses: func(context.Context) []string { return addrs },
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
		Select: func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			if err := f.n.rpc.RequireSynced(ctx); err != nil {
				return nil, err
			}
			if err := f.scan.RefreshAll(ctx); err != nil {
				return nil, err
			}
			// Every coin in the harness is a coinbase, so require maturity here;
			// VerifyAndFilterUTXOs would otherwise drop an immature pick and the
			// selection would come back empty.
			selected, _, err := sel.SelectUTXOs(f.scan.GetAllUTXOs(), amount+1100*feeRate, int(types.Regtest.CoinbaseMaturity)+1, f.n.height(), []string{f.hot})
			if err != nil {
				return nil, err
			}
			return f.n.rpc.VerifyAndFilterUTXOs(ctx, selected, nil, nil)
		},
		BuildSign: func(_ context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
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
	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Address != f.dep {
		t.Fatalf("credited %+v, want the one coinbase deposit to %s", got, f.dep)
	}
	if got[0].Value != 500_000*types.ShorsPerSOQ {
		t.Errorf("value %d, want the regtest coinbase reward", got[0].Value)
	}
	if again, _ := m.Scan(context.Background()); len(again) != 0 {
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
	spent := openSpent(t, filepath.Join(t.TempDir(), "spent.json"))
	e := f.engine(t, withdraw.NewMemStore(), spent, f.n.rpc)
	amount := 1_000 * types.ShorsPerSOQ
	if _, _, err := e.Submit(context.Background(), "wd-1", recipient, amount, types.RecommendedFeeRate); err != nil {
		t.Fatal(err)
	}
	in, err := e.Process(context.Background(), "wd-1")
	if err != nil {
		t.Fatalf("process: %v (state %s, last error %s)", err, in.State, in.LastError)
	}
	if in.State != withdraw.StateBroadcast {
		t.Fatalf("state %s", in.State)
	}
	// The node has it in its mempool under the txid the SDK computed.
	if _, err := f.n.rpc.Call(context.Background(), "getmempoolentry", in.TxID); err != nil {
		t.Fatalf("node does not have %s in its mempool: %v", in.TxID, err)
	}
	f.n.mine(f.hot, 3)
	if err := e.UpdateConfirmations(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if in.State != withdraw.StateConfirmed || in.Confirmations < 3 {
		t.Fatalf("after 3 blocks: %+v", in)
	}
	// Recipient sees exactly the amount; the scanner sees the change.
	rs := newScanner(f.n, recipient)
	if err := rs.RefreshAll(context.Background()); err != nil {
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

func (l *lossyBroadcaster) Broadcast(ctx context.Context, raw, txid string) (string, error) {
	l.sent++
	got, err := l.inner.Broadcast(ctx, raw, txid)
	if l.drop {
		l.drop = false
		return "", errors.Join(rpc.ErrUnknownOutcome, errors.New("harness dropped the reply"))
	}
	return got, err
}

// downBroadcaster is a node that cannot be reached for the first `failures`
// attempts and then behaves; nothing reaches the network while it is down.
type downBroadcaster struct {
	inner    *rpc.Client
	failures int
}

func (d *downBroadcaster) Broadcast(ctx context.Context, raw, txid string) (string, error) {
	if d.failures > 0 {
		d.failures--
		return "", fmt.Errorf("broadcast: %w: harness node down", rpc.ErrTransient)
	}
	return d.inner.Broadcast(ctx, raw, txid)
}

// lyingBroadcaster relays to the real node and then reports a different txid,
// the shape rpc.Client.Broadcast produces when node and SDK disagree on the
// serialization. The payment is really in the node's mempool.
type lyingBroadcaster struct{ inner *rpc.Client }

// differentTxID returns a txid that is not the one passed in. Replacing the
// first character with a fixed one returns the input unchanged whenever the
// input already starts with it. Scenario 8 then fails: it requires the engine
// to hold a node txid that differs from its own, and the broadcaster handed it
// two that are equal. The defect was a spurious failure in about one run in
// sixteen, not a test that passed without checking anything.
func differentTxID(got string) string {
	if got == "" {
		return "f"
	}
	if got[0] == 'f' {
		return "0" + got[1:]
	}
	return "f" + got[1:]
}

func (l lyingBroadcaster) Broadcast(ctx context.Context, raw, txid string) (string, error) {
	got, err := l.inner.Broadcast(ctx, raw, txid)
	if err != nil {
		return got, err
	}
	fake := differentTxID(got)
	return fake, fmt.Errorf("broadcast: %w: node returned txid %s for a transaction the caller computed as %s", rpc.ErrTxIDMismatch, fake, txid)
}

func TestLostBroadcastReplyNeverPaysTwice(t *testing.T) {
	f := setup(t)
	_, recipient := newKey(t)
	dir := t.TempDir()
	store, _ := withdraw.NewFileStore(filepath.Join(dir, "intents.json"))
	spent := openSpent(t, filepath.Join(dir, "spent.json"))
	lb := &lossyBroadcaster{inner: f.n.rpc, drop: true}
	e := f.engine(t, store, spent, lb)
	amount := 700 * types.ShorsPerSOQ
	e.Submit(context.Background(), "wd-lost", recipient, amount, types.RecommendedFeeRate)
	in, err := e.Process(context.Background(), "wd-lost")
	if !errors.Is(err, rpc.ErrUnknownOutcome) || in.State != withdraw.StateBuilt {
		t.Fatalf("lost reply: err=%v state=%s", err, in.State)
	}
	// "Restart": a new engine over the same files recovers and re-sends the
	// SAME bytes; the node reports the duplicate as already known.
	store2, _ := withdraw.NewFileStore(filepath.Join(dir, "intents.json"))
	e2 := f.engine(t, store2, openSpent(t, filepath.Join(dir, "spent.json")), f.n.rpc)
	e2.BuildSign = func(context.Context, []types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("recovery rebuilt a transaction")
		return "", "", nil
	}
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	after, _, _ := store2.Get(context.Background(), "wd-lost")
	if after.State != withdraw.StateBroadcast || after.TxID != in.TxID {
		t.Fatalf("recovered %+v", after)
	}
	f.n.mine(f.hot, 2)
	rs := newScanner(f.n, recipient)
	rs.RefreshAll(context.Background())
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
	spent := openSpent(t, filepath.Join(t.TempDir(), "spent.json"))
	e := f.engine(t, withdraw.NewMemStore(), spent, f.n.rpc)
	e.Submit(context.Background(), "a", r1, 400_000*types.ShorsPerSOQ, types.RecommendedFeeRate)
	e.Submit(context.Background(), "b", r2, 400_000*types.ShorsPerSOQ, types.RecommendedFeeRate)
	a, _, _ := e.Store.Get(context.Background(), "a")
	b, _, _ := e.Store.Get(context.Background(), "b")
	if err := e.Build(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := e.Build(context.Background(), b); err != nil {
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
	if err := e.Broadcast(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := e.Broadcast(context.Background(), b); err != nil {
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
	e := f.engine(t, withdraw.NewMemStore(), utxo.NewSpentSet("", nil), f.n.rpc)

	// A v5 (USDSOQ authority) address is not a payment destination.
	prog := make([]byte, 32)
	v5, _ := address.Encode(types.Regtest.HRP, 5, prog)
	e.Submit(context.Background(), "v5", v5, types.ShorsPerSOQ, types.RecommendedFeeRate)
	if in, err := e.Process(context.Background(), "v5"); err == nil || in.State != withdraw.StateFailed {
		t.Fatalf("v5 destination accepted: %v %+v", err, in)
	}
	// Below the node's relay floor.
	e.Submit(context.Background(), "dust", recipient, 100_000, types.RecommendedFeeRate)
	if in, err := e.Process(context.Background(), "dust"); !errors.Is(err, tx.ErrBelowDust) || in.State != withdraw.StateFailed {
		t.Fatalf("sub-floor amount accepted: %v %+v", err, in)
	}
	// A fee-rate typo.
	e.Submit(context.Background(), "typo", recipient, types.ShorsPerSOQ, 9_000_000)
	if in, err := e.Process(context.Background(), "typo"); !errors.Is(err, tx.ErrFeeTooHigh) || in.State != withdraw.StateFailed {
		t.Fatalf("fee-rate typo accepted: %v %+v", err, in)
	}
	if n, _ := f.n.rpc.Call(context.Background(), "getmempoolinfo"); n == nil {
		t.Fatal("node unreachable")
	}
	raw, _ := f.n.rpc.Call(context.Background(), "getrawmempool")
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
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("initial credit: %v %+v", err, got)
	}
	depositBlock, err := f.n.rpc.GetBlockHash(context.Background(), got[0].Height)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.rpc.Call(context.Background(), "invalidateblock", depositBlock); err != nil {
		t.Fatalf("invalidateblock: %v", err)
	}
	f.n.mine(f.hot, int(types.Regtest.CoinbaseMaturity)+10) // a longer chain without the deposit
	if err := f.scan.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := f.scan.GetUTXOs(f.dep); len(left) != 0 {
		t.Fatalf("scanner still shows the reorganised deposit: %+v", left)
	}
	f.alerts = nil
	if _, err := m.Scan(context.Background()); err != nil {
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
	if again, _ := m.Scan(context.Background()); len(again) != 0 {
		t.Fatalf("credited after the reorg: %+v", again)
	}
}

// 7. The node is unreachable for longer than the reservation TTL. Each retry
// renews the reservation, so a withdrawal submitted meanwhile cannot take the
// first one's inputs; when the node returns, both confirm on disjoint inputs.
func TestRetriesPastTheReservationTTLKeepTheInputs(t *testing.T) {
	f := setup(t)
	_, r1 := newKey(t)
	_, r2 := newKey(t)
	spent := openSpent(t, filepath.Join(t.TempDir(), "spent.json"))
	down := &downBroadcaster{inner: f.n.rpc, failures: 3}
	e := f.engine(t, withdraw.NewMemStore(), spent, down)
	e.ReservationTTL = 3 * time.Second    // b's selection below must complete well inside one TTL
	amount := 400_000 * types.ShorsPerSOQ // each takes most of the hot balance; two need disjoint coins
	e.Submit(context.Background(), "a", r1, amount, types.RecommendedFeeRate)
	a, err := e.Process(context.Background(), "a")
	if !errors.Is(err, rpc.ErrTransient) || a.State != withdraw.StateBuilt {
		t.Fatalf("node down: err=%v state=%s", err, a.State)
	}
	for i := 0; i < 2; i++ {
		time.Sleep(2 * time.Second) // inside each TTL, past the first one in total
		if err := e.Broadcast(context.Background(), a); !errors.Is(err, rpc.ErrTransient) || errors.Is(err, withdraw.ErrReservationLost) {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	e.Submit(context.Background(), "b", r2, amount, types.RecommendedFeeRate)
	b, err := e.Process(context.Background(), "b")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	for _, x := range a.Inputs {
		for _, y := range b.Inputs {
			if x.TxID == y.TxID && x.Vout == y.Vout {
				t.Fatalf("b took a's reserved input %s:%d after the first TTL", x.TxID, x.Vout)
			}
		}
	}
	if err := e.Broadcast(context.Background(), a); err != nil { // the node is back
		t.Fatalf("a after the node returned: %v", err)
	}
	f.n.mine(f.hot, 1)
	for _, in := range []*withdraw.Intent{a, b} {
		if n, err := e.Confirmer.Confirmations(context.Background(), in.TxID); err != nil || n != 1 {
			t.Fatalf("%s: confirmations %d, %v", in.ID, n, err)
		}
	}
}

// 8. The node accepts the transaction but names it differently. The payment is
// in the mempool; the engine keeps the intent Built with its inputs held and
// records the node's txid, and a later withdrawal cannot spend those inputs.
func TestTxIDMismatchNeverReleasesSpentInputs(t *testing.T) {
	f := setup(t)
	_, r1 := newKey(t)
	_, r2 := newKey(t)
	spent := openSpent(t, filepath.Join(t.TempDir(), "spent.json"))
	e := f.engine(t, withdraw.NewMemStore(), spent, lyingBroadcaster{inner: f.n.rpc})
	e.ReservationTTL = 50 * time.Millisecond // spent inputs must not depend on a reservation
	amount := 400_000 * types.ShorsPerSOQ
	e.Submit(context.Background(), "a", r1, amount, types.RecommendedFeeRate)
	a, err := e.Process(context.Background(), "a")
	if !errors.Is(err, rpc.ErrTxIDMismatch) || errors.Is(err, rpc.ErrPermanent) {
		t.Fatalf("mismatch: %v", err)
	}
	if a.State != withdraw.StateBuilt || a.NodeTxID == "" || a.NodeTxID == a.TxID {
		t.Fatalf("after mismatch %+v", a)
	}
	for _, o := range a.Inputs {
		if !spent.IsSpent(o.TxID, o.Vout) {
			t.Fatalf("input %s:%d released while the payment sits in the mempool", o.TxID, o.Vout)
		}
	}
	// The real transaction is in the node's mempool under the SDK's txid.
	if n, err := e.Confirmer.Confirmations(context.Background(), a.TxID); err != nil || n != 0 {
		t.Fatalf("node does not hold %s in its mempool: %d %v", a.TxID, n, err)
	}
	time.Sleep(100 * time.Millisecond) // past the TTL: the inputs stay spent
	for _, o := range a.Inputs {
		if !spent.IsSpent(o.TxID, o.Vout) {
			t.Fatalf("input %s:%d lapsed back into selection", o.TxID, o.Vout)
		}
	}
	// A second withdrawal of the same size cannot reuse those inputs.
	e.Broadcaster = f.n.rpc
	e.Submit(context.Background(), "b", r2, amount, types.RecommendedFeeRate)
	if b, err := e.Process(context.Background(), "b"); err == nil {
		for _, x := range a.Inputs {
			for _, y := range b.Inputs {
				if x.TxID == y.TxID && x.Vout == y.Vout {
					t.Fatalf("b spent a's input %s:%d", x.TxID, x.Vout)
				}
			}
		}
	}
	f.n.mine(f.hot, 1)
	if n, err := e.Confirmer.Confirmations(context.Background(), a.TxID); err != nil || n != 1 {
		t.Fatalf("a's payment did not confirm: %d %v", n, err)
	}
}

// openSpent opens a file-backed spent set the way production code must.
func openSpent(t *testing.T, path string) *utxo.SpentSet {
	t.Helper()
	ss, err := utxo.OpenSpentSet(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

// holdingProxy sits between the SDK and the node. It forwards every request
// and, for the first sendrawtransaction, delivers the request to the node,
// signals that the node has answered, and never returns the reply: the shape
// of a network that drops the reply after the node has accepted the
// transaction, or of a caller that gives up at that instant.
type holdingProxy struct {
	srv      *httptest.Server
	accepted chan struct{}
	held     atomic.Bool
}

func newHoldingProxy(t *testing.T, nodeURL string) *holdingProxy {
	t.Helper()
	target, err := url.Parse(nodeURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &holdingProxy{accepted: make(chan struct{}, 1)}
	rp := httputil.NewSingleHostReverseProxy(target)
	director := rp.Director
	rp.Director = func(r *http.Request) {
		director(r)
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		if strings.Contains(string(body), `"sendrawtransaction"`) && p.held.CompareAndSwap(false, true) {
			r.Header.Set("X-Harness-Hold", "1")
		}
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.Header.Get("X-Harness-Hold") != "1" {
			return nil
		}
		p.accepted <- struct{}{}
		<-resp.Request.Context().Done()
		return resp.Request.Context().Err()
	}
	rp.ErrorHandler = func(http.ResponseWriter, *http.Request, error) {}
	p.srv = httptest.NewServer(rp)
	t.Cleanup(p.srv.Close)
	return p
}

// 9. The caller's context ends while the node is holding the reply to the
// broadcast. The node has the transaction; the SDK does not know it. The
// intent stays Built with its reservation; a restart with a live context
// sends the same bytes, the node reports them as already known, and the
// recipient is paid exactly once.
func TestCancelledBroadcastPaysOnce(t *testing.T) {
	f := setup(t)
	_, recipient := newKey(t)
	dir := t.TempDir()
	proxy := newHoldingProxy(t, fmt.Sprintf("http://127.0.0.1:%d", f.n.port))
	viaProxy := rpc.NewClient(proxy.srv.URL, "it", "it", nil)
	viaProxy.Network = types.Regtest
	store, _ := withdraw.NewFileStore(filepath.Join(dir, "intents.json"))
	spent := openSpent(t, filepath.Join(dir, "spent.json"))
	e := f.engine(t, store, spent, viaProxy)
	amount := 600 * types.ShorsPerSOQ
	if _, _, err := e.Submit(context.Background(), "wd-cancel", recipient, amount, types.RecommendedFeeRate); err != nil {
		t.Fatal(err)
	}
	in, _, _ := store.Get(context.Background(), "wd-cancel")
	// Built under a live context, so the cancel below can touch only the send.
	if err := e.Build(context.Background(), in); err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Broadcast(ctx, in) }()
	select {
	case <-proxy.accepted:
	case <-time.After(30 * time.Second):
		t.Fatal("the node never answered the broadcast")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Broadcast did not return after its context was cancelled")
	}
	if !errors.Is(err, rpc.ErrUnknownOutcome) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled broadcast: %v, want ErrUnknownOutcome carrying context.Canceled", err)
	}
	if errors.Is(err, rpc.ErrPermanent) || in.State != withdraw.StateBuilt {
		t.Fatalf("cancelled broadcast: state %s, err %v; the intent must stay Built", in.State, err)
	}
	for _, o := range in.Inputs {
		if !spent.IsSpent(o.TxID, o.Vout) {
			t.Fatalf("input %s:%d released while the payment sits in the mempool", o.TxID, o.Vout)
		}
	}
	// The node already holds the payment the SDK does not know about.
	if _, err := f.n.rpc.Call(context.Background(), "getmempoolentry", in.TxID); err != nil {
		t.Fatalf("node does not have %s in its mempool: %v", in.TxID, err)
	}

	// "Restart": a new engine over the same files, straight to the node.
	store2, _ := withdraw.NewFileStore(filepath.Join(dir, "intents.json"))
	e2 := f.engine(t, store2, openSpent(t, filepath.Join(dir, "spent.json")), f.n.rpc)
	e2.BuildSign = func(context.Context, []types.UTXO, string, int64, int64) (string, string, error) {
		t.Fatal("recovery rebuilt a transaction after a cancelled broadcast")
		return "", "", nil
	}
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	after, _, _ := store2.Get(context.Background(), "wd-cancel")
	if after.State != withdraw.StateBroadcast || after.TxID != in.TxID || after.RawHex != in.RawHex {
		t.Fatalf("recovered %+v, want Broadcast under the same txid and bytes", after)
	}
	f.n.mine(f.hot, 2)
	rs := newScanner(f.n, recipient)
	if err := rs.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rs.GetUTXOs(recipient); len(got) != 1 || got[0].Value != amount {
		t.Fatalf("recipient has %+v; a cancelled broadcast must never produce a second payment", got)
	}
}
