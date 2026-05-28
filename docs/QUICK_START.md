# Quick Start

Get up and running with the Soqucoin SDK in five minutes.

## Prerequisites

- **Go 1.22+** — [download](https://go.dev/dl/)
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

	// For mainnet, use types.Mainnet.HRP — produces sq1p... addresses
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
	// Connect to ElectrumX (TCP, no TLS — standard for local/LAN)
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
		fmt.Printf("  %s:%d — %d sat (height %d)\n", u.TxID[:12], u.Vout, u.Value, u.Height)
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
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
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

	// 2. Track your address and refresh UTXOs
	myAddr := "ssq1p..."
	elx.TrackAddresses([]string{myAddr})
	elx.RefreshAll()

	// 3. Create a persistent spent set (prevents UTXO re-selection across restarts)
	spentSet := utxo.NewSpentSet("/tmp/my_wallet_spent_set.json")
	selector := utxo.NewCoinSelector(spentSet)

	// 4. Select UTXOs for the payment
	tipHeight, _ := rpcClient.GetBlockCount()
	allUTXOs := elx.GetAllUTXOs()
	paymentAmount := int64(1000_00000000) // 1000 SOQ
	fee := int64(100_000)                  // 0.001 SOQ

	selected, total, err := selector.SelectUTXOs(allUTXOs, paymentAmount+fee, 1, tipHeight, nil)
	if err != nil {
		log.Fatal("Coin selection failed:", err)
	}
	fmt.Printf("Selected %d UTXOs totaling %.4f SOQ\n", len(selected), float64(total)/1e8)

	// 5. Defense 11: Verify each UTXO is still unspent on-chain
	verified, err := rpcClient.VerifyAndFilterUTXOs(selected, elx.EvictUTXO, elx.SetAssetType)
	if err != nil {
		log.Fatal("UTXO verification failed:", err)
	}

	// 6. Build, sign, and broadcast (use tx.Build() with your keystore)
	// rawTxHex := tx.Build(verified, outputs, changeAddr, fee, keystore)
	// txid, err := rpcClient.SendRawTransaction(rawTxHex)
	_ = verified

	// 7. Mark UTXOs as spent
	txid := "your_txid_here"
	spentSet.MarkBroadcast(verified, txid)

	// 8. Inject change output for immediate availability (Defense 13)
	changeAmount := total - paymentAmount - fee
	if changeAmount > 0 {
		elx.AddChangeUTXO(txid, 1, changeAmount, myAddr)
	}

	fmt.Println("Broadcast! TxID:", txid)
}
```

---

## Network Selection

The SDK supports three networks. Use the `types` package constants:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/types"

// Mainnet — production. Addresses start with "sq1p"
types.Mainnet.HRP  // "sq"

// Stagenet — testing. Addresses start with "ssq1p"
types.Stagenet.HRP // "ssq"

// Regtest — local development. Addresses start with "ssqrt1p"
types.Regtest.HRP  // "ssqrt"
```

> **Tip:** Always develop and test on stagenet before deploying to mainnet. Stagenet SOQ has no value and can be obtained from the faucet.

## Production Hardening

For production systems (exchanges, pools, services), add these layers:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/resilience"

// Circuit breaker — halt after 3 failures, 15 min cooldown
cb := resilience.NewCircuitBreaker(3, 15*time.Minute)

// Webhook alerter — Slack notifications on CB state changes
alerter := resilience.NewAlerter(os.Getenv("ALERT_WEBHOOK_URL"))
alerter.WireToCircuitBreaker(cb)

// Always check before processing payments:
if err := cb.Allow(); err != nil {
    log.Printf("Payouts halted: %v", err)
    return
}
```

## Next Steps

- **[Exchange Integration Guide](EXCHANGE_INTEGRATION.md)** — Full walkthrough for listing SOQ
- **[Security Guide](SECURITY.md)** — Key storage, memory hygiene, vulnerability reporting
- **[API Reference](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)** — Full package documentation
