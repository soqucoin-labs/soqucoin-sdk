# Quick Start

Get up and running with the Soqucoin SDK in five minutes.

## Prerequisites

- **Go 1.22+**: [download](https://go.dev/dl/)
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
	"fmt"
	"log"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func main() {
	// Connect to ElectrumX (TCP, no TLS, standard for local/LAN)
	client := electrumx.NewClient("localhost:50001", 15*time.Second)
	client.HRP = types.Stagenet.HRP // "ssq" for stagenet
	if err := client.Connect(); err != nil {
		log.Fatal(err)
	}
	defer client.Stop()

	// Track your address
	myAddr := "ssq1p..."
	client.TrackAddresses([]string{myAddr})

	// Fetch UTXOs
	if err := client.RefreshAll(); err != nil {
		log.Fatal(err)
	}

	// Get balance (6 confirmations minimum)
	tipHeight, _ := client.GetTip()
	confirmed, unconfirmed := client.GetBalance(6, tipHeight)
	fmt.Printf("Confirmed:   %.8f SOQ\n", float64(confirmed)/float64(types.SatoshisPerSOQ))
	fmt.Printf("Unconfirmed: %.8f SOQ\n", float64(unconfirmed)/float64(types.SatoshisPerSOQ))

	// List individual UTXOs
	utxos := client.GetUTXOs(myAddr)
	for _, u := range utxos {
		fmt.Printf("  %s:%d, %d sat (height %d)\n", u.TxID[:12], u.Vout, u.Value, u.Height)
	}
}
```

## Step 3: Send a Transaction

Build, sign, and broadcast using the full defense stack:

```go
package main

import (
	"fmt"
	"log"
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
	// 1. Connect to ElectrumX and soqucoind RPC
	elx := electrumx.NewClient("localhost:50001", 15*time.Second)
	elx.HRP = types.Stagenet.HRP
	elx.Connect()
	defer elx.Stop()

	rpcClient := rpc.NewClient("http://127.0.0.1:19332", "rpcuser", "rpcpass")

	// 2. Open the keystore holding the key for myAddr, and track the address.
	//    *keys.Manager satisfies tx.Signer, so it can be passed to BuildAndSign.
	keystore := keys.NewManager("keystore.enc", os.Getenv("SOQ_KEYSTORE_PASSPHRASE"))
	if err := keystore.Load(); err != nil {
		log.Fatal("Load keystore:", err)
	}

	myAddr := keystore.GetSignableAddresses()[0]
	recipientAddr := "ssq1p..." // whoever you are paying

	elx.TrackAddresses([]string{myAddr})
	elx.RefreshAll()

	// 3. Create a persistent spent set (prevents UTXO re-selection across restarts)
	spentSet := utxo.NewSpentSet("/tmp/my_wallet_spent_set.json")
	selector := utxo.NewCoinSelector(spentSet)

	// 4. Select UTXOs for the payment
	tipHeight, _ := rpcClient.GetBlockCount()
	allUTXOs := elx.GetAllUTXOs()
	paymentAmount := int64(1000_00000000) // 1000 SOQ

	// feeRate is satoshis per vByte, not a flat fee. A single-input Dilithium
	// payment is roughly 1,073 vB, so budget against vsize when selecting coins.
	feeRate := int64(1000)
	feeBudget := 1200 * feeRate

	selected, total, err := selector.SelectUTXOs(allUTXOs, paymentAmount+feeBudget, 1, tipHeight, nil)
	if err != nil {
		log.Fatal("Coin selection failed:", err)
	}
	fmt.Printf("Selected %d UTXOs totaling %.4f SOQ\n", len(selected), float64(total)/1e8)

	// 5. Defense 11: Verify each UTXO is still unspent on-chain
	verified, err := rpcClient.VerifyAndFilterUTXOs(selected, elx.EvictUTXO, elx.SetAssetType)
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

	// tx.BuildAndSign does all three of the following in one call. They are
	// spelled out here only because step 9 needs the change value, and the
	// transaction is the only place that value actually exists: the fee is
	// derived from feeRate and the final size, not from a flat number.
	transaction, err := tx.BuildSendTransaction(
		verified, recipientSPK, paymentAmount, changeSPK, feeRate)
	if err != nil {
		log.Fatal("Build failed:", err)
	}
	if err := transaction.SignAll(keystore); err != nil {
		log.Fatal("Signing failed:", err)
	}
	rawTxHex, builtTxID := transaction.SerializeHex(), transaction.TxID()

	// 7. Broadcast, then confirm the node agreed with our serialization
	txid, err := rpcClient.SendRawTransaction(rawTxHex)
	if err != nil {
		log.Fatal("Broadcast failed:", err)
	}
	if txid != builtTxID {
		log.Fatalf("txid mismatch: node %s, SDK %s", txid, builtTxID)
	}

	// 8. Mark UTXOs as spent
	spentSet.MarkBroadcast(verified, txid)

	// 9. Inject change for immediate availability (Defense 13). Read the value
	//    off the transaction; BuildSendTransaction only adds a change output if
	//    the remainder is worth more than it costs to spend.
	if len(transaction.Outputs) > 1 {
		change := transaction.Outputs[1]
		elx.AddChangeUTXO(txid, 1, change.Value, myAddr)
	}

	fmt.Printf("Broadcast %s, spent %d sat of input to send %d sat\n",
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

// Regtest, local development. Addresses start with "ssqrt1p"
types.Regtest.HRP  // "ssqrt"
```

> **Tip:** Always develop and test on stagenet before deploying to mainnet. Stagenet SOQ has no value and can be obtained from the faucet.

## Production Hardening

For production systems (exchanges, pools, services), add these layers:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/resilience"

// Circuit breaker, halt after 3 failures, 15 min cooldown
cb := resilience.NewCircuitBreaker(3, 15*time.Minute)

// Webhook alerter, Slack notifications on CB state changes
alerter := resilience.NewAlerter(os.Getenv("ALERT_WEBHOOK_URL"))
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
