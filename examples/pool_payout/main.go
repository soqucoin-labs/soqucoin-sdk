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
// # WHAT THIS EXAMPLE USED TO DO, AND WHY IT WAS DANGEROUS
//
// This program previously described itself as "production-grade" and as "the
// same architecture that powers soqupool's live payouts" while building no
// transaction at all. It assigned rawTxHex = "..." and never broadcast. That by
// itself would only be misleading; what made it dangerous is that it then went on
// to mutate state as though it had:
//
//   - It called spentSet.MarkBroadcast with the literal txid
//     "simulated_txid_example", writing that fiction into a PERSISTENT file, so
//     real UTXOs were recorded as spent against a transaction that did not exist.
//   - It injected a change UTXO into the ElectrumX cache under the same fake txid.
//   - It logged "Broadcast TX" and returned nil, so the circuit breaker recorded
//     SUCCESS for every payout.
//
// Adapted for a real pool, that marks miners paid without paying them. The
// program now performs the payout, and -dry-run stops before any state changes
// rather than faking its way past them.
//
// Usage:
//
//	export SOQ_KEYSTORE_PASSPHRASE=...
//	go run ./examples/pool_payout/ \
//	  -rpc-url http://127.0.0.1:19332 \
//	  -rpc-user user -rpc-pass pass \
//	  -electrumx localhost:50001 \
//	  -keystore /var/lib/soq/keystore.enc \
//	  -pool-address ssq1p... \
//	  -payouts payouts.json \
//	  -dry-run
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/resilience"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// Payout is a single payment to a miner. Amount is in satoshis.
type Payout struct {
	Address string `json:"address"`
	Amount  int64  `json:"amount"`
}

func main() {
	rpcURL := flag.String("rpc-url", "http://127.0.0.1:19332", "soqucoind RPC URL")
	rpcUser := flag.String("rpc-user", "rpcuser", "RPC username")
	rpcPass := flag.String("rpc-pass", "rpcpassword", "RPC password")
	elxHost := flag.String("electrumx", "localhost:50001", "ElectrumX host:port")
	elxTLS := flag.Bool("electrumx-tls", false, "Use TLS to reach ElectrumX (required off-localhost)")
	keystorePath := flag.String("keystore", "", "Path to the encrypted keystore (required)")
	poolAddress := flag.String("pool-address", "", "Pool payout address, also receives change (required)")
	payoutsPath := flag.String("payouts", "", "JSON file: [{\"address\":\"...\",\"amount\":123}] (required)")
	feeRate := flag.Int64("fee-rate", 1000, "Fee rate in satoshis per vByte")
	spentSetPath := flag.String("spent-set", "pool_payout_spent_set.json", "Persistent spent-set path")
	webhookURL := flag.String("webhook", "", "Slack webhook URL for alerts (optional)")
	dryRun := flag.Bool("dry-run", false, "Build and sign but do not broadcast or record anything")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[pool-payout] ")

	if err := run(runConfig{
		rpcURL: *rpcURL, rpcUser: *rpcUser, rpcPass: *rpcPass,
		elxHost: *elxHost, elxTLS: *elxTLS,
		keystorePath: *keystorePath, poolAddress: *poolAddress,
		payoutsPath: *payoutsPath, feeRate: *feeRate,
		spentSetPath: *spentSetPath, webhookURL: *webhookURL,
		dryRun: *dryRun,
	}); err != nil {
		log.Fatalf("%v", err)
	}
}

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
}

func run(cfg runConfig) error {
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

	// Derive the network from the pool address rather than assuming one. Passing
	// the wrong HRP is how this SDK ended up unable to build mainnet transactions
	// at all, so nothing here hardcodes a prefix.
	network, err := address.NetworkOf(cfg.poolAddress)
	if err != nil {
		return fmt.Errorf("pool address: %w", err)
	}
	log.Printf("Network: %s", network.Name)

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

	passphrase := os.Getenv("SOQ_KEYSTORE_PASSPHRASE")
	if passphrase == "" {
		return errors.New("SOQ_KEYSTORE_PASSPHRASE is not set")
	}
	keystore := keys.NewManager(cfg.keystorePath, passphrase)
	if err := keystore.Load(); err != nil {
		return fmt.Errorf("load keystore: %w", err)
	}
	// Load treats a missing file as an empty keystore, so an empty result means
	// a wrong path or a wrong passphrase, not "no keys yet".
	if keystore.KeyCount() == 0 {
		return fmt.Errorf("keystore %s holds no keys: wrong path or passphrase",
			cfg.keystorePath)
	}

	rpcClient := rpc.NewClient(cfg.rpcURL, cfg.rpcUser, cfg.rpcPass)
	elxClient := electrumx.NewClient(cfg.elxHost, 15*time.Second)
	elxClient.HRP = network.HRP
	if cfg.elxTLS {
		elxClient.UseTLS()
	}

	spentSet := utxo.NewSpentSet(cfg.spentSetPath)
	selector := utxo.NewCoinSelector(spentSet)

	// Trip after 3 consecutive failures, then hold for 15 minutes. The point is
	// to stop a run that is failing for a systemic reason rather than retry into
	// a node outage.
	cb := resilience.NewCircuitBreaker(3, 15*time.Minute)
	alerter := resilience.NewAlerter(cfg.webhookURL)
	alerter.WireToCircuitBreaker(cb)

	if err := elxClient.Connect(); err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", cfg.elxHost, err)
	}
	defer elxClient.Stop()

	elxClient.TrackAddresses([]string{cfg.poolAddress})
	if err := elxClient.RefreshAll(); err != nil {
		return fmt.Errorf("initial UTXO refresh: %w", err)
	}

	if cfg.dryRun {
		log.Printf("DRY RUN: transactions will be built and signed but not broadcast, " +
			"and neither the spent set nor the UTXO cache will be modified")
	}

	var sent, failed int
	for i, payout := range payouts {
		log.Printf("Payout %d/%d: %.4f SOQ to %s",
			i+1, len(payouts), soq(payout.Amount), shortID(payout.Address, 20))

		if err := cb.Allow(); err != nil {
			log.Printf("Circuit breaker open: %v", err)
			log.Printf("Stopping. %d sent, %d failed, %d not attempted",
				sent, failed, len(payouts)-i)
			return fmt.Errorf("circuit breaker stopped the run after %d payouts", i)
		}

		txid, err := executePayout(rpcClient, elxClient, selector, spentSet, keystore,
			payout, cfg.poolAddress, cfg.feeRate, cfg.dryRun)
		if err != nil {
			log.Printf("  FAILED: %v", err)
			cb.RecordFailure(err)
			failed++
			continue
		}

		cb.RecordSuccess()
		sent++
		if cfg.dryRun {
			log.Printf("  would broadcast %s", shortID(txid, 16))
		} else {
			log.Printf("  broadcast %s", shortID(txid, 16))
		}
	}

	log.Printf("Done: %d sent, %d failed, of %d", sent, failed, len(payouts))
	if failed > 0 {
		return fmt.Errorf("%d of %d payouts failed", failed, len(payouts))
	}
	return nil
}

// executePayout builds, signs and broadcasts one payout, returning the txid.
//
// State is mutated only after the node has accepted the transaction. That
// ordering is the whole point: marking UTXOs spent before a successful broadcast
// loses them from selection while they are still spendable, and the previous
// version of this file did exactly that against a txid it invented.
func executePayout(
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
	tipHeight, err := rpcClient.GetBlockCount()
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
	log.Printf("  selected %d UTXOs totaling %.4f SOQ", len(selected), soq(total))

	// Defense 11: an ElectrumX cache can be stale. Confirm each input is still
	// unspent according to the node before signing over it.
	verified, err := rpcClient.VerifyAndFilterUTXOs(
		selected, elxClient.EvictUTXO, elxClient.SetAssetType)
	if err != nil {
		return "", fmt.Errorf("UTXO verification: %w", err)
	}
	if len(verified) < len(selected) {
		log.Printf("  Defense 11 filtered %d stale UTXOs", len(selected)-len(verified))
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

	// tx.BuildAndSign does these three steps in one call. They are separate here
	// only so the change value can be read off the transaction: the fee follows
	// from feeRate and the final size, so it cannot be recomputed as
	// total - amount - fee.
	transaction, err := tx.BuildSendTransaction(
		verified, recipientSPK, payout.Amount, changeSPK, feeRate)
	if err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	if err := transaction.SignAll(signer); err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	rawTxHex, builtTxID := transaction.SerializeHex(), transaction.TxID()

	var changeAmount int64
	if len(transaction.Outputs) > 1 {
		changeAmount = transaction.Outputs[1].Value
	}
	log.Printf("  built %s: %d inputs, %d vB, change %.4f SOQ",
		shortID(builtTxID, 16), len(verified), transaction.EstimateWeight()/4, soq(changeAmount))

	if dryRun {
		return builtTxID, nil
	}

	txid, err := rpcClient.SendRawTransaction(rawTxHex)
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}
	// If the node computed a different txid, our serialization disagrees with
	// consensus. Do not record anything against it and do not retry.
	if txid != builtTxID {
		return "", fmt.Errorf("txid mismatch: node %s, SDK %s", txid, builtTxID)
	}

	// Only now, with the transaction accepted, record the effects.
	spentSet.MarkBroadcast(verified, txid)
	if changeAmount > 0 {
		// Defense 13: make change spendable immediately rather than waiting for
		// the next ElectrumX poll, so back-to-back payouts do not stall.
		elxClient.AddChangeUTXO(txid, 1, changeAmount, changeAddr)
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

func soq(sats int64) float64 {
	return float64(sats) / float64(types.SatoshisPerSOQ)
}

// shortID truncates an identifier for display without panicking on short input.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
