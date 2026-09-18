// Command consolidate merges the smallest final outputs held by a keystore
// into one output at a destination address, in one transaction, against your
// own node. Run it on a schedule so a large withdrawal never needs more than
// the 80 inputs a transaction may carry, and to sweep deposit outputs whose
// keys are in the keystore to the hot wallet.
//
// The shape: every address in the keystore is tracked on the indexer; the
// selector takes up to -max-inputs of the smallest outputs with at least
// -min-confirmations, 289 by default, so a consolidation never spends an
// output a reorganisation could still remove; each input is confirmed unspent
// on the node before signing; tx.BuildSignedSweep builds the one-output
// transaction, signs it and verifies every input; the node broadcasts it and
// the spent set records the inputs. -dry-run stops before the broadcast.
//
// The fee comes from the node (rpc.FeeRateShorsPerVB) unless -fee-rate is
// given. Inputs worth less than the fee they add are left alone, and the
// input count is capped so the fee stays under tx.MaxFeeShors. A run that
// ends in rpc.ErrUnknownOutcome may or may not have broadcast; running it
// again is safe: the node check asks gettxout with the mempool included, so
// an input the first sweep spent, mined or still in the mempool, is filtered
// out before anything is built.
//
// Do not run this alongside a withdraw.Engine that spends from the same
// keystore. The engine's reservations live in its own spent set and are not
// visible to this process or to gettxout until the engine broadcasts, so a
// consolidation could take an input the engine has reserved and the engine's
// broadcast would then fail on a missing input; nothing is lost, the intent
// fails with its inputs released and the exchange submits it again, but the
// two should not overlap.
//
// Usage:
//
//	export SOQ_KEYSTORE_PASSPHRASE=... SOQ_RPC_USER=... SOQ_RPC_PASSWORD=...
//	go run ./examples/consolidate -keystore /var/lib/soq/keystore.enc \
//	    -to sq1p... -electrumx localhost:50001 -dry-run
package main

import (
	"context"
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
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

func main() {
	keystorePath := flag.String("keystore", "", "encrypted keystore holding the keys for the outputs to consolidate (required)")
	to := flag.String("to", "", "destination address, normally the hot wallet (required)")
	rpcURL := flag.String("rpc-url", "", "soqucoind RPC URL (default http://127.0.0.1:<the network's RPC port>)")
	elxHost := flag.String("electrumx", "", "ElectrumX host:port (default localhost:<the network's ElectrumX port>)")
	elxTLS := flag.Bool("electrumx-tls", false, "reach ElectrumX over TLS (required off-localhost)")
	maxInputs := flag.Int("max-inputs", utxo.MaxInputsPerTX, "most inputs to consolidate in one transaction")
	minConf := flag.Int("min-confirmations", int(types.MaxReorgDepth)+1, "confirmations an output needs before it is consolidated")
	feeRate := flag.Int64("fee-rate", 0, "fee rate in shors per vByte; 0 asks the node at a 6-block target")
	spentSetPath := flag.String("spent-set", "consolidate_spent_set.json", "persistent spent-set path")
	dryRun := flag.Bool("dry-run", false, "select, build and sign but do not broadcast or record anything")
	regtest := flag.Bool("regtest", false, "the node is a regtest node (the sq address prefix is shared with mainnet)")
	flag.Parse()

	// Ctrl-C ends the context. A broadcast interrupted by it reports
	// rpc.ErrUnknownOutcome; the next run filters the spent inputs out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, config{
		keystorePath: *keystorePath, to: *to, rpcURL: *rpcURL,
		elxHost: *elxHost, elxTLS: *elxTLS,
		maxInputs: *maxInputs, minConf: *minConf, feeRate: *feeRate,
		spentSetPath: *spentSetPath, dryRun: *dryRun, regtest: *regtest,
	}); err != nil {
		logger.Error("consolidation failed", "err", err)
		os.Exit(1)
	}
}

// logger is shared by the program and the SDK components it builds.
var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

type config struct {
	keystorePath, to, rpcURL, elxHost string
	elxTLS                            bool
	maxInputs, minConf                int
	feeRate                           int64
	spentSetPath                      string
	dryRun, regtest                   bool
}

func run(ctx context.Context, cfg config) error {
	switch {
	case cfg.keystorePath == "":
		return errors.New("-keystore is required")
	case cfg.to == "":
		return errors.New("-to is required")
	case cfg.maxInputs < 1 || cfg.maxInputs > utxo.MaxInputsPerTX:
		return fmt.Errorf("-max-inputs must be between 1 and %d", utxo.MaxInputsPerTX)
	case cfg.minConf < 1:
		return errors.New("-min-confirmations must be at least 1")
	}

	// The network comes from the destination address; every key in the
	// keystore has to be on it, which TrackAddresses enforces below.
	network, err := address.NetworkOf(cfg.to)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}
	if cfg.regtest {
		if network.Name != types.Mainnet.Name {
			return fmt.Errorf("-regtest given but the destination is on %s", network.Name)
		}
		network = types.Regtest
	}
	logger.Info("network", "name", network.Name)

	passphrase := os.Getenv("SOQ_KEYSTORE_PASSPHRASE")
	if passphrase == "" {
		return errors.New("SOQ_KEYSTORE_PASSPHRASE is not set")
	}
	keystore := keys.NewManager(cfg.keystorePath, passphrase)
	if err := keystore.Load(); err != nil { // a missing file is an error, not an empty keystore
		return fmt.Errorf("load keystore: %w", err)
	}
	addresses := keystore.GetAddresses()
	if len(addresses) == 0 {
		return fmt.Errorf("keystore %s holds no keys", cfg.keystorePath)
	}

	if cfg.rpcURL == "" {
		cfg.rpcURL = fmt.Sprintf("http://127.0.0.1:%d", network.RPCPort)
	}
	node := rpc.NewClient(cfg.rpcURL, os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"), logger)
	node.Network = network // refuse a node on another chain
	if err := node.RequireSynced(ctx); err != nil {
		return err
	}

	if cfg.elxHost == "" {
		cfg.elxHost = fmt.Sprintf("localhost:%d", network.ElectrumPort)
	}
	// The indexer sees every address tracked; off this machine that goes over
	// TLS or not at all.
	if !cfg.elxTLS && !loopbackHost(cfg.elxHost) {
		return fmt.Errorf("electrumx %s is not on this machine; pass -electrumx-tls", cfg.elxHost)
	}
	elx := electrumx.NewClient(cfg.elxHost, 15*time.Second, logger)
	if err := elx.SetHRP(network.HRP); err != nil {
		return err
	}
	if cfg.elxTLS {
		elx.UseTLS()
	}
	if err := elx.TrackAddresses(addresses); err != nil {
		return fmt.Errorf("track keystore addresses: %w", err)
	}
	if err := elx.Connect(ctx); err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", cfg.elxHost, err)
	}
	defer elx.Stop()
	if err := elx.RefreshAll(ctx); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}

	// A spent-set file that exists but cannot be read must stop the run.
	spent, err := utxo.OpenSpentSet(cfg.spentSetPath, logger)
	if err != nil {
		return fmt.Errorf("spent set: %w", err)
	}
	selector := utxo.NewCoinSelector(spent)

	feeRate, err := feeRateFor(ctx, node, cfg.feeRate)
	if err != nil {
		return err
	}
	// The builder refuses a fee above tx.MaxFeeShors; fewer inputs at a high
	// rate is a smaller consolidation, not a failed run.
	maxInputs := cfg.maxInputs
	if cap := inputsWithinFeeCap(feeRate); cap < maxInputs {
		logger.Info("fee cap limits the inputs", "fee_rate", feeRate, "allowed", cap, "asked", maxInputs)
		maxInputs = cap
	}

	tip, err := node.GetBlockCount(ctx)
	if err != nil {
		return fmt.Errorf("block count: %w", err)
	}
	selected, total, err := selector.SelectSmallestUTXOs(elx.GetAllUTXOs(), maxInputs, cfg.minConf, tip, nil)
	if errors.Is(err, utxo.ErrNoCandidates) {
		logger.Info("nothing to consolidate: no output has enough confirmations", "min_confirmations", cfg.minConf)
		return nil
	}
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	logger.Info("selected outputs", "count", len(selected), "total_soq", soq(total), "min_confirmations", cfg.minConf)

	// An output worth less than the fee it adds is left where it is: spending
	// it costs more than it is worth at this rate.
	perInput := (int64(tx.EstimatedInputWeight) + 3) / 4 * feeRate
	economic := selected[:0]
	for _, u := range selected {
		if u.Value > perInput {
			economic = append(economic, u)
		}
	}
	if skipped := len(selected) - len(economic); skipped > 0 {
		logger.Info("left outputs worth less than they cost to spend", "count", skipped, "cost_soq", soq(perInput), "fee_rate", feeRate)
	}
	if len(economic) == 0 || (len(economic) == 1 && economic[0].Address == cfg.to) {
		logger.Info("nothing to consolidate")
		return nil
	}

	// Defense 11: the indexer's cache can be stale. Each input is confirmed
	// unspent on the node before it is signed over.
	verified, err := node.VerifyAndFilterUTXOs(ctx, economic, elx.EvictUTXO, elx.SetAssetType)
	if err != nil {
		return fmt.Errorf("verify inputs: %w", err)
	}
	if len(verified) < len(economic) {
		logger.Info("dropped inputs the node no longer has", "count", len(economic)-len(verified))
	}
	if len(verified) == 0 {
		return errors.New("no spendable outputs remain after verification")
	}
	if len(verified) == 1 && verified[0].Address == cfg.to {
		logger.Info("nothing to consolidate: one output, already at the destination")
		return nil
	}
	destSPK, err := address.ScriptFor(cfg.to)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}
	// The set as a whole must also cover the transaction's base bytes and
	// leave an output at or above the relay floor, or the builder refuses it;
	// for a scheduled run that is "nothing to consolidate", not a failure.
	var in int64
	for _, u := range verified {
		in += u.Value
	}
	base := (int64(tx.TxOverheadWeight+tx.EstimatedOutputWeight)+3)/4 + tx.FeeMarginVBytes
	if in <= perInput*int64(len(verified))+base*feeRate+tx.MinOutputValue(destSPK) {
		logger.Info("nothing to consolidate: the outputs do not cover the fee and the relay floor", "count", len(verified), "total_soq", soq(in))
		return nil
	}

	transaction, err := tx.BuildSignedSweep(verified, destSPK, feeRate, keystore)
	if err != nil {
		return fmt.Errorf("build and sign: %w", err)
	}
	rawHex, txid := transaction.SerializeHex(), transaction.TxID()
	out := transaction.Outputs[0].Value
	logger.Info("built", "txid", txid, "inputs", len(verified), "in_soq", soq(in), "out_soq", soq(out), "fee_soq", soq(in-out), "vbytes", transaction.VSize())

	if cfg.dryRun {
		logger.Info("dry run: not broadcast, nothing recorded")
		return nil
	}

	// Broadcast with a known outcome: a lost reply is resolved against the
	// node, "already in chain" is success, a node txid other than ours is
	// refused. Only then are the inputs recorded as spent.
	if _, err := node.Broadcast(ctx, rawHex, txid); err != nil {
		return fmt.Errorf("broadcast: %w", err)
	}
	if err := spent.MarkBroadcast(verified, txid); err != nil {
		logger.Error("ALERT: broadcast, spent set not written", "txid", txid, "err", err)
	}
	logger.Info("broadcast", "txid", txid)
	return nil
}

// feeRateFor returns the flag's rate, or the node's estimate at a 6-block
// target, clamped to the range the builders accept and logged when the node
// had no estimate.
func feeRateFor(ctx context.Context, node *rpc.Client, flagRate int64) (int64, error) {
	if flagRate < 0 {
		return 0, fmt.Errorf("-fee-rate %d: must be 0 (ask the node) or a positive rate", flagRate)
	}
	if flagRate > 0 {
		return flagRate, nil
	}
	est, err := node.FeeRateShorsPerVB(ctx, 6)
	if err != nil {
		return 0, fmt.Errorf("fee estimate: %w", err)
	}
	if est.Fallback {
		logger.Warn("node has no fee estimate, using the floor", "fee_rate", est.Rate)
	}
	return est.Rate, nil
}

// inputsWithinFeeCap is how many inputs a one-output sweep at feeRate can
// carry before its fee passes tx.MaxFeeShors, from the builder's own weights
// rounded up per input, so the estimate is never below the fee charged.
func inputsWithinFeeCap(feeRate int64) int {
	perInput := (int64(tx.EstimatedInputWeight) + 3) / 4
	base := (int64(tx.TxOverheadWeight+tx.EstimatedOutputWeight)+3)/4 + tx.FeeMarginVBytes
	n := (tx.MaxFeeShors/feeRate - base) / perInput
	if n < 1 {
		return 1
	}
	if n > utxo.MaxInputsPerTX {
		return utxo.MaxInputsPerTX
	}
	return int(n)
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

func soq(shors int64) string {
	return fmt.Sprintf("%d.%08d", shors/types.ShorsPerSOQ, shors%types.ShorsPerSOQ)
}
