package electrumx

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// scriptedStub is a line-oriented fake ElectrumX. The handler receives each
// request and returns the lines to write back, in order, so a test can inject
// a notification or a stale reply in front of the real response, exactly as a
// real server does at a new block.
type scriptedStub struct {
	ln      net.Listener
	handler func(req request) []string
	mu      sync.Mutex
	conns   []net.Conn
	genesis string
}

func newScriptedStub(t *testing.T, genesis string, handler func(req request) []string) *scriptedStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &scriptedStub{ln: ln, handler: handler, genesis: genesis}
	go s.serve()
	t.Cleanup(func() {
		ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
	return s
}

func (s *scriptedStub) addr() string { return s.ln.Addr().String() }

func (s *scriptedStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReader(c)
			for {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var req request
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				var out []string
				switch req.Method {
				case "server.version":
					out = []string{reply(req.ID, `"ElectrumX 1.16"`)}
				case "server.features":
					out = []string{reply(req.ID, fmt.Sprintf(`{"genesis_hash":%q}`, s.genesis))}
				default:
					out = s.handler(req)
				}
				for _, l := range out {
					if _, err := c.Write([]byte(l + "\n")); err != nil {
						return
					}
				}
			}
		}(conn)
	}
}

func reply(id int64, result string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result)
}

func headersNotification(height int64) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"blockchain.headers.subscribe","params":[{"height":%d,"hex":"00"}]}`, height)
}

// craftAddr returns a real stagenet v1 address for a distinct program.
func craftAddr(t *testing.T, fill byte) string {
	t.Helper()
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = fill
	}
	a, err := address.Encode(types.Stagenet.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func scripthashOf(t *testing.T, a string) string {
	t.Helper()
	sh, err := address.AddressToScriptHash(types.Stagenet.HRP, a)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func firstParam(req request) string {
	ps, _ := req.Params.([]interface{})
	if len(ps) == 0 {
		return ""
	}
	s, _ := ps[0].(string)
	return s
}

func connect(t *testing.T, s *scriptedStub) *Client {
	t.Helper()
	c := NewClient(s.addr(), time.Hour)
	c.HRP = types.Stagenet.HRP
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Stop)
	return c
}

// The reply is identified by id, not by position. A headers notification
// pushed in front of the reply must be consumed, not returned as the reply.
func TestCallMatchesIDsAndRoutesNotifications(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		switch req.Method {
		case "blockchain.headers.subscribe":
			return []string{reply(req.ID, `{"height":100,"hex":"00"}`)}
		case "echo":
			// A new block arrives while the reply is in flight, then a stale
			// reply to a request that timed out long ago, then the real reply.
			return []string{
				headersNotification(101),
				reply(req.ID-1000, `"stale"`),
				reply(req.ID, `"real"`),
			}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)

	tip, err := c.GetTip()
	if err != nil || tip != 100 {
		t.Fatalf("GetTip: %d %v", tip, err)
	}
	res, err := c.Call("echo", []interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `"real"` {
		t.Fatalf("reply was %s: a notification or a stale line was returned as the reply", res)
	}
	if got := c.LastTip(); got != 101 {
		t.Errorf("LastTip = %d, want 101 from the routed notification", got)
	}
	// The connection is still in sync: the next call gets its own reply.
	res, err = c.Call("echo", []interface{}{})
	if err != nil || string(res) != `"real"` {
		t.Fatalf("second call: %s %v", res, err)
	}
}

// The P0 reproduction: two tracked addresses, a notification arrives before
// address A's listunspent reply. The old reader returned the notification as
// A's reply and then A's real reply as B's, storing A's UTXOs under B.
func TestRefreshDoesNotShiftAddressesAcrossANotification(t *testing.T) {
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	sh1, sh2 := scripthashOf(t, a1), scripthashOf(t, a2)
	utxo1 := `[{"tx_hash":"` + txA + `","tx_pos":0,"value":100,"height":10}]`
	utxo2 := `[{"tx_hash":"` + txB + `","tx_pos":1,"value":200,"height":20}]`

	var calls int
	var mu sync.Mutex
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method != "blockchain.scripthash.listunspent" {
			return []string{reply(req.ID, `null`)}
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		var out []string
		if n == 1 {
			out = append(out, headersNotification(500))
		}
		switch firstParam(req) {
		case sh1:
			out = append(out, reply(req.ID, utxo1))
		case sh2:
			out = append(out, reply(req.ID, utxo2))
		default:
			out = append(out, reply(req.ID, `[]`))
		}
		return out
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshAll(); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	got1, got2 := c.GetUTXOs(a1), c.GetUTXOs(a2)
	if len(got1) != 1 || got1[0].TxID != txA || got1[0].Value != 100 {
		t.Fatalf("address 1 cache = %+v, want its own outpoint %s", got1, txA)
	}
	if len(got2) != 1 || got2[0].TxID != txB || got2[0].Value != 200 {
		t.Fatalf("address 2 cache = %+v, want its own outpoint %s", got2, txB)
	}
	if got := c.LastTip(); got != 500 {
		t.Errorf("notification height not recorded: LastTip=%d", got)
	}
}

// The merge must take the server's height and value on every pass: a reorg
// sends a deposit back to the mempool (height 0), a re-mine moves it, and a
// wrongly injected change value must be corrected before it is signed over.
func TestMergeTakesFreshHeightAndValue(t *testing.T) {
	a1 := craftAddr(t, 0x33)
	var height int64 = 100
	var value int64 = 1000
	var mu sync.Mutex
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		mu.Lock()
		h, v := height, value
		mu.Unlock()
		return []string{reply(req.ID, fmt.Sprintf(`[{"tx_hash":"%s","tx_pos":0,"value":%d,"height":%d}]`, txA, v, h))}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}
	set := func(h, v int64) {
		mu.Lock()
		height, value = h, v
		mu.Unlock()
		if err := c.RefreshAll(); err != nil {
			t.Fatal(err)
		}
	}
	set(100, 1000)
	c.MarkSpentPending(txA, 0) // a flag the merge must preserve
	set(0, 1000)
	if u := c.GetUTXOs(a1)[0]; u.Height != 0 {
		t.Errorf("reorg to mempool not reflected: height %d, want 0", u.Height)
	}
	set(150, 1000)
	if u := c.GetUTXOs(a1)[0]; u.Height != 150 {
		t.Errorf("re-mine not reflected: height %d, want 150", u.Height)
	}
	set(150, 999)
	u := c.GetUTXOs(a1)[0]
	if u.Value != 999 {
		t.Errorf("value not corrected: %d, want 999", u.Value)
	}
	if !u.SpentPending {
		t.Error("merge dropped the SpentPending flag")
	}
}

// One failing address must not starve the others, and the failure must be
// visible through LastRefresh rather than only in a log line.
func TestRefreshAllContinuesPastFailingAddress(t *testing.T) {
	a1, a2 := craftAddr(t, 0x44), craftAddr(t, 0x55)
	sh1 := scripthashOf(t, a1)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if firstParam(req) == sh1 {
			return []string{fmt.Sprintf(`{"id":%d,"error":{"code":1,"message":"boom"}}`, req.ID)}
		}
		return []string{reply(req.ID, `[{"tx_hash":"`+txB+`","tx_pos":0,"value":5,"height":1}]`)}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	err := c.RefreshAll()
	if err == nil || !strings.Contains(err.Error(), a1) {
		t.Fatalf("error should name the failing address: %v", err)
	}
	if len(c.GetUTXOs(a2)) != 1 {
		t.Fatal("the address after the failing one was not refreshed")
	}
	at, lastErr := c.LastRefresh()
	if !at.IsZero() || lastErr == nil {
		t.Errorf("LastRefresh = (%v, %v): a partial failure must not read as a clean refresh", at, lastErr)
	}
}

// Without an explicit HRP the network comes from the addresses; with one,
// addresses on another network are refused. Nothing silently refreshes zero.
func TestTrackAddressesInfersAndEnforcesNetwork(t *testing.T) {
	ssq := craftAddr(t, 0x66)
	prog := make([]byte, 32)
	sq, err := address.Encode(types.Mainnet.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient("127.0.0.1:1", time.Hour)
	if err := c.TrackAddresses([]string{ssq}); err != nil {
		t.Fatalf("infer: %v", err)
	}
	if c.HRP != types.Stagenet.HRP {
		t.Errorf("HRP inferred as %q, want ssq", c.HRP)
	}
	if err := c.TrackAddresses([]string{sq}); !errors.Is(err, ErrNetworkMismatch) {
		t.Errorf("mainnet address accepted on a stagenet client: %v", err)
	}
	if err := c.TrackAddresses([]string{ssq, sq}); !errors.Is(err, ErrNetworkMismatch) {
		t.Errorf("mixed networks accepted: %v", err)
	}
	if err := c.TrackAddresses([]string{"ssq1notanaddress"}); err == nil {
		t.Error("undecodable address accepted")
	}
	// A rejected call registers nothing.
	if got := len(c.GetAllUTXOs()); got != 0 {
		t.Errorf("phantom UTXOs: %d", got)
	}
}

// Calls before Connect and after Stop fail with a typed error instead of a
// nil-pointer panic; Stop twice is a no-op.
func TestNotConnectedAndIdempotentStop(t *testing.T) {
	c := NewClient("127.0.0.1:1", time.Hour)
	if _, err := c.Call("x", nil); !errors.Is(err, ErrNotConnected) {
		t.Errorf("call before Connect: %v", err)
	}
	c.Stop()
	c.Stop()
	if _, err := c.GetTip(); !errors.Is(err, ErrNotConnected) {
		t.Errorf("call after Stop: %v", err)
	}
}

// An indexer for another chain is refused at Connect.
func TestConnectRejectsWrongGenesis(t *testing.T) {
	stub := newScriptedStub(t, types.Mainnet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `null`)}
	})
	c := NewClient(stub.addr(), time.Hour)
	c.HRP = types.Stagenet.HRP
	err := c.Connect()
	if !errors.Is(err, ErrGenesisMismatch) {
		t.Fatalf("stagenet client accepted a mainnet indexer: %v", err)
	}
	if _, err := c.Call("x", nil); !errors.Is(err, ErrNotConnected) {
		t.Errorf("a failed Connect must leave no usable connection: %v", err)
	}
	// Mainnet and regtest share an HRP, so a mainnet client accepts either.
	c2 := NewClient(stub.addr(), time.Hour)
	c2.HRP = types.Mainnet.HRP
	if err := c2.Connect(); err != nil {
		t.Fatalf("mainnet client refused a mainnet indexer: %v", err)
	}
	c2.Stop()
}

// Reconnect while other goroutines are mid-call. Meaningful under -race: the
// connection swap and the in-flight reads must be serialised by the same lock.
func TestReconnectConcurrentWithCallsIsSerialised(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `{"height":7,"hex":"00"}`)}
	})
	c := connect(t, stub)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				c.GetTip() // errors during a swap are acceptable; races are not
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 5; j++ {
			if err := c.Reconnect(); err != nil {
				t.Errorf("reconnect: %v", err)
			}
		}
	}()
	wg.Wait()
	if tip, err := c.GetTip(); err != nil || tip != 7 {
		t.Fatalf("after reconnects: %d %v", tip, err)
	}
}
