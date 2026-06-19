// Package soqsigner provides an HTTP client for the soq-signer REST API.
// soq-signer is the out-of-process Dilithium signing service used for SOQ payouts
// because soqucoind runs with disablewallet=1 (PQ keys cannot be in-daemon).
//
// API: POST /api/v1/send {"address": "ssq1...", "amount": <satoshis>, "fee_rate": 10}
// Auth: Bearer token in Authorization header (constant-time comparison server-side)
// Amounts: int64 satoshis (1 SOQ = 100,000,000 satoshis)
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Config holds soq-signer connection settings.
type Config struct {
	URL      string `json:"url"`       // e.g. "http://64.23.129.28:8550"
	APIToken string `json:"api_token"` // Bearer token for auth
	FeeRate  int64  `json:"fee_rate"`  // sat/vB (default 10)
}

// Client is the HTTP client for soq-signer.
type Client struct {
	config     Config
	httpClient *http.Client
}

// SendRequest is the JSON body for POST /api/v1/send.
type SendRequest struct {
	Address     string `json:"address"`
	Amount      int64  `json:"amount"`
	FeeRate     int64  `json:"fee_rate"`
	SubtractFee bool   `json:"subtract_fee,omitempty"`
}

// SendResult is the JSON response from POST /api/v1/send.
type SendResult struct {
	TxID    string `json:"txid"`
	RawTx   string `json:"raw_tx"`
	Fee     int64  `json:"fee"`
	Inputs  int    `json:"inputs"`
	Outputs int    `json:"outputs"`
	Weight  int    `json:"weight_wu"`
	Elapsed string `json:"elapsed"`
}

// ErrorResponse is the JSON error body.
type ErrorResponse struct {
	Error string `json:"error"`
}

// NewClient creates a new soq-signer HTTP client.
func NewClient(cfg Config) *Client {
	if cfg.FeeRate <= 0 {
		cfg.FeeRate = 10 // default 10 sat/vB
	}
	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// HealthCheck verifies soq-signer is running.
func (c *Client) HealthCheck() error {
	resp, err := c.httpClient.Get(c.config.URL + "/health")
	if err != nil {
		return fmt.Errorf("soq-signer health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("soq-signer health check: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Send sends SOQ to a single recipient address.
// amount is in satoshis (1 SOQ = 100,000,000 sat).
func (c *Client) Send(address string, amountSat int64) (string, error) {
	req := SendRequest{
		Address: address,
		Amount:  amountSat,
		FeeRate: c.config.FeeRate,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal send request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.config.URL+"/api/v1/send", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIToken)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("soq-signer send: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		var errResp ErrorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return "", fmt.Errorf("soq-signer send error (HTTP %d): %s", resp.StatusCode, errResp.Error)
		}
		return "", fmt.Errorf("soq-signer send error: HTTP %d, body: %.200s", resp.StatusCode, string(respBody))
	}

	var result SendResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse send response: %w", err)
	}

	return result.TxID, nil
}

// SendManyRequest is the request body for POST /api/v1/sendmany.
type SendManyRequest struct {
	Recipients map[string]int64 `json:"recipients"` // address → amount in satoshis
	FeeRate    int64            `json:"fee_rate"`    // sat/vB (optional, default: 10)
}

// SendMany sends SOQ to multiple recipients in ONE transaction.
// Uses the /api/v1/sendmany endpoint which builds a single TX with multiple outputs,
// eliminating the txn-mempool-conflict issue from sequential individual sends.
// transactions maps address -> amount in SOQ (float64, matching pool balance format).
func (c *Client) SendMany(transactions map[string]float64) (string, error) {
	if len(transactions) == 0 {
		return "", errors.New("no transactions to send")
	}

	if err := c.HealthCheck(); err != nil {
		return "", fmt.Errorf("pre-flight health check: %w", err)
	}

	// Convert float64 SOQ amounts to int64 satoshis
	recipients := make(map[string]int64, len(transactions))
	for address, amountSOQ := range transactions {
		amountSat := int64(amountSOQ * 1e8)
		if amountSat <= 0 {
			log.Printf("[soqsigner] Skipping zero/negative amount for %s: %.8f SOQ", address, amountSOQ)
			continue
		}
		recipients[address] = amountSat
		log.Printf("[soqsigner] Queued %d sat (%.4f SOQ) to %s", amountSat, amountSOQ, address)
	}

	if len(recipients) == 0 {
		return "", errors.New("no valid recipients after filtering")
	}

	req := SendManyRequest{
		Recipients: recipients,
		FeeRate:    c.config.FeeRate,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal sendmany request: %w", err)
	}

	log.Printf("[soqsigner] Sending batch of %d payments via /api/v1/sendmany", len(recipients))

	httpReq, err := http.NewRequest("POST", c.config.URL+"/api/v1/sendmany", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIToken)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("soq-signer sendmany: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		var errResp ErrorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return "", fmt.Errorf("soq-signer sendmany error (HTTP %d): %s", resp.StatusCode, errResp.Error)
		}
		return "", fmt.Errorf("soq-signer sendmany error: HTTP %d, body: %.200s", resp.StatusCode, string(respBody))
	}

	var result SendResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse sendmany response: %w", err)
	}

	log.Printf("[soqsigner] Batch payment sent: txid=%s, %d inputs, %d outputs, elapsed=%s",
		result.TxID, result.Inputs, result.Outputs, result.Elapsed)

	return result.TxID, nil
}
