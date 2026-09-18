# soqucoin-sdk

**Go SDK for Soqucoin: integrate SOQ in hours, not weeks.**

[![Go Reference](https://pkg.go.dev/badge/github.com/soqucoin-labs/soqucoin-sdk.svg)](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)
[![CI](https://github.com/soqucoin-labs/soqucoin-sdk/actions/workflows/test.yml/badge.svg)](https://github.com/soqucoin-labs/soqucoin-sdk/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What is Soqucoin?

Soqucoin (SOQ) is the first NIST FIPS 204 (ML-DSA / Dilithium) post-quantum cryptocurrency. Every on-chain signature uses ML-DSA-44, with no classical elliptic-curve fallback. This SDK gives you everything you need to build wallets, exchanges, mining pools, and services on top of SOQ.

## Install

```bash
go get github.com/soqucoin-labs/soqucoin-sdk
```

Requires **Go 1.26+** (per the `go` directive in `go.mod`).

## Integration model

**You run the node. You do not use the node's built-in wallet.** Run `soqucoind` with
`disablewallet=1`, so wallet RPCs (`listunspent`, `getbalance`, `sendtoaddress`) are not part of the
integration surface. You still perform wallet *functions* (address derivation, key custody,
signing), but they run in your own infrastructure through this SDK. The node is a chain reader and
broadcaster; your key vault stays your key vault. Run it with `txindex=1` and `disablewallet=1`;
ports, sizing and the indexer are in the guide's
[What you run](docs/EXCHANGE_INTEGRATION.md#what-you-run).

| Component | What you use | SDK package |
|-----------|--------------|-------------|
| Address derivation | Your own key store | [`keys`](./keys), [`address`](./address) |
| Chain watching | **ElectrumX indexer** (required, see note) | [`electrumx`](./electrumx) |
| Signing | Your signing infrastructure | [`keys`](./keys), [`tx`](./tx) |
| Broadcast + chain reads | Node JSON-RPC | [`rpc`](./rpc) |

The node behaves as an ordinary Bitcoin-style JSON-RPC daemon, and the post-quantum specifics are
confined to signature construction, which this SDK handles.

⚠️ **ElectrumX is required.** With the node wallet disabled there is no address index to query, so
deposit monitoring reads from an ElectrumX indexer. Upstream ElectrumX ships no Soqucoin coin
definition, so we publish a fork:

> **[soqucoin-labs/electrumx](https://github.com/soqucoin-labs/electrumx)**, branch `soqucoin`

Run your own instance (recommended: no dependency on our infrastructure) or connect to one we
operate. See
[Exchange Integration](docs/EXCHANGE_INTEGRATION.md#integration-model-read-this-first) for both
options and the full walkthrough.

## Features

| Feature | Description |
|---------|-------------|
| **Address generation** | Derive Dilithium keypairs and encode bech32m addresses |
| **Transaction construction** | Build, serialize, and deserialize SOQ transactions |
| **Dilithium signing** | Sign and verify with NIST FIPS 204 ML-DSA-44 |
| **ElectrumX UTXO tracking** | Production-hardened TCP client with 4MB buffer, merge refresh, auto-reconnect; every `listunspent` reply validated as a whole; plaintext only to a loopback host unless allowed |
| **Node RPC client** | JSON-RPC client for `soqucoind` with Defense 11 (gettxout pre-verify) |
| **UTXO coin selection** | Largest-first, smallest-first (consolidation), asset-type-aware, dust filtering |
| **Persistent spent set** | Never re-spend a UTXO: inputs reserved at build time, unconfirmed spends survive restarts |
| **Withdrawal state machine** | `withdraw.Engine`: idempotency keys, persist-before-broadcast, same-bytes retry on a lost reply, recovery without rebuilding |
| **Deposit crediting** | `deposit.Monitor`: credits only what your own node confirms; pauses while syncing or stale; alarms on a vanished credit |
| **Circuit breaker** | Halts on systemic failures only (per-request errors never count), one probe, recovers; a reconciler trip halts it until an operator's `Reset` |
| **Reconciliation** | Verifies the indexer cache against the node outpoint by outpoint, reading this process's own spends from the spent set; halts withdrawals on a mismatch |
| **Webhook alerting** | Slack-compatible notifications for circuit breaker transitions |

## Quick Example

Generate a new SOQ address:

```go
kp, err := keys.GenerateKeyForNetwork(types.Mainnet.HRP)
if err != nil {
    log.Fatal(err)
}
fmt.Println("Address:", kp.Address)
```

Monitor deposits via ElectrumX:

```go
client := electrumx.NewClient("electrumx.example.com:50002", 10*time.Minute, logger) // reconcile interval; nil logs nothing
client.UseTLS()
if err := client.TrackAddresses([]string{depositAddr}); err != nil { // network inferred, mixed refused
    log.Fatal(err)
}
if err := client.Connect(ctx); err != nil { // verifies the server's genesis hash
    log.Fatal(err)
}
client.Start(ctx) // subscribes to every address; ends with ctx or Stop

// Credit through deposit.Monitor, which checks every candidate against your
// own node before crediting; see docs/EXCHANGE_INTEGRATION.md Step 2. The
// confirmation table starts at 30 for small amounts, never 6. The balance is
// the indexer's view; what a payout can spend is what the selector accepts
// against the spent set.
confirmed, _ := client.GetBalance(30, tipHeight)
```

Build and broadcast a payment:

```go
// 1. Select UTXOs
selector := utxo.NewCoinSelector(spentSet)
inputs, total, err := selector.SelectUTXOs(allUTXOs, amount+fee, 1, tipHeight, nil)

// 2. Verify on-chain (Defense 11)
verified, err := rpcClient.VerifyAndFilterUTXOs(ctx, inputs, elxClient.EvictUTXO, nil)

// 3. Build, sign, verify and serialize in one call. feeRate is shors per vByte;
//    use types.RecommendedFeeRate (1000). The builder measures the real weight,
//    enforces the node's output floor, caps the fee and verifies every input
//    as the node will before returning.
recipientSPK, err := address.ScriptFor(recipientAddr)
changeSPK, err := address.ScriptFor(changeAddr)
rawTx, txid, err := tx.BuildAndSign(verified, recipientSPK, amount, changeSPK, types.RecommendedFeeRate, keystore)

// 4. Broadcast with a known outcome: a lost reply is resolved against the
//    node, "already in chain" is success, and rpc.ErrUnknownOutcome means
//    retry THESE bytes, never rebuild; a context that ends mid-send is
//    reported the same way. withdraw.Engine does all of this durably; use
//    it for real withdrawals (Step 3 of the exchange guide).
sentTxID, err := rpcClient.Broadcast(ctx, rawTx, txid)

// 5. Mark spent. A failed write is an alert: the payment is out.
if err := spentSet.MarkBroadcast(verified, sentTxID); err != nil {
    log.Printf("ALERT spent set not written: %v", err)
}
```

## Packages

| Package | Purpose |
|---------|---------|
| [`address`](./address) | Bech32m address encoding/decoding, script hash derivation |
| [`keys`](./keys) | ML-DSA-44 keypair generation, derivation from a seed, keystore encryption, signing |
| [`tx`](./tx) | Transaction building, signing, serialization (wire format) |
| [`types`](./types) | Shared types: UTXO, Network, asset type constants |
| [`electrumx`](./electrumx) | Production-hardened ElectrumX TCP client (PF-018, F5, Defense 12) |
| [`rpc`](./rpc) | JSON-RPC client for `soqucoind` (sendrawtransaction, gettxout, getblock) |
| [`utxo`](./utxo) | UTXO coin selection + persistent spent set tracking |
| [`withdraw`](./withdraw) | Durable withdrawal state machine: idempotency, reservation, same-bytes retry, recovery |
| [`deposit`](./deposit) | Deposit crediting verified against your own node |
| [`resilience`](./resilience) | Circuit breaker, reconciler against the node, Slack webhook alerter |

## Provenance

**The UTXO, network and resilience layers were extracted** from the `soq-signer`
service that has run in production since May 2026. Every defense below exists
because of a specific incident:

| Defense | What it prevents | Origin |
|---------|-----------------|--------|
| **Defense 11** | Stale UTXO signing, via `gettxout` pre-verification | 2 weeks of failed payouts |
| **Defense 12** | Loss of a per-output stamp on refresh (originally a spent-pending flag, now the asset type stamped after node verification), via merge refresh instead of replace | Race condition during refresh |
| **PF-018** | Bufio panic on large responses, via a 4MB read buffer | 18,000+ UTXO address |
| **F5** | Broken pipe after idle, via TCP keepalive at 30s | NAT/firewall timeout |
| **PF-018b** | TCP stream corruption, via a connection mutex | Concurrent broadcast+poll |
| **Circuit Breaker** | Infinite retry loops, via automatic backoff | Node outage cascade |

**The transaction construction and signing layer is a separate implementation**,
so rather than rest on the production record above, it is verified directly:

- **A confirmed on-chain transaction**, built, signed, serialized, broadcast and
  confirmed entirely by this SDK, with the identifiers to decode it yourself and
  the steps to reproduce it. See [Verification](docs/VERIFICATION.md).
- **Serialization pinned to the node's own format vectors**, so the SDK and
  consensus cannot diverge without a test failing.
- **Construction tested across all three networks**, with transactions that mix
  networks refused outright.

We think that is a stronger basis than shared lineage, because it is evidence you
can check rather than provenance you have to trust.

## Test Coverage

Every package carries unit tests, and the suite passes under the race detector.

```bash
go test ./...
go test -race ./...
go test -cover ./...
```

Per-package figures, what each package's tests cover, and where coverage is thin with the reason,
are in one table in
[Exchange Integration](docs/EXCHANGE_INTEGRATION.md#test-coverage-current-status). They are kept
in one place rather than two so that the two cannot disagree, and every row is one run of the
command above.

The tests target invariants whose failure is *silent* rather than a coverage percentage: txid byte
order, per-input BIP143 sighash separation, USDSOQ never counted as native SOQ, stale UTXOs both
dropped and evicted, and RPC errors never surfacing as usable zero values. Where coverage is thin
it is stated plainly, with the reason, in
[Exchange Integration](docs/EXCHANGE_INTEGRATION.md#test-coverage-current-status).

## Integration harness

```bash
SOQUCOIND=/path/to/soqucoind make integration
```

A throwaway regtest node, the real `deposit` and `withdraw` packages, thirteen end-to-end
scenarios, about seventy seconds. See the [Exchange Integration](docs/EXCHANGE_INTEGRATION.md) guide for what
each scenario proves.

`SOQUCOIND` must be a runnable binary. In an autotools build tree that is `src/soqucoind`, the
wrapper that resolves the uninstalled shared libraries, and not `src/.libs/soqucoind`, which does
not load outside it.

## Documentation

- **[Quick Start](docs/QUICK_START.md)**: Generate an address, check balance, send SOQ in 5 minutes
- **[Exchange Integration](docs/EXCHANGE_INTEGRATION.md)**: Step-by-step guide for listing SOQ on your exchange
- **[Security](docs/SECURITY.md)**: Key storage, memory hygiene, vulnerability reporting
- **[Verification](docs/VERIFICATION.md)**: a confirmed transaction built and signed by this SDK, with the identifiers to decode it yourself and the steps to reproduce it

## Post-Quantum Cryptography

Soqucoin uses **NIST FIPS 204 ML-DSA-44** (formerly CRYSTALS-Dilithium) for all on-chain signatures. This SDK wraps [Cloudflare's CIRCL](https://github.com/cloudflare/circl) implementation, which is widely audited and FIPS-aligned.

Key properties:
- **Public key size:** 1,312 bytes
- **Signature size:** 2,420 bytes
- **Security level:** NIST Level 2 (≥128-bit quantum security)
- **Standard:** [FIPS 204](https://csrc.nist.gov/pubs/fips/204/final) (August 2024)

## Examples

See the [`examples/`](./examples) directory:

- [`generate_address`](./examples/generate_address): Create a new wallet address
- [`send_transaction`](./examples/send_transaction): Build and sign a transaction (does not broadcast)
- [`exchange_deposit`](./examples/exchange_deposit): Credit deposits verified against your own node (exchange flow)
- [`pool_payout`](./examples/pool_payout): Batch payouts with circuit breaker
- [`consolidate`](./examples/consolidate): Merge the smallest final outputs into one at the hot wallet, through `tx.BuildSignedSweep` against your own node
- [`stagenet_withdrawal`](./examples/stagenet_withdrawal): The withdrawal recorded in [VERIFICATION.md](docs/VERIFICATION.md), through `withdraw.Engine` against your own node
- [`exchange_split`](./examples/exchange_split): The same withdrawal path as three processes sharing one directory: a watcher with the node credential, a signer that holds the key and opens no socket, and a broadcaster that sends

## Contributing

We welcome contributions. Please open an issue first to discuss what you'd like to change.

```bash
git clone https://github.com/soqucoin-labs/soqucoin-sdk.git
cd soqucoin-sdk
go test ./...
```

## License

MIT, see [LICENSE](LICENSE).

Copyright © 2026 [Soqucoin Labs Inc.](https://soqucoin.com)
