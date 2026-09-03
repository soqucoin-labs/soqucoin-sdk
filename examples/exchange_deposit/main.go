// Command exchange_deposit demonstrates how an exchange credits incoming SOQ
// deposits: indexer for discovery, the exchange's own node for the verdict.
//
// This example shows:
//   - Connecting to ElectrumX over TLS and tracking deposit addresses
//   - Crediting only what your own node confirms (value, destination, depth)
//   - Pausing while the node is syncing or the indexer is stale
//   - Applying a confirmation policy anchored to Soqucoin's finality horizon
//   - Re-verifying credited deposits until they are final, and alarming if
//     one disappears
//
// Confirmation thresholds follow the table in docs/EXCHANGE_INTEGRATION.md and
// are anchored to Soqucoin's own finality horizon rather than to a threshold
// carried over from another chain. Soqucoin targets 1-minute blocks and sets
// nMaxReorgDepth = 288, so a Bitcoin-style 6 confirmations would credit about
// six minutes into a window in which nodes still accept a reorganisation.
//
// Usage:
//
//	go run ./examples/exchange_deposit/
package main

import (
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Confirmation thresholds, anchored to consensus rather than to convention.
// types.MaxReorgDepth (288) is the point at which the chain itself treats a
// deposit as settled. Below it you are taking a position the chain has not
// taken. See docs/EXCHANGE_INTEGRATION.md for the full table and reasoning.
const (
	mediumDepth = 120 // ~2 h
	smallDepth  = 30  // ~30 min
	// Value boundaries between those tiers, in shors. These are illustrative.
	// Set them against your own value at risk; the table is a floor, not a ceiling.
	smallMax  = 1_000 * types.ShorsPerSOQ
	mediumMax = 50_000 * types.ShorsPerSOQ
)

const (
	electrumxHost = "localhost:50002"        // your indexer (TLS port)
	nodeURL       = "http://127.0.0.1:28332" // your soqucoind RPC (stagenet port shown)
	pollInterval  = 15 * time.Second
)

// requiredConfirmations returns the depth at which a deposit of this size may be
// credited. Larger amounts wait longer, so low-latency credit on small deposits
// does not force the same risk onto large ones.
func requiredConfirmations(value int64) int64 {
	switch {
	case value <= smallMax:
		return smallDepth
	case value <= mediumMax:
		return mediumDepth
	default:
		return types.MaxReorgDepth
	}
}

// memLedger stands in for your database, purely to keep the example
// self-contained. It is lost on restart, and every deposit would then be
// credited a second time. In production, record the credit in the same
// database transaction that moves the user's balance, keyed by txid:vout, or
// a crash between the two will either double-credit or silently drop a deposit.
type memLedger struct {
	mu       sync.Mutex
	credited map[string]deposit.Deposit
	final    map[string]bool
}

func key(txid string, vout uint32) string { return txid + ":" + string(rune('0'+vout)) }

func (l *memLedger) Credit(d deposit.Deposit) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credited[key(d.TxID, d.Vout)] = d
	log.Printf("CREDIT %s:%d, %.8f SOQ to %s... (%d confirmations, node-verified)",
		shortID(d.TxID, 12), d.Vout, float64(d.Value)/float64(types.ShorsPerSOQ), shortID(d.Address, 20), d.Confirmations)
	return nil
}

func (l *memLedger) IsCredited(txid string, vout uint32) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.credited[key(txid, vout)]
	return ok, nil
}

func (l *memLedger) Pending() ([]deposit.Deposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []deposit.Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}

func (l *memLedger) MarkFinal(txid string, vout uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.final[key(txid, vout)] = true
	return nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[exchange] ")

	// In production, these come from your database (one per user).
	depositAddresses := []string{
		"ssq1p...", // User 1's deposit address
		"ssq1p...", // User 2's deposit address
	}

	// ── Indexer: discovery only ──
	// The network is inferred from the addresses; mixed or undecodable
	// addresses are refused here rather than silently never refreshed.
	elx := electrumx.NewClient(electrumxHost, pollInterval)
	elx.UseTLS() // the server sees every address you track; keep that off the wire in the clear
	if err := elx.TrackAddresses(depositAddresses); err != nil {
		log.Fatalf("track deposit addresses: %v", err)
	}
	if err := elx.Connect(); err != nil { // verifies the server's genesis hash too
		log.Fatalf("connect to ElectrumX at %s: %v", electrumxHost, err)
	}
	defer elx.Stop()
	elx.StartPolling()
	log.Printf("tracking %d deposit addresses via %s", len(depositAddresses), electrumxHost)

	// ── Your node: the verdict ──
	node := rpc.NewClient(nodeURL, os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"))

	ledger := &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{}}
	monitor := &deposit.Monitor{
		Cache:     elx,
		Node:      node,
		Ledger:    ledger,
		Addresses: func() []string { return depositAddresses },
		Required:  requiredConfirmations,
		OnAlert: func(kind deposit.AlertKind, msg string) {
			// Every alert is a human's problem: page on it.
			log.Printf("ALERT %s: %s", kind, msg)
		},
	}

	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for range ticker.C {
			credited, err := monitor.Scan()
			if err != nil {
				// deposit.ErrPaused while the node is syncing or the indexer is
				// stale; nothing was credited. Node errors are returned as-is.
				log.Printf("scan: %v", err)
				continue
			}
			if len(credited) > 0 {
				log.Printf("credited %d deposit(s) this pass", len(credited))
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("stopping")
}

// shortID truncates an identifier for display without panicking on short input.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
