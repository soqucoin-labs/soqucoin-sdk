// Command watcher is the process in examples/exchange_split that talks to the
// chain. It holds the node's RPC credential and the indexer's address, and it
// holds no key, so nothing it can be made to do results in a signature.
//
// Two jobs, both through the shared directory:
//
//  1. Requests into intents. A JSON file in requests/ becomes a withdrawal
//     intent through withdraw.Engine.Submit, which is idempotent on the id, so
//     a request seen twice is one withdrawal.
//  2. The snapshot. Every pass it publishes what may be spent: the indexer's
//     view of the hot address, read back from the node output by output, with
//     the node's tip height and the time the node answered. The signer has no
//     node and no indexer of its own and selects from this file alone, so it
//     is written only when the indexer's answer for the address is fresh and
//     the node is caught up.
//
// Usage:
//
//	go run ./examples/exchange_split/watcher -dir state -network stagenet \
//	    -hot ssq1p... -electrumx 127.0.0.1:50001
//
// Environment: SOQ_RPC_USER, SOQ_RPC_PASSWORD and SOQ_RPC_URL (default is the
// network's own RPC port on localhost).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

func main() {
	dir := flag.String("dir", "", "the shared directory (required)")
	networkName := flag.String("network", "", "mainnet, stagenet or regtest (required)")
	hot := flag.String("hot", "", "the hot wallet address the snapshot covers (required)")
	elxHost := flag.String("electrumx", "127.0.0.1:50001", "ElectrumX host:port")
	elxTLS := flag.Bool("electrumx-tls", false, "connect to ElectrumX over TLS (required off this machine)")
	interval := flag.Duration("interval", 15*time.Second, "how often to accept requests and publish a snapshot")
	reconcile := flag.Duration("reconcile", 2*time.Minute, "full indexer reconcile interval, the safety net for a missed notification")
	maxCacheAge := flag.Duration("max-cache-age", 90*time.Second, "refuse to publish a snapshot when the indexer's answer for the address is older than this")
	minConf := flag.Int64("min-confirmations", 1, "confirmations an output needs before it enters the snapshot")
	flag.Parse()
	if *dir == "" || *networkName == "" || *hot == "" {
		flag.Usage()
		os.Exit(2)
	}
	network, err := split.NetworkFor(*networkName)
	if err != nil {
		fatal("network", err)
	}
	if err := address.Validate(network.HRP, *hot); err != nil {
		fatal("hot address", err)
	}
	if !*elxTLS && !split.LoopbackHost(*elxHost) {
		fatal("electrumx", fmt.Errorf("%s is not on this machine; pass -electrumx-tls", *elxHost))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, config{
		dir:         split.Dir(*dir),
		network:     network,
		hot:         *hot,
		elxHost:     *elxHost,
		elxTLS:      *elxTLS,
		interval:    *interval,
		reconcile:   *reconcile,
		maxCacheAge: *maxCacheAge,
		minConf:     *minConf,
	}); err != nil && !errors.Is(err, context.Canceled) {
		fatal("watcher", err)
	}
}

type config struct {
	dir         split.Dir
	network     types.Network
	hot         string
	elxHost     string
	elxTLS      bool
	interval    time.Duration
	reconcile   time.Duration
	maxCacheAge time.Duration
	minConf     int64
}

func run(ctx context.Context, cfg config) error {
	if err := split.EnsureDir(string(cfg.dir)); err != nil {
		return err
	}
	if err := split.EnsureDir(cfg.dir.Requests()); err != nil {
		return err
	}
	store, err := split.OpenDirStore(cfg.dir.Intents())
	if err != nil {
		return err
	}

	node := rpc.NewClient(rpcURL(cfg.network), os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"), logger)
	node.Network = cfg.network // refuse a node on another chain
	if err := node.RequireSynced(ctx); err != nil {
		return fmt.Errorf("node: %w", err)
	}

	elx := electrumx.NewClient(cfg.elxHost, cfg.reconcile, logger)
	if err := elx.SetHRP(cfg.network.HRP); err != nil {
		return err
	}
	if cfg.elxTLS {
		elx.UseTLS()
	}
	if err := elx.TrackAddresses([]string{cfg.hot}); err != nil {
		return err
	}
	if err := elx.Connect(ctx); err != nil {
		return fmt.Errorf("connect to electrumx %s: %w", cfg.elxHost, err)
	}
	defer elx.Stop()
	elx.Start(ctx)

	// Submit is the only engine method this process may call. The fields the
	// others need are absent on purpose: this engine has no spent set, no
	// signer and no broadcaster, because a reservation taken here would live
	// in a file no other process reads.
	engine := &withdraw.Engine{Store: store, Logger: logger}

	logger.Info("watcher started", "dir", string(cfg.dir), "hot", cfg.hot, "network", cfg.network.Name)
	for {
		accept(ctx, cfg, engine)
		publish(ctx, cfg, elx, node)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.interval):
		}
	}
}

// accept turns every request file into an intent. A request that cannot
// become one is logged and left where it is: this process does not delete the
// exchange's own files, and a request that is wrong stays visible.
func accept(ctx context.Context, cfg config, engine *withdraw.Engine) {
	reqs, errs := split.ReadRequests(cfg.dir)
	for _, err := range errs {
		logger.Error("request refused", "err", err)
	}
	for _, r := range reqs {
		if err := address.Validate(cfg.network.HRP, r.Address); err != nil {
			logger.Error("request refused", "id", r.ID, "err", err)
			continue
		}
		_, created, err := engine.Submit(ctx, r.ID, r.Address, r.AmountShors, r.FeeRate)
		if err != nil {
			// ErrConflict means this id was already used for a different
			// payment. It stays on disk and is never overwritten: one of the
			// two is a mistake and only the exchange knows which.
			logger.Error("submit", "id", r.ID, "err", err)
			continue
		}
		if err := split.Accept(cfg.dir, r.ID); err != nil {
			// The intent exists; the request will be submitted again next
			// pass and Submit will answer with the intent that exists.
			logger.Warn("request accepted but not moved", "id", r.ID, "err", err)
		}
		logger.Info("request is an intent", "id", r.ID, "new", created, "shors", r.AmountShors)
	}
}

// publish writes the snapshot, or writes nothing and says why.
//
// Three conditions, and the snapshot is withheld unless all three hold: the
// indexer has answered for the address recently enough, the node is caught up
// (VerifyAndFilterUTXOs refuses to act on a syncing node), and every output
// survives being read back from the node. A withheld snapshot stops the signer
// building, which is the safe direction: the alternative is signing against a
// view no one has confirmed.
func publish(ctx context.Context, cfg config, elx *electrumx.Client, node *rpc.Client) {
	at, err := elx.LastRefreshOf(cfg.hot)
	if err != nil {
		logger.Warn("snapshot withheld: the indexer's last answer for the address was an error", "err", err)
		return
	}
	if age := time.Since(at); age > cfg.maxCacheAge {
		logger.Warn("snapshot withheld: the indexer's answer is stale", "age", age.Round(time.Second), "limit", cfg.maxCacheAge)
		return
	}

	tip, err := node.GetBlockCount(ctx)
	if err != nil {
		logger.Warn("snapshot withheld: no tip height", "err", err)
		return
	}
	// Confirmed, unspent, native SOQ outputs only. An output the indexer has
	// not seen confirm has height 0, which no selector can spend anyway, and
	// it would make the snapshot fail its own consistency check.
	var candidates []types.UTXO
	for _, u := range elx.GetUTXOs(cfg.hot) {
		switch {
		case u.Height <= 0, tip-u.Height+1 < cfg.minConf:
			continue
		case u.SpentPending, u.AssetType != types.AssetTypeSOQ:
			continue
		}
		candidates = append(candidates, u)
	}
	verified, err := node.VerifyAndFilterUTXOs(ctx, candidates, elx.EvictUTXO, elx.SetAssetType)
	if err != nil {
		logger.Warn("snapshot withheld: the node could not confirm the outputs", "err", err)
		return
	}

	snap := split.Snapshot{At: time.Now().UTC(), Tip: tip, HotAddress: cfg.hot, Outputs: split.OutputsFrom(verified)}
	if err := split.WriteSnapshot(cfg.dir, snap); err != nil {
		logger.Error("snapshot not written", "err", err)
		return
	}
	var total int64
	for _, u := range verified {
		total += u.Value
	}
	logger.Info("snapshot published", "outputs", len(verified), "shors", total, "tip", tip,
		"dropped_by_the_node", len(candidates)-len(verified))
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
