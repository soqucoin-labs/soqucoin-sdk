// Command signer is the process in examples/exchange_split that holds the
// key. It has no node credential, no indexer address and no listening socket,
// so the shared directory is its only input and the only thing it does with
// that input is turn Created intents into Built ones. The host still reaches
// the directory, over a mount if the three processes are on three machines,
// and that mount is the whole of its exposure.
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
//     store holds as sent are recorded as spent rather than reserved,
//     reservations of intents that are Created or Failed are released, and
//     reservations of Built intents are renewed. The store is the durable truth about spends;
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
	store, err := cfg.dir.Open()
	if err != nil {
		return err
	}
	// Load rather than LoadOrCreate: a signer that silently creates an empty keystore
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
			return selectForFee(selector, snap, amount, feeRate, cfg.minConf)
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
		// Nothing is selected in a pass whose reconciliation did not finish.
		// The spent set is this process's only record of what the broadcaster
		// has sent, so selecting against a set that may be behind the store is
		// how one input ends up under two signatures.
		if err := reconcile(ctx, store, spent, engine.ReservationTTL); err != nil {
			logger.Error("reconcile failed; nothing is built this pass", "err", err)
			wait(ctx, cfg.interval)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
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
		wait(ctx, cfg.interval)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// wait sleeps for d or until ctx ends.
func wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
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
//   - Broadcast: the inputs are spent for good, whatever this file said
//     before. This is the one that matters. Without it the reservation expires
//     by its TTL while the transaction sits in a mempool and the next
//     selection offers the same inputs to another withdrawal. A withdrawal
//     already recorded here as a spend is left alone, since marking rewrites
//     the whole file.
//   - Confirmed: the entries this set already holds are flipped to confirmed,
//     so they can be pruned by age. Nothing is created: an input of a
//     confirmed withdrawal cannot come back.
//   - Built: the transaction is signed and waiting for the broadcaster, so the
//     reservation is renewed rather than left to expire.
//   - Created or Failed: nothing signed exists, so a reservation held for one
//     is a crash between reserving and saving, and the coins are free.
func reconcile(ctx context.Context, store withdraw.Store, spent *utxo.SpentSet, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = withdraw.DefaultReservationTTL
	}
	// Built is listed first and the sent states second, which is the order
	// that leaves no gap. These are two readings of a directory another
	// process is writing, so an intent can move between them: promoted after
	// the first listing it appears in both, is re-reserved by the first pass
	// and recorded as a spend by the second. In the other order it appeared in
	// neither, and a signer whose reservation for it had expired could select
	// its input while the broadcaster was sending it.
	built, err := store.List(ctx, withdraw.StateBuilt)
	if err != nil {
		return fmt.Errorf("list built withdrawals: %w", err)
	}
	for _, in := range built {
		if in.NodeTxID != "" {
			// The node accepted other bytes for this withdrawal. Those inputs
			// are spent for good and must not sit on a reservation that
			// expires, which is how Recover records the same case.
			if err := recordSpend(spent, in, in.NodeTxID); err != nil {
				return fmt.Errorf("record the spend of held withdrawal %s: %w", in.ID, err)
			}
			continue
		}
		if err := spent.Reserve(inputsOf(in), in.ID, ttl); err != nil {
			// Either the write failed or another withdrawal holds one of these
			// inputs, which means two signed transactions over one input. Both
			// stop this pass: selecting while either is true is how the second
			// payment gets made.
			return fmt.Errorf("reserve the inputs of built withdrawal %s: %w", in.ID, err)
		}
	}
	sent, err := store.List(ctx, withdraw.StateBroadcast, withdraw.StateConfirmed)
	if err != nil {
		return fmt.Errorf("list sent withdrawals: %w", err)
	}
	for _, in := range sent {
		inputs := inputsOf(in)
		if in.State == withdraw.StateConfirmed {
			// Confirmed on chain: flip the entries this set already holds so
			// the hour-old ones can be pruned. It writes only on a change and
			// creates nothing. An input of a confirmed withdrawal cannot come
			// back, and the watcher's snapshot is read from the node, so it
			// cannot offer one either.
			if err := spent.ConfirmSpentAll(inputs); err != nil {
				return fmt.Errorf("mark the inputs of confirmed withdrawal %s: %w", in.ID, err)
			}
			continue
		}
		txid := in.TxID
		if in.NodeTxID != "" {
			txid = in.NodeTxID // the node accepted other bytes; those are the spend
		}
		if err := recordSpend(spent, in, txid); err != nil {
			return fmt.Errorf("record the spend of withdrawal %s: %w", in.ID, err)
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
		case withdraw.StateCreated, withdraw.StateFailed:
			if err := spent.Release(in.ID); err != nil {
				return fmt.Errorf("release the reservation of withdrawal %s: %w", in.ID, err)
			}
			logger.Info("reconcile: released the reservation of a withdrawal with nothing built", "id", in.ID, "state", in.State)
		}
	}
	return nil
}

// recordSpend records an intent's inputs as spent under txid, unless this set
// already holds them as a spend for it. Marking rewrites the whole file, so
// without the test the set is rewritten once per historic withdrawal on every
// pass. The reservation test is asked of the set rather than of a list read
// earlier, so a withdrawal re-reserved a moment ago is not mistaken for one
// already recorded.
func recordSpend(spent *utxo.SpentSet, in *withdraw.Intent, txid string) error {
	inputs := inputsOf(in)
	if !holdsReservation(spent, in.ID) && allSpent(spent, inputs) {
		return nil
	}
	if err := spent.MarkBroadcastFor(inputs, txid, in.ID); err != nil {
		return err
	}
	logger.Info("reconcile: inputs of a sent withdrawal are recorded as spent", "id", in.ID, "txid", txid)
	return nil
}

// holdsReservation reports whether the set holds a reservation for id, as
// opposed to a spend.
func holdsReservation(spent *utxo.SpentSet, id string) bool {
	for _, held := range spent.ReservedIntents() {
		if held == id {
			return true
		}
	}
	return false
}

// selectForFee budgets the fee against the number of inputs the selection
// actually takes.
//
// The fee of an ML-DSA payment is dominated by its inputs, so the two obvious
// targets are both wrong. One input's worth can come back with three, whose
// fee is larger than the budget they were chosen under. The hard maximum of
// utxo.MaxInputsPerTX asks for about 77,100 vB, which at the recommended rate
// is 0.77 SOQ of headroom a payout does not need: a wallet with 25 SOQ in one
// output cannot then pay 24.99, and the last 0.77 SOQ of any hot wallet is
// unspendable. So it asks for n inputs' worth and asks again when the answer
// needs more; n only grows, and the selector's own cap bounds the loop.
func selectForFee(selector *utxo.CoinSelector, snap split.Snapshot, amount, feeRate int64, minConf int) ([]types.UTXO, error) {
	candidates := snap.Spendable()
	for n := 1; n <= utxo.MaxInputsPerTX; {
		selected, _, err := selector.SelectUTXOs(candidates, amount+vsizeFor(n)*feeRate, minConf, snap.Tip, []string{snap.HotAddress})
		if err != nil {
			return nil, err
		}
		if len(selected) <= n {
			return selected, nil
		}
		n = len(selected)
	}
	return nil, fmt.Errorf("no selection fits the fee of %d inputs", utxo.MaxInputsPerTX)
}

// vsizeFor is the vsize of a payment with n inputs and two outputs, measured:
// about 1,073 vB at one input and about 976 vB for each further ML-DSA-44
// input (docs/EXCHANGE_INTEGRATION.md, Transaction Size). Rounded up, as a fee
// target should be.
func vsizeFor(n int) int64 { return 1100 + 976*int64(n-1) }

// build builds every Created intent. An error stops that intent and not
// the pass: a selector error that the engine treats as transient leaves the
// intent Created for the next pass, and a permanent one fails it.
//
// The destination is checked against the network here as well as in the
// watcher, because the two run on different hosts and this is the one that
// signs. A prefix is not part of the script, so an address carried over from
// another network is not refused by anything downstream: the node would accept
// the transaction and the coins would go to whoever holds that program on this
// chain. The intent is left Created and reported rather than failed, since a
// destination this wrong is a question for a person.
func build(ctx context.Context, store withdraw.Store, engine *withdraw.Engine, hrp string) {
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

// allSpent reports whether the set already holds every input. It is asked
// only about an intent that holds no reservation here, so an entry it finds is
// a spend and not this intent's own reservation.
func allSpent(spent *utxo.SpentSet, inputs []types.UTXO) bool {
	for _, u := range inputs {
		if !spent.IsSpent(u.TxID, u.Vout) {
			return false
		}
	}
	return len(inputs) > 0
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
