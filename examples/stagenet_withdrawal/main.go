// Command stagenet_withdrawal produces a withdrawal on stagenet through
// withdraw.Engine against your own node and prints the record that
// docs/VERIFICATION.md carries: txid, block, sizes and witness stack lengths.
//
// It runs in two steps. The first creates a keystore holding two keys
// derived from a fresh master secret with keys.DeriveSeed and keys.FromSeed,
// and prints their addresses; fund the first from the stagenet faucet. The
// second spends the funded outputs to the second address and waits for the
// confirmations asked for.
//
//	go run ./examples/stagenet_withdrawal -dir state -init
//	go run ./examples/stagenet_withdrawal -dir state \
//	    -inputs txid:vout,txid:vout,txid:vout -to ssq1p... -amount 1200
//
// Environment: SOQ_KEYSTORE_PASSPHRASE, SOQ_RPC_USER, SOQ_RPC_PASSWORD and
// SOQ_RPC_URL (default http://127.0.0.1:28332, the stagenet RPC port). The
// node needs txindex=1 for the confirmation step (docs/EXCHANGE_INTEGRATION.md,
// What you run).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

var network = types.Stagenet

// logger is shared by the program and the SDK components it builds.
var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

func fatal(msg string, err error) {
	logger.Error(msg, "err", err)
	os.Exit(1)
}

func main() {
	dir := flag.String("dir", "stagenet_withdrawal_state", "directory for the keystore, spent set and intent store")
	initKeys := flag.Bool("init", false, "create the keystore and print the funding and destination addresses")
	inputs := flag.String("inputs", "", "comma-separated txid:vout outpoints that pay the funding address")
	to := flag.String("to", "", "destination address")
	amountSOQ := flag.Int64("amount", 0, "payment in whole SOQ")
	confirmations := flag.Int64("confirmations", 1, "confirmations to wait for before printing the record")
	flag.Parse()

	passphrase := os.Getenv("SOQ_KEYSTORE_PASSPHRASE")
	if passphrase == "" {
		fatal("SOQ_KEYSTORE_PASSPHRASE is not set", nil)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fatal("state directory", err)
	}
	// Load refuses a missing file; only -init, the first run, may create one.
	keystore := keys.NewManager(filepath.Join(*dir, "keys.enc"), passphrase)
	if *initKeys {
		if err := keystore.LoadOrCreate(); err != nil {
			fatal("create keystore", err)
		}
		if err := createKeys(keystore); err != nil {
			fatal("create keys", err)
		}
		return
	}
	if *inputs == "" || *to == "" || *amountSOQ <= 0 || *confirmations < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := keystore.Load(); err != nil {
		fatal("load keystore", err)
	}
	// Ctrl-C ends the context. A broadcast interrupted by it is a lost reply:
	// the intent stays Built and the next run's Recover sends the same bytes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *dir, keystore, *inputs, *to, *amountSOQ*types.ShorsPerSOQ, *confirmations); err != nil {
		fatal("withdrawal", err)
	}
}

// createKeys derives two keys from a fresh 32-byte master secret: the first
// index that yields a valid key is the funding address, the next one the
// destination. The master and every seed are zeroed before returning; the
// encrypted keystore is the only copy on disk.
func createKeys(keystore *keys.Manager) error {
	if keystore.KeyCount() > 0 {
		return errors.New("the keystore already holds keys; use another -dir")
	}
	master := make([]byte, keys.SeedSize)
	if _, err := rand.Read(master); err != nil {
		return err
	}
	defer zero(master)

	var addrs []string
	for index := uint32(0); len(addrs) < 2; index++ {
		seed, err := keys.DeriveSeed(master, network.HRP, index)
		if err != nil {
			return err
		}
		kp, err := keys.FromSeed(network.HRP, seed)
		seed = [keys.SeedSize]byte{}
		if errors.Is(err, keys.ErrInvalidPublicKey) {
			logger.Info("index derives the node's invalid-key marker, skipped", "index", index)
			continue
		}
		if err != nil {
			return err
		}
		if err := keystore.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			return err
		}
		zero(kp.PrivateKey)
		addrs = append(addrs, kp.Address)
	}
	if err := keystore.Save(); err != nil {
		return err
	}
	fmt.Printf("funding address:     %s\ndestination address: %s\n", addrs[0], addrs[1])
	return nil
}

func run(ctx context.Context, dir string, keystore *keys.Manager, inputList, to string, amount, required int64) error {
	if err := address.Validate(network.HRP, to); err != nil {
		return err
	}
	node := rpc.NewClient(rpcURL(), os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"), logger)
	node.Network = network
	if err := node.RequireSynced(ctx); err != nil {
		return err
	}

	funding, hot, err := fundingUTXOs(ctx, node, keystore, inputList)
	if err != nil {
		return err
	}
	changeSPK, err := address.ScriptFor(hot)
	if err != nil {
		return err
	}

	spent, err := utxo.OpenSpentSet(filepath.Join(dir, "spent_set.json"), logger)
	if err != nil {
		return err
	}
	selector := utxo.NewCoinSelector(spent)
	store, err := withdraw.NewFileStore(filepath.Join(dir, "withdrawals.json"))
	if err != nil {
		return err
	}

	engine := &withdraw.Engine{
		Store:                 store,
		Spent:                 spent,
		Broadcaster:           node,
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		RequiredConfirmations: required,
		Logger:                logger,
		// ReservationTTL left at its default, withdraw.DefaultReservationTTL.
		Select: func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			if err := node.RequireSynced(ctx); err != nil {
				return nil, err
			}
			tip, err := node.GetBlockCount(ctx)
			if err != nil {
				return nil, err
			}
			// Budget the fee against vsize: a one-input, two-output payment is about
			// 1,073 vB and each further ML-DSA-44 input adds about 976 vB.
			budget := amount + (1100+976*int64(len(funding)))*feeRate
			selected, _, err := selector.SelectUTXOs(funding, budget, 1, tip, []string{hot})
			if err != nil {
				return nil, err
			}
			return node.VerifyAndFilterUTXOs(ctx, selected, nil, nil)
		},
		BuildSign: func(_ context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
			recipientSPK, err := address.ScriptFor(to)
			if err != nil {
				return "", "", err
			}
			return tx.BuildAndSign(inputs, recipientSPK, amount, changeSPK, feeRate, keystore)
		},
	}
	if err := engine.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}

	// The idempotency key is derived from the first input, so running the
	// program again for the same outputs resumes the same intent.
	id := "verification-" + funding[0].TxID[:16]
	if _, _, err := engine.Submit(ctx, id, to, amount, types.RecommendedFeeRate); err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	intent, err := engine.Process(ctx, id)
	if err != nil {
		if intent != nil {
			return fmt.Errorf("process (state %s): %w", intent.State, err)
		}
		return fmt.Errorf("process: %w", err)
	}
	logger.Info("broadcast", "intent", id, "txid", intent.TxID)

	for intent.State != withdraw.StateConfirmed {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
		if err := engine.UpdateConfirmations(ctx, intent); err != nil {
			logger.Warn("confirmations", "err", err)
			continue
		}
		logger.Info("confirmations", "have", intent.Confirmations, "want", required)
	}
	return printRecord(ctx, node, intent.TxID)
}

// fundingUTXOs reads each outpoint from the node and returns them as inputs,
// with the one keystore address they all pay. An outpoint that is spent,
// unconfirmed or paid to any other script is refused.
func fundingUTXOs(ctx context.Context, node *rpc.Client, keystore *keys.Manager, list string) ([]types.UTXO, string, error) {
	scripts := map[string]string{}
	for _, addr := range keystore.GetAddresses() {
		spk, err := address.ScriptFor(addr)
		if err != nil {
			return nil, "", err
		}
		scripts[hex.EncodeToString(spk)] = addr
	}
	if len(scripts) == 0 {
		return nil, "", errors.New("the keystore holds no key; run -init first")
	}
	tip, err := node.GetBlockCount(ctx)
	if err != nil {
		return nil, "", err
	}

	var out []types.UTXO
	hot := ""
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			return nil, "", fmt.Errorf("outpoint %q: want txid:vout", item)
		}
		vout, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return nil, "", fmt.Errorf("outpoint %q: %w", item, err)
		}
		txout, err := node.GetTxOut(ctx, parts[0], uint32(vout), false)
		if err != nil {
			return nil, "", err
		}
		if txout == nil {
			return nil, "", fmt.Errorf("%s is not an unspent confirmed output", item)
		}
		addr, ok := scripts[txout.ScriptPubKey.Hex]
		if !ok {
			return nil, "", fmt.Errorf("%s does not pay an address in the keystore", item)
		}
		if hot != "" && addr != hot {
			return nil, "", errors.New("all inputs must pay the same address")
		}
		hot = addr
		out = append(out, types.UTXO{
			TxID:    parts[0],
			Vout:    uint32(vout),
			Value:   txout.Value,
			Height:  tip - txout.Confirmations + 1,
			Address: addr,
		})
	}
	return out, hot, nil
}

// printRecord prints the fields docs/VERIFICATION.md records, read back from
// the node so every figure is the node's own.
func printRecord(ctx context.Context, node *rpc.Client, txid string) error {
	raw, err := node.Call(ctx, "getrawtransaction", txid, true)
	if err != nil {
		return err
	}
	var decoded struct {
		TxID      string `json:"txid"`
		Size      int    `json:"size"`
		VSize     int    `json:"vsize"`
		BlockHash string `json:"blockhash"`
		Vin       []struct {
			TxID    string   `json:"txid"`
			Vout    uint32   `json:"vout"`
			Witness []string `json:"txinwitness"`
		} `json:"vin"`
		Vout []struct {
			Value json.Number `json:"value"` // the node's decimal SOQ figure, printed as received
		} `json:"vout"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var header struct {
		Height int64 `json:"height"`
		Time   int64 `json:"time"`
	}
	if decoded.BlockHash != "" {
		raw, err := node.Call(ctx, "getblockheader", decoded.BlockHash)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
	}

	fmt.Printf("txid            %s\n", decoded.TxID)
	fmt.Printf("block           %s (height %d, time %s)\n", decoded.BlockHash, header.Height, time.Unix(header.Time, 0).UTC().Format(time.RFC3339))
	fmt.Printf("inputs/outputs  %d in, %d out\n", len(decoded.Vin), len(decoded.Vout))
	fmt.Printf("size/vsize      %d bytes / %d vB\n", decoded.Size, decoded.VSize)
	for i, in := range decoded.Vin {
		var sizes []string
		for _, w := range in.Witness {
			sizes = append(sizes, strconv.Itoa(len(w)/2))
		}
		fmt.Printf("input %d         %s:%d witness [%s]\n", i, in.TxID, in.Vout, strings.Join(sizes, ", "))
	}
	for i, out := range decoded.Vout {
		fmt.Printf("output %d        %s SOQ\n", i, out.Value)
	}
	return nil
}

func rpcURL() string {
	if u := os.Getenv("SOQ_RPC_URL"); u != "" {
		return u
	}
	return fmt.Sprintf("http://127.0.0.1:%d", network.RPCPort)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
