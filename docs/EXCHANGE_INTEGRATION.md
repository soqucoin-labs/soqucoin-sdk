# Exchange Integration Guide

Step-by-step guide for listing Soqucoin (SOQ) on your exchange.

---

## Overview

To support SOQ deposits and withdrawals, your exchange needs to:

1. **Generate deposit addresses** — one unique address per user
2. **Monitor deposits** — track UTXOs via ElectrumX polling
3. **Process withdrawals** — build, sign, and broadcast transactions
4. **Confirm transactions** — wait for sufficient block confirmations

SOQ uses **NIST FIPS 204 ML-DSA-44** (Dilithium) for all signatures. Transaction structure is similar to Bitcoin/Dogecoin (UTXO model), but witness data contains Dilithium signatures (~2,420 bytes) and public keys (~1,312 bytes).

**Important:** All production Soqucoin nodes run with `disablewallet=1`. You cannot use `listunspent`, `getbalance`, or other wallet RPCs. The SDK uses ElectrumX for UTXO queries instead.

---

## Step 1: Generate Deposit Addresses

Create a unique deposit address for each user. Store the keypair securely — you'll need it to sweep funds.

```go
import (
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// GenerateDepositAddress creates a new deposit address for a user.
func GenerateDepositAddress() (addr string, kp *keys.KeyPair, err error) {
	// Use types.Mainnet.HRP for production ("sq" → sq1p... addresses)
	// Use types.Stagenet.HRP for testing ("ssq" → ssq1p... addresses)
	kp, err = keys.GenerateKeyForNetwork(types.Mainnet.HRP)
	if err != nil {
		return "", nil, err
	}
	return kp.Address, kp, nil
}
```

**Important:** Store seeds encrypted at rest. See the [Security Guide](SECURITY.md) for key storage recommendations.

---

## Step 2: Monitor Deposits

Use the ElectrumX client to poll for UTXO changes across all deposit addresses:

```go
import (
	"log"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func StartDepositMonitor(depositAddresses []string) {
	// Connect to your ElectrumX server
	client := electrumx.NewClient("electrumx.example.com:50001", 15*time.Second)
	client.HRP = types.Mainnet.HRP
	if err := client.Connect(); err != nil {
		log.Fatal(err)
	}
	defer client.Stop()

	// Track all deposit addresses (can be thousands)
	client.TrackAddresses(depositAddresses)

	// Start background polling (refreshes every 15 seconds)
	client.StartPolling()

	// Check for confirmed deposits periodically
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		tipHeight, err := client.GetTip()
		if err != nil {
			log.Printf("Cannot get tip: %v", err)
			continue
		}

		for _, addr := range depositAddresses {
			utxos := client.GetUTXOs(addr)
			for _, u := range utxos {
				if u.Height == 0 || u.AssetType != types.AssetTypeSOQ {
					continue
				}

				confirmations := tipHeight - u.Height + 1
				if confirmations >= 6 {
					// Credit user — use txid:vout as idempotency key
					log.Printf("Confirmed deposit: %s:%d — %.8f SOQ (%d conf)",
						u.TxID[:12], u.Vout,
						float64(u.Value)/float64(types.SatoshisPerSOQ),
						confirmations)
				}
			}
		}
	}
}
```

---

## Step 3: Process Withdrawals

When a user requests a withdrawal, build a transaction from your hot wallet UTXOs.
Use the full defense stack to prevent stale UTXO failures:

```go
import (
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/resilience"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

var (
	// Circuit breaker — halt after 3 consecutive failures
	cb       = resilience.NewCircuitBreaker(3, 15*time.Minute)
	// Persistent spent set — survives process restarts
	spentSet = utxo.NewSpentSet("/var/lib/exchange/spent_set.json")
	selector = utxo.NewCoinSelector(spentSet)
)

func ProcessWithdrawal(
	elxClient  *electrumx.Client,
	rpcClient  *rpc.Client,
	toAddress  string,
	amount     int64,  // in satoshis
	hotWalletAddr string,
) (txid string, err error) {
	// 1. Check circuit breaker
	if err := cb.Allow(); err != nil {
		return "", fmt.Errorf("withdrawals halted: %w", err)
	}

	// 2. Get chain tip
	tipHeight, err := rpcClient.GetBlockCount()
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}

	// 3. Select UTXOs (largest-first, spent-set-aware)
	fee := int64(100_000) // 0.001 SOQ
	allUTXOs := elxClient.GetAllUTXOs()
	selected, total, err := selector.SelectUTXOs(allUTXOs, amount+fee, 1, tipHeight, nil)
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}

	// 4. Defense 11: Verify each UTXO is still unspent on-chain
	verified, err := rpcClient.VerifyAndFilterUTXOs(
		selected,
		elxClient.EvictUTXO,     // Remove stale UTXOs from cache
		elxClient.SetAssetType,  // Stamp asset type from node
	)
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}

	// 5. Build, sign, broadcast (use tx package with your keystore)
	// rawTx := tx.Build(verified, outputs, hotWalletAddr, fee, keystore)
	// txid, err := rpcClient.SendRawTransaction(rawTx)

	// 6. Mark spent (prevents re-selection)
	spentSet.MarkBroadcast(verified, txid)

	// 7. Inject change for immediate availability (Defense 13)
	changeAmount := total - amount - fee
	if changeAmount > 0 {
		elxClient.AddChangeUTXO(txid, 1, changeAmount, hotWalletAddr)
	}

	cb.RecordSuccess()
	return txid, nil
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
| Max inputs per TX | 80 (due to MAX_STANDARD_TX_WEIGHT) |

### Fee Estimation

```go
// Query the node for dynamic fee estimates
feeRate, err := rpcClient.EstimateSmartFee(6) // target 6 blocks

// Or use a generous fallback (typical for Soqucoin's low-fee environment)
const defaultFee = 100_000 // 0.001 SOQ — covers most single-output TXs
```

### UTXO Consolidation

Exchanges accumulate many small UTXOs from deposits. Periodically consolidate them to avoid hitting the 80-input limit during large withdrawals:

```go
// Select the smallest UTXOs for consolidation
smallUTXOs, total, err := selector.SelectSmallestUTXOs(allUTXOs, 50, 6, tipHeight, nil)
// Build a single TX that merges them into one output to your hot wallet
```

---

## Security Recommendations

| Concern | Recommendation |
|---------|---------------|
| **Key storage** | Encrypt seeds with AES-256-GCM at rest. Use HSM for production hot wallets. |
| **Key rotation** | Generate fresh deposit addresses periodically. Sweep old addresses to cold storage. |
| **Cold storage** | Keep >95% of funds in air-gapped cold wallets. |
| **Monitoring** | Use the `resilience.Alerter` for Slack notifications on circuit breaker state changes. |
| **Rate limiting** | Enforce withdrawal rate limits and require manual approval above thresholds. |
| **Spent tracking** | Always use `utxo.SpentSet` with persistence. Never re-select a broadcast UTXO. |

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
