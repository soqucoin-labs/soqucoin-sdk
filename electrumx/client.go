// Package electrumx provides a production-hardened TCP client for ElectrumX servers.
//
// This client was extracted from the canonical soq-signer service (v1.0.0-alpha)
// which has been running in production since May 2026. It incorporates all
// battle-tested fixes:
//
//   - PF-018: 4MB read buffer for addresses with 18,000+ UTXOs
//   - F5: TCP keepalive at 30s to survive NAT/firewall timeouts
//   - PF-018b: Connection mutex to prevent concurrent TCP stream corruption
//   - Defense 12: Merge-based UTXO refresh that preserves SpentPending flags
//   - Panic recovery: Polling goroutine auto-restarts after crashes
//
// Usage:
//
//	client := electrumx.NewClient("host:50001", 15*time.Second, logger)
//	if err := client.Connect(ctx); err != nil {
//	    return err
//	}
//	defer client.Stop()
//
//	client.TrackAddresses([]string{"ssq1abc..."})
//	client.StartPolling(ctx)
//
//	utxos := client.GetUTXOs("ssq1abc...")
//	balance := client.GetBalance(1, tipHeight)
//
// Every method that reaches the server takes a context.Context first. A call
// whose context ends returns ctx.Err() at once: the connection deadline is
// moved to now so the blocked write or read returns. A call cut short while
// waiting for a reply that has not begun to arrive leaves the connection
// usable, and the reply, if it arrives later, carries an id the next call
// discards; a call cut short while writing, or after part of a reply line
// has been read, closes the connection, since the stream is no longer known
// to be at a line boundary, and the next call returns ErrNotConnected until
// Reconnect or the polling loop restores it. Methods that read the cache
// (GetUTXOs, GetBalance, LastRefresh and the rest) take no context; they
// never block on the network.
//
// Copyright (c) 2025-2026 Soqucoin Labs Inc. MIT License.
package electrumx

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Client is a production-hardened TCP JSON-RPC client for ElectrumX.
//
// It maintains a persistent connection, tracks addresses via polling,
// and provides battle-tested UTXO caching with merge-based refresh
// (Defense 12) that preserves spend-pending state across poll cycles.
type Client struct {
	mu           sync.RWMutex
	utxos        map[string][]types.UTXO // address -> UTXOs
	host         string
	conn         net.Conn      // guarded by connMu
	reader       *bufio.Reader // guarded by connMu
	connMu       sync.Mutex    // PF-018b: Serializes all TCP I/O and connection replacement
	reqID        atomic.Int64
	lastTip      atomic.Int64 // latest height seen in a headers.subscribe reply or notification
	addresses    []string
	pollInterval time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	log          *slog.Logger

	lastRefreshAt  time.Time                // guarded by mu: last time EVERY tracked address refreshed
	lastRefreshErr error                    // guarded by mu: error of the last RefreshAll, nil on success
	refreshed      map[string]refreshRecord // guarded by mu: per tracked address, see LastRefreshOf
	trackedSet     map[string]bool          // guarded by mu: the addresses list as a set

	// HRP is the network prefix the tracked addresses must carry. Leave it
	// empty and TrackAddresses infers it from the addresses themselves; set it
	// explicitly ("sq" mainnet, "ssq" stagenet) to have TrackAddresses reject
	// any address on another network. There is deliberately no default: a
	// silent stagenet default on a mainnet deployment refreshed nothing, ever.
	HRP string

	// OnRefresh is called after each successful UTXO refresh with the address
	// and current UTXO count. Useful for monitoring/logging.
	OnRefresh func(address string, utxoCount int)

	// TLSConfig, when non-nil, wraps the connection in TLS. It applies to
	// Reconnect as well, so a connection cannot silently downgrade to plaintext
	// after a reconnect.
	//
	// Leave it nil only when the transport is already private: ElectrumX on
	// localhost, or over a tunnel you control. An ElectrumX server sees every
	// address you track, so over the public internet a plaintext connection
	// discloses your entire deposit set to anyone on the path, and lets them
	// alter the balances and UTXOs you act on.
	//
	// ServerName is filled in from the host when empty. The zero value of
	// tls.Config verifies the server certificate against the system roots,
	// which is what you want; see UseTLS.
	TLSConfig *tls.Config
}

// UseTLS enables TLS with certificate verification against the system roots.
// This is the setting an exchange should use for any ElectrumX server it does
// not reach over a private network.
//
//	client := electrumx.NewClient("electrum.example.org:50002", 15*time.Second, logger)
//	client.UseTLS()
//	client.Connect(ctx)
//
// For a private CA or a pinned certificate, set TLSConfig directly instead.
func (c *Client) UseTLS() {
	c.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
}

// refreshRecord is one address's refresh state: when it last refreshed
// successfully and the error of the most recent attempt, nil on success.
type refreshRecord struct {
	at  time.Time
	err error
}

// request is a JSON-RPC request to ElectrumX.
type request struct {
	ID     int64       `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// response is a JSON-RPC response from ElectrumX.
type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// incoming is any line the server sends: a reply (id set) or a notification
// (method set, no id). ElectrumX pushes blockchain.headers.subscribe
// notifications on the same connection once GetTip has subscribed, so a reader
// that takes "the next line" as "the reply" goes off by one at every new block.
type incoming struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error,omitempty"`
}

var (
	// ErrNotConnected is returned by every call made before Connect or after Stop.
	ErrNotConnected = errors.New("electrumx: not connected")
	// ErrNetworkMismatch is returned when a tracked address is on a different
	// network than the client's HRP, or when addresses in one call disagree.
	ErrNetworkMismatch = errors.New("electrumx: address network does not match the client's")
	// ErrGenesisMismatch is returned by Connect when the server's reported
	// genesis hash is not one of the chains the client's HRP belongs to.
	ErrGenesisMismatch = errors.New("electrumx: server is indexing a different chain")
)

// maxSkippedLines bounds how many notifications or stale replies one call will
// read past before giving up; the read deadline bounds it in time as well.
const maxSkippedLines = 64

// NewClient creates a new ElectrumX client.
//
// Parameters:
//   - host: ElectrumX TCP address (e.g., "localhost:50001")
//   - pollInterval: How often to refresh UTXOs (recommended: 15s for production)
//   - logger: where connection events, reorgs and polling errors go; nil discards
func NewClient(host string, pollInterval time.Duration, logger *slog.Logger) *Client {
	return &Client{
		log:          logutil.Or(logger),
		utxos:        make(map[string][]types.UTXO),
		refreshed:    make(map[string]refreshRecord),
		host:         host,
		pollInterval: pollInterval,
		stopCh:       make(chan struct{}),
	}
}

// dial opens the transport. Keepalive is set on the TCP connection underneath
// any TLS layer, so it survives the wrapping.
//
// The TLS handshake carries its own deadline. Without one a server that accepts
// the TCP connection and then stalls would hang Connect indefinitely, which is
// exactly how a reconnect loop wedges. Both the dial and the handshake end
// early when ctx ends.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	// F5: TCP keepalive at 30 s, so an idle connection survives NAT and
	// firewall timeouts between polls.
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
// if TLSConfig is set.
//
// Production lesson (F5): ElectrumX connections sit idle between poll intervals.
// NAT/firewall timeouts silently kill the connection after ~4h on DigitalOcean
// droplets. TCP keepalive at 30s prevents this.
//
// The connection is replaced under connMu, the same lock every call holds, so
// a Reconnect from the polling goroutine can never race a caller mid-request.
// After the version handshake the server's genesis hash is checked against the
// chains the client's HRP belongs to (see verifyGenesisLocked): an indexer for
// the wrong chain would otherwise report "no deposits" forever.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = conn
	// PF-018 FIX: Use 4MB buffer instead of default 4KB.
	// ElectrumX responses for addresses with 18,000+ UTXOs can exceed
	// 2MB. The default bufio.NewReader (4KB) panics on buffer growth
	// in Go 1.26's bufio.ReadSlice when response > 4KB.
	// 4MB accommodates ~50,000 UTXOs with margin.
	c.reader = bufio.NewReaderSize(conn, 4*1024*1024)
	c.log.Info("connected", "host", c.host, "tls", c.TLSConfig != nil)

	fail := func(err error) error {
		conn.Close()
		c.conn = nil
		c.reader = nil
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
		if got == w {
			return nil
		}
	}
	return fmt.Errorf("%w: server genesis %q, client HRP %q expects one of %v", ErrGenesisMismatch, got, c.HRP, want)
}

// Reconnect closes the existing connection and re-establishes it.
func (c *Client) Reconnect(ctx context.Context) error {
	c.log.Info("reconnecting", "host", c.host)
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("reconnect failed: %w", err)
	}
	c.log.Info("reconnected", "host", c.host)
	return nil
}

// Call sends a JSON-RPC request and reads the response.
// This is exported for advanced usage; prefer the typed methods below.
//
// Production lesson (PF-018b): Multiple goroutines (polling, sendmany,
// consolidation) call this concurrently. Without the connection mutex,
// concurrent writes corrupt the TCP stream, and concurrent reads corrupt
// bufio's internal buffer → panic: slice bounds out of range.
func (c *Client) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	return c.call(ctx, method, params)
}

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	// PF-018b FIX: Serialize TCP I/O.
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.callLocked(ctx, method, params)
}

// callDeadline bounds one request/reply exchange when the context does not end
// it first.
const callDeadline = 30 * time.Second

// callLocked performs one request/reply exchange. Caller holds connMu.
//
// The reply is identified by id, never by position. Lines carrying a method
// are server notifications (headers.subscribe pushes one at every new block)
// and are consumed here; lines carrying another id are replies to an earlier
// request that timed out and are discarded. Before this, a notification was
// returned as the reply to whatever call happened to read it, and every later
// reply was off by one: listunspent for address A stored under address B.
//
// The context ends the exchange the same way the deadline does: when it is
// done the connection deadline is moved to now, the blocked write or read
// returns, and ctx.Err() is reported. A read cut short before any byte of a
// line arrived leaves the stream in step: the reply, if it arrives, is a
// stale id the next call discards. A write cut short, or a read cut short
// with part of a line consumed, does not, so the connection is closed; the
// next call returns ErrNotConnected and Reconnect, or the polling loop,
// restores it.
func (c *Client) callLocked(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := c.reqID.Add(1)
	req := request{
		ID:     id,
		Method: method,
		Params: params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// One deadline covers the write and the read: a stalled peer must not be
	// able to hold connMu, and every other caller behind it, forever. The
	// context ends the exchange through the same deadline, moved to now when
	// the context is done; it is not copied into the deadline up front,
	// because the socket's timer and the context's timer would then fire
	// together and the read could return before ctx.Err() is set.
	conn := c.conn
	if err := conn.SetDeadline(time.Now().Add(callDeadline)); err != nil {
		// A socket that refuses a deadline is closed or broken underneath.
		c.dropLocked()
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	// If the context ends while the call is in flight the callback moves the
	// deadline to now. On return the callback is stopped, or, when it has
	// already started, waited for: connMu is still held here, so it cannot
	// run after the next call has set its own deadline.
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		conn.SetDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-fired
		}
	}()
	// ctxErr reports the context's error in place of the deadline error the
	// connection produced when the context is what ended the exchange.
	ctxErr := func(err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return err
	}

	// ElectrumX uses newline-delimited JSON
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		// Part of the line may have gone out; the stream cannot be trusted.
		c.dropLocked()
		return nil, fmt.Errorf("write request: %w", ctxErr(err))
	}

	for skipped := 0; skipped < maxSkippedLines; skipped++ {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			if len(line) > 0 {
				// Part of a line was consumed; the rest would be read as the
				// start of the next reply. The stream cannot be trusted.
				c.dropLocked()
			}
			return nil, fmt.Errorf("read response: %w", ctxErr(err))
		}
		var in incoming
		if err := json.Unmarshal(line, &in); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		if in.Method != "" {
			c.handleNotification(in)
			continue
		}
		if in.ID == nil {
			c.log.Warn("discarding a line with neither id nor method")
			continue
		}
		if *in.ID != id {
			c.log.Debug("discarding a stale reply", "got_id", *in.ID, "want_id", id)
			continue
		}
		if len(in.Error) > 0 && string(in.Error) != "null" {
			return nil, fmt.Errorf("electrumx error: %s", string(in.Error))
		}
		if method == "blockchain.headers.subscribe" {
			c.recordTip(in.Result)
		}
		return in.Result, nil
	}
	return nil, fmt.Errorf("electrumx: no reply to request %d within %d lines", id, maxSkippedLines)
}

// dropLocked closes and forgets the connection. Caller holds connMu. The next
// call returns ErrNotConnected until Reconnect, or the polling loop, dials again.
func (c *Client) dropLocked() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = nil
	c.reader = nil
}

// handleNotification consumes a server push. Only the headers subscription is
// meaningful to this client; its height is recorded so LastTip stays current
// between GetTip calls.
func (c *Client) handleNotification(in incoming) {
	if in.Method != "blockchain.headers.subscribe" {
		return
	}
	var params []json.RawMessage
	if err := json.Unmarshal(in.Params, &params); err != nil || len(params) == 0 {
		return
	}
	c.recordTip(params[0])
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
// a GetTip reply or a server notification. Zero until the first GetTip.
func (c *Client) LastTip() int64 { return c.lastTip.Load() }

// TrackAddresses registers addresses for UTXO tracking.
//
// Every address must decode on one network. If HRP is unset it is inferred
// from the first address; if it is set, an address on another network is an
// error (ErrNetworkMismatch). Nothing is registered when any address fails, so
// a mainnet deployment that forgets to set HRP gets an error at startup rather
// than a cache that refreshes nothing for the life of the process.
func (c *Client) TrackAddresses(addresses []string) error {
	hrp := c.HRP
	for _, a := range addresses {
		n, err := address.NetworkOf(a)
		if err != nil {
			return fmt.Errorf("track %s: %w", a, err)
		}
		if hrp == "" {
			hrp = n.HRP
		} else if n.HRP != hrp {
			return fmt.Errorf("%w: %s is %s, client is %s", ErrNetworkMismatch, a, n.HRP, hrp)
		}
		if _, _, err := address.Decode(hrp, a); err != nil {
			return fmt.Errorf("track %s: %w", a, err)
		}
	}
	c.mu.Lock()
	c.HRP = hrp
	c.addresses = append([]string(nil), addresses...)
	c.trackedSet = make(map[string]bool, len(addresses))
	for _, a := range addresses {
		c.trackedSet[a] = true
	}
	for a := range c.refreshed {
		if !c.trackedSet[a] {
			delete(c.refreshed, a)
		}
	}
	c.mu.Unlock()
	return nil
}

// RefreshAll fetches UTXOs for all tracked addresses.
//
// One failing address does not stop the others: every address is attempted
// and the returned error joins the failures, naming each address. The result
// is recorded for LastRefresh, which callers must consult before treating an
// empty UTXO set as "no deposits", and per address for LastRefreshOf.
//
// The pass is one blockchain.scripthash.listunspent per tracked address, in
// sequence, on the one connection, each under the 30-second call deadline, so
// a pass takes the sum of the round trips. deposit.Monitor treats an address
// as stale once its last successful refresh is older than its MaxCacheAge,
// which puts a ceiling on the addresses one client can keep fresh: at a
// 5-minute MaxCacheAge, about 300 seconds divided by the round trip.
//
// When ctx ends mid-pass the pass stops there: the addresses reached keep
// their new records, the rest keep their old ones, and the error names the
// first address not refreshed.
func (c *Client) RefreshAll(ctx context.Context) error {
	c.mu.RLock()
	addrs := make([]string, len(c.addresses))
	copy(addrs, c.addresses)
	c.mu.RUnlock()

	var errs []error
	for _, addr := range addrs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", addr, err))
			break
		}
		err := c.refreshAddress(ctx, addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", addr, err))
		}
		c.mu.Lock()
		if c.trackedSet[addr] { // TrackAddresses may have dropped it during the call
			rec := c.refreshed[addr]
			rec.err = err
			if err == nil {
				rec.at = time.Now()
			}
			c.refreshed[addr] = rec
		}
		c.mu.Unlock()
	}
	err := errors.Join(errs...)

	c.mu.Lock()
	c.lastRefreshErr = err
	if err == nil {
		c.lastRefreshAt = time.Now()
	}
	c.mu.Unlock()
	return err
}

// LastRefresh reports when every tracked address last refreshed successfully
// and the error of the most recent RefreshAll (nil on success). A zero time or
// a non-nil error means the cache may be stale: GetUTXOs and GetBalance answer
// from the cache regardless, so this is how an indexer outage is told apart
// from "no deposits".
func (c *Client) LastRefresh() (time.Time, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefreshAt, c.lastRefreshErr
}

// LastRefreshOf reports when one tracked address last refreshed successfully
// and the error of the most recent attempt for it, nil on success. The zero
// time means never: the address is not tracked, or no pass has reached it.
// deposit.Monitor uses it to skip only the addresses the indexer has not
// answered for instead of pausing every credit when one of thousands fails.
func (c *Client) LastRefreshOf(addr string) (time.Time, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rec := c.refreshed[addr]
	return rec.at, rec.err
}

// refreshAddress fetches UTXOs for a single address via ElectrumX.
//
// Defense 12 (Merge Refresh): Uses a MERGE strategy instead of full replacement.
// The old code wiped SpentPending flags on every poll, creating a race where a
// UTXO could be re-selected while its prior TX was still in the mempool.
// The new code:
//  1. Preserves SpentPending and AssetType flags on UTXOs that still appear
//  2. Removes UTXOs that ElectrumX no longer reports (confirmed spent)
//  3. Adds new UTXOs that appeared since last poll (change outputs, new coinbases)
func (c *Client) refreshAddress(ctx context.Context, addr string) error {
	scriptHash, err := address.AddressToScriptHash(c.HRP, addr)
	if err != nil {
		return fmt.Errorf("address to script hash: %w", err)
	}

	result, err := c.call(ctx, "blockchain.scripthash.listunspent", []interface{}{scriptHash})
	if err != nil {
		return fmt.Errorf("listunspent: %w", err)
	}

	var freshUTXOs []types.UTXO
	if err := json.Unmarshal(result, &freshUTXOs); err != nil {
		return fmt.Errorf("parse utxos: %w", err)
	}

	// Build a lookup set of fresh UTXOs from ElectrumX
	type utxoKey struct {
		TxID string
		Vout uint32
	}
	freshSet := make(map[utxoKey]types.UTXO, len(freshUTXOs))
	for i := range freshUTXOs {
		freshUTXOs[i].Address = addr
		key := utxoKey{freshUTXOs[i].TxID, freshUTXOs[i].Vout}
		freshSet[key] = freshUTXOs[i]
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	existing := c.utxos[addr]

	// Step 1: Walk existing UTXOs — keep ones still in freshSet (preserving flags)
	var merged []types.UTXO
	kept := make(map[utxoKey]bool)

	for _, u := range existing {
		key := utxoKey{u.TxID, u.Vout}
		if fresh, stillExists := freshSet[key]; stillExists {
			// Preserve our flags (SpentPending, AssetType) but take the server's
			// height and value on every pass. Height must be allowed to fall
			// back to 0 (a reorg returned the tx to the mempool) or move (it
			// was re-mined elsewhere); value must be allowed to correct a
			// wrongly injected change output, because the amount is committed
			// into the sighash and a stale one makes every spend fail.
			if u.Height > 0 && fresh.Height == 0 {
				c.log.Warn("reorg: a confirmed output is back in the mempool", "txid", u.TxID, "vout", u.Vout, "height", u.Height)
			}
			u.Height = fresh.Height
			u.Value = fresh.Value
			merged = append(merged, u)
			kept[key] = true
		}
		// else: UTXO disappeared → confirmed spent, drop it
	}

	// Step 2: Add new UTXOs
	newCount := 0
	for key, u := range freshSet {
		if !kept[key] {
			merged = append(merged, u)
			newCount++
		}
	}

	c.utxos[addr] = merged

	if c.OnRefresh != nil {
		c.OnRefresh(addr, len(merged))
	}

	return nil
}

// StartPolling begins periodic UTXO refresh in a goroutine. The goroutine
// ends when ctx ends or Stop is called; every refresh it makes runs under ctx.
//
// Production lesson: The polling goroutine includes panic recovery
// and auto-reconnect. Without this, a bufio panic kills the entire
// process. With recovery, the goroutine logs the panic, reconnects,
// and resumes polling.
func (c *Client) StartPolling(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(c.pollInterval)
		defer ticker.Stop()

		// PF-018 FIX: Recover from panics in the polling goroutine.
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("panic in the polling goroutine, reconnecting", "panic", r)
				if reconErr := c.Reconnect(ctx); reconErr != nil {
					c.log.Error("reconnect after panic failed", "err", reconErr)
				}
				if ctx.Err() == nil {
					c.StartPolling(ctx)
				}
			}
		}()

		// Initial refresh
		if err := c.RefreshAll(ctx); err != nil {
			c.log.Warn("initial refresh failed", "err", err)
		}

		consecutiveErrors := 0

		for {
			select {
			case <-ticker.C:
				if err := c.RefreshAll(ctx); err != nil {
					consecutiveErrors++
					c.log.Warn("refresh failed", "consecutive", consecutiveErrors, "err", err)

					// F5: Auto-reconnect after 2 consecutive failures
					if consecutiveErrors >= 2 && ctx.Err() == nil {
						if reconErr := c.Reconnect(ctx); reconErr != nil {
							c.log.Warn("reconnect failed, retrying at the next poll", "err", reconErr)
						} else {
							consecutiveErrors = 0
						}
					}
				} else {
					consecutiveErrors = 0
				}
			case <-ctx.Done():
				return
			case <-c.stopCh:
				return
			}
		}
	}()
}

// Stop halts the polling goroutine and closes the connection. Safe to call
// more than once; calls after Stop return ErrNotConnected.
func (c *Client) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.reader = nil
	}
}

// GetBalance returns the total confirmed and unconfirmed balance across all tracked addresses.
// Only counts native SOQ UTXOs (AssetType=0). USDSOQ and future types have separate accounting.
func (c *Client) GetBalance(minConf int, tipHeight int64) (confirmed, unconfirmed int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, utxos := range c.utxos {
		for _, u := range utxos {
			if u.SpentPending {
				continue
			}
			if u.AssetType != types.AssetTypeSOQ {
				continue
			}
			if u.Height > 0 && (tipHeight-u.Height+1) >= int64(minConf) {
				confirmed += u.Value
			} else {
				unconfirmed += u.Value
			}
		}
	}
	return
}

// GetUTXOs returns a copy of all UTXOs for the given address.
func (c *Client) GetUTXOs(addr string) []types.UTXO {
	c.mu.RLock()
	defer c.mu.RUnlock()

	utxos := c.utxos[addr]
	result := make([]types.UTXO, len(utxos))
	copy(result, utxos)
	return result
}

// GetAllUTXOs returns a copy of all UTXOs across all tracked addresses.
func (c *Client) GetAllUTXOs() []types.UTXO {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var all []types.UTXO
	for _, utxos := range c.utxos {
		all = append(all, utxos...)
	}
	return all
}

// MarkSpentPending marks a UTXO as spent-pending (used in transit, awaiting confirmation).
func (c *Client) MarkSpentPending(txid string, vout uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for addr, utxos := range c.utxos {
		for i, u := range utxos {
			if u.TxID == txid && u.Vout == vout {
				c.utxos[addr][i].SpentPending = true
				return
			}
		}
	}
}

// UnmarkSpentPending reverses a spent-pending mark (e.g., if broadcast failed).
func (c *Client) UnmarkSpentPending(txid string, vout uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for addr, utxos := range c.utxos {
		for i, u := range utxos {
			if u.TxID == txid && u.Vout == vout {
				c.utxos[addr][i].SpentPending = false
				return
			}
		}
	}
}

// EvictUTXO permanently removes a UTXO from the in-memory cache.
// Called by Defense 11 (gettxout pre-verification) when a UTXO is confirmed
// spent on-chain but ElectrumX still returns it. The UTXO will be re-added
// on the next refresh ONLY if ElectrumX still reports it.
func (c *Client) EvictUTXO(txid string, vout uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for addr, utxos := range c.utxos {
		for i, u := range utxos {
			if u.TxID == txid && u.Vout == vout {
				c.utxos[addr] = append(utxos[:i], utxos[i+1:]...)
				c.log.Info("evicted a stale output from the cache", "txid", txid, "vout", vout)
				return
			}
		}
	}
}

// AddChangeUTXO records a change output in the UTXO cache before the indexer
// reports it, at height 0. It shows in GetBalance's unconfirmed figure and in
// GetUTXOs at once; the next refresh replaces it with the indexer's record, or
// drops it if the indexer does not know the transaction yet.
//
// It does not make the change spendable. utxo.CoinSelector selects only
// outputs with a height above zero at the confirmations asked for, so change
// becomes an input once it has confirmed and the indexer reports it, whether
// or not it was injected here. A run that needs the change of one payment to
// fund the next stalls; keep enough confirmed outputs for the run instead.
//
// Deprecated: the injection has no effect on selection and the cache shows
// the output within one poll anyway. Kept for callers that read the
// unconfirmed balance; it may be removed in v0.4.
func (c *Client) AddChangeUTXO(txid string, vout uint32, value int64, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	changeUTXO := types.UTXO{
		TxID:      txid,
		Vout:      vout,
		Value:     value,
		Height:    0, // Unconfirmed — updated by next refresh
		Address:   addr,
		AssetType: types.AssetTypeSOQ,
	}

	c.utxos[addr] = append(c.utxos[addr], changeUTXO)
	c.log.Info("added a change output to the cache", "txid", txid, "vout", vout, "value", value, "address", addr)
}

// SetAssetType stamps the asset type on a cached UTXO. Called by Defense 11
// (gettxout verification) after reading the "assettype" field from RC7+
// gettxout responses.
func (c *Client) SetAssetType(txid string, vout uint32, assetType uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for addr, utxos := range c.utxos {
		for i, u := range utxos {
			if u.TxID == txid && u.Vout == vout {
				c.utxos[addr][i].AssetType = assetType
				return
			}
		}
	}
}

// UTXOCount returns the total number of spendable native SOQ UTXOs.
func (c *Client) UTXOCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	count := 0
	for _, utxos := range c.utxos {
		for _, u := range utxos {
			if !u.SpentPending && u.AssetType == types.AssetTypeSOQ {
				count++
			}
		}
	}
	return count
}

// GetTip fetches the current chain tip height from ElectrumX.
func (c *Client) GetTip(ctx context.Context) (int64, error) {
	result, err := c.call(ctx, "blockchain.headers.subscribe", []interface{}{})
	if err != nil {
		return 0, fmt.Errorf("get tip: %w", err)
	}

	var header struct {
		Height int64 `json:"height"`
	}
	if err := json.Unmarshal(result, &header); err != nil {
		return 0, fmt.Errorf("parse tip: %w", err)
	}
	return header.Height, nil
}

// GetHistory fetches transaction history for an address.
func (c *Client) GetHistory(ctx context.Context, addr string) ([]TxHistoryEntry, error) {
	scriptHash, err := address.AddressToScriptHash(c.HRP, addr)
	if err != nil {
		return nil, fmt.Errorf("address to script hash: %w", err)
	}

	result, err := c.call(ctx, "blockchain.scripthash.get_history", []interface{}{scriptHash})
	if err != nil {
		return nil, fmt.Errorf("get_history: %w", err)
	}

	var entries []TxHistoryEntry
	if err := json.Unmarshal(result, &entries); err != nil {
		return nil, fmt.Errorf("parse history: %w", err)
	}
	return entries, nil
}

// TxHistoryEntry represents a single transaction in an address's history.
type TxHistoryEntry struct {
	TxHash string `json:"tx_hash"`
	Height int64  `json:"height"` // 0 = unconfirmed
}

// BroadcastTx broadcasts a raw transaction hex via ElectrumX.
func (c *Client) BroadcastTx(ctx context.Context, rawTxHex string) (string, error) {
	result, err := c.call(ctx, "blockchain.transaction.broadcast", []interface{}{rawTxHex})
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}

	var txid string
	if err := json.Unmarshal(result, &txid); err != nil {
		return "", fmt.Errorf("parse broadcast result: %w", err)
	}
	return txid, nil
}
