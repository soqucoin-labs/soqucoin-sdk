// Command pool_payout demonstrates a production-grade batch payout system
// using the SDK's resilience patterns — the same architecture that powers
// soqupool's live payouts.
//
// This example shows:
//   - Circuit breaker to prevent cascading failures
//   - Persistent spent set to prevent UTXO re-selection
//   - Defense 11 (gettxout pre-verification) to catch stale UTXOs
//   - Webhook alerting for operational monitoring
//   - Batch payment with back-to-back transactions
//
// Usage:
//
//	go run ./examples/pool_payout/ \
//	  -rpc-url http://127.0.0.1:19332 \
//	  -rpc-user user -rpc-pass pass \
//	  -electrumx localhost:50001
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/resilience"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

func main() {
	// ── CLI flags ──
	rpcURL := flag.String("rpc-url", "http://127.0.0.1:19332", "soqucoind RPC URL")
	rpcUser := flag.String("rpc-user", "rpcuser", "RPC username")
	rpcPass := flag.String("rpc-pass", "rpcpassword", "RPC password")
	elxHost := flag.String("electrumx", "localhost:50001", "ElectrumX host:port")
	webhookURL := flag.String("webhook", "", "Slack webhook URL for alerts (optional)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[pool-payout] ")

	// ── Step 1: Initialize components ──
	rpcClient := rpc.NewClient(*rpcURL, *rpcUser, *rpcPass)
	elxClient := electrumx.NewClient(*elxHost, 15*time.Second)
	elxClient.HRP = types.Stagenet.HRP

	// Persistent spent set — survives restarts
	spentSet := utxo.NewSpentSet("/tmp/pool_payout_spent_set.json")
	selector := utxo.NewCoinSelector(spentSet)

	// Circuit breaker: trip after 3 failures, 15 min cooldown
	cb := resilience.NewCircuitBreaker(3, 15*time.Minute)

	// Webhook alerter (optional)
	alerter := resilience.NewAlerter(*webhookURL)
	alerter.WireToCircuitBreaker(cb)

	// ── Step 2: Connect to ElectrumX ──
	if err := elxClient.Connect(); err != nil {
		log.Fatalf("ElectrumX connect failed: %v", err)
	}
	defer elxClient.Stop()

	// Track the pool's payout address
	poolAddress := "ssq1p..." // Your pool's payout address
	elxClient.TrackAddresses([]string{poolAddress})

	// Initial UTXO refresh
	if err := elxClient.RefreshAll(); err != nil {
		log.Fatalf("Initial UTXO refresh failed: %v", err)
	}

	// ── Step 3: Build payout list ──
	// In production, this comes from your pool's balance database
	payouts := []Payout{
		{Address: "ssq1p...", Amount: 5000_00000000},  // 5000 SOQ to miner A
		{Address: "ssq1p...", Amount: 2500_00000000},  // 2500 SOQ to miner B
		{Address: "ssq1p...", Amount: 1200_00000000},  // 1200 SOQ to miner C
	}

	// ── Step 4: Execute payouts with circuit breaker ──
	for i, payout := range payouts {
		log.Printf("Processing payout %d/%d: %s → %.4f SOQ",
			i+1, len(payouts), payout.Address[:20], float64(payout.Amount)/1e8)

		// Check circuit breaker BEFORE each payout
		if err := cb.Allow(); err != nil {
			log.Printf("Circuit breaker blocked payout: %v", err)
			log.Printf("Remaining payouts will be retried next cycle")
			os.Exit(1)
		}

		err := executePayout(rpcClient, elxClient, selector, spentSet, payout, poolAddress)
		if err != nil {
			log.Printf("Payout failed: %v", err)
			cb.RecordFailure(err)
			continue
		}

		cb.RecordSuccess()
		log.Printf("Payout %d/%d complete", i+1, len(payouts))
	}

	log.Println("All payouts processed.")
}

// Payout represents a single payout to a miner.
type Payout struct {
	Address string
	Amount  int64 // Satoshis
}

// executePayout performs a single payout using production-hardened patterns.
func executePayout(
	rpcClient *rpc.Client,
	elxClient *electrumx.Client,
	selector *utxo.CoinSelector,
	spentSet *utxo.SpentSet,
	payout Payout,
	changeAddr string,
) error {
	// Step 1: Get chain tip
	tipHeight, err := rpcClient.GetBlockCount()
	if err != nil {
		return fmt.Errorf("get block count: %w", err)
	}

	// Step 2: Get all UTXOs
	allUTXOs := elxClient.GetAllUTXOs()

	// Step 3: Coin selection (largest-first, with spent set filtering)
	fee := int64(100_000) // 0.001 SOQ fee (generous for PQ signatures)
	targetAmount := payout.Amount + fee

	selected, total, err := selector.SelectUTXOs(allUTXOs, targetAmount, 1, tipHeight, nil)
	if err != nil {
		return fmt.Errorf("coin selection: %w", err)
	}
	log.Printf("  Selected %d UTXOs totaling %.4f SOQ", len(selected), float64(total)/1e8)

	// Step 4: Defense 11 — verify each UTXO is still unspent on-chain
	verified, err := rpcClient.VerifyAndFilterUTXOs(
		selected,
		elxClient.EvictUTXO,       // Remove stale UTXOs from cache
		elxClient.SetAssetType,     // Stamp asset type from gettxout
	)
	if err != nil {
		return fmt.Errorf("UTXO verification: %w", err)
	}
	if len(verified) < len(selected) {
		log.Printf("  Defense 11 filtered %d stale UTXOs", len(selected)-len(verified))
	}

	// Step 5: Build and sign the transaction
	// (In production, use tx.Build() with your keystore)
	log.Printf("  Building TX: %d inputs → 1 output + change", len(verified))
	log.Printf("  Payment: %.4f SOQ to %s...", float64(payout.Amount)/1e8, payout.Address[:20])

	changeAmount := total - payout.Amount - fee
	if changeAmount > 0 {
		log.Printf("  Change: %.4f SOQ to %s...", float64(changeAmount)/1e8, changeAddr[:20])
	}

	// NOTE: Transaction building and signing requires the tx package and
	// your keystore. This example shows the flow pattern — substitute your
	// actual tx.Build() call here.
	rawTxHex := "..." // tx.Build(verified, outputs, changeAddr, fee, keystore)
	_ = rawTxHex

	// Step 6: Broadcast
	// txid, err := rpcClient.SendRawTransaction(rawTxHex)

	// Step 7: Mark spent in persistent set (prevents re-selection)
	txid := "simulated_txid_example" // Replace with actual txid
	spentSet.MarkBroadcast(verified, txid)

	// Step 8: Inject change UTXO for immediate availability (Defense 13)
	if changeAmount > 0 {
		elxClient.AddChangeUTXO(txid, 1, changeAmount, changeAddr)
	}

	log.Printf("  Broadcast TX %s", txid[:12])
	return nil
}
