package electrumx

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The connection layer: one TCP (or TLS) connection, one reader goroutine
// that owns every read on it, and a pending map that pairs each reply with
// the call that made the request, by id. Writes and connection replacement
// are serialised by the one-slot connSem; reads are not, they belong to the
// reader. Calls therefore run concurrently on the one connection: a call
// waiting for a slow reply does not hold the connection for the calls behind
// it, and a server notification is dispatched the moment it arrives, not when
// the next call happens to read it.

// callDeadline bounds one request's write and one reply's wait when the
// context does not end them first.
const callDeadline = 30 * time.Second

// pendingCall is one caller waiting for the reply to one id on one connection
// generation. The channel holds one reply and is closed, never sent on, when
// that connection is lost.
type pendingCall struct {
	ch  chan incoming
	gen uint64
}

// lockConn takes the connection semaphore or returns ctx.Err() when the
// context ends first. The caller must release with unlockConn on success.
func (c *Client) lockConn(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.connSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lockConnBlocking takes the connection semaphore without a context: for Stop
// and for the reader reporting a lost connection, which must run whatever
// else is happening.
func (c *Client) lockConnBlocking() { c.connSem <- struct{}{} }

func (c *Client) unlockConn() { <-c.connSem }

// dial opens the transport. Keepalive is set on the TCP connection underneath
// any TLS layer, so it survives the wrapping.
//
// The TLS handshake carries its own deadline. Without one a server that accepts
// the TCP connection and then stalls would hang Connect indefinitely, which is
// exactly how a reconnect loop wedges. Both the dial and the handshake end
// early when ctx ends.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	// F5: TCP keepalive at 30 s, so an idle connection survives NAT and
	// firewall timeouts between pings.
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", c.host)
	if err != nil {
		return nil, fmt.Errorf("connect to electrumx %s: %w", c.host, err)
	}

	if c.TLSConfig == nil {
		return raw, nil
	}

	cfg := c.TLSConfig.Clone()
	if cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		host, _, splitErr := net.SplitHostPort(c.host)
		if splitErr != nil {
			host = c.host
		}
		cfg.ServerName = host
	}

	tlsConn := tls.Client(raw, cfg)
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("electrumx %s: tls handshake: %w", c.host, err)
	}
	return tlsConn, nil
}

// Connect establishes a connection to ElectrumX with keepalive enabled, over TLS
// if TLSConfig is set, and subscribes to block headers on it.
//
// Production lesson (F5): ElectrumX connections sit idle between events.
// NAT/firewall timeouts silently kill the connection after ~4h on DigitalOcean
// droplets. TCP keepalive at 30s prevents this; the ping in Start keeps the
// server's own idle timer from closing the session.
//
// The connection is replaced under the connection lock, the same lock every
// write holds, and the generation counter moves with it: the reader of the old
// connection notices its generation is gone and fails only the calls that were
// waiting on it. A context that ends while another call holds the lock ends
// Connect too, and the new connection is closed unused. After the version
// handshake the server's genesis hash is checked against the chains the
// client's HRP belongs to (see verifyGenesisLocked): an indexer for the wrong
// chain would otherwise report "no deposits" forever. Address subscriptions
// belong to a connection and are re-established by Start's next pass.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}

	if err := c.lockConn(ctx); err != nil {
		conn.Close()
		return err
	}
	defer c.unlockConn()

	if c.conn != nil {
		c.conn.Close()
	}
	c.gen++
	gen := c.gen
	c.liveGen.Store(gen)
	c.conn = conn
	// PF-018 FIX: Use 4MB buffer instead of default 4KB.
	// ElectrumX responses for addresses with 18,000+ UTXOs can exceed
	// 2MB. The default bufio.NewReader (4KB) panics on buffer growth
	// in Go 1.26's bufio.ReadSlice when response > 4KB.
	// 4MB accommodates ~50,000 UTXOs with margin.
	reader := bufio.NewReaderSize(conn, 4*1024*1024)
	c.reader = reader
	go c.readLoop(gen, conn, reader)
	c.log.Info("connected", "host", c.host, "tls", c.TLSConfig != nil)

	fail := func(err error) error {
		c.dropLocked()
		return err
	}

	// Server version handshake
	resp, err := c.callLocked(ctx, "server.version", []interface{}{"soqucoin-sdk/1.0", "1.4"})
	if err != nil {
		return fail(fmt.Errorf("electrumx handshake: %w", err))
	}
	c.log.Info("server version", "host", c.host, "version", string(resp))

	if err := c.verifyGenesisLocked(ctx); err != nil {
		return fail(err)
	}

	// Headers arrive as notifications from here on; the reply carries the tip.
	if _, err := c.callLocked(ctx, "blockchain.headers.subscribe", []interface{}{}); err != nil {
		return fail(fmt.Errorf("electrumx headers.subscribe: %w", err))
	}
	return nil
}

// verifyGenesisLocked refuses a server that indexes a chain other than the one
// the client's addresses belong to. Skipped only while the HRP is still unknown
// (before TrackAddresses), in which case TrackAddresses is the gate.
func (c *Client) verifyGenesisLocked(ctx context.Context) error {
	if c.HRP == "" {
		return nil
	}
	want := types.GenesisHashesForHRP(c.HRP)
	if len(want) == 0 {
		return fmt.Errorf("%w: no known chain uses HRP %q", ErrNetworkMismatch, c.HRP)
	}
	raw, err := c.callLocked(ctx, "server.features", []interface{}{})
	if err != nil {
		return fmt.Errorf("electrumx server.features: %w", err)
	}
	var features struct {
		Genesis string `json:"genesis_hash"`
	}
	if err := json.Unmarshal(raw, &features); err != nil {
		return fmt.Errorf("electrumx server.features: parse: %w", err)
	}
	got := strings.ToLower(strings.TrimPrefix(features.Genesis, "0x"))
	for _, w := range want {
		if got == strings.ToLower(w) {
			return nil
		}
	}
	return fmt.Errorf("%w: server genesis %s, client HRP %q expects one of %v", ErrGenesisMismatch, features.Genesis, c.HRP, want)
}

// Reconnect closes the current connection and establishes a new one. The
// address subscriptions of the old connection are gone with it; Start's next
// pass re-subscribes every tracked address.
func (c *Client) Reconnect(ctx context.Context) error {
	c.log.Info("reconnecting", "host", c.host)
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("reconnect failed: %w", err)
	}
	c.log.Info("reconnected", "host", c.host)
	return nil
}

// Call sends a JSON-RPC request and returns its reply. This is exported for
// advanced usage; prefer the typed methods.
//
// Production lesson (PF-018b): Multiple goroutines (the refresher, sendmany,
// consolidation) call this concurrently. Writes are serialised; reads belong
// to one reader goroutine; replies are paired with calls by id. Without this,
// concurrent writes corrupt the TCP stream and concurrent reads corrupt
// bufio's internal buffer, and a notification read as a reply puts every later
// reply off by one.
//
// A call returns when its reply arrives, when ctx ends, or after callDeadline.
// A call whose context ends returns ctx.Err() whether it is waiting to write
// or waiting for the reply; the reply, if it arrives later, finds no waiter
// and is dropped. A write cut short closes the connection, since part of a
// line may be on the wire; the next call returns ErrNotConnected until
// Reconnect or Start restores it. A wait cut short leaves the connection as it
// is: nothing about the stream is in doubt.
func (c *Client) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	return c.call(ctx, method, params)
}

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	raw, _, err := c.callGen(ctx, method, params)
	return raw, err
}

// callGen is call reporting the connection generation the request went over,
// for callers that record state per connection (subscriptions, freshness).
func (c *Client) callGen(ctx context.Context, method string, params interface{}) (json.RawMessage, uint64, error) {
	if err := c.lockConn(ctx); err != nil {
		return nil, 0, err
	}
	conn, gen := c.conn, c.gen
	if conn == nil {
		c.unlockConn()
		return nil, 0, ErrNotConnected
	}
	id, ch, err := c.sendLocked(ctx, conn, gen, method, params)
	c.unlockConn()
	if err != nil {
		return nil, gen, err
	}
	raw, err := c.await(ctx, id, ch, method)
	return raw, gen, err
}

// callLocked is call for a caller that already holds the connection lock
// (Connect, during the handshake). The lock is held through the wait.
func (c *Client) callLocked(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	id, ch, err := c.sendLocked(ctx, c.conn, c.gen, method, params)
	if err != nil {
		return nil, err
	}
	return c.await(ctx, id, ch, method)
}

// sendLocked registers the pending id and writes one request line. Caller
// holds the connection lock. The id is registered before the write so the
// reply cannot arrive unclaimed, and under the lock so that lost, which takes
// the lock before failing a generation's waiters, cannot miss it.
//
// The write is bounded by callDeadline and, when the context ends during it,
// cut short by moving the write deadline to now. A socket that refuses that
// deadline is closed instead. Either way a write that did not complete leaves
// part of a line on the wire, and the connection is dropped.
func (c *Client) sendLocked(ctx context.Context, conn net.Conn, gen uint64, method string, params interface{}) (int64, chan incoming, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	id := c.reqID.Add(1)
	data, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}
	// ElectrumX uses newline-delimited JSON
	data = append(data, '\n')

	ch := make(chan incoming, 1)
	c.pendMu.Lock()
	c.pending[id] = pendingCall{ch: ch, gen: gen}
	c.pendMu.Unlock()

	if err := conn.SetWriteDeadline(time.Now().Add(callDeadline)); err != nil {
		// A socket that refuses a deadline is closed or broken underneath.
		c.unregister(id)
		c.dropLocked()
		return 0, nil, fmt.Errorf("set deadline: %w", err)
	}
	fired := make(chan struct{})
	var closedByCtx atomic.Bool
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		if err := conn.SetWriteDeadline(time.Now()); err != nil {
			closedByCtx.Store(true)
			conn.Close()
		}
	})
	_, werr := conn.Write(data)
	// The callback is stopped or, if it has started, waited for: the lock is
	// held here, so it cannot run after the next writer has set its own
	// deadline.
	if !stop() {
		<-fired
	}
	if werr != nil {
		c.unregister(id)
		c.dropLocked()
		if cerr := ctx.Err(); cerr != nil {
			werr = cerr
		}
		return 0, nil, fmt.Errorf("write request: %w", werr)
	}
	if closedByCtx.Load() {
		// The line went out whole and the socket was then closed by the
		// callback; the reader will fail this call's wait.
		c.dropLocked()
	}
	return id, ch, nil
}

// await waits for the reply to id, the context, or the call deadline.
func (c *Client) await(ctx context.Context, id int64, ch chan incoming, method string) (json.RawMessage, error) {
	timer := time.NewTimer(callDeadline)
	defer timer.Stop()
	select {
	case in, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%w: the connection was lost before the reply to %s", ErrNotConnected, method)
		}
		if len(in.Error) > 0 && string(in.Error) != "null" {
			return nil, fmt.Errorf("electrumx error: %s", string(in.Error))
		}
		if method == "blockchain.headers.subscribe" {
			c.recordTip(in.Result)
		}
		return in.Result, nil
	case <-ctx.Done():
		c.unregister(id)
		return nil, ctx.Err()
	case <-timer.C:
		c.unregister(id)
		return nil, fmt.Errorf("electrumx: no reply to %s (request %d) within %v", method, id, callDeadline)
	}
}

// unregister forgets a waiter. A reply arriving later finds no waiter and is
// dropped by the reader.
func (c *Client) unregister(id int64) {
	c.pendMu.Lock()
	delete(c.pending, id)
	c.pendMu.Unlock()
}

// readLoop owns every read on one connection. Lines carrying a method are
// server notifications and are dispatched; lines carrying an id are replies
// and are handed to the waiting call, or dropped when no call waits (the call
// was cancelled or timed out). A line that does not parse means the stream is
// no longer known to be at a line boundary: the connection is closed, which
// ends this loop through the read error on the next iteration.
func (c *Client) readLoop(gen uint64, conn net.Conn, r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			c.lost(gen, err)
			return
		}
		var in incoming
		if err := json.Unmarshal(line, &in); err != nil {
			c.log.Warn("unparseable line from the server; dropping the connection", "err", err)
			conn.Close()
			continue
		}
		if in.Method != "" {
			c.handleNotification(gen, in)
			continue
		}
		if in.ID == nil {
			c.log.Warn("discarding a line with neither id nor method")
			continue
		}
		c.pendMu.Lock()
		p, ok := c.pending[*in.ID]
		if ok {
			delete(c.pending, *in.ID)
		}
		c.pendMu.Unlock()
		if !ok {
			c.log.Debug("discarding a reply with no waiter", "id", *in.ID)
			continue
		}
		p.ch <- in
	}
}

// lost is the reader's report that its connection can no longer be read. Every
// call waiting on that generation is failed, the connection is dropped if it
// is still the current one, and Start is woken to reconnect. A read error on
// a connection Connect or Stop has already replaced or closed is not news.
//
// The waiters are failed before the lock is taken as well as after: Connect
// holds the lock through its handshake, and a connection that dies then must
// fail the handshake's wait at once rather than after the call deadline. The
// pass after the lock catches a call registered in between, which wrote to a
// dead socket and would otherwise wait out its deadline.
func (c *Client) lost(gen uint64, err error) {
	c.failPending(gen)
	c.lockConnBlocking()
	if c.gen == gen && c.conn != nil {
		c.log.Warn("connection lost", "host", c.host, "err", err)
		c.dropLocked()
	}
	c.unlockConn()
	c.failPending(gen)
	c.kick()
}

// failPending closes the reply channel of every call waiting on generation gen.
func (c *Client) failPending(gen uint64) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	for id, p := range c.pending {
		if p.gen == gen {
			close(p.ch)
			delete(c.pending, id)
		}
	}
}

// dropLocked closes and forgets the connection. Caller holds the connection
// lock. The next call returns ErrNotConnected until Reconnect, or Start,
// dials again; the reader ends on the closed socket and fails the waiters.
func (c *Client) dropLocked() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = nil
	c.reader = nil
	// No live connection: every subscription is void until Connect, and a
	// notification still in flight from the old reader is ignored.
	c.liveGen.Store(0)
}

// handleNotification consumes a server push on connection gen. A headers
// notification records the tip. A scripthash notification marks the address
// changed for the refresher; it writes nothing to the cache itself, and it
// names an address only through the client's own scripthash map, so a server
// cannot name an address the client did not compute. Anything else is ignored.
func (c *Client) handleNotification(gen uint64, in incoming) {
	switch in.Method {
	case "blockchain.headers.subscribe":
		var params []json.RawMessage
		if err := json.Unmarshal(in.Params, &params); err != nil || len(params) == 0 {
			return
		}
		c.recordTip(params[0])
	case "blockchain.scripthash.subscribe":
		var params []json.RawMessage
		if err := json.Unmarshal(in.Params, &params); err != nil || len(params) != 2 {
			c.log.Debug("ignoring a malformed scripthash notification")
			return
		}
		var sh string
		if err := json.Unmarshal(params[0], &sh); err != nil {
			c.log.Debug("ignoring a malformed scripthash notification")
			return
		}
		c.noteChange(gen, sh, parseStatus(params[1]))
	default:
		c.log.Debug("ignoring a notification", "method", in.Method)
	}
}

// parseStatus reads a scripthash status: a hex string, or null for an address
// with no history, which is read as the empty string.
func parseStatus(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func (c *Client) recordTip(header json.RawMessage) {
	var h struct {
		Height int64 `json:"height"`
	}
	if err := json.Unmarshal(header, &h); err == nil && h.Height > 0 {
		c.lastTip.Store(h.Height)
	}
}

// LastTip returns the most recent chain height this connection has seen, from
// a GetTip reply or a server notification. Zero until the first connection.
func (c *Client) LastTip() int64 { return c.lastTip.Load() }

// Stop halts the refresher and closes the connection. Safe to call more than
// once; calls after Stop return ErrNotConnected.
func (c *Client) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.lockConnBlocking()
	defer c.unlockConn()
	c.dropLocked()
}

// stopped reports whether Stop has been called.
func (c *Client) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// errIsConnection reports an error that means the connection is gone, so the
// refresher reconnects at once rather than after repeated failures.
func errIsConnection(err error) bool {
	return errors.Is(err, ErrNotConnected)
}
