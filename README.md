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
| **ElectrumX UTXO tracking** | Subscribe to addresses, list UTXOs, monitor deposits |
| **Node RPC client** | Full-featured JSON-RPC client for `soqucoind` |

## Quick Example

Generate a new SOQ address in three lines:

```go
package main

import (
	"fmt"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/address"
)

func main() {
	// Generate a new Dilithium keypair
	kp, err := keys.Generate()
	if err != nil {
		panic(err)
	}

	// Derive the bech32m address
	addr := address.FromPublicKey(kp.PublicKey)
	fmt.Println("Address:", addr)
}
```

## Packages

| Package | Purpose |
|---------|---------|
| [`address`](./address) | Bech32m address encoding/decoding, script hash derivation |
| [`keys`](./keys) | Dilithium keypair generation, serialization, seed recovery |
| [`tx`](./tx) | Transaction building, signing, serialization (wire format) |
| [`types`](./types) | Shared types: `Hash`, `OutPoint`, `TxIn`, `TxOut`, `Amount` |
| [`electrumx`](./electrumx) | ElectrumX client — UTXO queries, address subscriptions |
| [`rpc`](./rpc) | JSON-RPC client for `soqucoind` (send, getblock, etc.) |
| [`client`](./client) | High-level client combining RPC + ElectrumX for common flows |

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
- [`pool_payout`](./examples/pool_payout) — Batch payouts from a mining pool

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
