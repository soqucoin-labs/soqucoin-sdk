# Quick Start

Get up and running with the Soqucoin SDK in five minutes.

## Prerequisites

- **Go 1.26+**: [download](https://go.dev/dl/)
- A running `soqucoind` node or ElectrumX server (optional for address generation)

## Install

```bash
go get github.com/soqucoin-labs/soqucoin-sdk
```

---

## Step 1: Generate an Address

Create a new Dilithium keypair and derive its bech32m address:

```go
package main

import (
	"fmt"
	"log"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func main() {
	// Generate a fresh ML-DSA-44 keypair for stagenet
	kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Address: ", kp.Address)           // ssq1p...
	fmt.Printf("PubKey:   %d bytes\n", len(kp.PublicKey))  // 1312 bytes
	fmt.Printf("PrivKey:  %d bytes\n", len(kp.PrivateKey)) // 2560 bytes

	// For mainnet, use types.Mainnet.HRP, produces sq1p... addresses
}
```

## Step 2: Check Balance via ElectrumX

Connect to ElectrumX to monitor UTXOs and balances:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func main() {
	// Every call that reaches the network takes a context; every component
	// takes a logger, nil to log nothing.
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Connect to ElectrumX (plaintext is acceptable only on localhost or a
	// private network; call client.UseTLS() otherwise). The network is
	// inferred from the addresses you track.
	client := electrumx.NewClient("localhost:50001", 15*time.Second, logger)
	myAddr := "ssq1p..."
	if err := client.TrackAddresses([]string{myAddr}); err != nil {
		log.Fatal(err)
	}
	if err := client.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer client.Stop()

	// Fetch UTXOs
	if err := client.RefreshAll(ctx); err != nil {
		log.Fatal(err)
	}

	// Balance at 30 confirmations, the floor for small amounts in the
	// confirmation table (docs/EXCHANGE_INTEGRATION.md, Step 4). An error here
	// decides the money question, so it is not discarded.
	tipHeight, err := client.GetTip(ctx)
	if err != nil {
		log.Fatal(err)
	}
	confirmed, unconfirmed := client.GetBalance(30, tipHeight)
	fmt.Printf("Confirmed:   %.8f SOQ\n", float64(confirmed)/float64(types.ShorsPerSOQ))
	fmt.Printf("Unconfirmed: %.8f SOQ\n", float64(unconfirmed)/float64(types.ShorsPerSOQ))

	// List individual UTXOs
	for _, u := range client.GetUTXOs(myAddr) {
		fmt.Printf("  %s:%d, %d shors (height %d)\n", u.TxID, u.Vout, u.Value, u.Height)
	}
}
```

## Step 3: Send a Transaction

Build, sign, and broadcast using the full defense stack:

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
)

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// 1. Connect to ElectrumX and soqucoind RPC
	elx := electrumx.NewClient("localhost:50001", 15*time.Second, logger)
	defer elx.Stop()

	rpcClient := rpc.NewClient("http://127.0.0.1:28332", "rpcuser", "rpcpass", logger)
	rpcClient.Network = types.Stagenet // port 28332 is a stagenet node; RequireSynced checks the chain

	// 2. Open the keystore holding the key for myAddr, and track the address.
	//    *keys.Manager satisfies tx.Signer, so it can be passed to BuildAndSign.
	keystore := keys.NewManager("keystore.enc", os.Getenv("SOQ_KEYSTORE_PASSPHRASE"))
	if err := keystore.Load(); err != nil {
		log.Fatal("Load keystore:", err)
	}

	myAddr := keystore.GetAddresses()[0]
	recipientAddr := "ssq1p..." // whoever you are paying

	if err := elx.TrackAddresses([]string{myAddr}); err != nil {
		log.Fatal("track:", err)
	}
	if err := elx.Connect(ctx); err != nil {
		log.Fatal("connect:", err)
	}
	if err := elx.RefreshAll(ctx); err != nil {
		log.Fatal("refresh:", err)
	}

	// 3. Create a persistent spent set (prevents UTXO re-selection across restarts)
	spentSet, err := utxo.OpenSpentSet("/tmp/my_wallet_spent_set.json", logger)
	if err != nil {
		log.Fatal("spent set:", err) // a file that exists but cannot be read must not start empty
	}
	selector := utxo.NewCoinSelector(spentSet)

	// 4. Select UTXOs for the payment
	tipHeight, err := rpcClient.GetBlockCount(ctx)
	if err != nil {
		log.Fatal("tip:", err)
	}
	allUTXOs := elx.GetAllUTXOs()
	paymentAmount := int64(1000_00000000) // 1000 SOQ

	// feeRate is shors per vByte, not a flat fee. A single-input Dilithium
	// payment is roughly 1,073 vB, so budget against vsize when selecting coins.
	feeRate := types.RecommendedFeeRate
	feeBudget := 1200 * feeRate

	selected, total, err := selector.SelectUTXOs(allUTXOs, paymentAmount+feeBudget, 1, tipHeight, nil)
	if err != nil {
		log.Fatal("Coin selection failed:", err)
	}
	fmt.Printf("Selected %d UTXOs totaling %.4f SOQ\n", len(selected), float64(total)/1e8)

	// 5. Defense 11: Verify each UTXO is still unspent on-chain
	verified, err := rpcClient.VerifyAndFilterUTXOs(ctx, selected, elx.EvictUTXO, elx.SetAssetType)
	if err != nil {
		log.Fatal("UTXO verification failed:", err)
	}

	// 6. Build, sign and serialize in one call. At feeRate 10 the node
	//    rate-limits the result as a free transaction, so 1000 is the floor
	//    that actually relays. Validate with testmempoolaccept before you rely
	//    on any of this.
	recipientSPK, err := address.ScriptFor(recipientAddr)
	if err != nil {
		log.Fatal("recipient address:", err)
	}
	changeSPK, err := address.ScriptFor(myAddr)
	if err != nil {
		log.Fatal("change address:", err)
	}

	// Build and sign in one call. The transaction itself is returned so the
	// change can be read off it (Outputs[1], present only when the remainder
	// is worth more than it costs to spend); tx.BuildAndSign returns just the
	// hex and txid.
	transaction, err := tx.BuildSignedTransaction(
		verified, recipientSPK, paymentAmount, changeSPK, feeRate, keystore)
	if err != nil {
		log.Fatal("Build and sign failed:", err)
	}
	rawTxHex, builtTxID := transaction.SerializeHex(), transaction.TxID()

	// 7. Broadcast with a known outcome. A lost reply is resolved against the
	//    node, "already in chain" is success, and a node txid that differs from
	//    ours is refused. rpc.ErrUnknownOutcome means the transaction MAY be
	//    out: retry these same bytes, never rebuild (withdraw.Engine does this
	//    durably for real withdrawals). A context that ends during the send
	//    is reported the same way, as an unknown outcome, never a rejection.
	txid, err := rpcClient.Broadcast(ctx, rawTxHex, builtTxID)
	if err != nil {
		log.Fatal("Broadcast failed:", err)
	}

	// 8. Mark UTXOs as spent. A write failure is an alert: the payment is out.
	if err := spentSet.MarkBroadcast(verified, txid); err != nil {
		log.Printf("ALERT spent set not written after broadcast %s: %v", txid, err)
	}

	// 9. The change output reaches the cache on the next poll and becomes an
	//    input once it has confirmed; a second payment in the same block needs
	//    another confirmed output.
	if len(transaction.Outputs) > 1 {
		fmt.Printf("Change %d shors returns to %s after confirmation\n", transaction.Outputs[1].Value, myAddr)
	}

	fmt.Printf("Broadcast %s, spent %d shors of input to send %d shors\n",
		txid, total, paymentAmount)
}
```

---

## Network Selection

The SDK supports three networks. Use the `types` package constants:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/types"

// Mainnet, production. Addresses start with "sq1p"
types.Mainnet.HRP  // "sq"

// Stagenet, testing. Addresses start with "ssq1p"
types.Stagenet.HRP // "ssq"

// Regtest, local development. Shares the mainnet HRP: addresses start with "sq1p"
types.Regtest.HRP  // "sq"
```

> **Tip:** Always develop and test on stagenet before deploying to mainnet. Stagenet SOQ has no value and can be obtained from the faucet.

## Production Hardening

For production systems (exchanges, pools, services), add these layers:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/resilience"

// Circuit breaker, halt after 3 failures, 15 min cooldown; logger nil discards
cb := resilience.NewCircuitBreaker(3, 15*time.Minute, logger)

// Webhook alerter, Slack notifications on CB state changes
alerter := resilience.NewAlerter(os.Getenv("ALERT_WEBHOOK_URL"), logger)
alerter.WireToCircuitBreaker(cb)

// Always check before processing payments:
if err := cb.Allow(); err != nil {
    log.Printf("Payouts halted: %v", err)
    return
}
```

## Next Steps

- **[Exchange Integration Guide](EXCHANGE_INTEGRATION.md)**: Full walkthrough for listing SOQ
- **[Security Guide](SECURITY.md)**: Key storage, memory hygiene, vulnerability reporting
- **[API Reference](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)**: Full package documentation
