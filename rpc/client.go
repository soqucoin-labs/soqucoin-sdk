// Package rpc provides a JSON-RPC client for soqucoind.
//
// This client supports the subset of RPC methods needed for wallet operations:
//   - sendrawtransaction: Broadcast signed transactions
//   - gettxout: Verify UTXO existence before signing (Defense 11)
//   - getblockcount: Get current chain tip height
//   - estimatesmartfee: Get fee rate estimation
//   - decoderawtransaction: Parse raw TX hex
//   - getblock/getblockhash: Block data retrieval
//
// Authentication uses HTTP Basic Auth with the RPC credentials from soqucoin.conf.
//
// IMPORTANT: All production Soqucoin nodes run with disablewallet=1.
// Wallet RPCs (listunspent, getbalance, etc.) are NOT available.
// Use the ElectrumX client (electrumx package) for UTXO queries instead.
//
// Copyright (c) 2025-2026 Soqucoin Labs Inc. MIT License.
package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Client is a JSON-RPC client for soqucoind.
type Client struct {
	url      string
	user     string
	password string
	client   *http.Client
}

// NewClient creates a new soqucoind RPC client.
//
// Parameters:
//   - url: Full URL including port (e.g., "http://127.0.0.1:33389" on mainnet)
//   - user: RPC username from soqucoin.conf
//   - password: RPC password from soqucoin.conf
func NewClient(url, user, password string) *Client {
	return &Client{
		url:      url,
		user:     user,
		password: password,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// rpcRequest is a JSON-RPC 1.0 request.
type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// rpcResponse is a JSON-RPC 1.0 response.
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
	ID     int             `json:"id"`
}

// Error kinds. Every error this package returns wraps exactly one of these,
// so a caller decides what to do with errors.Is and never by string matching:
//
//	ErrTransient      retry later; the node was unreachable, warming up, or
//	                  still syncing. Nothing about the request was wrong.
//	ErrPermanent      do not retry; the request was rejected (bad parameter,
//	                  transaction refused by consensus or policy).
//	ErrUnknownOutcome the request MAY have taken effect. Only broadcast can
//	                  produce this: the transaction was written to the node
//	                  and the reply was lost. A caller that treats this as
//	                  "failed" and rebuilds pays twice.
//	ErrAlreadyInChain the transaction is already mined. For a broadcast that
//	                  is success, not failure.
var (
	ErrTransient      = errors.New("rpc: transient failure, retry later")
	ErrPermanent      = errors.New("rpc: request rejected")
	ErrUnknownOutcome = errors.New("rpc: outcome unknown, the request may have taken effect")
	ErrAlreadyInChain = errors.New("rpc: transaction already in chain")
	ErrNodeSyncing    = fmt.Errorf("%w: node is in initial block download or behind its headers", ErrTransient)
)

// Node error codes this package interprets (src/rpc/protocol.h in the node).
const (
	CodeInvalidAddressOrKey       = -5
	CodeInvalidParameter          = -8
	CodeClientInInitialDownload   = -10
	CodeVerifyError               = -25
	CodeVerifyRejected            = -26
	CodeTransactionAlreadyInChain = -27
	CodeInWarmup                  = -28
)

// Error is a JSON-RPC error returned by the node, exported so callers can
// read the code with errors.As. It also reports the kind it belongs to
// through errors.Is (see the kinds above).
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("RPC error %d: %s", e.Code, e.Message)
}

// Is classifies the node's error code into a kind.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrTransient:
		return e.Code == CodeInWarmup || e.Code == CodeClientInInitialDownload
	case ErrAlreadyInChain:
		return e.Code == CodeTransactionAlreadyInChain
	case ErrPermanent:
		return e.Code != CodeInWarmup && e.Code != CodeClientInInitialDownload &&
			e.Code != CodeTransactionAlreadyInChain
	}
	return false
}

// transportError is a failure to complete the HTTP exchange. Before the
// request was written it is transient; a broadcast whose reply was lost is
// reported by Broadcast as ErrUnknownOutcome instead.
type transportError struct {
	err error
}

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }
func (e *transportError) Is(target error) bool {
	return target == ErrTransient
}

// SetTimeout replaces the per-request HTTP timeout (default 30 s).
func (c *Client) SetTimeout(d time.Duration) { c.client.Timeout = d }

// Call sends a JSON-RPC request and returns the raw result.
func (c *Client) Call(method string, params ...interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{}
	}

	req := rpcRequest{
		JSONRPC: "1.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.SetBasicAuth(c.user, c.password)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("send request: %w", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("read response: %w", err)}
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		// A body that is not JSON-RPC is a proxy or auth page, not the node.
		// Keep it short: this string travels into logs and alerts.
		return nil, &transportError{err: fmt.Errorf("parse response (status %d): %w; body: %.200s", resp.StatusCode, err, string(respBody))}
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	return rpcResp.Result, nil
}

// SendRawTransaction broadcasts a signed transaction to the network.
// Returns the transaction ID on success.
//
// A transport failure here is reported as ErrUnknownOutcome, not as a
// rejection: the node may have accepted and relayed the transaction before
// the reply was lost, and a caller that rebuilds with other inputs pays the
// recipient twice. Prefer Broadcast, which resolves that case against the
// node using the transaction id the builder already knows.
func (c *Client) SendRawTransaction(rawTxHex string) (string, error) {
	result, err := c.Call("sendrawtransaction", rawTxHex)
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			return "", fmt.Errorf("sendrawtransaction: %w: %v", ErrUnknownOutcome, err)
		}
		return "", fmt.Errorf("sendrawtransaction: %w", err)
	}

	var txid string
	if err := json.Unmarshal(result, &txid); err != nil {
		return "", fmt.Errorf("parse txid: %w", err)
	}
	return txid, nil
}

// Broadcast sends a signed transaction whose id the caller has already
// computed (tx.Transaction.TxID) and returns only once the outcome is known.
//
//   - Accepted, or already in the mempool: returns txid, nil.
//   - Already mined (node code -27): returns txid, nil. Re-sending the same
//     bytes is idempotent; this is the normal reply to a retried broadcast.
//   - Rejected by the node: returns the node's error (ErrPermanent).
//   - Reply lost: the node is asked whether it knows txid (getrawtransaction,
//     then gettxout on output 0). Found means accepted. Not found is still
//     not proof of rejection, so ErrUnknownOutcome is returned and the caller
//     must retry THIS transaction, never build another one for the same
//     withdrawal.
//
// The txid must match the transaction; a mismatch is refused before sending.
func (c *Client) Broadcast(rawTxHex, txid string) (string, error) {
	if txid == "" {
		return "", fmt.Errorf("broadcast: %w: txid is required", ErrPermanent)
	}
	result, err := c.Call("sendrawtransaction", rawTxHex)
	if err == nil {
		var got string
		if err := json.Unmarshal(result, &got); err != nil {
			return "", fmt.Errorf("broadcast: parse txid: %w", err)
		}
		if got != txid {
			return got, fmt.Errorf("broadcast: %w: node returned txid %s for a transaction the caller computed as %s", ErrPermanent, got, txid)
		}
		return txid, nil
	}
	if errors.Is(err, ErrAlreadyInChain) {
		return txid, nil
	}
	var te *transportError
	if !errors.As(err, &te) {
		return "", fmt.Errorf("broadcast: %w", err)
	}
	// Reply lost. Resolve against the node before reporting anything.
	known, lookupErr := c.knowsTransaction(txid)
	if lookupErr == nil && known {
		return txid, nil
	}
	return "", fmt.Errorf("broadcast of %s: %w: %v", txid, ErrUnknownOutcome, err)
}

// knowsTransaction reports whether the node has txid in its mempool or chain.
func (c *Client) knowsTransaction(txid string) (bool, error) {
	if _, err := c.Call("getrawtransaction", txid); err == nil {
		return true, nil
	} else if errors.Is(err, ErrTransient) {
		return false, err
	}
	// Without -txindex a mined transaction is not served by getrawtransaction;
	// its own first output is, unless already spent.
	out, err := c.GetTxOut(txid, 0, true)
	if err != nil {
		return false, err
	}
	return out != nil, nil
}

// GetTxOut queries the UTXO set for a specific output.
// Returns nil if the output is not in the node's UTXO set.
//
// This is Defense 11: gettxout pre-verification catches stale UTXOs
// BEFORE signing. A nil result means "spent" ONLY on a node that has caught
// up; a node in initial block download or behind its headers has not seen
// recent outputs yet. Call RequireSynced first, as VerifyAndFilterUTXOs does.
func (c *Client) GetTxOut(txid string, vout uint32, includeMempool bool) (*TxOut, error) {
	result, err := c.Call("gettxout", txid, vout, includeMempool)
	if err != nil {
		return nil, fmt.Errorf("gettxout: %w", err)
	}

	// null result means the output is spent
	if string(result) == "null" {
		return nil, nil
	}

	var txout TxOut
	if err := json.Unmarshal(result, &txout); err != nil {
		return nil, fmt.Errorf("parse gettxout: %w", err)
	}
	return &txout, nil
}

// TxOut represents a gettxout response.
type TxOut struct {
	BestBlock     string       `json:"bestblock"`
	Confirmations int64        `json:"confirmations"`
	Value         float64      `json:"value"`
	ScriptPubKey  ScriptPubKey `json:"scriptPubKey"`
	Coinbase      bool         `json:"coinbase"`
	AssetType     uint8        `json:"assettype"` // RC7+: 0=SOQ, 1=USDSOQ
}

// ValueShors returns the output value in shors. The node reports SOQ as a
// JSON number; rounding to the nearest shor is exact for every value the
// node can produce.
func (o *TxOut) ValueShors() int64 {
	return int64(math.Round(o.Value * float64(types.ShorsPerSOQ)))
}

// ScriptPubKey contains the output script details.
type ScriptPubKey struct {
	ASM     string `json:"asm"`
	Hex     string `json:"hex"`
	Type    string `json:"type"`
	Address string `json:"address,omitempty"`
}

// GetBlockCount returns the current chain tip height.
func (c *Client) GetBlockCount() (int64, error) {
	result, err := c.Call("getblockcount")
	if err != nil {
		return 0, fmt.Errorf("getblockcount: %w", err)
	}

	var height int64
	if err := json.Unmarshal(result, &height); err != nil {
		return 0, fmt.Errorf("parse height: %w", err)
	}
	return height, nil
}

// EstimateSmartFee returns the estimated fee rate in SOQ/kB for a target
// number of confirmation blocks.
func (c *Client) EstimateSmartFee(confTarget int) (float64, error) {
	result, err := c.Call("estimatesmartfee", confTarget)
	if err != nil {
		return 0, fmt.Errorf("estimatesmartfee: %w", err)
	}

	var resp struct {
		FeeRate float64  `json:"feerate"`
		Errors  []string `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return 0, fmt.Errorf("parse fee estimate: %w", err)
	}

	if resp.FeeRate <= 0 {
		// Fallback: 0.01 SOQ/kB (generous for Soqucoin)
		return 0.01, nil
	}
	return resp.FeeRate, nil
}

// DecodeRawTransaction parses a raw transaction hex string.
func (c *Client) DecodeRawTransaction(rawTxHex string) (json.RawMessage, error) {
	result, err := c.Call("decoderawtransaction", rawTxHex)
	if err != nil {
		return nil, fmt.Errorf("decoderawtransaction: %w", err)
	}
	return result, nil
}

// GetBlockHash returns the block hash for a given height.
func (c *Client) GetBlockHash(height int64) (string, error) {
	result, err := c.Call("getblockhash", height)
	if err != nil {
		return "", fmt.Errorf("getblockhash: %w", err)
	}

	var hash string
	if err := json.Unmarshal(result, &hash); err != nil {
		return "", fmt.Errorf("parse blockhash: %w", err)
	}
	return hash, nil
}

// GetBlock returns the full block data for a given hash.
// verbosity: 0=hex, 1=object, 2=object+tx details
func (c *Client) GetBlock(hash string, verbosity int) (json.RawMessage, error) {
	result, err := c.Call("getblock", hash, verbosity)
	if err != nil {
		return nil, fmt.Errorf("getblock: %w", err)
	}
	return result, nil
}

// GetBlockchainInfo returns chain state info (chain name, blocks, headers, etc.)
func (c *Client) GetBlockchainInfo() (*BlockchainInfo, error) {
	result, err := c.Call("getblockchaininfo")
	if err != nil {
		return nil, fmt.Errorf("getblockchaininfo: %w", err)
	}

	var info BlockchainInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("parse blockchain info: %w", err)
	}
	return &info, nil
}

// BlockchainInfo contains chain state information.
type BlockchainInfo struct {
	Chain         string  `json:"chain"`
	Blocks        int64   `json:"blocks"`
	Headers       int64   `json:"headers"`
	BestBlockHash string  `json:"bestblockhash"`
	Difficulty    float64 `json:"difficulty"`
	MedianTime    int64   `json:"mediantime"`
	InitialSync   bool    `json:"initialblockdownload"`
}

// RequireSynced returns ErrNodeSyncing (a transient error) unless the node
// is out of initial block download and its block height has caught up with
// its header height. Until then the node's UTXO set is incomplete and a nil
// gettxout is not evidence of a spend.
func (c *Client) RequireSynced() error {
	info, err := c.GetBlockchainInfo()
	if err != nil {
		return err
	}
	if info.InitialSync || info.Headers > info.Blocks {
		return fmt.Errorf("%w (blocks %d, headers %d, initialblockdownload %v)",
			ErrNodeSyncing, info.Blocks, info.Headers, info.InitialSync)
	}
	return nil
}

// VerifyUTXO checks if a UTXO exists on-chain using gettxout (Defense 11).
// Returns (exists, assetType, error). If exists is false, the UTXO is stale.
//
// This is the critical pre-signing check that prevents stale UTXO failures.
// Always call this for each UTXO before building a transaction.
func (c *Client) VerifyUTXO(txid string, vout uint32) (exists bool, assetType uint8, err error) {
	txout, err := c.GetTxOut(txid, vout, true)
	if err != nil {
		return false, 0, err
	}
	if txout == nil {
		return false, 0, nil // Spent
	}
	return true, txout.AssetType, nil
}

// VerifyAndFilterUTXOs runs Defense 11 on a slice of UTXOs, returning only
// those verified as still unspent on-chain.
//
// For each UTXO that fails verification:
//   - It's removed from the result
//   - The evictFn callback is called (if provided) to remove it from the cache
//
// This is the production-hardened pattern from soq-signer/signer.go.
func (c *Client) VerifyAndFilterUTXOs(
	utxos []types.UTXO,
	evictFn func(txid string, vout uint32),
	setAssetTypeFn func(txid string, vout uint32, assetType uint8),
) ([]types.UTXO, error) {
	if len(utxos) == 0 {
		return nil, nil
	}
	// Eviction is only sound on a node that has caught up: on a syncing node a
	// nil gettxout means "not seen yet", and evicting on it would drain the
	// cache of live outputs and stall withdrawals with "no spendable UTXOs".
	if err := c.RequireSynced(); err != nil {
		return nil, err
	}

	var verified []types.UTXO

	for _, u := range utxos {
		txout, err := c.GetTxOut(u.TxID, u.Vout, true)
		if err != nil {
			return nil, fmt.Errorf("verify UTXO %s:%d: %w", shortID(u.TxID, 12), u.Vout, err)
		}
		if txout == nil {
			log.Printf("[rpc] Defense 11: UTXO %s:%d is STALE (gettxout=null), skipping", shortID(u.TxID, 12), u.Vout)
			if evictFn != nil {
				evictFn(u.TxID, u.Vout)
			}
			continue
		}
		// An immature coinbase output is real but not yet spendable
		// (consensus: nCoinbaseMaturity). Keep it in the cache, leave it out
		// of this selection.
		if txout.Coinbase && txout.Confirmations < types.CoinbaseMaturity {
			log.Printf("[rpc] UTXO %s:%d is an immature coinbase (%d of %d confirmations), skipping",
				shortID(u.TxID, 12), u.Vout, txout.Confirmations, types.CoinbaseMaturity)
			continue
		}

		// Stamp asset type from gettxout response
		if setAssetTypeFn != nil {
			setAssetTypeFn(u.TxID, u.Vout, txout.AssetType)
		}
		u.AssetType = txout.AssetType

		verified = append(verified, u)
	}

	return verified, nil
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
