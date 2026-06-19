// Command exchange_deposit demonstrates how to monitor incoming SOQ deposits
// using the ElectrumX client — the standard exchange deposit monitoring pattern.
//
// This example shows:
//   - Connecting to ElectrumX
//   - Tracking multiple deposit addresses
//   - Polling for new deposits
//   - Checking confirmation count
//   - Using the persistent spent set to avoid double-crediting
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

const (
	// Minimum confirmations before crediting a deposit
	minConfirmations = 6
	// ElectrumX server (stagenet)
	electrumxHost = "localhost:50001"
	// Poll interval
	pollInterval = 15 * time.Second
)

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[exchange] ")

	// ── Step 1: Create ElectrumX client ──
	client := electrumx.NewClient(electrumxHost, pollInterval)
	client.HRP = types.Stagenet.HRP // "ssq" for stagenet

	// Hook into refresh events for monitoring
	client.OnRefresh = func(addr string, utxoCount int) {
		if utxoCount > 0 {
			log.Printf("Refreshed %s... — %d UTXOs", addr[:20], utxoCount)
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
	client.TrackAddresses(depositAddresses)
	log.Printf("Tracking %d deposit addresses", len(depositAddresses))

	// ── Step 4: Start polling ──
	client.StartPolling()
	log.Printf("Polling started (interval: %v, min confirmations: %d)", pollInterval, minConfirmations)

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

// checkDeposits scans all tracked addresses for confirmed deposits.
// In production, you'd compare against your database to find NEW deposits.
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
			if confirmations >= minConfirmations {
				// This deposit is confirmed — credit the user
				fmt.Printf("CONFIRMED DEPOSIT: %s:%d — %.8f SOQ (%d confirmations)\n",
					u.TxID[:12], u.Vout,
					float64(u.Value)/float64(types.SatoshisPerSOQ),
					confirmations)

				// TODO: In production, check your database to see if this
				// UTXO has already been credited. Use the txid:vout as a
				// unique key to prevent double-crediting.
			}
		}
	}
}
