// Command broadcaster is the process in examples/exchange_split that sends.
// It holds the node's RPC credential and no key: it never builds anything and
// never signs anything, so a host that is compromised here can spend nothing
// that is not already signed and waiting.
//
// It owns two transitions, both of which need the node:
//
//	Built      -> Broadcast, or Failed on a permanent rejection, or held
//	Broadcast  -> Confirmed at -confirmations
//
// Recover runs first, once, and it is this process's job rather than the
// signer's: it re-marks the inputs of every intent the store holds as
// Broadcast, releases reservations that belong to nothing built, and re-sends
// the persisted bytes of every Built intent. It never rebuilds, so the same
// transaction goes out and a lost reply cannot become a second payment.
//
// While the store holds a held intent (the node accepted other bytes for it,
// or rejected bytes an earlier attempt may have relayed) nothing is sent:
// the condition is read from the store on every pass, so it survives a
// restart and ends when an operator has moved the intent on through
// withdraw.Engine.Abandon. Confirmations are still read meanwhile.
//
// Usage:
//
//	go run ./examples/exchange_split/broadcaster -dir state -network stagenet \
//	    -state broadcaster-state
//
// -confirmations defaults to the chain's finality horizon, types.MaxReorgDepth
// (288), the figure the integration guide's Step 4 gives for withdrawal
// release; pass a lower one only on a network where a demo has to finish.
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
	confirmations := flag.Int64("confirmations", types.MaxReorgDepth, "confirmations before a withdrawal is Confirmed (default: the finality horizon)")
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
		Network:               network,
		Broadcaster:           node,
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		Chain:                 withdraw.RPCChain{Client: node}, // Abandon's node checks
		RequiredConfirmations: confirmations,
		Logger:                logger,
		// No Select and no BuildSign: there is no key on this host, so Build
		// is not available here and a Created intent waits for the signer.
	}

	// Startup: re-mark what was sent, release what was reserved for nothing,
	// re-send what was built. Every error is reported and none of them stops
	// the loop; a held intent (ErrHeld) is one a person must resolve, and
	// send refuses to send anything while the store holds one.
	if err := engine.Recover(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		logger.Error("recover", "err", err)
		if errors.Is(err, withdraw.ErrHeld) {
			logger.Error("withdrawals halted: a held intent must be resolved by hand before anything is sent")
		}
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
// the inputs unless an earlier reply was lost, and a txid mismatch or a
// rejection after a lost reply holds it for a person.
//
// A held intent stops the sending, as the integration guide's Step 3 says:
// nothing is sent while the store holds one, and a hold that arises inside a
// pass ends the pass, so an intent listed after it is not sent either. The
// store is the record of the hold, so the rule is the same after a restart.
func send(ctx context.Context, store *split.DirStore, engine *withdraw.Engine) {
	built, err := store.List(ctx, withdraw.StateBuilt)
	if err != nil {
		logger.Error("send: the store could not be read", "err", err)
		return
	}
	if n := heldCount(built); n > 0 {
		logger.Error("withdrawals halted: held intents must be resolved by hand before anything is sent", "held", n)
		return
	}
	for _, in := range built {
		if err := engine.Broadcast(ctx, in); err != nil {
			if errors.Is(err, withdraw.ErrHeld) || errors.Is(err, rpc.ErrTxIDMismatch) {
				logger.Error("withdrawals halted: the intent is held; resolve it by hand before anything else is sent", "id", in.ID, "err", err)
				return
			}
			logger.Warn("not sent", "id", in.ID, "state", in.State, "err", err)
			continue
		}
		logger.Info("sent", "id", in.ID, "txid", in.TxID)
	}
}

// heldCount is how many of the Built intents are held: the node accepted
// other bytes for them (NodeTxID) or rejected bytes an earlier attempt may
// have relayed (Hold), the engine's own definition.
func heldCount(built []*withdraw.Intent) int {
	n := 0
	for _, in := range built {
		if in.NodeTxID != "" || in.Hold != "" {
			n++
		}
	}
	return n
}

// confirm advances every Broadcast intent. A confirmation count that cannot be
// read is this intent's problem and not the pass's: the transaction is out
// either way, and the next pass asks again. A transaction the node no longer
// knows (rpc.ErrUnknownOutcome) is sent again with the same bytes; an
// operator abandons it through withdraw.Engine.Abandon after the wait.
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
			if errors.Is(err, rpc.ErrUnknownOutcome) {
				if err := engine.Rebroadcast(ctx, in); err != nil {
					logger.Warn("rebroadcast", "id", in.ID, "err", err)
				}
			}
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
