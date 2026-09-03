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
//	client := electrumx.NewClient("host:50001", 15*time.Second)
//	if err := client.Connect(); err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Stop()
//
//	client.TrackAddresses([]string{"ssq1abc..."})
//	client.StartPolling()
//
//	utxos := client.GetUTXOs("ssq1abc...")
//	balance := client.GetBalance(1, tipHeight)
//
// Copyright (c) 2025-2026 Soqucoin Labs Inc. MIT License.
package electrumx

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
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

	lastRefreshAt  time.Time // guarded by mu: last time EVERY tracked address refreshed
	lastRefreshErr error     // guarded by mu: error of the last RefreshAll, nil on success

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
//	client := electrumx.NewClient("electrum.example.org:50002", 15*time.Second)
//	client.UseTLS()
//	client.Connect()
//
// For a private CA or a pinned certificate, set TLSConfig directly instead.
func (c *Client) UseTLS() {
	c.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
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
func NewClient(host string, pollInterval time.Duration) *Client {
	return &Client{
		utxos:        make(map[string][]types.UTXO),
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
// exactly how a reconnect loop wedges.
func (c *Client) dial() (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", c.host, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to electrumx %s: %w", c.host, err)
	}

	// F5: Enable TCP keepalive to prevent broken pipe after idle periods.
	if tcpConn, ok := raw.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		log.Printf("[electrumx] TCP keepalive enabled (30s interval)")
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
	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		raw.Close()
		return nil, fmt.Errorf("electrumx %s: set handshake deadline: %w", c.host, err)
	}
	if err := tlsConn.Handshake(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("electrumx %s: tls handshake: %w", c.host, err)
	}
	// Clear the handshake deadline; per-call timeouts govern from here.
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("electrumx %s: clear handshake deadline: %w", c.host, err)
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
func (c *Client) Connect() error {
	conn, err := c.dial()
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
	if c.TLSConfig != nil {
		log.Printf("[electrumx] Connected to ElectrumX at %s over TLS", c.host)
	} else {
		log.Printf("[electrumx] Connected to ElectrumX at %s in plaintext", c.host)
	}

	fail := func(err error) error {
		conn.Close()
		c.conn = nil
		c.reader = nil
		return err
	}

	// Server version handshake
	resp, err := c.callLocked("server.version", []interface{}{"soqucoin-sdk/1.0", "1.4"})
	if err != nil {
		return fail(fmt.Errorf("electrumx handshake: %w", err))
	}
	log.Printf("[electrumx] Server version: %s", string(resp))

	if err := c.verifyGenesisLocked(); err != nil {
		return fail(err)
	}
	return nil
}

// verifyGenesisLocked refuses a server that indexes a chain other than the one
// the client's addresses belong to. Skipped only while the HRP is still unknown
// (before TrackAddresses), in which case TrackAddresses is the gate.
func (c *Client) verifyGenesisLocked() error {
	if c.HRP == "" {
		return nil
	}
	want := types.GenesisHashesForHRP(c.HRP)
	if len(want) == 0 {
		return fmt.Errorf("%w: no known chain uses HRP %q", ErrNetworkMismatch, c.HRP)
	}
	raw, err := c.callLocked("server.features", []interface{}{})
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
func (c *Client) Reconnect() error {
	log.Printf("[electrumx] Reconnecting to ElectrumX at %s...", c.host)
	if err := c.Connect(); err != nil {
		return fmt.Errorf("reconnect failed: %w", err)
	}
	log.Printf("[electrumx] Reconnected successfully")
	return nil
}

// Call sends a JSON-RPC request and reads the response.
// This is exported for advanced usage; prefer the typed methods below.
//
// Production lesson (PF-018b): Multiple goroutines (polling, sendmany,
// consolidation) call this concurrently. Without the connection mutex,
// concurrent writes corrupt the TCP stream, and concurrent reads corrupt
// bufio's internal buffer → panic: slice bounds out of range.
func (c *Client) Call(method string, params interface{}) (json.RawMessage, error) {
	return c.call(method, params)
}

func (c *Client) call(method string, params interface{}) (json.RawMessage, error) {
	// PF-018b FIX: Serialize TCP I/O.
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.callLocked(method, params)
}

// callLocked performs one request/reply exchange. Caller holds connMu.
//
// The reply is identified by id, never by position. Lines carrying a method
// are server notifications (headers.subscribe pushes one at every new block)
// and are consumed here; lines carrying another id are replies to an earlier
// request that timed out and are discarded. Before this, a notification was
// returned as the reply to whatever call happened to read it, and every later
// reply was off by one: listunspent for address A stored under address B.
func (c *Client) callLocked(method string, params interface{}) (json.RawMessage, error) {
	if c.conn == nil {
		return nil, ErrNotConnected
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
	// able to hold connMu, and every other caller behind it, forever.
	if err := c.conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	// ElectrumX uses newline-delimited JSON
	data = append(data, '\n')
	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	for skipped := 0; skipped < maxSkippedLines; skipped++ {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
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
			log.Printf("[electrumx] discarding line with neither id nor method")
			continue
		}
		if *in.ID != id {
			log.Printf("[electrumx] discarding stale reply id=%d while waiting for id=%d", *in.ID, id)
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
	c.mu.Unlock()
	return nil
}

// RefreshAll fetches UTXOs for all tracked addresses.
//
// One failing address does not stop the others: every address is attempted
// and the returned error joins the failures, naming each address. The result
// is recorded for LastRefresh, which callers must consult before treating an
// empty UTXO set as "no deposits".
func (c *Client) RefreshAll() error {
	c.mu.RLock()
	addrs := make([]string, len(c.addresses))
	copy(addrs, c.addresses)
	c.mu.RUnlock()

	var errs []error
	for _, addr := range addrs {
		if err := c.refreshAddress(addr); err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", addr, err))
		}
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

// refreshAddress fetches UTXOs for a single address via ElectrumX.
//
// Defense 12 (Merge Refresh): Uses a MERGE strategy instead of full replacement.
// The old code wiped SpentPending flags on every poll, creating a race where a
// UTXO could be re-selected while its prior TX was still in the mempool.
// The new code:
//  1. Preserves SpentPending and AssetType flags on UTXOs that still appear
//  2. Removes UTXOs that ElectrumX no longer reports (confirmed spent)
//  3. Adds new UTXOs that appeared since last poll (change outputs, new coinbases)
func (c *Client) refreshAddress(addr string) error {
	scriptHash, err := address.AddressToScriptHash(c.HRP, addr)
	if err != nil {
		return fmt.Errorf("address to script hash: %w", err)
	}

	result, err := c.call("blockchain.scripthash.listunspent", []interface{}{scriptHash})
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
				log.Printf("[electrumx] REORG: %s:%d left height %d and is back in the mempool",
					shortID(u.TxID, 12), u.Vout, u.Height)
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

// StartPolling begins periodic UTXO refresh in a goroutine.
//
// Production lesson: The polling goroutine includes panic recovery
// and auto-reconnect. Without this, a bufio panic kills the entire
// process. With recovery, the goroutine logs the panic, reconnects,
// and resumes polling.
func (c *Client) StartPolling() {
	go func() {
		ticker := time.NewTicker(c.pollInterval)
		defer ticker.Stop()

		// PF-018 FIX: Recover from panics in the polling goroutine.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[electrumx] PANIC in polling goroutine (recovered): %v", r)
				log.Printf("[electrumx] Attempting reconnect after panic...")
				if reconErr := c.Reconnect(); reconErr != nil {
					log.Printf("[electrumx] Post-panic reconnect failed: %v", reconErr)
				}
				// Restart polling after recovery
				c.StartPolling()
			}
		}()

		// Initial refresh
		if err := c.RefreshAll(); err != nil {
			log.Printf("[electrumx] Initial refresh error: %v", err)
		}

		consecutiveErrors := 0

		for {
			select {
			case <-ticker.C:
				if err := c.RefreshAll(); err != nil {
					consecutiveErrors++
					log.Printf("[electrumx] Refresh error (%d consecutive): %v", consecutiveErrors, err)

					// F5: Auto-reconnect after 2 consecutive failures
					if consecutiveErrors >= 2 {
						log.Printf("[electrumx] Connection appears dead, attempting reconnect...")
						if reconErr := c.Reconnect(); reconErr != nil {
							log.Printf("[electrumx] Reconnect failed: %v (will retry next poll)", reconErr)
						} else {
							consecutiveErrors = 0
						}
					}
				} else {
					consecutiveErrors = 0
				}
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
				log.Printf("[electrumx] Evicted stale UTXO %s:%d from cache", shortID(txid, 12), vout)
				return
			}
		}
	}
}

// AddChangeUTXO injects a known change output into the UTXO cache immediately
// after broadcast. This eliminates the delay between broadcast and ElectrumX
// discovering the new UTXO — critical for back-to-back payments.
//
// Defense 13 (DL-ENTERPRISE-PAYOUT): The change output from a payment TX is
// deterministic — the builder knows the exact txid, vout, value, and address.
// By adding it to the cache with height=0 (unconfirmed), it becomes available
// for the next payment's coin selection immediately.
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
	// Previously this whole log was skipped when either identifier was short,
	// which silently dropped the diagnostic for exactly the inputs most likely to
	// be wrong. shortID keeps the line and truncates safely instead.
	log.Printf("[electrumx] Added change UTXO %s:%d (%d shors) for %s...",
		shortID(txid, 12), vout, value, shortID(addr, 20))
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
func (c *Client) GetTip() (int64, error) {
	result, err := c.call("blockchain.headers.subscribe", []interface{}{})
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
func (c *Client) GetHistory(addr string) ([]TxHistoryEntry, error) {
	scriptHash, err := address.AddressToScriptHash(c.HRP, addr)
	if err != nil {
		return nil, fmt.Errorf("address to script hash: %w", err)
	}

	result, err := c.call("blockchain.scripthash.get_history", []interface{}{scriptHash})
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
func (c *Client) BroadcastTx(rawTxHex string) (string, error) {
	result, err := c.call("blockchain.transaction.broadcast", []interface{}{rawTxHex})
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}

	var txid string
	if err := json.Unmarshal(result, &txid); err != nil {
		return "", fmt.Errorf("parse broadcast result: %w", err)
	}
	return txid, nil
}

// shortID truncates an identifier for logging without panicking on short input.
// Log formatting must never be able to crash the caller: these helpers sit on
// error paths, and a panic there replaces a handled error with process death.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
