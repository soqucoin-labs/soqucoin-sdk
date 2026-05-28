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
	"encoding/json"
	"fmt"
	"log"
	"net"
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
	mu        sync.RWMutex
	utxos     map[string][]types.UTXO // address -> UTXOs
	host      string
	conn      net.Conn
	reader    *bufio.Reader
	connMu    sync.Mutex   // PF-018b: Serializes all TCP I/O
	reqID     atomic.Int64
	addresses []string
	pollInterval time.Duration
	stopCh    chan struct{}

	// Network HRP for address-to-script-hash conversion.
	// Defaults to "ssq" (stagenet). Set to "sq" for mainnet.
	HRP string

	// OnRefresh is called after each successful UTXO refresh with the address
	// and current UTXO count. Useful for monitoring/logging.
	OnRefresh func(address string, utxoCount int)
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
		HRP:          "ssq", // Default to stagenet
	}
}

// Connect establishes a TCP connection to ElectrumX with keepalive enabled.
//
// Production lesson (F5): ElectrumX connections sit idle between poll intervals.
// NAT/firewall timeouts silently kill the connection after ~4h on DigitalOcean
// droplets. TCP keepalive at 30s prevents this.
func (c *Client) Connect() error {
	conn, err := net.DialTimeout("tcp", c.host, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", c.host, err)
	}

	// F5: Enable TCP keepalive to prevent broken pipe after idle periods.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		log.Printf("[electrumx] TCP keepalive enabled (30s interval)")
	}

	c.conn = conn
	// PF-018 FIX: Use 4MB buffer instead of default 4KB.
	// ElectrumX responses for addresses with 18,000+ UTXOs can exceed
	// 2MB. The default bufio.NewReader (4KB) panics on buffer growth
	// in Go 1.26's bufio.ReadSlice when response > 4KB.
	// 4MB accommodates ~50,000 UTXOs with margin.
	c.reader = bufio.NewReaderSize(conn, 4*1024*1024)
	log.Printf("[electrumx] Connected to ElectrumX at %s", c.host)

	// Server version handshake
	resp, err := c.call("server.version", []interface{}{"soqucoin-sdk/1.0", "1.4"})
	if err != nil {
		c.conn.Close()
		return fmt.Errorf("electrumx handshake: %w", err)
	}
	log.Printf("[electrumx] Server version: %s", string(resp))

	return nil
}

// Reconnect closes the existing connection and re-establishes it.
func (c *Client) Reconnect() error {
	if c.conn != nil {
		c.conn.Close()
	}
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

	// PF-018b FIX: Serialize TCP I/O.
	c.connMu.Lock()
	defer c.connMu.Unlock()

	// ElectrumX uses newline-delimited JSON
	data = append(data, '\n')
	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Read response
	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(resp.Error) > 0 && string(resp.Error) != "null" {
		return nil, fmt.Errorf("electrumx error: %s", string(resp.Error))
	}

	return resp.Result, nil
}

// TrackAddresses registers addresses for UTXO tracking.
func (c *Client) TrackAddresses(addresses []string) {
	c.mu.Lock()
	c.addresses = addresses
	c.mu.Unlock()
}

// RefreshAll fetches UTXOs for all tracked addresses.
func (c *Client) RefreshAll() error {
	c.mu.RLock()
	addrs := make([]string, len(c.addresses))
	copy(addrs, c.addresses)
	c.mu.RUnlock()

	for _, addr := range addrs {
		if err := c.refreshAddress(addr); err != nil {
			return fmt.Errorf("refresh %s: %w", addr, err)
		}
	}
	return nil
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
			// Preserve our copy (SpentPending, AssetType) but update height
			if u.Height == 0 && fresh.Height > 0 {
				u.Height = fresh.Height
			}
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

// Stop halts the polling goroutine and closes the connection.
func (c *Client) Stop() {
	close(c.stopCh)
	if c.conn != nil {
		c.conn.Close()
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
				log.Printf("[electrumx] Evicted stale UTXO %s:%d from cache", txid[:12], vout)
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
	if len(txid) >= 12 && len(addr) >= 20 {
		log.Printf("[electrumx] Added change UTXO %s:%d (%d sat) for %s...",
			txid[:12], vout, value, addr[:20])
	}
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
