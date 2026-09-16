// Command broadcaster is the process in examples/exchange_split that sends.
// It holds the node's RPC credential and no key: it never builds anything and
// never signs anything, so a host that is compromised here can spend nothing
// that is not already signed and waiting.
//
// It owns two transitions, both of which need the node:
//
//	Built      -> Broadcast, or Failed on a permanent rejection
//	Broadcast  -> Confirmed at -confirmations
//
// Recover runs first, once, and it is this process's job rather than the
// signer's: it re-marks the inputs of every intent the store holds as
// Broadcast, releases reservations that belong to nothing built, and re-sends
// the persisted bytes of every Built intent. It never rebuilds, so the same
// transaction goes out and a lost reply cannot become a second payment.
//
// Usage:
//
//	go run ./examples/exchange_split/broadcaster -dir state -network stagenet \
//	    -state broadcaster-state -confirmations 6
//
// Environment: SOQ_RPC_USER, SOQ_RPC_PASSWORD and SOQ_RPC_URL (default is the
// network's own RPC port on localhost). The node needs txindex=1 to report
// confirmations of a transaction whose outputs are all spent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

func main() {
	dir := flag.String("dir", "", "the shared directory (required)")
	state := flag.String("state", "", "this process's own directory, for its spent set (required)")
	networkName := flag.String("network", "", "mainnet, stagenet or regtest (required)")
	confirmations := flag.Int64("confirmations", 6, "confirmations before a withdrawal is Confirmed")
	interval := flag.Duration("interval", 10*time.Second, "how often to send and to check confirmations")
	flag.Parse()
	if *dir == "" || *state == "" || *networkName == "" || *confirmations < 1 {
		flag.Usage()
		os.Exit(2)
	}
	network, err := split.NetworkFor(*networkName)
	if err != nil {
		fatal("network", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, split.Dir(*dir), *state, network, *confirmations, *interval); err != nil && !errors.Is(err, context.Canceled) {
		fatal("broadcaster", err)
	}
}

func run(ctx context.Context, dir split.Dir, state string, network types.Network, confirmations int64, interval time.Duration) error {
	if err := split.EnsureDir(state); err != nil {
		return err
	}
	store, err := dir.Open()
	if err != nil {
		return err
	}
	node := rpc.NewClient(rpcURL(network), os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"), logger)
	node.Network = network // refuse a node on another chain
	if err := node.RequireSynced(ctx); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	// This process's own spent set: it records what it has sent, and it is
	// rebuilt from the shared store by Recover on every start. The signer's
	// set is a different file on a different host, and the store is what the
	// two of them agree through.
	spent, err := utxo.OpenSpentSet(filepath.Join(state, "spent_set.json"), logger)
	if err != nil {
		return err
	}

	engine := &withdraw.Engine{
		Store:                 store,
		Spent:                 spent,
		Broadcaster:           node,
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		RequiredConfirmations: confirmations,
		Logger:                logger,
		// No Select and no BuildSign: there is no key on this host, so Build
		// is not available here and a Created intent waits for the signer.
	}

	// Startup: re-mark what was sent, release what was reserved for nothing,
	// re-send what was built. Every error is reported and none of them stops
	// the loop; ErrHeld in particular is a withdrawal a person must resolve.
	if err := engine.Recover(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		logger.Error("recover", "err", err)
	}

	logger.Info("broadcaster started", "dir", string(dir), "state", state, "confirmations", confirmations)
	for {
		send(ctx, store, engine)
		confirm(ctx, store, engine)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// send broadcasts every Built intent. The engine decides what each outcome
// means: a lost reply keeps the intent Built with its reservation renewed and
// the same bytes go out next pass, a permanent rejection fails it and releases
// the inputs, and a txid mismatch holds it for a person.
func send(ctx context.Context, store *split.DirStore, engine *withdraw.Engine) {
	built, err := store.List(ctx, withdraw.StateBuilt)
	if err != nil {
		logger.Error("send: the store could not be read", "err", err)
		return
	}
	for _, in := range built {
		if in.NodeTxID != "" {
			continue // held after a txid mismatch; Recover has already reported it
		}
		if err := engine.Broadcast(ctx, in); err != nil {
			logger.Warn("not sent", "id", in.ID, "state", in.State, "err", err)
			continue
		}
		logger.Info("sent", "id", in.ID, "txid", in.TxID)
	}
}

// confirm advances every Broadcast intent. A confirmation count that cannot be
// read is this intent's problem and not the pass's: the transaction is out
// either way, and the next pass asks again.
func confirm(ctx context.Context, store *split.DirStore, engine *withdraw.Engine) {
	sent, err := store.List(ctx, withdraw.StateBroadcast)
	if err != nil {
		logger.Error("confirm: the store could not be read", "err", err)
		return
	}
	for _, in := range sent {
		before := in.State
		if err := engine.UpdateConfirmations(ctx, in); err != nil {
			logger.Warn("confirmations", "id", in.ID, "err", err)
			continue
		}
		if in.State != before {
			logger.Info("confirmed", "id", in.ID, "txid", in.TxID, "confirmations", in.Confirmations)
			continue
		}
		logger.Info("waiting", "id", in.ID, "confirmations", in.Confirmations, "want", engine.RequiredConfirmations)
	}
}

func rpcURL(n types.Network) string {
	if u := os.Getenv("SOQ_RPC_URL"); u != "" {
		return u
	}
	return fmt.Sprintf("http://127.0.0.1:%d", n.RPCPort)
}

func fatal(msg string, err error) {
	logger.Error(msg, "err", err)
	os.Exit(1)
}
