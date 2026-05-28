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
	"fmt"
	"io"
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
//   - url: Full URL including port (e.g., "http://127.0.0.1:19332")
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
	Error  *rpcError       `json:"error"`
	ID     int             `json:"id"`
}

// rpcError is a JSON-RPC error.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("RPC error %d: %s", e.Code, e.Message)
}

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
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse response (status %d): %w\nbody: %s", resp.StatusCode, err, string(respBody))
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	return rpcResp.Result, nil
}

// SendRawTransaction broadcasts a signed transaction to the network.
// Returns the transaction ID on success.
func (c *Client) SendRawTransaction(rawTxHex string) (string, error) {
	result, err := c.Call("sendrawtransaction", rawTxHex)
	if err != nil {
		return "", fmt.Errorf("sendrawtransaction: %w", err)
	}

	var txid string
	if err := json.Unmarshal(result, &txid); err != nil {
		return "", fmt.Errorf("parse txid: %w", err)
	}
	return txid, nil
}

// GetTxOut queries the UTXO set for a specific output.
// Returns nil if the output has been spent.
//
// This is Defense 11: gettxout pre-verification catches stale UTXOs
// BEFORE signing. If gettxout returns nil, the UTXO was already spent
// on-chain even though ElectrumX may still report it.
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
		FeeRate float64 `json:"feerate"`
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
	var verified []types.UTXO

	for _, u := range utxos {
		exists, assetType, err := c.VerifyUTXO(u.TxID, u.Vout)
		if err != nil {
			return nil, fmt.Errorf("verify UTXO %s:%d: %w", u.TxID[:12], u.Vout, err)
		}
		if !exists {
			if len(u.TxID) >= 12 {
				fmt.Printf("[rpc] Defense 11: UTXO %s:%d is STALE (gettxout=null), skipping\n", u.TxID[:12], u.Vout)
			}
			if evictFn != nil {
				evictFn(u.TxID, u.Vout)
			}
			continue
		}

		// Stamp asset type from gettxout response
		if setAssetTypeFn != nil {
			setAssetTypeFn(u.TxID, u.Vout, assetType)
		}
		u.AssetType = assetType

		verified = append(verified, u)
	}

	return verified, nil
}
