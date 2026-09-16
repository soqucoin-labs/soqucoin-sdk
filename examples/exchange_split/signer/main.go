// Command signer is the process in examples/exchange_split that holds the
// key. It has no node credential, no indexer address and no listening socket:
// the shared directory is its only input, and the only thing it does with it
// is turn Created intents into Built ones.
//
// It has no node credential because the node cannot give it a safe one. There
// is no per-method access control in the node's RPC interface, so a credential
// that may read may also send; a "read-only" signer account is not something
// the node can enforce. The watcher therefore does the reading and leaves the
// result in snapshot.json, which it has already checked against the node
// output by output.
//
// What it does each pass, in this order:
//
//  1. Reconciles its spent set with the shared store. Inputs of intents the
//     store holds as Broadcast or Confirmed are marked spent, reservations of
//     intents that are Created or Failed are released, and reservations of
//     Built intents are renewed. The store is the durable truth about spends;
//     this file is only this process's copy of it. Without this pass a
//     reservation would expire while the transaction sat in a mempool and the
//     inputs would be offered to the next withdrawal.
//  2. Reads the snapshot and refuses it unless it is current. An old snapshot
//     may list outputs that are already spent.
//  3. Builds every Created intent: select, reserve, sign, save. Nothing here
//     touches the network, so nothing here can be a broadcast.
//
// Usage:
//
//	go run ./examples/exchange_split/signer -dir state -network stagenet \
//	    -state signer-state
//
// Environment: SOQ_KEYSTORE_PASSPHRASE. The keystore is the signer's own file
// under -state, never in the shared directory.
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

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

func main() {
	dir := flag.String("dir", "", "the shared directory (required)")
	state := flag.String("state", "", "this process's own directory, for the keystore and the spent set (required)")
	networkName := flag.String("network", "", "mainnet, stagenet or regtest (required)")
	interval := flag.Duration("interval", 10*time.Second, "how often to reconcile and build")
	maxSnapshotAge := flag.Duration("max-snapshot-age", 2*time.Minute, "refuse to build from a snapshot older than this")
	skew := flag.Duration("clock-skew", 5*time.Second, "how far ahead of this clock a snapshot may be stamped")
	minConf := flag.Int("min-confirmations", 1, "confirmations an input needs to be selected")
	flag.Parse()
	if *dir == "" || *state == "" || *networkName == "" {
		flag.Usage()
		os.Exit(2)
	}
	network, err := split.NetworkFor(*networkName)
	if err != nil {
		fatal("network", err)
	}
	passphrase := os.Getenv("SOQ_KEYSTORE_PASSPHRASE")
	if passphrase == "" {
		fatal("SOQ_KEYSTORE_PASSPHRASE is not set", nil)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, config{
		dir:            split.Dir(*dir),
		state:          *state,
		network:        network,
		passphrase:     passphrase,
		interval:       *interval,
		maxSnapshotAge: *maxSnapshotAge,
		skew:           *skew,
		minConf:        *minConf,
	}); err != nil && !errors.Is(err, context.Canceled) {
		fatal("signer", err)
	}
}

type config struct {
	dir            split.Dir
	state          string
	network        types.Network
	passphrase     string
	interval       time.Duration
	maxSnapshotAge time.Duration
	skew           time.Duration
	minConf        int
}

func run(ctx context.Context, cfg config) error {
	if err := split.EnsureDir(cfg.state); err != nil {
		return err
	}
	store, err := split.OpenDirStore(cfg.dir.Intents())
	if err != nil {
		return err
	}
	// Load, not LoadOrCreate: a signer that silently creates an empty keystore
	// would build nothing and say nothing about why.
	keystore := keys.NewManager(filepath.Join(cfg.state, "keys.enc"), cfg.passphrase)
	if err := keystore.Load(); err != nil {
		return fmt.Errorf("keystore: %w", err)
	}
	if keystore.KeyCount() == 0 {
		return errors.New("the keystore holds no key")
	}
	// A spent-set file that exists but cannot be read must stop the run: an
	// empty set would re-expose every reservation and every spend.
	spent, err := utxo.OpenSpentSet(filepath.Join(cfg.state, "spent_set.json"), logger)
	if err != nil {
		return err
	}
	selector := utxo.NewCoinSelector(spent)

	// The snapshot of the pass in flight. Select reads it rather than the
	// network, which this process does not have.
	var snap split.Snapshot
	engine := &withdraw.Engine{
		Store:  store,
		Spent:  spent,
		Logger: logger,
		Select: func(_ context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			// Budget the fee against vsize, as the guide does: a one-input,
			// two-output payment is about 1,073 vB and each further
			// ML-DSA-44 input adds about 976 vB.
			budget := amount + (1100+950*int64(utxo.MaxInputsPerTX))*feeRate
			selected, _, err := selector.SelectUTXOs(snap.Spendable(), budget, cfg.minConf, snap.Tip, []string{snap.HotAddress})
			if err != nil {
				return nil, err
			}
			return selected, nil
		},
		BuildSign: func(_ context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
			recipientSPK, err := address.ScriptFor(to)
			if err != nil {
				return "", "", err
			}
			changeSPK, err := address.ScriptFor(snap.HotAddress)
			if err != nil {
				return "", "", err
			}
			return tx.BuildAndSign(inputs, recipientSPK, amount, changeSPK, feeRate, keystore)
		},
		// No Broadcaster and no Confirmer: this process may call Build and
		// nothing else. Recover is not usable here either, because it
		// re-broadcasts Built intents; reconcile below is the part of it that
		// belongs to a signer.
	}

	logger.Info("signer started", "dir", string(cfg.dir), "state", cfg.state, "keys", keystore.KeyCount())
	for {
		reconcile(ctx, store, spent, engine.ReservationTTL)
		var err error
		snap, err = split.ReadSnapshot(cfg.dir, cfg.maxSnapshotAge, cfg.skew, time.Now().UTC())
		switch {
		case errors.Is(err, split.ErrNoSnapshot):
			logger.Info("no snapshot yet; nothing is built until the watcher publishes one")
		case err != nil:
			// Stale, or inconsistent with itself. Either way this process
			// does not know what may be spent, so it builds nothing.
			logger.Warn("snapshot refused; nothing is built this pass", "err", err)
		default:
			// The change of every payment goes back to the hot address and
			// every input is spent from it. A snapshot naming an address on
			// another network, or one this keystore holds no key for, is
			// either pointed at another wallet or forged; nothing is built.
			if err := address.Validate(cfg.network.HRP, snap.HotAddress); err != nil {
				logger.Error("snapshot refused: the hot address is not on this network", "hot", snap.HotAddress, "err", err)
			} else if !keystore.HasKey(snap.HotAddress) {
				logger.Error("snapshot refused: the keystore holds no key for the hot address", "hot", snap.HotAddress)
			} else {
				build(ctx, store, engine, cfg.network.HRP)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.interval):
		}
	}
}

// reconcile makes this process's spent set agree with the shared store before
// anything is selected.
//
// It is the repair half of withdraw.Engine.Recover, which a signer cannot call
// because the other half re-broadcasts. In one process the engine does this
// once at startup, since nothing else changes the facts while it runs. Here
// the broadcaster changes them continuously, so it runs every pass:
//
//   - Broadcast or Confirmed: the inputs are spent for good, whatever this
//     file said before. This is the one that matters. Without it the
//     reservation expires by its TTL while the transaction sits in a mempool
//     and the next selection offers the same inputs to another withdrawal.
//   - Built: the transaction is signed and waiting for the broadcaster, so the
//     reservation is renewed rather than left to expire.
//   - Created or Failed: nothing signed exists, so a reservation held for one
//     is a crash between reserving and saving, and the coins are free.
func reconcile(ctx context.Context, store *split.DirStore, spent *utxo.SpentSet, ttl time.Duration) {
	if ttl <= 0 {
		ttl = withdraw.DefaultReservationTTL
	}
	sent, err := store.List(ctx, withdraw.StateBroadcast, withdraw.StateConfirmed)
	if err != nil {
		logger.Error("reconcile: the store could not be read; nothing is selected this pass", "err", err)
		return
	}
	for _, in := range sent {
		txid := in.TxID
		if in.NodeTxID != "" {
			txid = in.NodeTxID // the node accepted other bytes; those are the spend
		}
		if err := spent.MarkBroadcastFor(inputsOf(in), txid, in.ID); err != nil {
			logger.Error("reconcile: inputs of a sent withdrawal not marked spent", "id", in.ID, "err", err)
		}
	}
	for _, id := range spent.ReservedIntents() {
		in, ok, err := store.Get(ctx, id)
		if err != nil || !ok {
			// A reservation whose intent cannot be read is kept: a stale hold
			// costs a withdrawal its inputs for a while, releasing one that
			// belongs to a signed transaction costs the exchange a payment.
			logger.Warn("reconcile: reservation kept, its intent could not be read", "id", id, "found", ok, "err", err)
			continue
		}
		switch in.State {
		case withdraw.StateBuilt:
			if err := spent.Reserve(inputsOf(in), in.ID, ttl); err != nil {
				logger.Error("reconcile: a built withdrawal's reservation could not be renewed", "id", in.ID, "err", err)
			}
		case withdraw.StateCreated, withdraw.StateFailed:
			if err := spent.Release(in.ID); err != nil {
				logger.Error("reconcile: reservation not released", "id", in.ID, "err", err)
				continue
			}
			logger.Info("reconcile: released the reservation of a withdrawal with nothing built", "id", in.ID, "state", in.State)
		}
	}
}

// build builds every Created intent. An error is this intent's, not the
// pass's: a selector error that the engine treats as transient leaves the
// intent Created for the next pass, and a permanent one fails it.
//
// The destination is checked against the network here as well as in the
// watcher, because the two run on different hosts and this is the one that
// signs. A prefix is not part of the script, so an address carried over from
// another network is not refused by anything downstream: the node would accept
// the transaction and the coins would go to whoever holds that program on this
// chain. The intent is left Created and reported rather than failed, since a
// destination this wrong is a question for a person.
func build(ctx context.Context, store *split.DirStore, engine *withdraw.Engine, hrp string) {
	created, err := store.List(ctx, withdraw.StateCreated)
	if err != nil {
		logger.Error("build: the store could not be read", "err", err)
		return
	}
	for _, in := range created {
		if err := address.Validate(hrp, in.Address); err != nil {
			logger.Error("not built: the destination is not an address on this network", "id", in.ID, "address", in.Address, "err", err)
			continue
		}
		if err := engine.Build(ctx, in); err != nil {
			logger.Warn("not built", "id", in.ID, "state", in.State, "err", err)
			continue
		}
		logger.Info("built", "id", in.ID, "txid", in.TxID, "inputs", len(in.Inputs))
	}
}

func inputsOf(in *withdraw.Intent) []types.UTXO {
	out := make([]types.UTXO, 0, len(in.Inputs))
	for _, o := range in.Inputs {
		out = append(out, types.UTXO{TxID: o.TxID, Vout: o.Vout, Value: o.Value, Address: o.Address})
	}
	return out
}

func fatal(msg string, err error) {
	logger.Error(msg, "err", err)
	os.Exit(1)
}
