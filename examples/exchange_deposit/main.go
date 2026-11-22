// Command exchange_deposit demonstrates how an exchange credits incoming SOQ
// deposits: indexer for discovery, the exchange's own node for the verdict.
//
// This example shows:
//   - Connecting to ElectrumX over TLS and tracking deposit addresses
//   - Crediting only what your own node confirms (value, destination, depth)
//   - Pausing while the node is syncing or the indexer is stale
//   - Applying a confirmation policy anchored to Soqucoin's finality horizon,
//     with the safeguards that go with its two lower tiers (deposit.Tiers)
//   - Re-verifying credited deposits until they are final, and alarming if
//     one disappears
//
// Confirmation thresholds follow the table in docs/EXCHANGE_INTEGRATION.md:
// three tiers by the value of the deposit. Soqucoin targets 1-minute blocks
// and sets nMaxReorgDepth = 288, the depth from which a synced node refuses a
// branch that would remove a deposit. Below it a reorganisation remains
// possible, so the two lower tiers are bounded by value.
//
// Usage:
//
//	go run ./examples/exchange_deposit/
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Confirmation tiers by the value of the deposit: deposit.SmallDepth (10)
// below the first value line, deposit.StandardDepth (30) below the second and
// types.MaxReorgDepth (288) at or above it, the point at which the chain
// itself treats a deposit as settled. Below it a reorganisation remains
// possible, and the two value lines bound what one could cost. See
// docs/EXCHANGE_INTEGRATION.md for the table, the safeguards that go with the
// two lower tiers, and the reasoning.
//
// The three figures are in shors and are placeholders. Set the two value lines
// to the amounts of SOQ worth about 100 USD and about 1,000 USD at your own
// price, or to the loss you accept on one deposit in each tier, and the
// ceiling to about 10,000 USD. Move them with Tiers.SetLines when the price
// moves.
var lines = deposit.TierLines{
	SmallBelow: 500_000 * types.ShorsPerSOQ,
	LargeFrom:  5_000_000 * types.ShorsPerSOQ,
	// The most that may be credited below 288 confirmations and not yet be
	// past the horizon, across every deposit.
	Ceiling: 50_000_000 * types.ShorsPerSOQ,
}

const (
	electrumxHost = "localhost:50002"        // your indexer (TLS port)
	nodeURL       = "http://127.0.0.1:28332" // your soqucoind RPC (stagenet port shown)
	// The indexer pushes changes as they happen; the reconcile is the full
	// pass over every address that covers a notification it never sent.
	reconcileInterval = 10 * time.Minute
	// How often the Monitor reads the cache and credits what the node
	// confirms. A pushed deposit is in the cache within milliseconds; this is
	// the credit latency you choose.
	scanInterval = 15 * time.Second
)

// memLedger stands in for your database, purely to keep the example
// self-contained. It is lost on restart, and every deposit would then be
// credited a second time. In production, record the credit in the same
// database transaction that moves the user's balance, keyed by txid:vout, or
// a crash between the two will either double-credit or silently drop a deposit.
//
// The context is Scan's. A database-backed ledger passes it to every query.
type memLedger struct {
	mu       sync.Mutex
	credited map[string]deposit.Deposit
	final    map[string]bool
	log      *slog.Logger
}

func key(txid string, vout uint32) string { return txid + ":" + string(rune('0'+vout)) }

func (l *memLedger) Credit(_ context.Context, d deposit.Deposit) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credited[key(d.TxID, d.Vout)] = d
	l.log.Info("CREDIT", "txid", d.TxID, "vout", d.Vout, "shors", d.Value, "address", d.Address, "confirmations", d.Confirmations)
	return nil
}

func (l *memLedger) IsCredited(_ context.Context, txid string, vout uint32) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.credited[key(txid, vout)]
	return ok, nil
}

// Pending is what Monitor re-verifies on the node until it is final. Leave
// out any output you spent yourself, such as a deposit swept to the hot
// wallet before it was final: the node reports a swept output and a
// reorganised-away one the same way, gone, and Monitor would alarm the sweep
// as a vanished deposit on every scan. This ledger never sweeps.
func (l *memLedger) Pending(context.Context) ([]deposit.Deposit, error) {
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

func (l *memLedger) MarkFinal(_ context.Context, txid string, vout uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.final[key(txid, vout)] = true
	return nil
}

// CreditedSince is what the ceiling counts (deposit.Book): every deposit
// credited in a block at or above the height, as it was credited. Unlike
// Pending it keeps an output you have swept, which a reorganisation can still
// remove. In production this is one query on the height column.
func (l *memLedger) CreditedSince(_ context.Context, height int64) ([]deposit.Deposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []deposit.Deposit
	for _, d := range l.credited {
		if d.Height >= height {
			out = append(out, d)
		}
	}
	return out, nil
}

func main() {
	// One logger for the program and for every SDK component it builds; the
	// SDK logs nothing unless it is given one.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fatal := func(msg string, err error) {
		logger.Error(msg, "err", err)
		os.Exit(1)
	}

	// The context ends on SIGINT or SIGTERM. Every network call below runs
	// under it, so shutdown stops the polling and the scan loop together.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// In production, these come from your database (one per user).
	depositAddresses := []string{
		"ssq1p...", // User 1's deposit address
		"ssq1p...", // User 2's deposit address
	}

	// ── Indexer: discovery only ──
	// The network is inferred from the addresses; mixed or undecodable
	// addresses are refused here rather than silently never refreshed.
	elx := electrumx.NewClient(electrumxHost, reconcileInterval, logger)
	elx.UseTLS() // the server sees every address you track; keep that off the wire in the clear
	if err := elx.TrackAddresses(depositAddresses); err != nil {
		fatal("track deposit addresses", err)
	}
	if err := elx.Connect(ctx); err != nil { // verifies the server's genesis hash too
		fatal("connect to ElectrumX at "+electrumxHost, err)
	}
	defer elx.Stop()
	elx.Start(ctx) // subscribes every address, refreshes on push, reconciles on the interval
	logger.Info("tracking deposit addresses", "count", len(depositAddresses), "indexer", electrumxHost)

	// ── Your node: the verdict ──
	// The node must serve the chain the deposit addresses belong to;
	// RequireSynced refuses it otherwise, and the chain's coinbase maturity
	// gates mined-to deposits. Regtest shares mainnet's address prefix, so
	// against a regtest node set network = types.Regtest here.
	network, err := address.NetworkOf(depositAddresses[0])
	if err != nil {
		fatal("deposit address network", err)
	}
	node := rpc.NewClient(nodeURL, os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"), logger)
	node.Network = network

	ledger := &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{}, log: logger}

	// The three tiers and their safeguards. Required is the tier of a value.
	// As the Safeguard, the Tiers reads the node and the ledger on every pass
	// and holds a deposit that would pass the ceiling, every deposit below 288
	// confirmations while the chain is unsettled, and every deposit while the
	// node holds a branch with more work than its chain. Set both fields:
	// Required alone is the tiers without their safeguards.
	tiers, err := deposit.NewTiers(node, ledger, lines)
	if err != nil {
		fatal("deposit tiers", err)
	}
	monitor := &deposit.Monitor{
		Cache:     elx,
		Node:      node,
		Network:   network,
		Ledger:    ledger,
		Addresses: func(context.Context) []string { return depositAddresses },
		Required:  tiers.Required,
		Safeguard: tiers,
		OnAlert: func(kind deposit.AlertKind, msg string) {
			// Every alert is a human's problem: page on it.
			logger.Error("ALERT", "kind", string(kind), "msg", msg)
		},
		Logger: logger,
	}

	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	var waiting bool
	for {
		select {
		case <-ctx.Done():
			logger.Info("stopping")
			return
		case <-ticker.C:
		}
		// Each pass is bounded on its own, so one stalled node call cannot
		// hold the loop past the scan interval.
		passCtx, cancel := context.WithTimeout(ctx, scanInterval)
		credited, err := monitor.Scan(passCtx)
		cancel()
		// What the safeguards hold back is a human's problem too: page when
		// the chain turns unsettled or a branch with more work appears, and
		// again when it clears.
		if st := tiers.Status(); (st.Ahead != "" || st.Unsettled != "") != waiting {
			waiting = !waiting
			logger.Error("ALERT", "kind", "deposit_safeguard", "every_deposit_waits", st.Ahead,
				"lower_tiers_wait_for_288", st.Unsettled, "tip", st.Tip, "below_the_horizon_shors", st.Exposed)
		}
		if err != nil {
			// deposit.ErrPaused while the node is syncing or the indexer is
			// stale; nothing was credited. Node errors are returned as-is.
			logger.Warn("scan", "err", err)
			continue
		}
		if len(credited) > 0 {
			logger.Info("credited deposits this pass", "count", len(credited))
		}
	}
}
