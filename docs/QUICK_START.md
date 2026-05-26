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

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
)

func main() {
	// Generate a fresh ML-DSA-44 keypair
	kp, err := keys.Generate()
	if err != nil {
		log.Fatal(err)
	}

	// Derive the bech32m address (soq1...)
	addr := address.FromPublicKey(kp.PublicKey)
	fmt.Println("New address:", addr)

	// Save the seed for recovery (store securely!)
	seed := kp.Seed()
	fmt.Printf("Seed (%d bytes): %x\n", len(seed), seed)
}
```

Output:
```
New address: soq1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh
Seed (32 bytes): a1b2c3d4...
```

## Step 2: Check Balance

Query UTXOs via ElectrumX to get the balance for an address:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
)

func main() {
	ctx := context.Background()

	// Connect to an ElectrumX server
	client, err := electrumx.Dial(ctx, "electrumx.soqu.org:50002", electrumx.WithTLS())
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Get balance for an address
	balance, err := client.GetBalance(ctx, "soq1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Confirmed:   %s SOQ\n", balance.Confirmed)
	fmt.Printf("Unconfirmed: %s SOQ\n", balance.Unconfirmed)
}
```

## Step 3: Send a Transaction

Build, sign, and broadcast a transaction:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func main() {
	ctx := context.Background()

	// 1. Load your keypair from a stored seed
	kp, err := keys.FromSeed(yourSeedBytes)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Connect to ElectrumX for UTXO lookup
	ec, err := electrumx.Dial(ctx, "electrumx.soqu.org:50002", electrumx.WithTLS())
	if err != nil {
		log.Fatal(err)
	}
	defer ec.Close()

	// 3. List unspent outputs for your address
	myAddr := address.FromPublicKey(kp.PublicKey)
	utxos, err := ec.ListUnspent(ctx, myAddr)
	if err != nil {
		log.Fatal(err)
	}

	// 4. Build the transaction
	builder := tx.NewBuilder()
	builder.AddInputs(utxos...)
	builder.AddOutput("soq1recipient...", types.NewAmount(1000, 0)) // 1000 SOQ
	builder.SetChangeAddress(myAddr)

	// 5. Sign and finalize
	signedTx, err := builder.Sign(kp)
	if err != nil {
		log.Fatal(err)
	}

	// 6. Broadcast
	txid, err := ec.BroadcastTransaction(ctx, signedTx)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Broadcast! TxID:", txid)
}
```

---

## Network Selection

By default, the SDK targets **mainnet**. For development, use stagenet:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/address"

// Mainnet addresses start with "soq1"
address.SetNetwork(address.Mainnet)

// Stagenet addresses start with "tsoq1"
address.SetNetwork(address.Stagenet)
```

> **Tip:** Always develop and test on stagenet before deploying to mainnet. Stagenet SOQ has no value and can be obtained from the faucet.

## Next Steps

- **[Exchange Integration Guide](EXCHANGE_INTEGRATION.md)** — Full walkthrough for listing SOQ
- **[Security Guide](SECURITY.md)** — Key storage, memory hygiene, vulnerability reporting
- **[API Reference](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)** — Full package documentation
