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

// fakeIndexer is an in-process ElectrumX speaking the subset the client uses:
// server.version, server.features, server.ping, blockchain.headers.subscribe,
// blockchain.scripthash.subscribe and blockchain.scripthash.listunspent. Its
// state is the harness scanner's UTXO set. After the node mines, a scenario
// calls advance: the scanner rescans, every connection gets a header, and
// every subscribed scripthash whose status changed gets a notification. Knobs
// drop notifications, fail listunspent for one scripthash, and close the
// clients' connections from the server side.
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
}

// indexerConn is one client connection: its write lock and what it subscribed.
type indexerConn struct {
	conn    net.Conn
	wmu     sync.Mutex
	headers bool
	subs    map[string]string // scripthash -> last status told
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
		ln.Close()
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
		c.Close()
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

func (f *fakeIndexer) listunspent(sh string) string {
	type entry struct {
		TxHash string `json:"tx_hash"`
		TxPos  uint32 `json:"tx_pos"`
		Value  int64  `json:"value"`
		Height int64  `json:"height"`
	}
	out := []entry{}
	if a, ok := f.byHash[sh]; ok {
		for _, u := range f.scan.GetUTXOs(a) {
			out = append(out, entry{u.TxID, u.Vout, u.Value, u.Height})
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (f *fakeIndexer) tip() int64 {
	f.scan.mu.Lock()
	defer f.scan.mu.Unlock()
	return f.scan.scanned
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
		if !paused {
			f.conns[conn] = &indexerConn{conn: conn, subs: map[string]string{}}
		}
		ic := f.conns[conn]
		f.mu.Unlock()
		if paused {
			conn.Close()
			continue
		}
		go f.handle(ic)
	}
}

func (f *fakeIndexer) handle(ic *indexerConn) {
	defer func() {
		ic.conn.Close()
		f.mu.Lock()
		delete(f.conns, ic.conn)
		f.mu.Unlock()
	}()
	r := bufio.NewReader(ic.conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     int64         `json:"id"`
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		sh := ""
		if len(req.Params) > 0 {
			sh, _ = req.Params[0].(string)
		}
		f.mu.Lock()
		f.counts[req.Method+" "+sh]++
		fail := f.failList[sh]
		f.mu.Unlock()

		var result string
		switch req.Method {
		case "server.version":
			result = `"ElectrumX 1.16"`
		case "server.features":
			result = fmt.Sprintf(`{"genesis_hash":%q}`, types.Regtest.GenesisHash)
		case "server.ping":
			result = `null`
		case "blockchain.headers.subscribe":
			f.mu.Lock()
			ic.headers = true
			f.mu.Unlock()
			result = fmt.Sprintf(`{"height":%d,"hex":"00"}`, f.tip())
		case "blockchain.scripthash.subscribe":
			st := f.status(sh)
			f.mu.Lock()
			ic.subs[sh] = st
			f.mu.Unlock()
			if st == "" {
				result = `null`
			} else {
				result = fmt.Sprintf("%q", st)
			}
		case "blockchain.scripthash.listunspent":
			if fail {
				ic.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":1,"message":"indexer failure"}}`, req.ID))
				continue
			}
			result = f.listunspent(sh)
		default:
			ic.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"unknown method"}}`, req.ID))
			continue
		}
		if err := ic.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)); err != nil {
			return
		}
	}
}

// advance is a block being processed: the scanner rescans, every connection
// subscribed to headers gets one, and every subscribed scripthash whose
// status changed gets a notification (unless dropping is on).
func (f *fakeIndexer) advance() {
	f.t.Helper()
	if err := f.scan.RefreshAll(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	tip := f.tip()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ic := range f.conns {
		if ic.headers {
			ic.write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.headers.subscribe","params":[{"height":%d,"hex":"00"}]}`, tip))
		}
		for sh, last := range ic.subs {
			st := f.status(sh)
			if st == last {
				continue
			}
			ic.subs[sh] = st
			if f.dropNotifications {
				continue
			}
			stJSON := "null"
			if st != "" {
				stJSON = fmt.Sprintf("%q", st)
			}
			ic.write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.scripthash.subscribe","params":[%q,%s]}`, sh, stJSON))
		}
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
		waitUntil(t, "initial refresh of "+a, func() bool {
			at, err := elx.LastRefreshOf(a)
			return !at.IsZero() || err != nil
		})
	}
	return idx, elx
}

// startClientOn connects an electrumx.Client to the fake indexer, tracking
// addrs, and starts its refresher.
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

// waitUntil polls cond for up to five seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
