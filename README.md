# soqucoin-sdk

**Go SDK for Soqucoin — integrate SOQ in hours, not weeks.**

[![Go Reference](https://pkg.go.dev/badge/github.com/soqucoin-labs/soqucoin-sdk.svg)](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)
[![CI](https://github.com/soqucoin-labs/soqucoin-sdk/actions/workflows/test.yml/badge.svg)](https://github.com/soqucoin-labs/soqucoin-sdk/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What is Soqucoin?

Soqucoin (SOQ) is the first NIST FIPS 204 (ML-DSA / Dilithium) post-quantum cryptocurrency. Every on-chain signature uses ML-DSA-44 — no classical elliptic-curve fallback. This SDK gives you everything you need to build wallets, exchanges, mining pools, and services on top of SOQ.

## Install

```bash
go get github.com/soqucoin-labs/soqucoin-sdk
```

Requires **Go 1.22+**.

## Features

| Feature | Description |
|---------|-------------|
| **Address generation** | Derive Dilithium keypairs and encode bech32m addresses |
| **Transaction construction** | Build, serialize, and deserialize SOQ transactions |
| **Dilithium signing** | Sign and verify with NIST FIPS 204 ML-DSA-44 |
| **ElectrumX UTXO tracking** | Production-hardened TCP client with 4MB buffer, merge refresh, auto-reconnect |
| **Node RPC client** | JSON-RPC client for `soqucoind` with Defense 11 (gettxout pre-verify) |
| **UTXO coin selection** | Largest-first, smallest-first (consolidation), asset-type-aware, dust filtering |
| **Persistent spent set** | Never re-spend a UTXO — survives process restarts via JSON persistence |
| **Circuit breaker** | Halt operations after consecutive failures, probe, recover |
| **Reconciliation** | Periodic UTXO balance verification to detect drift |
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
client := electrumx.NewClient("electrumx.example.com:50001", 15*time.Second)
client.Connect()
client.TrackAddresses([]string{depositAddr})
client.StartPolling()

// Check balance periodically
confirmed, _ := client.GetBalance(6, tipHeight)
```

Build and broadcast a payment:

```go
// 1. Select UTXOs
selector := utxo.NewCoinSelector(spentSet)
inputs, total, err := selector.SelectUTXOs(allUTXOs, amount+fee, 1, tipHeight, nil)

// 2. Verify on-chain (Defense 11)
verified, err := rpcClient.VerifyAndFilterUTXOs(inputs, elxClient.EvictUTXO, nil)

// 3. Build, sign, broadcast
rawTx := tx.Build(verified, outputs, changeAddr, fee, keystore)
txid, err := rpcClient.SendRawTransaction(rawTx)

// 4. Mark spent (Defense 12)
spentSet.MarkBroadcast(verified, txid)
```

## Packages

| Package | Purpose |
|---------|---------|
| [`address`](./address) | Bech32m address encoding/decoding, script hash derivation |
| [`keys`](./keys) | Dilithium keypair generation, keystore encryption, signing |
| [`tx`](./tx) | Transaction building, signing, serialization (wire format) |
| [`types`](./types) | Shared types: UTXO, Network, asset type constants |
| [`electrumx`](./electrumx) | Production-hardened ElectrumX TCP client (PF-018, F5, Defense 12) |
| [`rpc`](./rpc) | JSON-RPC client for `soqucoind` — sendrawtransaction, gettxout, getblock |
| [`utxo`](./utxo) | UTXO coin selection + persistent spent set tracking |
| [`resilience`](./resilience) | Circuit breaker, reconciler, and Slack webhook alerter |
| [`client`](./client) | High-level client combining RPC + ElectrumX for common flows |

## Production-Hardened

This SDK was extracted from the canonical `soq-signer` service that has been running in production since May 2026. Every defense layer comes from a real incident:

| Defense | What it prevents | Origin |
|---------|-----------------|--------|
| **Defense 11** | Stale UTXO signing — `gettxout` pre-verification | 2 weeks of failed payouts |
| **Defense 12** | SpentPending flag loss — merge refresh instead of replace | Race condition during polling |
| **Defense 13** | Change output delay — inject change immediately | Back-to-back payment failures |
| **PF-018** | Bufio panic on large responses — 4MB read buffer | 18,000+ UTXO address |
| **F5** | Broken pipe after idle — TCP keepalive 30s | NAT/firewall timeout |
| **PF-018b** | TCP stream corruption — connection mutex | Concurrent broadcast+poll |
| **Circuit Breaker** | Infinite retry loops — automatic backoff | Node outage cascade |

## Documentation

- **[Quick Start](docs/QUICK_START.md)** — Generate an address, check balance, send SOQ in 5 minutes
- **[Exchange Integration](docs/EXCHANGE_INTEGRATION.md)** — Step-by-step guide for listing SOQ on your exchange
- **[Security](docs/SECURITY.md)** — Key storage, memory hygiene, vulnerability reporting

## Post-Quantum Cryptography

Soqucoin uses **NIST FIPS 204 ML-DSA-44** (formerly CRYSTALS-Dilithium) for all on-chain signatures. This SDK wraps [Cloudflare's CIRCL](https://github.com/cloudflare/circl) implementation, which is widely audited and FIPS-aligned.

Key properties:
- **Public key size:** 1,312 bytes
- **Signature size:** 2,420 bytes
- **Security level:** NIST Level 2 (≥128-bit quantum security)
- **Standard:** [FIPS 204](https://csrc.nist.gov/pubs/fips/204/final) (August 2024)

## Examples

See the [`examples/`](./examples) directory:

- [`generate_address`](./examples/generate_address) — Create a new wallet address
- [`send_transaction`](./examples/send_transaction) — Build and broadcast a transaction
- [`exchange_deposit`](./examples/exchange_deposit) — Monitor incoming deposits (exchange flow)
- [`pool_payout`](./examples/pool_payout) — Batch payouts with circuit breaker

## Contributing

We welcome contributions. Please open an issue first to discuss what you'd like to change.

```bash
git clone https://github.com/soqucoin-labs/soqucoin-sdk.git
cd soqucoin-sdk
go test ./...
```

## License

MIT — see [LICENSE](LICENSE).

Copyright © 2026 [Soqucoin Labs Inc.](https://soqucoin.com)
