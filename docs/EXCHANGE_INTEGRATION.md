# Exchange Integration Guide

Step-by-step guide for listing Soqucoin (SOQ) on your exchange.

---

## Overview

To support SOQ deposits and withdrawals, your exchange needs to:

1. **Generate deposit addresses**: one unique address per user
2. **Monitor deposits**: track UTXOs via ElectrumX polling
3. **Process withdrawals**: build, sign, and broadcast transactions
4. **Confirm transactions**: wait for sufficient block confirmations

SOQ uses **NIST FIPS 204 ML-DSA-44** (Dilithium) for all signatures. Transaction structure is similar to Bitcoin/Dogecoin (UTXO model), but witness data contains Dilithium signatures (~2,420 bytes) and public keys (~1,312 bytes).

---

## Integration model, read this first

**You run our node. You do not use the node's built-in wallet.** Those are different things and the
distinction is the whole integration:

| | |
|---|---|
| ✅ **You run** | `soqucoind`, our node, this is how you see the chain and broadcast |
| ❌ **You do not use** | the wallet subsystem *inside* that node. It ships disabled (`disablewallet=1`), so `listunspent`, `getbalance` and `sendtoaddress` are not part of the integration surface |
| ✅ **You still need, and still do** | wallet *functions*, address derivation, key custody, signing. Those run in **your** infrastructure via this SDK |

In other words the node is a chain reader and broadcaster; your key vault stays your key vault.
Exchanges already work this way for other UTXO coins rather than keeping customer keys inside
`bitcoind`.

This is the same pattern exchanges use to integrate any UTXO chain at scale, deposit addresses
derived from your own key store, chain watched by an indexer, withdrawals signed in your own signing
infrastructure, and the node used only to read the chain and broadcast. **It is not a
Soqucoin-specific arrangement**, and if you already integrate Bitcoin or Litecoin this way, the
shape will be familiar.

| Component | What you use | SDK package |
|-----------|--------------|-------------|
| **Deposit addresses** | Derive per-user keypairs in your own key store | [`keys`](../keys), [`address`](../address) |
| **Deposit monitoring** | ElectrumX indexer, polled for UTXO changes | [`electrumx`](../electrumx) |
| **Withdrawal signing** | Your signing infrastructure, offline or HSM-backed | [`keys`](../keys), [`tx`](../tx) |
| **Broadcast + chain reads** | Node JSON-RPC (`sendrawtransaction`, `gettxout`, `getblock`) | [`rpc`](../rpc) |

**Why the node wallet is not used:** ML-DSA-44 keys are roughly 60x larger than the ECDSA keys a
Bitcoin-derived wallet was designed around (2,560-byte private, 1,312-byte public, versus ~32 and
~33). At exchange scale, one deposit address per user, that key material belongs in your own
storage rather than in the node's embedded wallet database. Nothing about this reduces what you can
do; it moves key custody to where you want it anyway.

**What this means for your integration estimate:** the node is a standard Bitcoin-style JSON-RPC
daemon for every call in this guide, and the post-quantum specifics are confined to signature
construction, which this SDK handles. Be aware, however, that this is **not** the templated
`getnewaddress` / `sendtoaddress` integration path, it requires wiring this SDK into your deposit
and withdrawal flow, plus an ElectrumX indexer. See the next section, which is the item most likely
to affect your estimate.

### ⚠️ ElectrumX indexer, a decision we need from you

Because the node's wallet is disabled, there is no address index to query for deposits. The SDK
reads UTXOs from an **ElectrumX indexer**, and one has to exist. There are two ways to arrange it,
and we would rather you choose than discover it later:

| Option | What it means | Trade-off |
|--------|---------------|-----------|
| **A. You run your own** | You operate an ElectrumX instance alongside your node, as you may already do for other UTXO coins | ✅ No dependency on our infrastructure, no shared point of failure |
| **B. You connect to ours** | You point the SDK at an ElectrumX endpoint we operate | ✅ Nothing extra to run. ⚠️ Creates a hard dependency on our availability for your deposit crediting, which most exchanges reasonably refuse |

**We recommend Option A, and the software is published:**

> **https://github.com/soqucoin-labs/electrumx**, branch `soqucoin`

Upstream ElectrumX ships no Soqucoin coin definition, so that fork is what you need. It is based on
a pinned upstream commit rather than tracking `master`, so the entire Soqucoin delta is reviewable
in one command:

```bash
git diff 24865dc..soqucoin -- src/
```

Roughly 183 lines across four files: the coin definitions (AuxPoW-aware headers), asset and
visibility derivation from the witness version, UTXO records carrying that metadata, and one
protocol extension (`get_multi_balance`) for per-asset balances. [`SOQUCOIN.md`](https://github.com/soqucoin-labs/electrumx/blob/soqucoin/SOQUCOIN.md)
in that repository documents the configuration and the caveats, including the fact that the
**mainnet genesis hash is a placeholder until genesis is mined**. Upstream `LICENCE` is unchanged.

**So the question we need answered:** do you run your own indexer for UTXO-model coins, or would
you expect to connect to one we operate? That answer changes both the integration effort and where
the operational risk sits, so it is worth settling before scoping.

If neither option suits your architecture, tell us, we would rather adapt than have you commit to
something that does not fit your operations.

---

## Step 1: Generate Deposit Addresses

Create a unique deposit address for each user. Store the keypair securely, you'll need it to sweep funds.

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

				// See "Step 4: Confirm Transactions" for thresholds. 288 is the
				// chain's own finality horizon (nMaxReorgDepth); crediting below it
				// accepts reorg risk the protocol does not consider settled.
				const minConfirmations = 288
				confirmations := tipHeight - u.Height + 1
				if confirmations >= minConfirmations {
					// Credit user, use txid:vout as idempotency key
					log.Printf("Confirmed deposit: %s:%d, %.8f SOQ (%d conf)",
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
	// Circuit breaker, halt after 3 consecutive failures
	cb       = resilience.NewCircuitBreaker(3, 15*time.Minute)
	// Persistent spent set, survives process restarts
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

	// 5. Build, sign and serialize. *keys.Manager satisfies tx.Signer.
	//    feeRate is per vByte, not a flat fee: see the note below.
	recipientSPK, err := address.ScriptFor(toAddr)
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}
	changeSPK, err := address.ScriptFor(hotWalletAddr)
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}
	rawTx, builtTxID, err := tx.BuildAndSign(
		verified, recipientSPK, amount, changeSPK, feeRate, keystore)
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}

	// 6. Broadcast. The node's txid must equal the one BuildAndSign computed;
	//    if it does not, serialization disagrees with consensus. Do not proceed.
	txid, err := rpcClient.SendRawTransaction(rawTx)
	if err != nil {
		cb.RecordFailure(err)
		return "", err
	}
	if txid != builtTxID {
		return "", fmt.Errorf("txid mismatch: node %s, SDK %s", txid, builtTxID)
	}

	// 7. Mark spent (prevents re-selection)
	spentSet.MarkBroadcast(verified, txid)

	// 8. Inject change for immediate availability (Defense 13)
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

SOQ uses a **1-minute block target**. Anchor your thresholds to two consensus parameters rather
than to a rule of thumb carried over from another chain:

| Parameter | Value | Meaning |
|-----------|:-----:|---------|
| `nMaxReorgDepth` | **288 blocks** (~4.8 h) | The chain's own finality horizon. Nodes reject reorganisations deeper than this |
| `nCoinbaseMaturity` | **240 blocks** (~4 h) | Newly mined coins are unspendable until this depth, enforced by consensus |

Recommended thresholds:

| Use Case | Confirmations | Approximate Time | Rationale |
|----------|:-------------:|:----------------:|-----------|
| Zero-confirmation display | 0 | instant | Show as *pending only*. Never credit |
| Small deposits | 30 | ~30 min | Bounded, recoverable loss if reorganised |
| Medium deposits | 120 | ~2 h | Materially past any plausible reorg depth |
| Large deposits | **288** | ~4.8 h | Matches the chain's own finality horizon |
| Mining / coinbase payouts | **240 minimum** | ~4 h | Consensus-enforced; the output cannot be spent earlier regardless of policy |
| Withdrawal release | 288 | ~4.8 h | Do not release outbound value against inbound funds the chain does not yet treat as final |

**Set your own thresholds against value at risk. The table is a floor, not a ceiling.**

### Why these are higher than a typical Bitcoin-style table

Two reasons, both worth understanding before tuning them down:

1. **The chain declares its own finality at 288 blocks.** Crediting a large deposit at 6
   confirmations means accepting reorganisation risk the protocol itself does not consider settled.
   Below 288 you are taking a position the chain has not taken.
2. **Absolute hashrate matters more than block interval on a young chain.** Confirmation count is a
   proxy for accumulated work. Early in a chain's life, and while hashrate is concentrated among a
   small number of mining participants, producing a competing chain of N blocks costs far less than
   the same N blocks would cost on a mature network. Prefer depth over speed until sustained
   third-party hashrate exists.

If low-latency credit matters to your product, credit small amounts quickly against your own risk
budget and hold larger amounts to 288, rather than lowering the threshold uniformly.

---

## Verification: a real confirmed transaction

Rather than asking you to trust that the signing path works, there is a
[verification record](VERIFICATION.md) for a stagenet transaction **built, signed,
serialized, broadcast and confirmed entirely by this SDK**:

| | |
|---|---|
| Transaction id | `99fd147aaa4d575ee8f6266acfda4b09a5b0dc730d964294efded2cf3cd2eae7` |
| Block | `ad12368c1e083a6f0efe8da7cc65b52613b05d3301f0e609ba4660fdcffcf380` |
| Witness stack | `[2421, 1313]` bytes, the consensus-required format |

The transaction id the SDK computed matches the one the node assigned, which
independently confirms that serialization agrees with consensus byte for byte.

That document also records the **two defects the exercise found that inspection had
not**, the rejection sequence showing which check each fix satisfied, and the steps
to reproduce it against your own node. It is worth reading before you scope the
integration: one of the findings is that **the fee rate used in our own examples
was too low for the node to relay.**

---

## Test Coverage, current status

Every package now carries unit tests. Measured with `go test -cover ./...`:

| Package | Coverage | What is covered |
|---------|:--------:|-----------------|
| `address` | **88.8%** | Bech32m encoding, checksum validation, script-hash derivation |
| `client` | **86.8%** | soq-signer auth, error propagation, SOQ-to-satoshi conversion |
| `utxo` | **83.9%** | Coin selection, persistent spent set |
| `keys` | **69.2%** | Dilithium keypair generation, keystore encryption |
| `rpc` | **64.8%** | JSON-RPC plumbing, Defense 11 stale-UTXO filtering |
| `tx` | **60.3%** | Txid byte order, BIP143 sighash, weight and fee, script builders |
| `resilience` | **33.8%** | Circuit breaker state transitions |
| `electrumx` | **30.5%** | UTXO cache, balance filtering, eviction, change injection |

Also passes under the race detector (`go test -race`), which matters for `electrumx` because its
UTXO cache is shared between the polling goroutine and caller threads.

**Where the coverage is thin, and why.** The two low numbers are honest rather than accidental:

- **`electrumx` (30.5%)**: the covered part is the UTXO cache, which is where incorrect state can
  exist without any network error being raised. The uncovered majority is the TCP transport,
  reconnection and polling loop. Simulating that faithfully needs an ElectrumX protocol double; the
  transport is instead exercised continuously in production.
- **`resilience` (33.8%)**: circuit breaker transitions are covered. The reconciler and the Slack
  alerter are not, and both are operational conveniences rather than parts of the money path.

**What the tests deliberately target.** Rather than chasing a percentage, they pin the invariants
whose failure is silent: transaction ID byte-order reversal, per-input BIP143 sighash separation,
exclusion of witness data from the txid, USDSOQ never counted as native SOQ, spent-pending UTXOs
never double-selected, a stale UTXO both dropped *and* evicted from cache, and RPC errors never
surfacing as usable zero values.

Two real defects were found and fixed while writing them, both in the payout path, see the
`v0.3.0` release notes.

If coverage on a specific path is a gating requirement for your review, tell us which and we will
prioritise it.

---

## Important Notes

### Transaction Size

SOQ transactions are larger than Bitcoin transactions due to Dilithium signatures:

Sizes below are measured by building and signing with this SDK, not estimated.
Each figure is for a payment plus a change output, which is what the builders
produce whenever a remainder is left over.

| Component | Size |
|-----------|------|
| ML-DSA-44 public key | 1,312 bytes |
| ML-DSA-44 signature | 2,420 bytes |
| Witness stack per input | 3,734 bytes (2,421 + 1,313, including the sighash byte and the `0x00` key prefix) |
| 1-in, 2-out | 3,880 bytes, 4,288 WU, ~1,072 vB |
| 2-in, 2-out | 7,662 bytes, 8,184 WU, ~2,046 vB |
| 10-in, 2-out | 37,918 bytes, 39,352 WU, ~9,838 vB |
| 80-in, 2-out | 302,658 bytes, 312,072 WU, ~78,018 vB |
| Max inputs per TX | 80, enforced by `utxo.MaxInputsPerTX` |

Roughly 3,782 bytes per additional input, so estimate
`3,880 + 3,782 x (inputs - 1)` bytes.

**The 80-input cap is not the node's weight limit.** `MAX_STANDARD_TX_WEIGHT` is
800,000 WU, and 80 inputs use 312,072 of it, about 39%. The cap is sized against
the older 400,000 WU limit because not every production node runs the build that
raised it. It was reverted from 200 to 80 in May 2026 after transactions were
rejected in production for size. Treat it as an operational floor that will rise,
not as a protocol constant.

`SelectUTXOs` returns `ErrInputLimitReached` with a partial selection when it hits
the cap before reaching the target. Handle that case: it means the payment needs
consolidation first, and ignoring the error sends less than intended.

### Fee estimation

**`feeRate` is satoshis per vByte, not a flat fee.** This is worth stating twice,
because the SDK's own examples got it wrong: at a feeRate of 10, a ~1,072 vB
payment pays about 10,700 satoshis, which the node treats as effectively free and
rate-limits rather than relays. The observed rejection is in the
[verification record](VERIFICATION.md).

```go
// Query the node for a dynamic estimate. It returns SOQ per kB.
soqPerKB, err := rpcClient.EstimateSmartFee(6) // target: 6 blocks
if err != nil {
    return err
}
feeRate := int64(soqPerKB * float64(types.SatoshisPerSOQ) / 1000) // satoshis per vByte
if feeRate < 1000 {
    feeRate = 1000 // floor: below this the node rate-limits as free
}
```

`EstimateSmartFee` falls back to 0.01 SOQ/kB when the node has no estimate, which
is exactly 1,000 satoshis per vByte, so the floor above and the fallback agree.

Always confirm before you rely on a broadcast:

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
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
