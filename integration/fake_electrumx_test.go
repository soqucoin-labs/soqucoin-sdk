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
// property: the set lives in indexerMethods below, and
// fake_electrumx_coverage_test.go reads the client's own source for its call
// sites and fails when one has no handler here. A method the client sends and
// this double does not answer meets "unknown method", which the client reads
// as an error from the server, so a scenario reaching that path takes the
// error branch and reports success without exercising what it names. A double
// that has drifted from its client is worse than no double, because it is
// believed, and this one stands in front of the deposit path that decides
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
	unknown           []string        // scripthashes asked about that this indexer was not given
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
// sends, read from its source, so a call site there with no handler here
// fails the harness.
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
// The counter key is built from it for these and left empty for the rest, so
// that a count reads per address. Without this the key would be whatever each
// other method's first parameter happens to be: the client's version string
// for server.version, the raw transaction for transaction.broadcast, and
// nothing at all for server.ping, server.features and headers.subscribe.
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
		// On the test goroutine, so a scripthash the fake was never given is
		// a failure and not a panic from a connection goroutine.
		f.mu.Lock()
		unknown := append([]string(nil), f.unknown...)
		f.mu.Unlock()
		if len(unknown) > 0 {
			t.Errorf("the client asked about %d scripthash(es) this indexer was not given (%v); pass every tracked address to newFakeIndexer too, or it answers 'no outputs' for them", len(unknown), unknown)
		}
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

// notifications reports how many scripthash notifications the server wrote,
// counted after the write returned, and how many a drop withheld. A scenario
// that claims the client learned of a deposit by a push, or in spite of one
// never arriving, asserts on these rather than on a quiet interval, which
// only ever meant "the reconcile has not fired yet on this machine".
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

// utxosFor is the scanner's set for the address behind a scripthash.
//
// A scripthash the indexer was never given is recorded and reported by the
// cleanup newFakeIndexer registers, rather than answered with "no outputs".
// newFakeIndexer and startClientOn take their addresses separately, so a
// scenario can track an address it forgot to give the fake; answering that
// with an affirmative empty set is the silent "no deposits" this file exists
// to keep out of the harness.
//
// It is recorded and not reported here because this runs on a connection
// goroutine, and testing.T.Errorf from a goroutine still running after the
// test function has returned panics instead of failing.
func (f *fakeIndexer) utxosFor(sh string) []types.UTXO {
	f.mu.Lock()
	a, ok := f.byHash[sh]
	if !ok {
		f.unknown = append(f.unknown, sh)
	}
	f.mu.Unlock()
	if !ok {
		return nil
	}
	return f.scan.GetUTXOs(a)
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
	// The status is read under the lock that records the subscription. Read
	// outside it, an advance between the read and the store rescans to a new
	// status, finds no subscription to notify, and the store then records the
	// older one as the status last told: the client is told a status the
	// server has already moved past, and learns of the move only on the next
	// advance, which finds the recorded status stale and notifies.
	f.mu.Lock()
	st := f.status(sh)
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
// No scenario reaches it today: nothing under integration/ calls BroadcastTx,
// and the withdrawal scenarios broadcast through the node's own RPC. It is
// here because the client has the call site, and it relays rather than
// inventing a txid so that the scenario which first does reach it cannot
// believe a transaction entered a mempool it never entered.
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
		ic     *indexerConn
		line   string
		notify bool // a scripthash notification, as opposed to a header
	}
	var lines []outgoing
	f.mu.Lock()
	for _, ic := range f.conns {
		if ic.headers {
			lines = append(lines, outgoing{ic, fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.headers.subscribe","params":[{"height":%d,"hex":"00"}]}`, tip), false})
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
			lines = append(lines, outgoing{ic, fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":[%q,%s]}`, sh, stJSON), true})
		}
	}
	f.mu.Unlock()

	// Counted after the write returns, so notifications() reports what the
	// server put on a socket and not what it queued: a connection that closes
	// between the gather and the write would otherwise be recorded as a push
	// the client never received.
	for _, o := range lines {
		err := o.ic.write(o.line)
		if err != nil || !o.notify {
			continue
		}
		f.mu.Lock()
		f.notified++
		f.mu.Unlock()
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
