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
	"github.com/soqucoin-labs/soqucoin-sdk/resilience"
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
	flag.StringVar(&cfg.webhookURL, "webhook", "", "Slack webhook URL for alerts (optional)")
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

	// Stop a run that is failing for a systemic reason. One refused request
	// never counts; three systemic failures in a row hold for 15 minutes.
	cb := resilience.NewCircuitBreaker(3, 15*time.Minute, logger)
	resilience.NewAlerter(cfg.webhookURL, logger).WireToCircuitBreaker(cb)

	if cfg.dryRun {
		logger.Info("dry run: every batch is built and signed; nothing is written and nothing is sent")
	} else if err := engine.Recover(ctx); err != nil {
		// After a restart: spends the store knows are marked again and built
		// transactions are sent again with the same bytes. Do not pay while
		// this fails; withdraw.ErrHeld is a batch an operator resolves first.
		return fmt.Errorf("recover: %w", err)
	}

	batches := (len(payees) + cfg.batchSize - 1) / cfg.batchSize
	for n := 0; n < batches; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped before batch %d of %d: %w", n+1, batches, err)
		}
		batch := payees[n*cfg.batchSize : min((n+1)*cfg.batchSize, len(payees))]
		id := fmt.Sprintf("%s/%03d", cfg.runID, n+1)
		var total int64
		for _, p := range batch {
			total += p.Amount
		}
		log := logger.With("batch", id, "payees", len(batch), "shors", total)

		// The same id with other payees is ErrConflict; a payee the engine
		// refuses is ErrInvalidIntent and names the payee. Nothing is recorded
		// for either.
		intent, _, err := engine.SubmitBatch(ctx, id, batch, cfg.feeRate)
		if err != nil {
			return fmt.Errorf("submit %s: %w", id, err)
		}
		if cfg.dryRun {
			if err := engine.Build(ctx, intent); err != nil {
				return fmt.Errorf("build %s: %w", id, err)
			}
			log.Info("would broadcast", "txid", intent.TxID, "inputs", len(intent.Inputs), "bytes", len(intent.RawHex)/2)
			continue
		}

		if err := cb.Allow(); err != nil {
			return fmt.Errorf("circuit breaker stopped the run at %s: %w", id, err)
		}
		intent, err = engine.Process(ctx, id)
		cb.RecordResult(err)
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
			return fmt.Errorf("batch %s is %s: %w; run the same command again to continue", id, state, err)
		}
		if intent.State == withdraw.StateBroadcast {
			if err := engine.UpdateConfirmations(ctx, intent); err != nil {
				log.Warn("confirmations not read", "err", err)
			}
		}
		log.Info("batch sent", "state", intent.State, "txid", intent.TxID, "confirmations", intent.Confirmations)
	}
	logger.Info("run complete", "run", cfg.runID, "batches", batches, "payees", len(payees))
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
