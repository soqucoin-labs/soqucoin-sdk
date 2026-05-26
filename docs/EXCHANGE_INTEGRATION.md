# Exchange Integration Guide

Step-by-step guide for listing Soqucoin (SOQ) on your exchange.

---

## Overview

To support SOQ deposits and withdrawals, your exchange needs to:

1. **Generate deposit addresses** — one unique address per user
2. **Monitor deposits** — subscribe to address activity via ElectrumX
3. **Process withdrawals** — build, sign, and broadcast transactions
4. **Confirm transactions** — wait for sufficient block confirmations

SOQ uses **NIST FIPS 204 ML-DSA-44** (Dilithium) for all signatures. Transaction structure is similar to Bitcoin/Dogecoin (UTXO model), but witness data contains Dilithium signatures (~2,420 bytes) and public keys (~1,312 bytes).

---

## Step 1: Generate Deposit Addresses

Create a unique deposit address for each user. Store the keypair securely — you'll need it to sweep funds.

```go
package main

import (
	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
)

// GenerateDepositAddress creates a new deposit address for a user.
// Returns the address string and the seed bytes for secure storage.
func GenerateDepositAddress() (addr string, seed []byte, err error) {
	kp, err := keys.Generate()
	if err != nil {
		return "", nil, err
	}

	addr = address.FromPublicKey(kp.PublicKey)
	seed = kp.Seed()
	return addr, seed, nil
}
```

**Important:** Store seeds encrypted at rest. See the [Security Guide](SECURITY.md) for key storage recommendations.

---

## Step 2: Monitor Deposits

Use the ElectrumX client to subscribe to address notifications and detect incoming deposits:

```go
package main

import (
	"context"
	"log"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
)

func MonitorDeposits(ctx context.Context, addresses []string) error {
	client, err := electrumx.Dial(ctx, "electrumx.soqu.org:50002", electrumx.WithTLS())
	if err != nil {
		return err
	}
	defer client.Close()

	// Subscribe to each deposit address
	for _, addr := range addresses {
		notifications, err := client.SubscribeAddress(ctx, addr)
		if err != nil {
			log.Printf("Failed to subscribe %s: %v", addr, err)
			continue
		}

		go func(addr string, ch <-chan electrumx.StatusNotification) {
			for notif := range ch {
				log.Printf("Activity on %s: status=%s", addr, notif.Status)

				// Fetch the full transaction history
				history, err := client.GetHistory(ctx, addr)
				if err != nil {
					log.Printf("Error fetching history: %v", err)
					continue
				}

				for _, entry := range history {
					if entry.Confirmations >= 6 {
						log.Printf("Confirmed deposit: txid=%s amount=%s",
							entry.TxID, entry.Amount)
						// Credit user account here
					}
				}
			}
		}(addr, notifications)
	}

	// Block until context is cancelled
	<-ctx.Done()
	return nil
}
```

---

## Step 3: Process Withdrawals

When a user requests a withdrawal, build a transaction from your hot wallet UTXOs:

```go
package main

import (
	"context"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// ProcessWithdrawal sends SOQ from the hot wallet to a user's external address.
func ProcessWithdrawal(
	ctx context.Context,
	hotWalletSeed []byte,
	toAddress string,
	amount types.Amount,
) (txid string, err error) {
	// 1. Reconstruct hot wallet keypair
	kp, err := keys.FromSeed(hotWalletSeed)
	if err != nil {
		return "", err
	}

	// 2. Connect to ElectrumX
	ec, err := electrumx.Dial(ctx, "electrumx.soqu.org:50002", electrumx.WithTLS())
	if err != nil {
		return "", err
	}
	defer ec.Close()

	// 3. Gather UTXOs from hot wallet
	hotAddr := address.FromPublicKey(kp.PublicKey)
	utxos, err := ec.ListUnspent(ctx, hotAddr)
	if err != nil {
		return "", err
	}

	// 4. Build transaction
	builder := tx.NewBuilder()
	builder.AddInputs(utxos...)
	builder.AddOutput(toAddress, amount)
	builder.SetChangeAddress(hotAddr)
	builder.SetFeeRate(tx.RecommendedFeeRate) // ~1 SOQ/KB

	// 5. Sign
	signedTx, err := builder.Sign(kp)
	if err != nil {
		return "", err
	}

	// 6. Broadcast
	return ec.BroadcastTransaction(ctx, signedTx)
}
```

---

## Step 4: Confirm Transactions

SOQ uses a 1-minute block target. Recommended confirmation thresholds:

| Use Case | Confirmations | Approximate Time |
|----------|:-------------:|:----------------:|
| Small deposits (<1,000 SOQ) | 6 | ~6 minutes |
| Medium deposits (1K–100K SOQ) | 12 | ~12 minutes |
| Large deposits (>100K SOQ) | 30 | ~30 minutes |
| Withdrawal finality | 6 | ~6 minutes |

```go
// Poll for confirmations
func WaitForConfirmations(ctx context.Context, ec *electrumx.Client, txid string, required int) error {
	for {
		info, err := ec.GetTransaction(ctx, txid)
		if err != nil {
			return err
		}
		if info.Confirmations >= required {
			return nil // Confirmed
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			// Poll again
		}
	}
}
```

---

## Important Notes

### Transaction Size

SOQ transactions are larger than Bitcoin transactions due to Dilithium signatures:

| Component | Size |
|-----------|------|
| Dilithium public key | 1,312 bytes |
| Dilithium signature | 2,420 bytes |
| Typical 1-in-1-out TX | ~4.8 KB |
| Typical 2-in-1-out TX | ~8.5 KB |

Plan your block size and fee estimation accordingly.

### Fee Estimation

The SDK provides fee estimation helpers:

```go
feeRate := tx.RecommendedFeeRate    // Standard: ~1 SOQ/KB
feeRate := tx.HighPriorityFeeRate   // Priority: ~2 SOQ/KB

// Or query the node for dynamic estimates
feeRate, err := rpcClient.EstimateFee(ctx, 6) // target 6 blocks
```

### Confirmation Times

- **Block time:** ~1 minute target
- **Block reward:** follows halving schedule (see whitepaper)
- **Reorg protection:** 6 confirmations recommended minimum for deposits

---

## Security Recommendations

| Concern | Recommendation |
|---------|---------------|
| **Key storage** | Encrypt seeds with AES-256-GCM at rest. Use HSM for production hot wallets. |
| **Key rotation** | Generate fresh deposit addresses periodically. Sweep old addresses to cold storage. |
| **Cold storage** | Keep >95% of funds in air-gapped cold wallets with multi-signature schemes. |
| **Monitoring** | Set up alerts for unusual withdrawal patterns and large deposits. |
| **Rate limiting** | Enforce withdrawal rate limits and require manual approval above thresholds. |

See the full [Security Guide](SECURITY.md) for detailed recommendations.

---

## API Reference

Full API documentation is available at:

**[pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)**

---

## Support

- **Technical questions:** Open an issue on [GitHub](https://github.com/soqucoin-labs/soqucoin-sdk/issues)
- **Security issues:** [security@soqucoin.com](mailto:security@soqucoin.com)
- **Exchange listing inquiries:** [listings@soqucoin.com](mailto:listings@soqucoin.com)
