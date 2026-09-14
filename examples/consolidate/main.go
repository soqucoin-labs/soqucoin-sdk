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
// again is safe, since the same inputs are either spent on the node, and
// filtered out, or in the mempool, and a second sweep of them is refused as a
// conflict.
//
// Usage:
//
//	export SOQ_KEYSTORE_PASSPHRASE=... SOQ_RPC_USER=... SOQ_RPC_PASSWORD=...
//	go run ./examples/consolidate -keystore /var/lib/soq/keystore.enc \
//	    -to sq1p... -electrumx localhost:50001 -dry-run
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
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

	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[consolidate] ")

	if err := run(config{
		keystorePath: *keystorePath, to: *to, rpcURL: *rpcURL,
		elxHost: *elxHost, elxTLS: *elxTLS,
		maxInputs: *maxInputs, minConf: *minConf, feeRate: *feeRate,
		spentSetPath: *spentSetPath, dryRun: *dryRun, regtest: *regtest,
	}); err != nil {
		log.Fatalf("%v", err)
	}
}

type config struct {
	keystorePath, to, rpcURL, elxHost string
	elxTLS                            bool
	maxInputs, minConf                int
	feeRate                           int64
	spentSetPath                      string
	dryRun, regtest                   bool
}

func run(cfg config) error {
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
	log.Printf("Network: %s", network.Name)

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
	node := rpc.NewClient(cfg.rpcURL, os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"))
	node.Network = network // refuse a node on another chain
	if err := node.RequireSynced(); err != nil {
		return err
	}

	if cfg.elxHost == "" {
		cfg.elxHost = fmt.Sprintf("localhost:%d", network.ElectrumPort)
	}
	elx := electrumx.NewClient(cfg.elxHost, 15*time.Second)
	elx.HRP = network.HRP
	if cfg.elxTLS {
		elx.UseTLS()
	}
	if err := elx.TrackAddresses(addresses); err != nil {
		return fmt.Errorf("track keystore addresses: %w", err)
	}
	if err := elx.Connect(); err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", cfg.elxHost, err)
	}
	defer elx.Stop()
	if err := elx.RefreshAll(); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}

	// A spent-set file that exists but cannot be read must stop the run.
	spent, err := utxo.OpenSpentSet(cfg.spentSetPath)
	if err != nil {
		return fmt.Errorf("spent set: %w", err)
	}
	selector := utxo.NewCoinSelector(spent)

	feeRate, err := feeRateFor(node, cfg.feeRate)
	if err != nil {
		return err
	}
	// The builder refuses a fee above tx.MaxFeeShors; fewer inputs at a high
	// rate is a smaller consolidation, not a failed run.
	maxInputs := cfg.maxInputs
	if cap := inputsWithinFeeCap(feeRate); cap < maxInputs {
		log.Printf("at %d shors/vB the fee cap allows %d inputs, not %d", feeRate, cap, maxInputs)
		maxInputs = cap
	}

	tip, err := node.GetBlockCount()
	if err != nil {
		return fmt.Errorf("block count: %w", err)
	}
	selected, total, err := selector.SelectSmallestUTXOs(elx.GetAllUTXOs(), maxInputs, cfg.minConf, tip, nil)
	if errors.Is(err, utxo.ErrNoCandidates) {
		log.Printf("nothing to consolidate: no output has %d confirmations", cfg.minConf)
		return nil
	}
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	log.Printf("selected %d outputs totalling %s SOQ at %d or more confirmations", len(selected), soq(total), cfg.minConf)

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
		log.Printf("left %d outputs worth less than the %s SOQ each costs to spend at %d shors/vB", skipped, soq(perInput), feeRate)
	}
	if len(economic) == 0 || (len(economic) == 1 && economic[0].Address == cfg.to) {
		log.Printf("nothing to consolidate")
		return nil
	}

	// Defense 11: the indexer's cache can be stale. Each input is confirmed
	// unspent on the node before it is signed over.
	verified, err := node.VerifyAndFilterUTXOs(economic, elx.EvictUTXO, elx.SetAssetType)
	if err != nil {
		return fmt.Errorf("verify inputs: %w", err)
	}
	if len(verified) < len(economic) {
		log.Printf("%d inputs the node no longer has were dropped", len(economic)-len(verified))
	}
	if len(verified) == 0 {
		return errors.New("no spendable outputs remain after verification")
	}

	destSPK, err := address.ScriptFor(cfg.to)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}
	transaction, err := tx.BuildSignedSweep(verified, destSPK, feeRate, keystore)
	if err != nil {
		return fmt.Errorf("build and sign: %w", err)
	}
	rawHex, txid := transaction.SerializeHex(), transaction.TxID()
	var in int64
	for _, u := range verified {
		in += u.Value
	}
	out := transaction.Outputs[0].Value
	log.Printf("built %s: %d inputs, %s SOQ in, %s SOQ out, fee %s SOQ, %d vB",
		txid, len(verified), soq(in), soq(out), soq(in-out), transaction.VSize())

	if cfg.dryRun {
		log.Printf("DRY RUN: not broadcast, nothing recorded")
		return nil
	}

	// Broadcast with a known outcome: a lost reply is resolved against the
	// node, "already in chain" is success, a node txid other than ours is
	// refused. Only then are the inputs recorded as spent.
	if _, err := node.Broadcast(rawHex, txid); err != nil {
		return fmt.Errorf("broadcast: %w", err)
	}
	if err := spent.MarkBroadcast(verified, txid); err != nil {
		log.Printf("ALERT %s broadcast, spent set not written: %v", txid, err)
	}
	log.Printf("broadcast %s", txid)
	return nil
}

// feeRateFor returns the flag's rate, or the node's estimate at a 6-block
// target, clamped to the range the builders accept and logged when the node
// had no estimate.
func feeRateFor(node *rpc.Client, flagRate int64) (int64, error) {
	if flagRate > 0 {
		return flagRate, nil
	}
	est, err := node.FeeRateShorsPerVB(6)
	if err != nil {
		return 0, fmt.Errorf("fee estimate: %w", err)
	}
	if est.Fallback {
		log.Printf("node has no fee estimate; using the floor, %d shors/vB", est.Rate)
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

func soq(shors int64) string {
	return fmt.Sprintf("%d.%08d", shors/types.ShorsPerSOQ, shors%types.ShorsPerSOQ)
}
