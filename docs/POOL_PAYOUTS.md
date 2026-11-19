# Pool Payouts

A payout run is a list of miners and amounts. `withdraw.Engine` pays a run as batch transactions:
one transaction pays up to 500 miners, and its signed bytes are on disk before they are sent. A
crash, a lost reply or a second start of the same job never pays a miner twice.

| You have | You call | You get |
|----------|----------|---------|
| A run id and up to 500 payees | `Engine.SubmitBatch` | One durable intent, or a refusal that names the payee |
| A registered batch | `Engine.Process` | One signed transaction, saved, then sent |
| A sent batch | `Engine.UpdateConfirmations` | The confirmation count, and `Confirmed` at your depth |
| A restart | `Engine.Recover` | Every spend marked again, every saved transaction sent again with the same bytes |

## What you run

| Component | Setting | Why |
|-----------|---------|-----|
| `soqucoind` | `disablewallet=1`, `txindex=1`, RPC on port 33389 (mainnet) | The node reads the chain and broadcasts. It holds no key. |
| ElectrumX | [soqucoin-labs/electrumx](https://github.com/soqucoin-labs/electrumx), branch `soqucoin`, port 50001 | The address index the payout process reads the hot wallet's outputs from. |
| The payout process | This SDK, the encrypted keystore, one state directory | It selects, signs, records and sends. |

Ports, sizing and the indexer are in the exchange guide's
[What you run](EXCHANGE_INTEGRATION.md#what-you-run). Key storage is in [Security](SECURITY.md).

Build with the latest patch release of Go 1.26. The `go` line in `go.mod` is the minimum the code
needs. The patch releases carry the standard library's security fixes.

## How a run is paid

1. **Debit the ledger.** In your database, in one transaction, debit each miner and record the
   run id with the payees and amounts of each batch. Do this before anything is submitted.
2. **Register the batch.** `SubmitBatch(ctx, id, payees, feeRate)` checks every payee and records
   the batch as one intent. A refused batch records nothing.
3. **Send it.** `Process(ctx, id)` selects confirmed inputs, reserves them, builds and signs one
   transaction, saves it, and sends it.
4. **Record the transaction id** from `Intent.TxID` against the batch in your ledger. Output `i` of
   the transaction pays `payees[i]`. The change, when there is one, is the last output.
5. **Follow it** with `UpdateConfirmations` until the intent is `Confirmed`.

The id is the idempotency key. Give each batch an id your ledger derives from the run, for example
`run-2026-11-20/001`. The same id with the same payees, amounts, order and fee rate returns the
intent already recorded. The same id with anything changed is `withdraw.ErrConflict`. An id is
never used for a second set of payees.

A batch is paid whole or it is refused. The engine never lowers an amount and never leaves a payee
out. One bad payee refuses the batch and the error names the payee. Correct the ledger row and
submit the batch again under the same id.

## Limits

| Limit | Value | Source |
|-------|-------|--------|
| Payees in one batch | 500 | `withdraw.MaxBatchPayees` |
| Inputs in one transaction | 80 | `utxo.MaxInputsPerTX` |
| Smallest payout | 279,500 shors (0.002795 SOQ) | `tx.MinOutputValue`, the node's relay floor for an address output |
| Payee address | Witness version 1, on the engine's network, named once in the batch | `Engine.SubmitBatch` |
| Fee rate | 1,000 shors per vByte recommended, 100,000 at most | `types.RecommendedFeeRate`, `tx.MaxFeeRateShorsPerVB` |
| Fee of one transaction | 2 SOQ at most | `tx.MaxFeeShors` |
| Transaction weight | 800,000 weight units at most | `tx.MaxStandardTxWeight`, the node's standard limit |

The largest batch the SDK builds, 80 inputs and 500 payees, weighs 398,622 weight units. Every
batch inside the limits is a standard transaction on the node.

Set the pool's minimum payout at or above the smallest payout. A balance under it stays in the
ledger until it grows.

## Fees

The fee is `(vBytes + 1) × fee rate`. The weight is exact:

```
weight = inputs × 3,905 + (payees + 1) × 172 + 42      (+ 8 from 252 payees)
vBytes = weight ÷ 4, rounded up
```

At the recommended rate of 1,000 shors per vByte:

| Inputs | Payees | vBytes | Fee |
|--------|--------|--------|-----|
| 1 | 1 | 1,073 | 0.01074 SOQ |
| 1 | 100 | 5,330 | 0.05331 SOQ |
| 1 | 500 | 22,532 | 0.22533 SOQ |
| 10 | 500 | 31,318 | 0.31319 SOQ |
| 80 | 500 | 99,656 | 0.99657 SOQ |

One hundred miners paid in one batch cost 0.05331 SOQ. The same hundred paid one transaction each
cost 1.074 SOQ. An ML-DSA-44 input is 3,905 weight units and an output is 172, so the saving comes
from spending few inputs for many payees.

The engine asks the selector for the batch total plus `tx.BatchFeeAllowance`, which is the fee of
the outputs beyond the first. The selector adds the fee of the inputs it takes.

## Funding the hot wallet

**Inputs are confirmed outputs.** The selector takes outputs with at least the confirmations you
set, and `VerifyAndFilterUTXOs` asks your node for each one before it is signed.

**A block reward matures after 288 confirmations on mainnet** (`types.Mainnet.CoinbaseMaturity`).
`VerifyAndFilterUTXOs` drops a reward that has not matured. When block rewards pay the hot wallet
directly, set the selector's minimum confirmations to 288 so it never picks one early.

**Change returns to the hot wallet and is spendable after one confirmation.** A run of several
batches needs one confirmed output set for each batch, or one block between batches. When the
wallet cannot fund the next batch yet, return the selector's error wrapped in `rpc.ErrTransient`.
The batch stays `Created` with nothing reserved, and the next `Process` call builds it.

**Eighty inputs is the most one transaction spends.** A wallet of many small outputs pays less per
batch. Merge them first with [`examples/consolidate`](../examples/consolidate), described under
[UTXO Consolidation](EXCHANGE_INTEGRATION.md#utxo-consolidation).

## When something goes wrong

| What `Process` returns | State | What happened | What to do |
|------------------------|-------|---------------|------------|
| An error that is `rpc.ErrTransient` | `Created` | The node is syncing, or the wallet cannot fund the batch yet. Nothing is reserved or built. | Call `Process` again later with the same id. |
| An error that is `rpc.ErrUnknownOutcome` | `Built` | The bytes are saved. The reply was lost, so the node may have them. | Call `Process` again with the same id. The same bytes go out. Never pay these miners under a new id. |
| `withdraw.ErrHeld` | `Built`, `Hold` set | The node refused bytes that an earlier send may have relayed, or accepted them under another id. | An operator resolves it. See [Step 3](EXCHANGE_INTEGRATION.md#step-3-process-withdrawals) of the exchange guide. |
| A rejection from the node | `Failed` | The node refused the first send. The inputs are free and nothing is outstanding. | Read `Intent.LastError`, correct the cause, and pay those miners under a new id. |
| `withdraw.ErrConflict` from `SubmitBatch` | Unchanged | The id is recorded with other payees. | Fix the caller. The ledger gives one id to one set of payees. |
| `withdraw.ErrInvalidIntent` from `SubmitBatch` | Nothing recorded | A payee broke a limit. The error names it. | Correct the ledger row and submit again. |

**After a restart, call `Engine.Recover` before paying.** It marks every recorded spend again and
sends every saved transaction again with the same bytes. Do not pay while it returns an error.

**Keep the state directory.** It holds the intent store and the spent set. Losing it loses the
record of what was signed, and a new directory would let the selector pick inputs a sent
transaction already spends. Back it up with the ledger.

**A store file that holds a batch is version 2.** An SDK release from before batches refuses to
open it. Run one SDK release against one state directory.

## A complete payout

One batch, from the keystore to the transaction id. The documentation check builds this program
against the repository on every change.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
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
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	network := types.Mainnet
	hotWallet := os.Getenv("SOQ_POOL_ADDRESS") // the hot wallet; it also receives the change

	// The key. The keystore is encrypted at rest; the passphrase comes from
	// the environment or your secret manager, never from a file in the repo.
	keystore := keys.NewManager("/var/lib/pool/keystore.enc", os.Getenv("SOQ_KEYSTORE_PASSPHRASE"))
	if err := keystore.Load(); err != nil {
		log.Fatalf("keystore: %v", err)
	}
	changeSPK, err := address.ScriptFor(hotWallet)
	if err != nil {
		log.Fatalf("pool address: %v", err)
	}

	// Your node and your indexer. The node client refuses a node on another
	// chain; the indexer client refuses plaintext to any host but this one.
	node := rpc.NewClient("http://127.0.0.1:33389", os.Getenv("SOQ_RPC_USER"), os.Getenv("SOQ_RPC_PASSWORD"), logger)
	node.Network = network
	elx := electrumx.NewClient("localhost:50001", 10*time.Minute, logger)
	if err := elx.TrackAddresses([]string{hotWallet}); err != nil {
		log.Fatalf("track: %v", err)
	}
	if err := elx.Connect(ctx); err != nil {
		log.Fatalf("electrumx: %v", err)
	}
	defer elx.Stop()
	if err := elx.RefreshAll(ctx); err != nil {
		log.Fatalf("refresh: %v", err)
	}

	// The state directory: the intents and the spent set. Both survive a
	// restart, and a spent set that cannot be read stops the program.
	store, err := withdraw.NewFileStore("/var/lib/pool/payouts/intents.json")
	if err != nil {
		log.Fatalf("intent store: %v", err)
	}
	spent, err := utxo.OpenSpentSet("/var/lib/pool/payouts/spent_set.json", logger)
	if err != nil {
		log.Fatalf("spent set: %v", err)
	}
	selector := utxo.NewCoinSelector(spent)

	engine := &withdraw.Engine{
		Store: store, Spent: spent, Broadcaster: node, Network: network,
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		Chain:                 withdraw.RPCChain{Client: node},
		RequiredConfirmations: types.MaxReorgDepth,
		Logger:                logger,
		// amount is the batch total plus tx.BatchFeeAllowance. Every error is
		// returned as transient: a batch the wallet cannot fund yet stays
		// Created with nothing reserved, and the next Process builds it.
		Select: func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			if err := node.RequireSynced(ctx); err != nil {
				return nil, err
			}
			tip, err := node.GetBlockCount(ctx)
			if err != nil {
				return nil, err
			}
			// A one-input payment is about 1,100 vBytes and each further
			// input adds about 976. Budget the fee for the inputs taken.
			var selected []types.UTXO
			var want int64
			for n := 1; n <= utxo.MaxInputsPerTX; {
				want = amount + int64(1100+976*(n-1))*feeRate
				selected, _, err = selector.SelectUTXOs(elx.GetAllUTXOs(), want, 1, tip, []string{hotWallet})
				if err != nil {
					return nil, fmt.Errorf("%w: the wallet cannot fund this batch now: %v", rpc.ErrTransient, err)
				}
				if len(selected) <= n {
					break
				}
				n = len(selected)
			}
			// The node must still have every input, and a block reward
			// must have matured.
			verified, err := node.VerifyAndFilterUTXOs(ctx, selected, elx.EvictUTXO, elx.SetAssetType)
			if err != nil {
				return nil, err
			}
			var have int64
			for _, u := range verified {
				have += u.Value
			}
			if have < want {
				return nil, fmt.Errorf("%w: the node holds %d of the %d shors selected", rpc.ErrTransient, have, want)
			}
			return verified, nil
		},
		BuildSignBatch: func(_ context.Context, inputs []types.UTXO, payees []withdraw.Payee, feeRate int64) (string, string, error) {
			outputs := make([]tx.TxOutput, len(payees))
			for i, p := range payees {
				spk, err := address.ScriptFor(p.Address)
				if err != nil {
					return "", "", err
				}
				outputs[i] = tx.TxOutput{Value: p.Amount, ScriptPubKey: spk}
			}
			return tx.BuildAndSignBatch(inputs, outputs, changeSPK, feeRate, keystore)
		},
	}

	// After a restart: mark the recorded spends, send the saved bytes again.
	if err := engine.Recover(ctx); err != nil {
		log.Fatalf("recover: %v", err)
	}

	// The ledger has debited these miners and recorded the id with them.
	id := "run-2026-11-20/001"
	payees := []withdraw.Payee{
		{Address: "sq1p...", Amount: 125 * types.ShorsPerSOQ},
		{Address: "sq1p...", Amount: 80 * types.ShorsPerSOQ},
	}
	if _, _, err := engine.SubmitBatch(ctx, id, payees, types.RecommendedFeeRate); err != nil {
		log.Fatalf("submit %s: %v", id, err) // nothing recorded; ErrInvalidIntent names the payee
	}
	intent, err := engine.Process(ctx, id)
	if err != nil {
		// Created or Built: call Process again with the same id. Failed:
		// nothing is outstanding; pay these miners under a new id.
		log.Fatalf("batch %s: %v", id, err)
	}
	fmt.Printf("batch %s sent as %s\n", id, intent.TxID) // record the txid in the ledger
}
```

[`examples/pool_payout`](../examples/pool_payout) is the same path as a command. It reads a run
from a file and cuts it into batches. It continues a run that stopped. Its dry run signs every
batch and sends nothing.

## Before mainnet

1. **Run the harness.** `SOQUCOIND=/path/to/soqucoind make integration` pays a 120-payee batch on a
   throwaway regtest node and reads every output back from the node. It also drops the reply to a
   broadcast and shows that the retry sends the same bytes and each payee holds one output. It
   builds `examples/pool_payout` and runs it as a process: a dry run, a run of three batches, the
   same command a second time, and a changed file under the same run id.
2. **Dry-run a real run.** `examples/pool_payout -dry-run` builds and signs every batch of your
   file against your wallet and reports the inputs, the size and the id of each.
3. **Pay a run twice.** On regtest or stagenet, run the same command line a second time. The
   second run reports the batches as sent and sends no new transaction.
4. **Stop it in the middle.** Kill the process between two batches and start it again with the
   same run id. It sends what was saved and builds what was not.
