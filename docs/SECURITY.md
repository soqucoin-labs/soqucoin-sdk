# Security Guide

How to use the Soqucoin SDK securely, and which protections are yours to provide
rather than ours.

Every code example here is compiled against this version of the SDK by
`scripts/check-docs.py`, which runs in CI. If anything below does not behave as
described, that is a bug worth reporting.

---

## Division of responsibility

The SDK is a transaction construction and signing library. It is not a custody
system. Knowing where its remit ends is the first step in integrating it safely.

**Provided by the SDK:**

| | |
|---|---|
| Signing algorithm | ML-DSA-44 (FIPS 204), via [Cloudflare CIRCL](https://github.com/cloudflare/circl) |
| Keys encrypted at rest | AES-256-GCM with Argon2id key derivation |
| Network safety | Script derived per address; transactions mixing networks refused |
| Transport encryption to ElectrumX | Supported, opt-in. See [Network security](#network-security) |
| Consensus agreement | Serialization pinned to the node's own format vectors |

**Yours to provide:**

| | |
|---|---|
| Spend limits, rate limiting, approval workflow | The SDK enforces no policy on amounts or authorisation |
| Deposit crediting idempotency | See [the deposit example](../examples/exchange_deposit) for the pattern |
| Zeroing key material after use | Not reliably possible in pure Go. See [Memory hygiene](#memory-hygiene) |
| Transport encryption to `soqucoind` RPC | The RPC client has no TLS options; protect it at the network layer |

---

## Key storage

### Use the keystore, not your own file format

`keys.Manager` stores keypairs encrypted with AES-256-GCM under a key derived by
Argon2id (time 3, 64 MiB memory, 4 threads). The passphrase is supplied at
construction and never written to the keystore.

```go
keystore := keys.NewManager("/var/lib/soq/keystore.enc", os.Getenv("SOQ_PASSPHRASE"))
if err := keystore.Load(); err != nil {
    return fmt.Errorf("load keystore: %w", err)
}
```

`Load` treats a missing file as an empty keystore, so a typo in the path yields a
manager with no keys rather than an error. If you expect keys to be present, check:

```go
if keystore.KeyCount() == 0 {
    return errors.New("keystore empty: wrong path or wrong passphrase")
}
```

`*keys.Manager` satisfies `tx.Signer`, so it can be handed directly to
`tx.BuildAndSign` and the private key never leaves the manager.

### Passphrase handling

The passphrase is the whole of the at-rest protection. Argon2id makes guessing
expensive, not impossible.

- Source it from a secrets manager or an operator prompt, not from a file beside
  the keystore.
- An environment variable is readable from `/proc/<pid>/environ` by root and by
  anything that inherits it. It is acceptable for a container whose environment
  you control, and a poor choice on a shared host.
- Never log it, and never include it in a crash report.

### Separate keys by role

Use different keystores, on different hosts, for hot, warm and cold funds. A
single keystore holding everything means one passphrase compromise is total.

Do not reuse a key across mainnet and stagenet. Addresses differ by prefix, so
reuse is not a signing hazard, but it destroys the operational separation that
makes stagenet safe to experiment on.

---

## Memory hygiene

**The SDK does not zero key material, and no pure-Go library can do so reliably.**

`KeyPair.PrivateKey` is a `[]byte`. Go's garbage collector may relocate a slice
during its lifetime, so overwriting the copy you hold does not overwrite copies
it has made. A `Wipe` method would therefore offer assurance it could not keep,
which is why the SDK does not provide one.

What actually reduces exposure, in rough order of effectiveness:

- **Disable core dumps** on the signing process: `ulimit -c 0`, or
  `LimitCORE=0` in a systemd unit.
- **Disable swap**, or encrypt it. Swapped pages persist after reboot.
- **Isolate signing** in its own process with minimal privileges and no inbound
  network exposure, so a compromise elsewhere cannot read its memory.
- **Restrict `ptrace`**: `kernel.yama.ptrace_scope=1` or higher stops one
  unprivileged process attaching to another.
- **Keep the process short-lived** where the workload allows it.

If your threat model includes an attacker who can read process memory, use an HSM
or a hardware-isolated signer. This SDK cannot defend against that.

---

## Signature verification

`keys.Verify` checks an ML-DSA-44 signature:

```go
ok, err := keys.Verify(pubKey, digest, signature)
if err != nil {
    return fmt.Errorf("malformed key or signature: %w", err)
}
if !ok {
    return errors.New("signature does not verify")
}
```

Note the two-value result. `err` reports malformed input, meaning a public key or
signature of the wrong length. A cryptographically invalid signature returns
`false, nil`. Checking only `err` accepts every forged signature of the correct
size.

### Constant-time behaviour

Constant-time verification is a property of CIRCL's ML-DSA implementation, which
this SDK calls. `keys.Verify` performs two length checks and delegates the
comparison, so the guarantee is CIRCL's rather than ours. That is the right place
for it to live, but worth knowing precisely if you are reasoning about side
channels.

If you compare cryptographic values in your own code, use `crypto/subtle`:

```go
if subtle.ConstantTimeCompare(expected, actual) != 1 {
    return ErrMismatch
}
```

### Malleability

ML-DSA signatures are not malleable in the ECDSA sense, so the classic txid
mutation does not apply. The property you should still enforce is agreement:

```go
txid, err := rpcClient.SendRawTransaction(rawHex)
if err != nil {
    return err
}
if txid != builtTxID {
    return fmt.Errorf("node txid %s != SDK txid %s", txid, builtTxID)
}
```

`tx.BuildAndSign` returns the txid it computed. If the node reports a different
one, the SDK's serialization disagrees with consensus. Stop; do not retry.

---

## Network security

### ElectrumX

An ElectrumX server sees **every address you track**. Over an untrusted path a
plaintext connection discloses your entire deposit set to anyone in between, and
lets them alter the balances and UTXOs you act on. Coin selection acts on that
data, so this is an integrity problem and not only a privacy one.

The client speaks plaintext by default, because the common deployment is a server
on localhost. Enable TLS for anything else:

```go
client := electrumx.NewClient("electrum.example.org:50002", 15*time.Second)
client.UseTLS()
if err := client.Connect(); err != nil {
    return err
}
```

`UseTLS` requires TLS 1.2 or better and verifies the server certificate against
the system roots. For a private CA or a pinned certificate, set `TLSConfig`
directly instead of calling `UseTLS`:

```go
client := electrumx.NewClient("electrum.internal:50002", 15*time.Second)
client.TLSConfig = &tls.Config{RootCAs: myPool, MinVersion: tls.VersionTLS13}
if err := client.Connect(); err != nil {
    return err
}
```

`TLSConfig` applies to reconnects as well. This matters: the client reconnects
automatically after two failed polls and after a panic in the polling goroutine,
so a downgrade there would be silent and could last for days. There is a test
that pins it.

Do not set `InsecureSkipVerify`. An unverified TLS connection is worse than a
plaintext one, because it looks secure while an on-path attacker can still
substitute their own certificate.

Where TLS is not available, run ElectrumX on localhost or reach it over a tunnel
you control. That is a legitimate configuration and is why plaintext remains the
default.

### soqucoind RPC

```go
rpcClient := rpc.NewClient("http://127.0.0.1:33389", rpcUser, rpcPassword)
```

The client offers no TLS options. Protect the RPC transport at the network layer:

- Bind `soqucoind` RPC to `127.0.0.1` and never expose it publicly.
- Use a long random password. `rpcauth` with a salted hash is preferable to a
  plaintext `rpcpassword` in `soqucoin.conf`.
- Cross-host RPC belongs in a tunnel, not on the open internet with a password.

---

## Input validation

### Addresses

Validate before you build anything. `Validate` takes the expected HRP, so it
checks the network at the same time as the checksum:

```go
if err := address.Validate(types.Mainnet.HRP, userProvidedAddress); err != nil {
    return fmt.Errorf("invalid address: %w", err)
}
```

If you accept addresses on more than one network, derive the network from the
address instead of guessing:

```go
network, err := address.NetworkOf(userProvidedAddress)
if err != nil {
    return fmt.Errorf("unrecognized address: %w", err)
}
if network.Name != types.Mainnet.Name {
    return fmt.Errorf("refusing to send to a %s address", network.Name)
}
```

`NetworkOf` refuses a prefix belonging to no supported network, which matters
because a fabricated prefix can carry a perfectly valid bech32m checksum.

The builders enforce this too: they derive each input's script from its own
address and reject a transaction whose inputs mix networks. That check exists
because the script derived from an address is what BIP143 commits to as the
`scriptCode`, so a wrong network is a signing fault and not merely a decoding one.

### Amounts

**All amounts in this SDK are `int64` shors.** There is no `Amount` type and no
parser, so nothing converts or validates on your behalf. 1 SOQ is
`types.ShorsPerSOQ` shors.

Parse user input yourself, and do not route it through `float64`:

```go
// Reject anything non-positive before it reaches a builder.
if amountSats <= 0 {
    return errors.New("amount must be positive")
}
```

A `float64` holds 53 bits of mantissa. Large SOQ amounts in shors exceed that
and round silently, so `strconv.ParseFloat` on a user-supplied amount can produce
a value that differs from what was typed.

### Fee rate is per vByte

`feeRate` in `tx.BuildAndSign` and `tx.BuildSendTransaction` is shors per
vByte, not a flat fee. A single-input ML-DSA payment is roughly 1,073 vB, so a
feeRate below about 1,000 produces a transaction the node treats as effectively
free and rate-limits rather than relays. Validate before you rely on a broadcast:

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
```

See [Exchange Integration](EXCHANGE_INTEGRATION.md#fee-estimation) for converting
a node fee estimate into a per-vByte rate.

---

## Reporting vulnerabilities

Please do not open a public issue for a security defect.

Email **[security@soqucoin.com](mailto:security@soqucoin.com)** with a
description, reproduction steps, and your assessment of the impact. Include a
suggested fix if you have one.

We will acknowledge receipt and give you an initial assessment, and we will agree
a disclosure timeline with you rather than imposing one. We are a small team and
would rather not publish a response-time commitment we cannot consistently meet.

In scope:

- Cryptographic flaws in key generation, signing, or verification
- Key material exposure
- Transaction construction defects, including fee miscalculation and anything
  that produces a signature over the wrong message
- Injection or protocol abuse via the ElectrumX or RPC clients

Reporters are credited in release notes with their permission.

---

## Releases and supply chain

- **Dependencies.** Two direct: [Cloudflare CIRCL](https://github.com/cloudflare/circl) for
  ML-DSA-44 and `golang.org/x/crypto` for the keystore's Argon2id. No `replace` directives, no
  vendored code; `go mod verify` is clean. CI runs `govulncheck` on the symbols this module calls and
  `gitleaks` over the full history on every push, and Dependabot proposes updates weekly. GitHub
  Actions are pinned by commit, not by tag.
- **Signed tags.** Release tags are signed with OpenPGP key
  `5C30 55F9 F986 6B23 7D69 A247 32ED 260F 83A0 BA88`. Verify before you depend on a tag:

  ```bash
  gpg --recv-keys 5C3055F9F9866B237D69A24732ED260F83A0BA88
  git tag -v v0.3.4
  ```

- **Reproducibility.** The module is pure Go with no cgo and no code generation, so a build from a
  tag is reproducible with the toolchain declared in `go.mod`. To list exactly what went into a
  binary you built: `go version -m ./your-binary`. To produce a CycloneDX SBOM of the module:

  ```bash
  go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest mod -json -output sbom.json
  ```

---

## Security review status

| Layer | External review |
|-------|-----------------|
| ML-DSA-44 cryptography | [Cloudflare CIRCL](https://github.com/cloudflare/circl), widely deployed and independently analysed |
| Consensus rules, script validation, signing | Soqucoin Core, audited by [Halborn Security](https://halborn.com) across two engagements |
| This SDK's construction and client layer | Verified against the node's own format vectors and by confirmed on-chain transactions; no separate engagement |

Both the cryptography and the consensus rules this SDK targets have been reviewed
externally. The SDK is the integration layer above them, and its agreement with
consensus is established by evidence you can check rather than by assertion:
serialization is pinned byte-for-byte to the node's own format vectors, and
[Verification](VERIFICATION.md) records a confirmed transaction with the
identifiers to decode it and the steps to reproduce it.

As with any integration library, we recommend validating the signing path against
your own known-answer vectors during onboarding, and we would like to hear what
you find. Two of the improvements in recent releases came from exactly that kind
of outside reading.

---

© 2026 Soqucoin Labs Inc., [soqucoin.com](https://soqucoin.com)
