package electrumx

import (
	"bufio"
	"bytes"
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

// errNoReply is wrapped by a call whose reply did not arrive within
// callDeadline: the connection may be hung, and two in a row make the
// refresher rebuild it. An application error from the server is not this.
var errNoReply = errors.New("electrumx: no reply within the call deadline")

// pendingCall is one caller waiting for the reply to one id on one connection
// generation. ch holds one reply and is closed, never sent on, when that
// connection is lost. done is the generation's own channel, closed by the
// reader the moment it cannot read, before any lock is taken: a waiter that
// holds the connection lock through its wait (Connect's handshake) learns of
// the loss from it at once.
type pendingCall struct {
	ch   chan incoming
	gen  uint64
	done chan struct{}
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
		_ = raw.Close()
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
		_ = conn.Close()
		return err
	}
	defer c.unlockConn()
	if c.stopped() {
		// A Stop that raced the dial stands: the client is not reopened.
		_ = conn.Close()
		return ErrNotConnected
	}

	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.gen++
	gen := c.gen
	c.liveGen.Store(gen)
	c.conn = conn
	c.connDone = make(chan struct{})
	// PF-018 FIX: Use 4MB buffer instead of default 4KB.
	// ElectrumX responses for addresses with 18,000+ UTXOs can exceed
	// 2MB. The default bufio.NewReader (4KB) panics on buffer growth
	// in Go 1.26's bufio.ReadSlice when response > 4KB.
	// 4MB accommodates ~50,000 UTXOs with margin.
	reader := bufio.NewReaderSize(conn, 4*1024*1024)
	c.reader = reader
	go c.readLoop(gen, conn, reader, c.connDone)
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
	hrp := c.hrp()
	if hrp == "" {
		return nil
	}
	want := types.GenesisHashesForHRP(hrp)
	if len(want) == 0 {
		return fmt.Errorf("%w: no known chain uses HRP %q", ErrNetworkMismatch, hrp)
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
	return fmt.Errorf("%w: server genesis %s, client HRP %q expects one of %v", ErrGenesisMismatch, features.Genesis, hrp, want)
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
	id, p, err := c.sendLocked(ctx, conn, gen, method, params)
	c.unlockConn()
	if err != nil {
		return nil, gen, err
	}
	raw, err := c.await(ctx, id, p, method)
	return raw, gen, err
}

// callLocked is call for a caller that already holds the connection lock
// (Connect, during the handshake). The lock is held through the wait.
func (c *Client) callLocked(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	id, p, err := c.sendLocked(ctx, c.conn, c.gen, method, params)
	if err != nil {
		return nil, err
	}
	return c.await(ctx, id, p, method)
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
func (c *Client) sendLocked(ctx context.Context, conn net.Conn, gen uint64, method string, params interface{}) (int64, pendingCall, error) {
	if err := ctx.Err(); err != nil {
		return 0, pendingCall{}, err
	}
	id := c.reqID.Add(1)
	data, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return 0, pendingCall{}, fmt.Errorf("marshal request: %w", err)
	}
	// ElectrumX uses newline-delimited JSON
	data = append(data, '\n')

	p := pendingCall{ch: make(chan incoming, 1), gen: gen, done: c.connDone}
	c.pendMu.Lock()
	c.pending[id] = p
	c.pendMu.Unlock()

	if err := conn.SetWriteDeadline(time.Now().Add(callDeadline)); err != nil {
		// A socket that refuses a deadline is closed or broken underneath.
		c.unregister(id)
		c.dropLocked()
		return 0, pendingCall{}, fmt.Errorf("set deadline: %w", err)
	}
	fired := make(chan struct{})
	var closedByCtx atomic.Bool
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		if err := conn.SetWriteDeadline(time.Now()); err != nil {
			closedByCtx.Store(true)
			_ = conn.Close()
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
		return 0, pendingCall{}, fmt.Errorf("write request: %w", writeFailure(ctx, werr))
	}
	if closedByCtx.Load() {
		// The line went out whole and the socket was then closed by the
		// callback; the reader will fail this call's wait.
		c.dropLocked()
	}
	return id, p, nil
}

// writeFailure is what a failed write reports. The socket is gone and
// dropLocked has forgotten it, so the refresher has to read this as a lost
// connection: without ErrNotConnected in the chain it reads a broken pipe as
// an error the server chose to send and waits out a backoff before rebuilding
// a connection that no longer exists. A context that ended during the write is
// what the caller asked for and is reported as itself.
func writeFailure(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return fmt.Errorf("%w: %w", ErrNotConnected, err)
}

// recordTipIfLive records the tip from a headers.subscribe reply that came
// over gen, and only while gen is still the live connection. The notification
// path makes the same test: a reply buffered on a connection that has since
// been replaced still answers its caller, and the connection that replaced it
// has already recorded a newer tip in its own handshake.
func (c *Client) recordTipIfLive(gen uint64, result json.RawMessage) {
	if live := c.liveGen.Load(); gen != live {
		return
	}
	c.recordTip(result)
}

// await waits for the reply to id, the loss of its connection, Stop, the
// context, or the call deadline. Stop is watched because Connect waits here
// holding the connection lock Stop needs: a shutdown must not wait for a
// silent server's call deadline.
func (c *Client) await(ctx context.Context, id int64, p pendingCall, method string) (json.RawMessage, error) {
	timer := time.NewTimer(callDeadline)
	defer timer.Stop()
	deliver := func(in incoming) (json.RawMessage, error) {
		if len(in.Error) > 0 && string(in.Error) != "null" {
			return nil, fmt.Errorf("electrumx error: %s", string(in.Error))
		}
		if method == "blockchain.headers.subscribe" {
			c.recordTipIfLive(p.gen, in.Result)
		}
		return in.Result, nil
	}
	lostErr := func() error {
		if err := ctx.Err(); err != nil {
			return err // the context ended too; it is what the caller asked for
		}
		return fmt.Errorf("%w: the connection was lost before the reply to %s", ErrNotConnected, method)
	}
	select {
	case in, ok := <-p.ch:
		if !ok {
			return nil, lostErr()
		}
		return deliver(in)
	case <-p.done:
		c.unregister(id)
		// A reply that arrived just before the loss is still a reply.
		select {
		case in, ok := <-p.ch:
			if ok {
				return deliver(in)
			}
		default:
		}
		return nil, lostErr()
	case <-c.stopCh:
		c.unregister(id)
		return nil, ErrNotConnected
	case <-ctx.Done():
		c.unregister(id)
		return nil, ctx.Err()
	case <-timer.C:
		c.unregister(id)
		return nil, fmt.Errorf("%w: %s (request %d)", errNoReply, method, id)
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
// no longer known to be at a line boundary: the connection is reported lost
// at once, every waiter fails, and nothing buffered behind it is delivered.
func (c *Client) readLoop(gen uint64, conn net.Conn, r *bufio.Reader, done chan struct{}) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			c.lost(gen, done, err)
			return
		}
		var in incoming
		if err := json.Unmarshal(line, &in); err != nil {
			// Lines already buffered behind this one are not delivered: the
			// connection is reported lost here, so every waiter fails now.
			c.log.Warn("unparseable line from the server; dropping the connection", "err", err)
			c.lost(gen, done, fmt.Errorf("unparseable line: %w", err))
			return
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
		if ok && p.gen == gen {
			delete(c.pending, *in.ID)
		}
		c.pendMu.Unlock()
		if !ok || p.gen != gen {
			// No waiter, or a waiter on another connection: a reply on this
			// stream never satisfies a request made on a different one.
			c.log.Debug("discarding a reply with no waiter on this connection", "id", *in.ID)
			continue
		}
		p.ch <- in
	}
}

// lost is the reader's report that its connection can no longer be read. The
// generation's done channel is closed first, with no lock: every waiter on it,
// including Connect holding the connection lock through its handshake, returns
// at once. Then the connection is dropped if it is still the current one, the
// waiters registered since are failed, and Start is woken to reconnect. A read
// error on a connection Connect or Stop has already replaced or closed is not
// news.
func (c *Client) lost(gen uint64, done chan struct{}, err error) {
	close(done)
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
		_ = c.conn.Close()
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
	if gen != c.liveGen.Load() {
		return // from a connection already replaced or dropped
	}
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
		status, ok := parseStatus(params[1])
		if !ok {
			c.log.Debug("ignoring a scripthash notification whose status is neither a string nor null")
			return
		}
		c.noteChange(gen, sh, status)
	default:
		c.log.Debug("ignoring a notification", "method", in.Method)
	}
}

// parseStatus reads a scripthash status: a string, or null for an address
// with no history, which is read as the empty string. Anything else is not a
// status and is reported as such rather than mistaken for either.
func parseStatus(raw json.RawMessage) (string, bool) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
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

// errIsNoReply reports a call that timed out waiting for its reply: a sign
// the connection may be hung, though the server may only be slow.
func errIsNoReply(err error) bool {
	return errors.Is(err, errNoReply)
}
