// Command pool_payout is a working batch payout tool built on the SDK's
// resilience patterns.
//
// It demonstrates:
//   - Circuit breaker to stop a run rather than hammer a failing node
//   - Persistent spent set to prevent UTXO re-selection across restarts
//   - Defense 11 (gettxout pre-verification) to catch stale UTXOs
//   - Webhook alerting for operational monitoring
//   - Build, sign, broadcast and confirm, with the txid checked against the node
//
// The ordering here is deliberate and worth copying: nothing is recorded as spent
// until the node has ACCEPTED the transaction. Marking inputs spent
// optimistically drops them from selection while they are still spendable, and
// recording a payout that did not land is how a pool marks a miner paid without
// paying them. -dry-run stops before any of those effects.
//
// Each payout is one transaction and its change is not spendable until it has
// confirmed and the indexer reports it (the selector takes no output at height
// 0), so a run needs enough confirmed outputs to fund every payout in it; with
// one large output the second payout fails for lack of inputs. Split the hot
// wallet into several outputs ahead of a run, or wait a block between payouts.
//
// Usage:
//
//	export SOQ_KEYSTORE_PASSPHRASE=...
//	go run ./examples/pool_payout/ \
//	  -rpc-url http://127.0.0.1:28332 \
//	  -rpc-user user -rpc-pass pass \
//	  -electrumx localhost:50001 \
//	  -keystore /var/lib/soq/keystore.enc \
//	  -pool-address ssq1p... \
//	  -payouts payouts.json \
//	  -dry-run
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/loopback"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/resilience"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// Payout is a single payment to a miner. Amount is in shors.
type Payout struct {
	Address string `json:"address"`
	Amount  int64  `json:"amount"`
}

func main() {
	rpcURL := flag.String("rpc-url", "http://127.0.0.1:28332", "soqucoind RPC URL")
	rpcUser := flag.String("rpc-user", "rpcuser", "RPC username")
	rpcPass := flag.String("rpc-pass", "rpcpassword", "RPC password")
	elxHost := flag.String("electrumx", "localhost:50001", "ElectrumX host:port")
	elxTLS := flag.Bool("electrumx-tls", false, "Use TLS to reach ElectrumX (required off-localhost)")
	keystorePath := flag.String("keystore", "", "Path to the encrypted keystore (required)")
	poolAddress := flag.String("pool-address", "", "Pool payout address, also receives change (required)")
	payoutsPath := flag.String("payouts", "", "JSON file: [{\"address\":\"...\",\"amount\":123}] (required)")
	feeRate := flag.Int64("fee-rate", 1000, "Fee rate in shors per vByte")
	spentSetPath := flag.String("spent-set", "pool_payout_spent_set.json", "Persistent spent-set path")
	webhookURL := flag.String("webhook", "", "Slack webhook URL for alerts (optional)")
	dryRun := flag.Bool("dry-run", false, "Build and sign but do not broadcast or record anything")
	regtest := flag.Bool("regtest", false, "The node is a regtest node (the sq address prefix is shared with mainnet)")
	flag.Parse()

	// Ctrl-C ends the context: the payout in flight reports an unknown
	// outcome if its reply is lost, and no further payout starts.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, runConfig{
		rpcURL: *rpcURL, rpcUser: *rpcUser, rpcPass: *rpcPass,
		elxHost: *elxHost, elxTLS: *elxTLS,
		keystorePath: *keystorePath, poolAddress: *poolAddress,
		payoutsPath: *payoutsPath, feeRate: *feeRate,
		spentSetPath: *spentSetPath, webhookURL: *webhookURL,
		dryRun: *dryRun, regtest: *regtest,
	}); err != nil {
		logger.Error("payout run failed", "err", err)
		os.Exit(1)
	}
}

// logger is shared by the program and the SDK components it builds.
var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

type runConfig struct {
	rpcURL, rpcUser, rpcPass string
	elxHost                  string
	elxTLS                   bool
	keystorePath             string
	poolAddress              string
	payoutsPath              string
	feeRate                  int64
	spentSetPath             string
	webhookURL               string
	dryRun                   bool
	regtest                  bool
}

func run(ctx context.Context, cfg runConfig) error {
	switch {
	case cfg.keystorePath == "":
		return errors.New("-keystore is required")
	case cfg.poolAddress == "":
		return errors.New("-pool-address is required")
	case cfg.payoutsPath == "":
		return errors.New("-payouts is required")
	}

	payouts, err := loadPayouts(cfg.payoutsPath)
	if err != nil {
		return err
	}

	// Derive the network from the pool address rather than assuming one. Nothing
	// here hardcodes a prefix: the script derived from an address is what BIP143
	// commits to as the scriptCode, so the network has to come from the address.
	network, err := address.NetworkOf(cfg.poolAddress)
	if err != nil {
		return fmt.Errorf("pool address: %w", err)
	}
	logger.Info("network", "name", network.Name)

	// Every recipient must be on the same network as the pool. Check before
	// spending anything, not after the first transaction is already on the wire.
	for _, p := range payouts {
		n, err := address.NetworkOf(p.Address)
		if err != nil {
			return fmt.Errorf("payout to %s: %w", shortID(p.Address, 20), err)
		}
		if n.Name != network.Name {
			return fmt.Errorf("payout to %s is on %s but the pool is on %s",
				shortID(p.Address, 20), n.Name, network.Name)
		}
		if p.Amount <= 0 {
			return fmt.Errorf("payout to %s has non-positive amount %d",
				shortID(p.Address, 20), p.Amount)
		}
	}
	// Regtest shares mainnet's address prefix, so the addresses cannot tell the
	// two apart; the operator says so. The chain check in rpc.RequireSynced then
	// holds the node to it, and the regtest coinbase maturity (60) applies.
	if cfg.regtest {
		if network.Name != types.Mainnet.Name {
			return fmt.Errorf("-regtest given but the pool address is on %s", network.Name)
		}
		network = types.Regtest
	}

	passphrase := os.Getenv("SOQ_KEYSTORE_PASSPHRASE")
	if passphrase == "" {
		return errors.New("SOQ_KEYSTORE_PASSPHRASE is not set")
	}
	keystore := keys.NewManager(cfg.keystorePath, passphrase)
	if err := keystore.Load(); err != nil {
		return fmt.Errorf("load keystore: %w", err)
	}
	// Load refuses a missing file and a wrong passphrase; a keystore that was
	// created and never given a key is the one case left to catch here.
	if keystore.KeyCount() == 0 {
		return fmt.Errorf("keystore %s holds no keys", cfg.keystorePath)
	}

	rpcClient := rpc.NewClient(cfg.rpcURL, cfg.rpcUser, cfg.rpcPass, logger)
	rpcClient.Network = network // refuse a node on another chain; apply its coinbase maturity
	// The indexer sees every address tracked; off this machine that goes over
	// TLS or not at all.
	if !cfg.elxTLS && !loopbackHost(cfg.elxHost) {
		return fmt.Errorf("electrumx %s is not on this machine; pass -electrumx-tls", cfg.elxHost)
	}
	elxClient := electrumx.NewClient(cfg.elxHost, 15*time.Second, logger)
	if err := elxClient.SetHRP(network.HRP); err != nil {
		return err
	}
	if cfg.elxTLS {
		elxClient.UseTLS()
	}

	// A spent-set file that exists but cannot be read must stop the run: an
	// empty set would re-expose every unconfirmed spend.
	spentSet, err := utxo.OpenSpentSet(cfg.spentSetPath, logger)
	if err != nil {
		return fmt.Errorf("spent set: %w", err)
	}
	selector := utxo.NewCoinSelector(spentSet)

	// Trip after 3 consecutive failures, then hold for 15 minutes. The point is
	// to stop a run that is failing for a systemic reason rather than retry into
	// a node outage.
	cb := resilience.NewCircuitBreaker(3, 15*time.Minute, logger)
	alerter := resilience.NewAlerter(cfg.webhookURL, logger)
	alerter.WireToCircuitBreaker(cb)

	if err := elxClient.Connect(ctx); err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", cfg.elxHost, err)
	}
	defer elxClient.Stop()

	if err := elxClient.TrackAddresses([]string{cfg.poolAddress}); err != nil {
		return fmt.Errorf("track pool address: %w", err)
	}
	if err := elxClient.RefreshAll(ctx); err != nil {
		return fmt.Errorf("initial UTXO refresh: %w", err)
	}

	if cfg.dryRun {
		logger.Info("dry run: transactions will be built and signed but not broadcast, and neither the spent set nor the UTXO cache will be modified")
	}

	var sent, failed int
	for i, payout := range payouts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped after %d payouts: %w", i, err)
		}
		logger.Info("payout", "n", i+1, "of", len(payouts), "soq", soq(payout.Amount), "address", payout.Address)

		if err := cb.Allow(); err != nil {
			logger.Error("circuit breaker refused, stopping", "err", err, "sent", sent, "failed", failed, "not_attempted", len(payouts)-i)
			return fmt.Errorf("circuit breaker stopped the run after %d payouts", i)
		}

		txid, err := executePayout(ctx, rpcClient, elxClient, selector, spentSet, keystore,
			payout, cfg.poolAddress, cfg.feeRate, cfg.dryRun)
		if err != nil {
			logger.Error("payout failed", "err", err)
			cb.RecordResult(err)
			failed++
			continue
		}

		cb.RecordSuccess()
		sent++
		if cfg.dryRun {
			logger.Info("would broadcast", "txid", txid)
		} else {
			logger.Info("broadcast", "txid", txid)
		}
	}

	logger.Info("done", "sent", sent, "failed", failed, "of", len(payouts))
	if failed > 0 {
		return fmt.Errorf("%d of %d payouts failed", failed, len(payouts))
	}
	return nil
}

// executePayout builds, signs and broadcasts one payout, returning the txid.
//
// State is mutated only after the node has accepted the transaction. That
// ordering is the whole point: marking UTXOs spent before a successful broadcast
// loses them from selection while they are still spendable.
func executePayout(
	ctx context.Context,
	rpcClient *rpc.Client,
	elxClient *electrumx.Client,
	selector *utxo.CoinSelector,
	spentSet *utxo.SpentSet,
	signer tx.Signer,
	payout Payout,
	changeAddr string,
	feeRate int64,
	dryRun bool,
) (string, error) {
	tipHeight, err := rpcClient.GetBlockCount(ctx)
	if err != nil {
		return "", fmt.Errorf("get block count: %w", err)
	}

	// feeRate is per vByte. A single-input ML-DSA payment is roughly 1,073 vB and
	// each additional input adds about another 1,000, so budget generously here
	// and let the builder compute the real fee from the final size.
	feeBudget := 4000 * feeRate

	selected, total, err := selector.SelectUTXOs(
		elxClient.GetAllUTXOs(), payout.Amount+feeBudget, 1, tipHeight, nil)
	if err != nil {
		return "", fmt.Errorf("coin selection: %w", err)
	}
	logger.Info("selected inputs", "count", len(selected), "total_soq", soq(total))

	// Defense 11: an ElectrumX cache can be stale. Confirm each input is still
	// unspent according to the node before signing over it.
	verified, err := rpcClient.VerifyAndFilterUTXOs(
		ctx, selected, elxClient.EvictUTXO, elxClient.SetAssetType)
	if err != nil {
		return "", fmt.Errorf("UTXO verification: %w", err)
	}
	if len(verified) < len(selected) {
		logger.Info("dropped inputs the node no longer has", "count", len(selected)-len(verified))
	}
	if len(verified) == 0 {
		return "", errors.New("no spendable UTXOs remain after verification")
	}

	recipientSPK, err := address.ScriptFor(payout.Address)
	if err != nil {
		return "", fmt.Errorf("recipient address: %w", err)
	}
	changeSPK, err := address.ScriptFor(changeAddr)
	if err != nil {
		return "", fmt.Errorf("change address: %w", err)
	}

	// Build and sign in one call; the transaction is returned because the
	// change value can only be read off it: the fee follows from feeRate and
	// the final size, so it cannot be recomputed as total - amount - fee.
	transaction, err := tx.BuildSignedTransaction(
		verified, recipientSPK, payout.Amount, changeSPK, feeRate, signer)
	if err != nil {
		return "", fmt.Errorf("build and sign: %w", err)
	}
	rawTxHex, builtTxID := transaction.SerializeHex(), transaction.TxID()

	var changeAmount int64
	if len(transaction.Outputs) > 1 {
		changeAmount = transaction.Outputs[1].Value
	}
	logger.Info("built", "txid", builtTxID, "inputs", len(verified), "vbytes", transaction.EstimateWeight()/4, "change_soq", soq(changeAmount))

	if dryRun {
		return builtTxID, nil
	}

	// Broadcast with a known outcome: a lost reply is resolved against the
	// node, "already in chain" is success, and a node txid that differs from
	// the one we computed is refused (serialization would disagree with
	// consensus). rpc.ErrUnknownOutcome means the transaction MAY be out: retry
	// these same bytes later, never rebuild. withdraw.Engine does that
	// durably; this example keeps the single-shot shape for readability.
	txid, err := rpcClient.Broadcast(ctx, rawTxHex, builtTxID)
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}

	// Only now, with the transaction accepted, record the effect. A spent-set
	// write failure here is an alert, not a retry: the payment is out and this
	// process still refuses the inputs; a restart would not. The change output
	// reaches the cache on the next poll and becomes an input once confirmed.
	if err := spentSet.MarkBroadcast(verified, txid); err != nil {
		logger.Error("ALERT: broadcast, spent set not written", "txid", txid, "err", err)
	}
	return txid, nil
}

func loadPayouts(path string) ([]Payout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read payouts: %w", err)
	}
	var payouts []Payout
	if err := json.Unmarshal(data, &payouts); err != nil {
		return nil, fmt.Errorf("parse payouts %s: %w", path, err)
	}
	if len(payouts) == 0 {
		return nil, fmt.Errorf("payouts file %s is empty", path)
	}
	return payouts, nil
}

// loopbackHost reports whether host:port names this machine: the name
// localhost or a loopback IP literal. Nothing is resolved.
func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return loopback.Host(host)
}

func soq(sats int64) float64 {
	return float64(sats) / float64(types.ShorsPerSOQ)
}

// shortID truncates an identifier for display without panicking on short input.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
