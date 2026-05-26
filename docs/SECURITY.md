# Security Guide

Best practices for using the Soqucoin SDK securely in production.

---

## Key Storage

### Seed Management

Your Dilithium keypair is derived from a 32-byte seed. Whoever has the seed controls the funds.

**DO:**
- Encrypt seeds at rest with AES-256-GCM (or equivalent AEAD cipher)
- Store encrypted seeds in a dedicated secrets manager (HashiCorp Vault, AWS KMS, GCP Secret Manager)
- Use HSMs (Hardware Security Modules) for production hot wallets
- Back up seeds in geographically distributed, physically secure locations
- Use separate seeds for hot wallet, warm wallet, and cold storage

**DON'T:**
- Store seeds in plaintext files, environment variables, or source code
- Log seeds, private keys, or key material at any log level
- Transmit seeds over unencrypted channels
- Reuse seeds across mainnet and stagenet

### Key Hierarchy

For exchanges and services managing many addresses:

```
Master Seed (cold storage, HSM-protected)
├── Hot Wallet Seed (online, encrypted, rate-limited)
├── Deposit Address Seeds (per-user, encrypted at rest)
└── Change Address Seeds (rotated periodically)
```

---

## Memory Hygiene

### Wiping Key Material

After using private keys or seeds in memory, zero them immediately:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/keys"

kp, err := keys.FromSeed(seed)
if err != nil {
    log.Fatal(err)
}
defer kp.Wipe() // Zeros all key material in memory

// Use the keypair...
signedTx, err := builder.Sign(kp)
```

**Why this matters:** Memory dumps, core dumps, and swap files can expose key material. Wiping reduces the window of exposure.

### Process Isolation

- Run signing operations in a dedicated process with minimal privileges
- Disable core dumps: `ulimit -c 0`
- Use `mlock` to prevent key material from being swapped to disk
- Consider using a separate signing service with minimal network exposure

---

## Signature Verification

### Constant-Time Comparison

When verifying signatures or comparing cryptographic values, always use constant-time operations:

```go
import "crypto/subtle"

// ✅ Correct: constant-time comparison
if subtle.ConstantTimeCompare(expected, actual) != 1 {
    return ErrInvalidSignature
}

// ❌ Wrong: timing side-channel via early exit
if !bytes.Equal(expected, actual) {
    return ErrInvalidSignature
}
```

The SDK's built-in verification functions already use constant-time comparison internally. This guidance applies if you're implementing custom verification logic.

### Signature Malleability

SOQ transactions use Dilithium signatures which are not malleable by design (FIPS 204 §3.6). However, always verify signatures against the canonical transaction hash — never re-serialize a transaction after signature attachment.

---

## Network Security

### TLS for ElectrumX

Always use TLS when connecting to ElectrumX servers:

```go
// ✅ Correct: TLS enabled
client, err := electrumx.Dial(ctx, "electrumx.soqu.org:50002", electrumx.WithTLS())

// ❌ Dangerous: plaintext connection
client, err := electrumx.Dial(ctx, "electrumx.soqu.org:50001")
```

### RPC Authentication

When connecting to `soqucoind`, always use authenticated RPC:

```go
rpcClient, err := rpc.Dial("http://127.0.0.1:19335", rpc.WithAuth("rpcuser", "rpcpassword"))
```

- Bind RPC to `127.0.0.1` only — never expose to the public internet
- Use a strong, randomly generated RPC password
- Consider TLS for RPC connections, even on localhost

---

## Input Validation

### Address Validation

Always validate addresses before sending funds:

```go
import "github.com/soqucoin-labs/soqucoin-sdk/address"

// Validate the address format and checksum
if err := address.Validate(userProvidedAddress); err != nil {
    return fmt.Errorf("invalid address: %w", err)
}

// Check the network (mainnet vs stagenet)
network := address.Network(userProvidedAddress)
if network != address.Mainnet {
    return errors.New("refusing to send to non-mainnet address")
}
```

### Amount Validation

```go
import "github.com/soqucoin-labs/soqucoin-sdk/types"

// Amounts use fixed-point arithmetic — no floating point
amount, err := types.ParseAmount("1000.5") // 1000.5 SOQ
if err != nil {
    return fmt.Errorf("invalid amount: %w", err)
}

// Check for negative or zero amounts
if amount.IsZero() || amount.IsNegative() {
    return errors.New("amount must be positive")
}
```

---

## WASM Integrity Verification

If you're cross-referencing this Go SDK with the JavaScript/WASM SDK, verify the WASM binary integrity before loading:

```javascript
// Verify WASM hash before instantiation
const expectedHash = "sha384-<published-hash>";
const wasmBytes = await fetch("soqucoin-sdk.wasm").then(r => r.arrayBuffer());
const hash = await crypto.subtle.digest("SHA-384", wasmBytes);
const b64 = btoa(String.fromCharCode(...new Uint8Array(hash)));
if (`sha384-${b64}` !== expectedHash) {
    throw new Error("WASM binary integrity check failed");
}
```

Published WASM hashes are available in each release's `checksums.txt`.

---

## Reporting Vulnerabilities

If you discover a security vulnerability in the Soqucoin SDK:

1. **DO NOT** open a public GitHub issue
2. Email **[security@soqucoin.com](mailto:security@soqucoin.com)** with:
   - Description of the vulnerability
   - Steps to reproduce
   - Potential impact assessment
   - Your suggested fix (if any)
3. We will acknowledge receipt within **48 hours**
4. We will provide an initial assessment within **5 business days**
5. We coordinate disclosure timelines with the reporter

### Scope

The following are in scope for security reports:
- Cryptographic implementation flaws (key generation, signing, verification)
- Memory safety issues (key material exposure, buffer overflows)
- Transaction construction bugs (double-spend, fee miscalculation)
- Network protocol vulnerabilities (ElectrumX, RPC injection)

### Recognition

We maintain a security hall of fame for responsible disclosures. Reporters will be credited (with permission) in release notes and on [soqu.org](https://soqu.org).

---

## Audit Status

The Soqucoin SDK builds on:
- **[Cloudflare CIRCL](https://github.com/cloudflare/circl)** — widely reviewed ML-DSA implementation
- **Soqucoin Core** — audited by [Halborn Security](https://halborn.com) (SSC report available on request)

The SDK itself has not yet undergone a formal third-party audit. We recommend exchange integrators perform their own security review before handling significant funds.

---

© 2026 Soqucoin Labs Inc. — [soqucoin.com](https://soqucoin.com)
