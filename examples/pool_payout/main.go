// Command pool_payout pays one payout run from a pool's hot wallet as batch
// transactions, through withdraw.Engine.
//
// A run is a file of payees and amounts and a run id. The payees are cut, in
// file order, into batches of at most -batch-size, and batch n is submitted
// under the id "<run-id>/<n>". Each batch is one transaction that pays every
// payee in it the amount given, or nothing.
//
// The run id is the idempotency key. The signed transaction of a batch is
// written to the state directory before it is sent, so running the command
// again with the same run id and the same file never pays a batch twice: a
// batch already sent is reported, a batch whose reply was lost is sent again
// with the same bytes, and a batch not yet built is built. Changing the file
// under a run id that has been used is refused (withdraw.ErrConflict).
//
// A batch spends confirmed outputs only, and its change is spendable after
// one confirmation. A run of several batches therefore needs one confirmed
// output set per batch, or one block between batches: when the wallet cannot
// fund the next batch yet, the command stops and the same command line
// continues the run after the next block.
//
// Every batch of the file is checked and registered before the first one is
// sent, so a bad payee in the last batch stops the run with nothing paid. Once
// every batch is registered the run is recorded: the payees in order, the
// batch size and the fee rate. A later start under the same run id with any of
// them changed, a payee added at the end included, is refused.
//
// One process works on a state directory at a time. The command holds
// payout.lock in it for the length of the run and refuses to start while the
// file exists. A process that was killed leaves the file behind: make sure no
// payout process is running, remove it, and run the same command line again.
//
// The command exits non-zero with the cause when the run stops. Alert on the
// exit status from the scheduler that starts it.
//
// -dry-run builds and signs every batch against a store and a spent set held
// in memory. Nothing is written and nothing is sent.
//
// Usage:
//
//	export SOQ_KEYSTORE_PASSPHRASE=...
//	export SOQ_RPC_PASSWORD=...
//	go run ./examples/pool_payout/ \
//	  -rpc-url http://127.0.0.1:33389 -rpc-user pool \
//	  -electrumx localhost:50001 \
//	  -keystore /var/lib/pool/keystore.enc \
//	  -pool-address sq1p... \
//	  -state-dir /var/lib/pool/payouts \
//	  -payouts run-2026-11-20.json -run-id run-2026-11-20 \
//	  -dry-run
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

func main() {
	var cfg runConfig
	flag.StringVar(&cfg.rpcURL, "rpc-url", "http://127.0.0.1:33389", "soqucoind RPC URL (mainnet port shown)")
	flag.StringVar(&cfg.rpcUser, "rpc-user", "", "RPC username; the password is read from SOQ_RPC_PASSWORD")
	flag.StringVar(&cfg.elxHost, "electrumx", "localhost:50001", "ElectrumX host:port")
	flag.BoolVar(&cfg.elxTLS, "electrumx-tls", false, "Use TLS to reach ElectrumX (required off this machine)")
	flag.StringVar(&cfg.keystorePath, "keystore", "", "Path to the encrypted keystore (required)")
	flag.StringVar(&cfg.poolAddress, "pool-address", "", "Hot wallet address; also receives the change (required)")
	flag.StringVar(&cfg.payoutsPath, "payouts", "", `JSON file: [{"address":"...","amount":123}], amounts in shors (required)`)
	flag.StringVar(&cfg.runID, "run-id", "", "Idempotency key of this run; never reuse it for another file (required)")
	flag.StringVar(&cfg.stateDir, "state-dir", ".", "Directory of the intent store and the spent set")
	flag.Int64Var(&cfg.feeRate, "fee-rate", types.RecommendedFeeRate, "Fee rate in shors per vByte")
	flag.IntVar(&cfg.batchSize, "batch-size", withdraw.MaxBatchPayees, "Payees per transaction, at most withdraw.MaxBatchPayees")
	flag.IntVar(&cfg.minConf, "min-confirmations", 1, "Confirmations an input needs; 288 on mainnet (the coinbase maturity) for a wallet that block rewards pay")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "Build and sign every batch; write nothing and send nothing")
	flag.BoolVar(&cfg.regtest, "regtest", false, "The node is a regtest node (the sq address prefix is shared with mainnet)")
	flag.Parse()
	cfg.rpcPass = os.Getenv("SOQ_RPC_PASSWORD")

	// Ctrl-C ends the context. A batch in flight is then a lost reply: it
	// stays Built and the next run sends the same bytes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		logger.Error("payout run stopped", "err", err)
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
	runID                    string
	stateDir                 string
	feeRate                  int64
	batchSize                int
	minConf                  int
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
	case cfg.runID == "":
		return errors.New("-run-id is required")
	case cfg.batchSize < 1 || cfg.batchSize > withdraw.MaxBatchPayees:
		return fmt.Errorf("-batch-size must be between 1 and %d", withdraw.MaxBatchPayees)
	case cfg.minConf < 1:
		return errors.New("-min-confirmations must be at least 1")
	}
	payees, err := loadPayouts(cfg.payoutsPath)
	if err != nil {
		return err
	}

	// The network comes from the hot wallet's address. Regtest shares
	// mainnet's prefix, so the operator names it. The engine refuses a payee
	// on any other network, and the node client refuses a node on another
	// chain.
	network, err := address.NetworkOf(cfg.poolAddress)
	if err != nil {
		return fmt.Errorf("pool address: %w", err)
	}
	if cfg.regtest {
		if network.Name != types.Mainnet.Name {
			return fmt.Errorf("-regtest given but the pool address is on %s", network.Name)
		}
		network = types.Regtest
	}
	changeSPK, err := address.ScriptFor(cfg.poolAddress)
	if err != nil {
		return fmt.Errorf("pool address: %w", err)
	}

	passphrase := os.Getenv("SOQ_KEYSTORE_PASSPHRASE")
	if passphrase == "" {
		return errors.New("SOQ_KEYSTORE_PASSPHRASE is not set")
	}
	keystore := keys.NewManager(cfg.keystorePath, passphrase)
	if err := keystore.Load(); err != nil {
		return fmt.Errorf("load keystore: %w", err)
	}
	if keystore.KeyCount() == 0 {
		return fmt.Errorf("keystore %s holds no keys", cfg.keystorePath)
	}

	node := rpc.NewClient(cfg.rpcURL, cfg.rpcUser, cfg.rpcPass, logger)
	node.Network = network
	// Off this machine the indexer is reached over TLS: Connect refuses a
	// plaintext connection to any other host (electrumx.ErrPlaintextRemote).
	elx := electrumx.NewClient(cfg.elxHost, 10*time.Minute, logger)
	if err := elx.SetHRP(network.HRP); err != nil {
		return err
	}
	if cfg.elxTLS {
		elx.UseTLS()
	}
	if err := elx.TrackAddresses([]string{cfg.poolAddress}); err != nil {
		return fmt.Errorf("track pool address: %w", err)
	}
	if err := elx.Connect(ctx); err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", cfg.elxHost, err)
	}
	defer elx.Stop()
	if err := elx.RefreshAll(ctx); err != nil {
		return fmt.Errorf("initial UTXO refresh: %w", err)
	}

	// The store and the spent set. A dry run keeps both in memory, so it takes
	// the same path as a real run and leaves nothing behind.
	var store withdraw.Store = withdraw.NewMemStore()
	spent := utxo.NewSpentSet("", logger)
	if !cfg.dryRun {
		// One process at a time: each holds the store in memory and writes
		// it whole, so a second one would write over the first one's records.
		if err := os.MkdirAll(cfg.stateDir, 0700); err != nil {
			return fmt.Errorf("state directory: %w", err)
		}
		unlock, err := lockStateDir(cfg.stateDir)
		if err != nil {
			return err
		}
		defer unlock()
		if store, err = withdraw.NewFileStore(filepath.Join(cfg.stateDir, "payout_intents.json")); err != nil {
			return fmt.Errorf("open intent store: %w", err)
		}
		// A spent-set file that exists but cannot be read stops the run: an
		// empty set would offer every unconfirmed spend for selection again.
		if spent, err = utxo.OpenSpentSet(filepath.Join(cfg.stateDir, "payout_spent_set.json"), logger); err != nil {
			return fmt.Errorf("spent set: %w", err)
		}
	}
	selector := utxo.NewCoinSelector(spent)

	engine := &withdraw.Engine{
		Store: store, Spent: spent, Broadcaster: node, Network: network,
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		Chain:                 withdraw.RPCChain{Client: node},
		RequiredConfirmations: types.MaxReorgDepth,
		Logger:                logger,
		// Every selection error is returned as transient, so a batch the
		// wallet cannot fund yet stays Created with nothing reserved and the
		// next run builds it. The usual cause is change that has not
		// confirmed; a wallet that never holds the total needs funding.
		Select: func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			if err := node.RequireSynced(ctx); err != nil {
				return nil, err
			}
			tip, err := node.GetBlockCount(ctx)
			if err != nil {
				return nil, err
			}
			// A one-input payment is about 1,100 vB and each further
			// ML-DSA-44 input adds about 976 vB. Budget the fee for the
			// inputs the selection takes, and ask again when it takes more.
			var selected []types.UTXO
			var want int64
			for n := 1; n <= utxo.MaxInputsPerTX; {
				want = amount + int64(1100+976*(n-1))*feeRate
				selected, _, err = selector.SelectUTXOs(elx.GetAllUTXOs(), want, cfg.minConf, tip, []string{cfg.poolAddress})
				if err != nil {
					return nil, fmt.Errorf("%w: the wallet cannot fund this batch from confirmed outputs now: %v", rpc.ErrTransient, err)
				}
				if len(selected) <= n {
					break
				}
				n = len(selected)
			}
			// The node must still have every input; it also drops a block
			// reward that has not matured. A selection that no longer covers
			// the batch waits for the next run instead of failing at the
			// builder.
			verified, err := node.VerifyAndFilterUTXOs(ctx, selected, elx.EvictUTXO, elx.SetAssetType)
			if err != nil {
				return nil, err
			}
			var have int64
			for _, u := range verified {
				have += u.Value
			}
			if have < want {
				return nil, fmt.Errorf("%w: the node holds %d of the %d shors selected; a block reward needs the coinbase maturity before it is spent", rpc.ErrTransient, have, want)
			}
			return verified, nil
		},
		BuildSign: func(_ context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
			spk, err := address.ScriptFor(to)
			if err != nil {
				return "", "", err
			}
			return tx.BuildAndSign(inputs, spk, amount, changeSPK, feeRate, keystore)
		},
		BuildSignBatch: func(_ context.Context, inputs []types.UTXO, batch []withdraw.Payee, feeRate int64) (string, string, error) {
			outputs := make([]tx.TxOutput, len(batch))
			for i, p := range batch {
				spk, err := address.ScriptFor(p.Address)
				if err != nil {
					return "", "", err
				}
				outputs[i] = tx.TxOutput{Value: p.Amount, ScriptPubKey: spk}
			}
			return tx.BuildAndSignBatch(inputs, outputs, changeSPK, feeRate, keystore)
		},
	}

	// Before anything is sent: the file is the run this id was first started
	// with, and every batch of it passes the engine's rules and is registered.
	digest := runDigest(payees, cfg.batchSize, cfg.feeRate)
	manifest := "run_" + digestOf([]byte(cfg.runID))[:16] + ".json" // in the state directory
	if !cfg.dryRun {
		if err := checkManifest(cfg.stateDir, manifest, cfg.runID, digest); err != nil {
			return err
		}
	}
	type registered struct {
		id     string
		intent *withdraw.Intent
		payees int
	}
	var run []registered
	for n := 0; n*cfg.batchSize < len(payees); n++ {
		batch := payees[n*cfg.batchSize : min((n+1)*cfg.batchSize, len(payees))]
		id := fmt.Sprintf("%s/%03d", cfg.runID, n+1)
		// The same id with other payees is ErrConflict; a payee the engine
		// refuses is ErrInvalidIntent and names the payee. Neither records
		// the batch, and no batch of the run has been sent.
		intent, _, err := engine.SubmitBatch(ctx, id, batch, cfg.feeRate)
		if err != nil {
			return fmt.Errorf("submit %s: %w; nothing of this run was sent by this command", id, err)
		}
		run = append(run, registered{id, intent, len(batch)})
	}

	if cfg.dryRun {
		logger.Info("dry run: every batch is built and signed; nothing is written and nothing is sent")
		for _, b := range run {
			if err := engine.Build(ctx, b.intent); err != nil {
				return fmt.Errorf("build %s: %w", b.id, err)
			}
			logger.Info("would broadcast", "batch", b.id, "payees", b.payees, "shors", b.intent.Amount,
				"txid", b.intent.TxID, "inputs", len(b.intent.Inputs), "bytes", len(b.intent.RawHex)/2)
		}
		return nil
	}
	if err := writeManifest(cfg.stateDir, manifest, cfg.runID, digest); err != nil {
		return err
	}

	// The node is on this chain and has caught up before any saved bytes go
	// to it. Then, as after any restart: spends the store knows are marked
	// again and built transactions are sent again with the same bytes. Do not
	// pay while Recover fails; withdraw.ErrHeld is a batch an operator
	// resolves first.
	if err := node.RequireSynced(ctx); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	if err := engine.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}

	for i, b := range run {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped before batch %d of %d: %w", i+1, len(run), err)
		}
		log := logger.With("batch", b.id, "payees", b.payees, "shors", b.intent.Amount)
		intent, err := engine.Process(ctx, b.id)
		if err != nil {
			// Created: nothing was built; the next run builds it. Built: the
			// bytes are saved and may be in a mempool; the next run sends the
			// same bytes. Failed: nothing this run sent is outstanding for
			// it; read the cause and pay those payees under a new run id.
			// Later batches spend from the same wallet, so the run stops here.
			state := withdraw.State("unknown")
			if intent != nil {
				state = intent.State
			}
			return fmt.Errorf("batch %s is %s: %w; run the same command again to continue", b.id, state, err)
		}
		if intent.State == withdraw.StateBroadcast {
			if err := engine.UpdateConfirmations(ctx, intent); err != nil {
				log.Warn("confirmations not read", "err", err)
			}
		}
		log.Info("batch sent", "state", intent.State, "txid", intent.TxID, "confirmations", intent.Confirmations)
	}
	logger.Info("run complete", "run", cfg.runID, "batches", len(run), "payees", len(payees))
	return nil
}

// lockStateDir creates payout.lock in dir, which no other run may hold, and
// returns the call that removes it.
func lockStateDir(dir string) (unlock func(), err error) {
	path := filepath.Join(dir, "payout.lock")
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile("payout.lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil, fmt.Errorf("%s exists: another payout process works on this directory, or one was killed; when none is running, remove the file and run the same command again", path)
	}
	if err != nil {
		return nil, fmt.Errorf("lock the state directory: %w", err)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	return func() { _ = os.Remove(path) }, nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runDigest is what a run id stands for: the payees in file order with their
// amounts, the batch size that cuts them, and the fee rate.
func runDigest(payees []withdraw.Payee, batchSize int, feeRate int64) string {
	b, _ := json.Marshal(struct {
		Payees    []withdraw.Payee
		BatchSize int
		FeeRate   int64
	}{payees, batchSize, feeRate})
	return digestOf(b)
}

type manifestFile struct {
	RunID  string `json:"run_id"`
	Digest string `json:"digest"`
}

// checkManifest refuses a run id that was recorded with another file, batch
// size or fee rate. A run id with no record passes: it has not registered
// every batch yet.
func checkManifest(dir, name, runID, digest string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the run record: %w", err)
	}
	var m manifestFile
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse the run record %s: %w", name, err)
	}
	if m.RunID != runID || m.Digest != digest {
		return fmt.Errorf("%w: run %q was recorded with other payees, amounts, order, batch size or fee rate; a changed run takes a new run id", withdraw.ErrConflict, runID)
	}
	return nil
}

// writeManifest records the run, synced to disk before the first send.
func writeManifest(dir, name, runID, digest string) error {
	data, _ := json.Marshal(manifestFile{RunID: runID, Digest: digest})
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	tmp := name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("write the run record: %w", err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		return fmt.Errorf("write the run record: %w", err)
	}
	return nil
}

// loadPayouts reads the run's file. The engine applies every rule to each
// payee at submission; the file is only required to parse and to hold one.
func loadPayouts(path string) ([]withdraw.Payee, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read payouts: %w", err)
	}
	var payees []withdraw.Payee
	if err := json.Unmarshal(data, &payees); err != nil {
		return nil, fmt.Errorf("parse payouts %s: %w", path, err)
	}
	if len(payees) == 0 {
		return nil, fmt.Errorf("payouts file %s is empty", path)
	}
	return payees, nil
}
