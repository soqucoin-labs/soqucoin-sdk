//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// fakeIndexer is an in-process ElectrumX for the push scenarios. Its state is
// the harness scanner's UTXO set: after the node mines, a scenario calls
// advance, the scanner rescans, every connection gets a header, and every
// subscribed scripthash whose status changed gets a notification. Knobs drop
// notifications, fail listunspent for one scripthash, and refuse service
// altogether.
//
// It answers every method electrumx.Client sends, and that is a checked
// property rather than a claim: the set lives in indexerMethods below and
// fake_electrumx_coverage_test.go reads the client's own source for its call
// sites and fails when one has no handler here. A double that answers six of
// the client's eight methods is worse than no double, because a scenario that
// reaches a seventh does not hang — it quietly skips the path and reports
// success, and this double stands in front of the deposit path that decides
// whether an exchange credits a customer.
type fakeIndexer struct {
	t      *testing.T
	ln     net.Listener
	scan   *scanner
	byHash map[string]string // scripthash -> address

	mu                sync.Mutex
	conns             map[net.Conn]*indexerConn
	pauseAccepts      bool // accepted connections are closed at once: the indexer is down
	dropNotifications bool
	failList          map[string]bool // scripthash -> listunspent answers an error
	counts            map[string]int  // method+" "+scripthash
	notified          int             // scripthash notifications written
	suppressed        int             // scripthash notifications a drop withheld
}

// indexerConn is one client connection: its write lock and what it subscribed.
type indexerConn struct {
	conn    net.Conn
	wmu     sync.Mutex
	headers bool
	subs    map[string]string // scripthash -> last status told
}

// indexerRequest is one JSON-RPC request off the wire.
type indexerRequest struct {
	ID     int64             `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// rpcError is an error the server chooses to send, as opposed to a connection
// it drops. The client's refresh policy tells the two apart, so the double has
// to be able to produce either.
type rpcError struct {
	code    int
	message string
}

// indexerHandler answers one request with a raw JSON result, or with an error
// the server sends.
type indexerHandler func(f *fakeIndexer, ic *indexerConn, req indexerRequest) (string, *rpcError)

// indexerMethods is what this double answers, keyed by method name. The
// coverage test compares these keys against the methods electrumx.Client
// sends, so adding a call site there fails the harness until a handler lands
// here.
var indexerMethods = map[string]indexerHandler{
	"server.version":                    (*fakeIndexer).handleVersion,
	"server.features":                   (*fakeIndexer).handleFeatures,
	"server.ping":                       (*fakeIndexer).handlePing,
	"blockchain.headers.subscribe":      (*fakeIndexer).handleHeadersSubscribe,
	"blockchain.scripthash.subscribe":   (*fakeIndexer).handleScripthashSubscribe,
	"blockchain.scripthash.listunspent": (*fakeIndexer).handleListUnspent,
	"blockchain.scripthash.get_history": (*fakeIndexer).handleGetHistory,
	"blockchain.transaction.broadcast":  (*fakeIndexer).handleBroadcast,
}

// scripthashMethods are the methods whose first parameter is a scripthash.
// Only these are counted and failed per address; server.version's first
// parameter is the client's version string, and keying the counters on it
// would have made setFail(addr) collide with a client identifier.
var scripthashMethods = map[string]bool{
	"blockchain.scripthash.subscribe":   true,
	"blockchain.scripthash.listunspent": true,
	"blockchain.scripthash.get_history": true,
}

func newFakeIndexer(t *testing.T, scan *scanner, addrs ...string) *fakeIndexer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIndexer{t: t, ln: ln, scan: scan, byHash: map[string]string{},
		conns: map[net.Conn]*indexerConn{}, failList: map[string]bool{}, counts: map[string]int{}}
	for _, a := range addrs {
		sh, err := address.AddressToScriptHash(types.Regtest.HRP, a)
		if err != nil {
			t.Fatal(err)
		}
		f.byHash[sh] = a
	}
	go f.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		f.closeClients()
	})
	return f
}

func (f *fakeIndexer) addr() string { return f.ln.Addr().String() }

func (f *fakeIndexer) scripthash(a string) string {
	for sh, addr := range f.byHash {
		if addr == a {
			return sh
		}
	}
	f.t.Fatalf("no scripthash for %s", a)
	return ""
}

func (f *fakeIndexer) count(method, sh string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[method+" "+sh]
}

// notifications reports how many scripthash notifications the server wrote and
// how many a drop withheld. A scenario that claims the client learned of a
// deposit by a push, or in spite of one never arriving, asserts on these
// rather than on a quiet interval, which only ever meant "the reconcile has
// not fired yet on this machine".
func (f *fakeIndexer) notifications() (sent, suppressed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notified, f.suppressed
}

// setPaused makes the indexer refuse service: connections are accepted and
// closed at once, so the client's reconnects fail until it is unpaused.
func (f *fakeIndexer) setPaused(paused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseAccepts = paused
}

func (f *fakeIndexer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func (f *fakeIndexer) setDrop(drop bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropNotifications = drop
}

func (f *fakeIndexer) setFail(a string, fail bool) {
	sh := f.scripthash(a)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failList[sh] = fail
}

// closeClients drops every client connection from the server side.
func (f *fakeIndexer) closeClients() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		_ = c.Close()
		delete(f.conns, c)
	}
}

// status is the server's view of a scripthash's history: a hash over the
// sorted outpoints and heights the scanner holds for its address, empty when
// there are none, as ElectrumX returns null for an address with no history.
func (f *fakeIndexer) status(sh string) string {
	a, ok := f.byHash[sh]
	if !ok {
		return ""
	}
	utxos := f.scan.GetUTXOs(a)
	if len(utxos) == 0 {
		return ""
	}
	parts := make([]string, 0, len(utxos))
	for _, u := range utxos {
		parts = append(parts, fmt.Sprintf("%s:%d:%d", u.TxID, u.Vout, u.Height))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])
}

func (f *fakeIndexer) tip() int64 {
	f.scan.mu.Lock()
	defer f.scan.mu.Unlock()
	return f.scan.scanned
}

// utxosFor is the scanner's set for the address behind a scripthash, empty for
// one the indexer does not track.
func (f *fakeIndexer) utxosFor(sh string) []types.UTXO {
	if a, ok := f.byHash[sh]; ok {
		return f.scan.GetUTXOs(a)
	}
	return nil
}

func (f *fakeIndexer) handleVersion(_ *indexerConn, _ indexerRequest) (string, *rpcError) {
	return `"ElectrumX 1.16"`, nil
}

func (f *fakeIndexer) handleFeatures(_ *indexerConn, _ indexerRequest) (string, *rpcError) {
	return fmt.Sprintf(`{"genesis_hash":%q}`, types.Regtest.GenesisHash), nil
}

func (f *fakeIndexer) handlePing(_ *indexerConn, _ indexerRequest) (string, *rpcError) {
	return `null`, nil
}

func (f *fakeIndexer) handleHeadersSubscribe(ic *indexerConn, _ indexerRequest) (string, *rpcError) {
	f.mu.Lock()
	ic.headers = true
	f.mu.Unlock()
	return fmt.Sprintf(`{"height":%d,"hex":"00"}`, f.tip()), nil
}

func (f *fakeIndexer) handleScripthashSubscribe(ic *indexerConn, req indexerRequest) (string, *rpcError) {
	sh := firstString(req)
	st := f.status(sh)
	f.mu.Lock()
	ic.subs[sh] = st
	f.mu.Unlock()
	if st == "" {
		return `null`, nil // no history, as ElectrumX reports it
	}
	return fmt.Sprintf("%q", st), nil
}

// handleListUnspent answers with the scanner's set, marshalled through
// types.UTXO so the wire shape is the same struct the client parses back. The
// failList knob answers with an error the server chose to send, which the
// client's policy must read as one refused address rather than a lost
// connection.
func (f *fakeIndexer) handleListUnspent(_ *indexerConn, req indexerRequest) (string, *rpcError) {
	sh := firstString(req)
	f.mu.Lock()
	fail := f.failList[sh]
	f.mu.Unlock()
	if fail {
		return "", &rpcError{code: 1, message: "indexer failure"}
	}
	utxos := f.utxosFor(sh)
	if utxos == nil {
		utxos = []types.UTXO{}
	}
	b, err := json.Marshal(utxos)
	if err != nil {
		f.t.Errorf("marshal listunspent for %s: %v", sh, err)
		return "", &rpcError{code: 1, message: "internal"}
	}
	return string(b), nil
}

// handleGetHistory answers with the transactions that created the address's
// unspent outputs, one entry per transaction, ordered by height. This is not
// a full history: the scanner keeps a UTXO set, so a transaction whose outputs
// are all spent is not in it. No scenario reads history for its own sake — the
// handler is here because the client has the call site, and a scenario that
// reaches it must meet an answer rather than "unknown method". Anything that
// comes to depend on history being complete must widen the scanner first.
func (f *fakeIndexer) handleGetHistory(_ *indexerConn, req indexerRequest) (string, *rpcError) {
	sh := firstString(req)
	type entry struct {
		TxHash string `json:"tx_hash"`
		Height int64  `json:"height"`
	}
	seen := map[string]bool{}
	out := []entry{}
	for _, u := range f.utxosFor(sh) {
		if seen[u.TxID] {
			continue
		}
		seen[u.TxID] = true
		out = append(out, entry{u.TxID, u.Height})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height < out[j].Height
		}
		return out[i].TxHash < out[j].TxHash
	})
	b, err := json.Marshal(out)
	if err != nil {
		f.t.Errorf("marshal history for %s: %v", sh, err)
		return "", &rpcError{code: 1, message: "internal"}
	}
	return string(b), nil
}

// handleBroadcast relays to the harness node, which is what an ElectrumX does.
// A double that invented a txid here would let a scenario believe a
// transaction reached a mempool it never entered.
func (f *fakeIndexer) handleBroadcast(_ *indexerConn, req indexerRequest) (string, *rpcError) {
	raw := firstString(req)
	txid, err := f.scan.node.rpc.SendRawTransaction(context.Background(), raw)
	if err != nil {
		return "", &rpcError{code: 1, message: err.Error()}
	}
	return fmt.Sprintf("%q", txid), nil
}

// firstString reads the first parameter as a string, empty when there is none
// or it is not one.
func firstString(req indexerRequest) string {
	if len(req.Params) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(req.Params[0], &s); err != nil {
		return ""
	}
	return s
}

func (ic *indexerConn) write(line string) error {
	ic.wmu.Lock()
	defer ic.wmu.Unlock()
	_, err := ic.conn.Write([]byte(line + "\n"))
	return err
}

func (f *fakeIndexer) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		paused := f.pauseAccepts
		var ic *indexerConn
		if !paused {
			ic = &indexerConn{conn: conn, subs: map[string]string{}}
			f.conns[conn] = ic
		}
		f.mu.Unlock()
		if paused {
			_ = conn.Close()
			continue
		}
		go f.handle(ic)
	}
}

func (f *fakeIndexer) handle(ic *indexerConn) {
	defer func() {
		_ = ic.conn.Close()
		f.mu.Lock()
		delete(f.conns, ic.conn)
		f.mu.Unlock()
	}()
	r := bufio.NewReaderSize(ic.conn, 1024*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req indexerRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}

		key := ""
		if scripthashMethods[req.Method] {
			key = firstString(req)
		}
		f.mu.Lock()
		f.counts[req.Method+" "+key]++
		f.mu.Unlock()

		h, ok := indexerMethods[req.Method]
		if !ok {
			// The coverage test exists so that this line is never reached by
			// a method the client sends; reaching it in a scenario means the
			// double is behind the client again.
			if werr := ic.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"unknown method %s"}}`, req.ID, req.Method)); werr != nil {
				return
			}
			continue
		}
		result, rerr := h(f, ic, req)
		var reply string
		if rerr != nil {
			reply = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":%d,"message":%q}}`, req.ID, rerr.code, rerr.message)
		} else {
			reply = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
		}
		if err := ic.write(reply); err != nil {
			return
		}
	}
}

// advance is a block being processed: the scanner rescans, every connection
// subscribed to headers gets one, and every subscribed scripthash whose status
// changed gets a notification (unless dropping is on). The lines are gathered
// under the lock and written outside it: a client that is slow to read must
// not block the requests of another connection behind a held mutex.
func (f *fakeIndexer) advance() {
	f.t.Helper()
	if err := f.scan.RefreshAll(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	tip := f.tip()

	type outgoing struct {
		ic   *indexerConn
		line string
	}
	var lines []outgoing
	f.mu.Lock()
	for _, ic := range f.conns {
		if ic.headers {
			lines = append(lines, outgoing{ic, fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.headers.subscribe","params":[{"height":%d,"hex":"00"}]}`, tip)})
		}
		for sh, last := range ic.subs {
			st := f.status(sh)
			if st == last {
				continue
			}
			ic.subs[sh] = st
			if f.dropNotifications {
				f.suppressed++
				continue
			}
			stJSON := "null"
			if st != "" {
				stJSON = fmt.Sprintf("%q", st)
			}
			f.notified++
			lines = append(lines, outgoing{ic, fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":[%q,%s]}`, sh, stJSON)})
		}
	}
	f.mu.Unlock()

	for _, o := range lines {
		_ = o.ic.write(o.line)
	}
}

// indexer starts a fake ElectrumX over the fixture's scanner and an
// electrumx.Client tracking addrs, subscribed and running, with the given
// reconcile interval. It waits until every address has been refreshed once.
func (f *fixture) indexer(t *testing.T, reconcile time.Duration, addrs ...string) (*fakeIndexer, *electrumx.Client) {
	t.Helper()
	idx := newFakeIndexer(t, f.scan, addrs...)
	elx, cancel := startClientOn(t, idx, reconcile, addrs...)
	t.Cleanup(cancel)
	for _, a := range addrs {
		a := a
		waitUntil(t, "initial refresh of "+a, func() bool {
			at, err := elx.LastRefreshOf(a)
			if err != nil {
				t.Fatalf("initial refresh of %s failed: %v", a, err)
			}
			return !at.IsZero()
		})
	}
	return idx, elx
}

// startClientOn connects an electrumx.Client to the fake indexer, tracking
// addrs, and starts its refresher. The ping interval is an hour: a ping
// advances the freshness of a clean address, and a scenario that counts calls
// must not have one land in the middle of what it is counting.
func startClientOn(t *testing.T, idx *fakeIndexer, reconcile time.Duration, addrs ...string) (*electrumx.Client, context.CancelFunc) {
	t.Helper()
	elx := electrumx.NewClient(idx.addr(), reconcile, nil)
	elx.HRP = types.Regtest.HRP
	elx.PingInterval = time.Hour
	if err := elx.TrackAddresses(addrs); err != nil {
		t.Fatal(err)
	}
	if err := elx.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(elx.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	elx.Start(ctx)
	return elx, cancel
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitUpTo(t, 5*time.Second, what, cond)
}

// waitUpTo is waitUntil with the bound stated, for a wait whose worst case is
// set by the client's reconnect backoff rather than by a local round trip.
func waitUpTo(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
