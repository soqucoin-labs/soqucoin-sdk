# Security Guide

How to use the Soqucoin SDK securely, and where it will not help you.

Every code example here compiles against this version of the SDK, and
`scripts/check-docs.py` enforces that. The previous revision of this document did
not have that property: it described nine functions that had never existed, and
told you to enable a TLS option the client did not implement. Treat anything
below as checkable, and please report it if it is not.

---

## What the SDK does and does not protect

Read this before the rest.

| | |
|---|---|
| Signing algorithm | ML-DSA-44 (FIPS 204), via [Cloudflare CIRCL](https://github.com/cloudflare/circl) |
| Keys encrypted at rest | Yes: AES-256-GCM, Argon2id key derivation |
| Key material zeroed after use | **No.** See [Memory hygiene](#memory-hygiene) |
| Transport encryption to ElectrumX | Yes, opt-in. See [Network security](#network-security) |
| Transport encryption to `soqucoind` RPC | Whatever your URL scheme provides. No client-side TLS options |
| Rate limiting, spend limits, approval workflow | **No.** Your responsibility |
| Third-party security audit of this SDK | **None.** See [Audit status](#audit-status) |

The SDK is a construction and signing library. It is not a custody system, and it
enforces no policy about how much may be spent or by whom.

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

**The SDK does not zero key material, and cannot reliably do so.**

An earlier revision of this document told you to call `kp.Wipe()`. No such method
has ever existed. Rather than add one that would not work, here is the actual
situation.

`KeyPair.PrivateKey` is a `[]byte`. Go's garbage collector may copy a slice during
its lifetime, so overwriting the copy you hold does not overwrite the copies it
made. Zeroing is therefore best-effort in any pure-Go library, and a `Wipe` method
would mostly provide false assurance.

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
this SDK calls. It is not something this SDK implements, and the previous
revision of this document overstated it by claiming the SDK's own functions use
constant-time comparison internally. They do not; `keys.Verify` performs two
length checks and delegates.

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
rpcClient := rpc.NewClient("http://127.0.0.1:19332", rpcUser, rpcPassword)
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

**All amounts in this SDK are `int64` satoshis.** There is no `Amount` type and
no parser; a previous revision of this document described both, and neither has
existed. 1 SOQ is `types.SatoshisPerSOQ` satoshis.

Parse user input yourself, and do not route it through `float64`:

```go
// Reject anything non-positive before it reaches a builder.
if amountSats <= 0 {
    return errors.New("amount must be positive")
}
```

A `float64` holds 53 bits of mantissa. Large SOQ amounts in satoshis exceed that
and round silently, so `strconv.ParseFloat` on a user-supplied amount can produce
a value that differs from what was typed.

### Fee rate is per vByte

`feeRate` in `tx.BuildAndSign` and `tx.BuildSendTransaction` is satoshis per
vByte, not a flat fee. A single-input ML-DSA payment is roughly 1,073 vB, so a
feeRate of 10 produces a transaction the node rate-limits as free and never
relays. Validate before you rely on a broadcast:

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
```

See the [verification record](VERIFICATION.md) for the observed rejection
sequence.

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

## Audit status

**This SDK has not been audited by a third party.**

It builds on ML-DSA from [Cloudflare CIRCL](https://github.com/cloudflare/circl),
which is widely reviewed, and it targets Soqucoin Core, which has been audited by
[Halborn Security](https://halborn.com). Neither of those is an audit of this
code, and neither covers the transaction construction and signing logic that
lives here.

Two defects that made every transaction this SDK produced invalid were found in
the last two releases: a serialization mismatch with consensus, and a witness
format the node rejected outright. Both were found by an outside reviewer and by
broadcasting a real transaction, not by inspection or by the test suite.

The practical implication: perform your own review before this SDK handles
significant funds, and validate the signing path against your own known-answer
vectors during onboarding. The [verification record](VERIFICATION.md) documents a
confirmed transaction you can decode yourself, and states plainly what it does
not cover.

---

© 2026 Soqucoin Labs Inc., [soqucoin.com](https://soqucoin.com)
