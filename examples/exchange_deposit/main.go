// Command exchange_deposit demonstrates how to monitor incoming SOQ deposits
// using the ElectrumX client — the standard exchange deposit monitoring pattern.
//
// This example shows:
//   - Connecting to ElectrumX
//   - Tracking multiple deposit addresses
//   - Polling for new deposits
//   - Applying a confirmation threshold anchored to Soqucoin's finality horizon
//   - Not crediting the same deposit twice
//
// Confirmation thresholds follow the table in docs/EXCHANGE_INTEGRATION.md and are
// anchored to Soqucoin's own finality horizon rather than to a threshold carried
// over from another chain. Soqucoin targets 1-minute blocks and sets
// nMaxReorgDepth = 288, so a Bitcoin-style 6 confirmations would credit about six
// minutes into a window in which nodes still accept a reorganisation.
//
// Usage:
//
//	go run ./examples/exchange_deposit/
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Confirmation thresholds, anchored to consensus rather than to convention.
// nMaxReorgDepth is 288 blocks on mainnet and stagenet: a node will not
// reorganise deeper than that, so 288 is the point at which the chain itself
// treats a deposit as settled. Below it you are taking a position the chain has
// not taken. See docs/EXCHANGE_INTEGRATION.md for the full table and reasoning.
const (
	finalityDepth = 288 // nMaxReorgDepth, ~4.8 h at the 1-minute target
	mediumDepth   = 120 // ~2 h
	smallDepth    = 30  // ~30 min

	// Value boundaries between those tiers, in satoshis. These are illustrative.
	// Set them against your own value at risk; the table is a floor, not a ceiling.
	smallMax  = 1_000 * types.SatoshisPerSOQ
	mediumMax = 50_000 * types.SatoshisPerSOQ
)

const (
	// ElectrumX server (stagenet)
	electrumxHost = "localhost:50001"
	// Poll interval
	pollInterval = 15 * time.Second
)

// requiredConfirmations returns the depth at which a deposit of this size may be
// credited. Larger amounts wait longer, so low-latency credit on small deposits
// does not force the same risk onto large ones.
func requiredConfirmations(valueSats int64) int64 {
	switch {
	case valueSats <= smallMax:
		return smallDepth
	case valueSats <= mediumMax:
		return mediumDepth
	default:
		return finalityDepth
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[exchange] ")

	// ── Step 1: Create ElectrumX client ──
	client := electrumx.NewClient(electrumxHost, pollInterval)
	client.HRP = types.Stagenet.HRP // "ssq" for stagenet

	// Hook into refresh events for monitoring
	client.OnRefresh = func(addr string, utxoCount int) {
		if utxoCount > 0 {
			log.Printf("Refreshed %s... — %d UTXOs", shortID(addr, 20), utxoCount)
		}
	}

	// ── Step 2: Connect to ElectrumX ──
	if err := client.Connect(); err != nil {
		log.Fatalf("Failed to connect to ElectrumX at %s: %v", electrumxHost, err)
	}
	defer client.Stop()
	log.Printf("Connected to ElectrumX at %s", electrumxHost)

	// ── Step 3: Track deposit addresses ──
	// In production, these come from your database (one per user).
	depositAddresses := []string{
		"ssq1p...", // User 1's deposit address
		"ssq1p...", // User 2's deposit address
	}
	if err := client.TrackAddresses(depositAddresses); err != nil {
		log.Fatalf("track deposit addresses: %v", err)
	}
	log.Printf("Tracking %d deposit addresses", len(depositAddresses))

	// ── Step 4: Start polling ──
	client.StartPolling()
	log.Printf("Polling started (interval: %v; crediting at %d/%d/%d confirmations by size)",
		pollInterval, smallDepth, mediumDepth, finalityDepth)

	// ── Step 5: Periodically check for confirmed deposits ──
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for range ticker.C {
			checkDeposits(client, depositAddresses)
		}
	}()

	// ── Step 6: Wait for shutdown signal ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
}

// credited records which UTXOs have already been credited, keyed by txid:vout.
// In a real system this belongs in your database, not in process memory: this map
// is lost on restart, and every deposit would be credited a second time.
var credited = map[string]bool{}

// checkDeposits scans all tracked addresses and credits any deposit that has
// reached the confirmation depth required for its size.
func checkDeposits(client *electrumx.Client, addresses []string) {
	tipHeight, err := client.GetTip()
	if err != nil {
		log.Printf("WARNING: cannot get tip height: %v", err)
		return
	}

	for _, addr := range addresses {
		utxos := client.GetUTXOs(addr)
		for _, u := range utxos {
			if u.Height == 0 {
				continue // Unconfirmed — skip
			}
			if u.AssetType != types.AssetTypeSOQ {
				continue // Not native SOQ
			}

			confirmations := tipHeight - u.Height + 1
			required := requiredConfirmations(u.Value)
			if confirmations < required {
				continue // Show as pending if you like, but do not credit
			}

			// Credit exactly once. txid:vout is unique for the life of the
			// chain, which makes it the right idempotency key: a reorg, a
			// restart, or an overlapping poll cycle will all re-present the
			// same UTXO, and this example polls every address on every tick.
			//
			// This map is in-memory purely to keep the example self-contained.
			// Use your database, and record the credit in the same transaction
			// that moves the user's balance, or a crash between the two will
			// either double-credit or silently drop a deposit.
			key := fmt.Sprintf("%s:%d", u.TxID, u.Vout)
			if credited[key] {
				continue
			}
			credited[key] = true

			fmt.Printf("CREDIT %s:%d, %.8f SOQ (%d confirmations, %d required)\n",
				shortID(u.TxID, 12), u.Vout,
				float64(u.Value)/float64(types.SatoshisPerSOQ),
				confirmations, required)
		}
	}
}

// shortID truncates an identifier for display without panicking on short input.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
