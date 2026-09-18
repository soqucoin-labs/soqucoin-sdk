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
| Signing algorithm | ML-DSA-44 (FIPS 204), via [Cloudflare CIRCL](https://github.com/cloudflare/circl); `keys.Manager.Sign` uses the hedged (randomized) variant, so two signatures of one digest differ and both verify |
| Keys encrypted at rest | AES-256-GCM, under a passphrase stretched by Argon2id or under a 32-byte key from your own key-management system |
| Network safety | Script derived per address; transactions mixing networks refused |
| Transport encryption to ElectrumX | Supported, opt-in. See [Network security](#network-security) |
| Consensus agreement | Serialization pinned to the node's own format vectors |

**Yours to provide:**

| | |
|---|---|
| Spend limits, rate limiting, approval workflow | The SDK enforces no policy on amounts or authorisation |
| Deposit crediting idempotency | See [the deposit example](../examples/exchange_deposit) for the pattern |
| Zeroing key material after use | Not reliably possible in pure Go. See [Memory hygiene](#memory-hygiene) |
| Transport to a `soqucoind` on another host | `rpc.Client` refuses a URL that is not loopback until `AllowRemote` is set, and refuses `http://` to such a host whether or not it is set. An `https://` URL is verified against the system roots; there is no private-CA or pinning option, so a private CA means a tunnel. See [Network security](#network-security) |

---

## Key storage

### Derive deposit keys, store hot-wallet keys

Deposit keys are derived, not stored. `keys.DeriveSeed(master, hrp, index)` and
`keys.FromSeed(hrp, seed)` produce the same key and address for the same master,
network and index on every run, so the master secret in your key-management
system and the index recorded with each user are the only recovery material. The
seed is the private key: hold the master with at least the care of a hot-wallet
key, and zero seeds after use (see [Memory hygiene](#memory-hygiene)). The network
prefix is part of the derivation, so one master yields different keys on mainnet
and stagenet and a production master that reaches a stagenet host yields stagenet
keys there, not keys that also spend mainnet funds. Regtest shares the `sq` prefix
with mainnet, so a regtest host gets no such separation. Give every test host its
own master all the same: the derivation limits what a leaked derived key is worth,
not what a leaked master is worth. `FromSeed` refuses the roughly 1 seed in 256 whose key the
node can never spend from (`keys.ErrInvalidPublicKey`); skip that index rather than
retrying with it.

`keys.Manager` is the hot-wallet store, for the few addresses that hold
operating funds.

### Use the keystore, not your own file format

`keys.Manager` stores keypairs encrypted with AES-256-GCM. The encryption key
comes from one of two places, and the file records which.

**A passphrase**, stretched by Argon2id. The passphrase is supplied at
construction and never written to the keystore.

```go
keystore := keys.NewManager("/var/lib/soq/keystore.enc", os.Getenv("SOQ_PASSPHRASE"))
if err := keystore.Load(); err != nil {
    return fmt.Errorf("load keystore: %w", err)
}
```

**A 32-byte key you already hold**, from Vault, an HSM or anything else that
unseals a key into the process at start. There is then no passphrase to prompt
for, type or leave in an environment variable, and the at-rest protection is
your key-management system's rather than Argon2id over a human secret.

```go
key, err := fetchKeyFromVault(ctx) // 32 bytes, keys.ExternalKeySize
if err != nil {
    return err
}
keystore, err := keys.NewManagerWithKey("/var/lib/soq/keystore.enc", key)
if err != nil {
    return fmt.Errorf("keystore: %w", err)
}
for i := range key {
    key[i] = 0 // the manager keeps its own copy
}
if err := keystore.Load(); err != nil {
    return fmt.Errorf("load keystore: %w", err)
}
```

A manager opens only keystores written under the key source it was built with.
Opening a passphrase keystore with an external key, or the reverse, is
`keys.ErrKDFMismatch` and says so, rather than a decryption failure that sends
you looking for a wrong passphrase.

Load before you save. `Save` writes the manager's keys over whatever is at the
path, so a manager that has not read that file does not know what it is
replacing. It refuses (`keys.ErrKeystoreUnread`) rather than replacing a
populated keystore with its own idea of the contents. Creating a keystore on a
path that does not exist yet, `LoadOrCreate`, and every save after either of
those are unaffected; what is refused is a process writing a keystore it never
opened. To replace one deliberately, remove the file or write to a new path.

`Load` refuses a missing file (`keys.ErrKeystoreMissing`): a mistyped path would
otherwise start a signer that hands out deposit addresses it can never spend from.
The first run, and only the first run, calls `LoadOrCreate`, which writes an empty
encrypted keystore at the path:

```go
if err := keystore.LoadOrCreate(); err != nil {
    return fmt.Errorf("create keystore: %w", err)
}
```

`*keys.Manager` satisfies `tx.Signer`, so it can be handed directly to
`tx.BuildAndSign` and the private key never leaves the manager. The only method
that returns private key material is `ExportPrivateKey`, which returns a copy and
is named so that a search of your code base finds every use. `PublicKeyFor`
returns a copy too. Printing a `keys.KeyPair`, a pointer to one, or a struct,
slice or map holding one in an exported position shows the address and the public
key hash under every `fmt` verb, `%d` and `%x` included, never the private key.
Two shapes `fmt` prints raw and no method can intercept: `%p` applied to a
non-pointer, and a `KeyPair` behind an unexported struct field. Do not hold a
`KeyPair` where a struct dump can reach it that way.

### The keystore file, version 2

A version 2 keystore carries its own KDF identifier and, for Argon2id, the
parameters it was written with (time 3, 64 MiB memory, 4 threads, a 32-byte
derived key). They are in the file so they can be raised later without a format
break; putting them there is only safe with the rules below.

- **A ceiling, against a hostile file.** A header claiming more memory than a
  gigabyte is refused before Argon2id is asked to run with it, so it cannot be
  used as a memory bomb against whatever opens the file.
- **A floor, against a weak one.** Parameters below time 3, 64 MiB and a
  32-byte derived key are refused (`keys.ErrKDFParams`). This is not a defence
  against someone editing your header: the parameters feed the derivation, so
  an edited file simply stops opening, and anyone guessing offline against a
  stolen copy would use the parameters it was really written with. What the
  floor catches is a file that was legitimately written weak, and would
  otherwise open without a word while its protection was worth less than you
  believed.
- **The header is authenticated.** Everything in the file except the
  ciphertext is bound into AES-GCM as additional data: the version, the KDF and
  its parameters, the salt, the nonce, and the unencrypted list of public keys
  and addresses. For that to mean anything the file has to have exactly one
  reading, so a keystore must be one JSON object, carrying only the members
  this release knows, each named once, with nothing after it. Anything else is
  refused rather than ignored, because content that does not survive into the
  values this release decodes is content the binding does not cover, and
  another tool reading the same file could see it.
- **The address list is checked against the keys.** The unencrypted list is
  what you read to find an address to send to, so it is compared with the
  records that come out of the ciphertext, and a file where the two disagree is
  refused (`keys.ErrPubKeyList`). Version 1 authenticated nothing, so on a
  version 1 keystore this check carries that list on its own.

Version 1 keystores, written by v0.3.6 and earlier, are read as before and
rewritten as version 2 by the next `Save`. The strictness above applies to them
too: a field this release does not recognise, a member named twice, or anything
after the JSON object is refused rather than ignored. Nothing the SDK has ever written
carries either, so this reaches only a file something else annotated; the error
names the field, and removing it restores the file. Nothing is required of you; opening
one yields the same keys and the same addresses. Reading alone does not rewrite,
so a process that only loads leaves the file as it found it. Version 1 is
passphrase-only, so open it with `keys.NewManager`.

The rewrite goes one way. v0.3.6 reads version 1 only, and reports
`unsupported keystore version: 2` for a file this release has saved. Copy the
keystore before you upgrade if you may roll the binary back.

The encryption key is drawn again on every `Save`, from a fresh salt. With an
external key that never changes in your key-management system, that is what
keeps two saves from sharing an AES key.

### State files survive a crash

The keystore, the spent set (`utxo.SpentSet`) and the withdrawal intent file
(`withdraw.FileStore`) are written the same way: to a temporary file in the same
directory, which is synced, renamed into place, and followed by a sync of the
directory. A crash or power loss at any point leaves the previous file or the new
one, never a partial one, and a key, a reserved input or an intent is on disk when
the call that saved it returns without error. An error from `Save`, `MarkBroadcast`
or `Update` means the write is not known to be durable. Whether the record stayed
depends on the error: `ErrWrittenNotDurable` is reported after the file is in place
and the record stays, `withdraw.ErrStale` writes nothing, and `withdraw.FileStore`
puts its previous record back on any other failure. Keep the in-memory state (the
engine does for a spent set it could not write) and alert.

### Passphrase handling

This section is about `keys.NewManager`. On the `NewManagerWithKey` path there
is no passphrase; the at-rest protection is whatever guards the 32-byte key in
your key-management system, and the rules to read instead are that system's.

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

**The SDK zeroes the stack copies it makes, but it cannot zero key material
reliably, and no pure-Go library can.**

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

### A signed transaction

`tx.Transaction.VerifyAll` checks every input the way the node's single-key
Dilithium path does (`src/script/interpreter.cpp`): the witness is two items of
the consensus sizes, the public key carries its `0x00` prefix and hashes to the
output's 32-byte program, the hashtype byte at the end of the signature is
`SIGHASH_ALL`, and the signature verifies over the BIP 143 sighash recomputed from
the transaction with that hashtype. It is stricter than the node in one respect: the
node also accepts an unprefixed 1312-byte key, a form this SDK never emits; nothing
that passes `VerifyAll` is refused by the node. `tx.BuildSignedTransaction` and
`tx.BuildAndSign` call it before they return, so a transaction they hand back has
passed it. Call it yourself after signing by hand and before broadcasting a
transaction that was stored and reloaded:

```go
if err := signed.VerifyAll(); err != nil {
    // Wraps one of tx.ErrUnsigned, tx.ErrWitnessForm, tx.ErrHashType,
    // tx.ErrWrongKey, tx.ErrSignature with the input index. Do not broadcast.
    return err
}
```

The hashtype is read from the witness, not supplied by the caller. The node reads
that byte, refuses the `ANYPREVOUT` types on this path, and computes the sighash
for the other types under BIP 143 rules this SDK does not implement, so any byte
other than `SIGHASH_ALL` is refused (`tx.ErrHashType`). The check costs about
50 µs per input; an 80-input payout verifies in a few milliseconds
(`BenchmarkVerifyAll80Inputs` in `tx/verify_test.go`).

### A signature over your own digest

`keys.Verify` checks an ML-DSA-44 signature against a digest you computed:

```go
ok, err := keys.Verify(pubKey, digest, signature)
if err != nil {
    return fmt.Errorf("malformed key or signature: %w", err)
}
if !ok {
    return errors.New("signature does not verify")
}
```

Note the two-value result. `err` reports malformed input: a public key or
signature of the wrong length, a public key beginning with `0xFF`, the node's
invalid-key marker (`keys.ErrInvalidPublicKey`), or a witness-form signature
(2421 bytes) whose trailing hashtype byte is not `SIGHASH_ALL` (`keys.ErrHashType`). A
cryptographically invalid signature returns `false, nil`. Checking only `err`
accepts every forged signature of the correct size.

`keys.Verify` cannot tell whether your digest is the one the node will compute
from a transaction; that is what `VerifyAll` is for. Use `keys.Verify` for
signatures over messages of your own, such as a signed statement of an address.

### Constant-time behaviour

Constant-time verification is a property of CIRCL's ML-DSA implementation, which
this SDK calls. `keys.Verify` performs its length and hashtype checks and delegates
the comparison, so the guarantee is CIRCL's rather than ours. That is the right
place for it to live, but worth knowing precisely if you are reasoning about side
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
// Broadcast reports a node txid that differs from the one the SDK computed as
// rpc.ErrTxIDMismatch (the payment is out; hold the inputs, investigate),
// resolves a lost reply against the node, and reports "already in chain" as
// success. rpc.ErrUnknownOutcome means retry these bytes, never rebuild. A
// context that ends during the send is an unknown outcome too.
txid, err := rpcClient.Broadcast(ctx, rawHex, builtTxID)
if err != nil {
    return err
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
client := electrumx.NewClient("electrum.example.org:50002", 15*time.Second, logger)
client.UseTLS()
if err := client.Connect(ctx); err != nil {
    return err
}
```

`UseTLS` requires TLS 1.2 or better and verifies the server certificate against
the system roots. For a private CA or a pinned certificate, set `TLSConfig`
directly instead of calling `UseTLS`:

```go
client := electrumx.NewClient("electrum.internal:50002", 15*time.Second, logger)
client.TLSConfig = &tls.Config{RootCAs: myPool, MinVersion: tls.VersionTLS13}
if err := client.Connect(ctx); err != nil {
    return err
}
```

`TLSConfig` applies to reconnects as well. This matters: the client reconnects
automatically when the connection is lost, when two calls in a row time out
waiting for a reply, and after a panic in the refresher goroutine, so a
downgrade there would be silent and could last for days. There is a test that
pins it.

Do not set `InsecureSkipVerify`. An unverified TLS connection is worse than a
plaintext one, because it looks secure while an on-path attacker can still
substitute their own certificate.

Where TLS is not available, run ElectrumX on localhost or reach it over a tunnel
you control. That is a legitimate configuration and is why plaintext remains the
default.

### soqucoind RPC

```go
rpcClient := rpc.NewClient("http://127.0.0.1:33389", rpcUser, rpcPassword, logger)
```

Every request carries the RPC password in a Basic Auth header. The client refuses a
URL whose host is not loopback (the name `localhost`, or an IP literal in `127.0.0.0/8` or
`::1` written as a dotted quad or a bracketed IPv6 address, read from the URL without
resolving it) with `rpc.ErrRemoteNode` before anything is sent, until `AllowRemote` is set.
A URL copied from another deployment, or mistyped, fails instead of handing the password to
whichever host it names. `AllowRemote` permits the host, not plaintext to it: with the flag
set, a non-loopback URL whose scheme is not `https` is refused with `rpc.ErrPlaintextRemote`,
again before anything is sent, so a remote node is reached over `https://` or not at all.
Loopback is read from the URL as written and nothing is resolved, so a Docker service
name, `host.docker.internal`, a Kubernetes service, or another spelling of a loopback
address such as `127.1` or `localhost.` is a remote host to the client even when it reaches
this machine, and takes `AllowRemote` with `https://`, or a tunnel. The client does not follow
an HTTP redirect either: a node never sends one, and a proxy that answers `http://` with a
redirect to `https://` fails as a transport error instead of being followed.

- Bind `soqucoind` RPC to `127.0.0.1` and never expose it publicly. The node itself
  speaks plaintext only.
- Use a long random password. `rpcauth` with a salted hash is preferable to a
  plaintext `rpcpassword` in `soqucoin.conf`.
- For a node on another host, either run a tunnel that ends on this machine, so the
  URL stays loopback and needs no setting, or set `AllowRemote` and use an `https://`
  URL to a TLS terminator in front of the node. Go's default transport verifies the
  certificate against the system roots and refuses an invalid one; the client has no
  option for a private CA or a pinned certificate, so a private CA means a tunnel.
  Once `AllowRemote` is set, `http://` to a remote host is refused by the client
  (`rpc.ErrPlaintextRemote`): the password and every transaction would cross the network
  in plaintext.

```go
node := rpc.NewClient("https://node.internal:33389", rpcUser, rpcPassword, logger)
node.AllowRemote = true // the host is not loopback, and that is intended
```

---

## Input validation

### Addresses

Validate before you build anything. `Validate` takes the expected HRP, so it
checks the network at the same time as the checksum. It accepts exactly what the
node's own `DecodeDestination` accepts as a payment destination: witness version 1
with a 32-byte program. Versions 5 and 7 are USDSOQ script forms, not addresses;
v0.3.3 and earlier accepted them, and paying one is a loss of funds.

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

**All amounts in this SDK are `int64` shors.** There is no `Amount` type. 1 SOQ is
`types.ShorsPerSOQ` shors. `types.ParseSOQ` converts a decimal SOQ figure to shors
exactly; the RPC client reads every output value the node prints through it, and it accepts
the same form from a user (`"12.5"`, up to eight fraction digits, no sign or
exponent). Nothing validates the amount's meaning on your behalf.

Do not route an amount through `float64`. The builders
refuse amounts outside `0 < v <= tx.MaxMoney` (`tx.ErrInvalidAmount`), recipient
amounts below the node's relay floor (`tx.MinOutputValue`, `tx.ErrBelowDust`), and
input sums that would overflow; treat all of those as per-request errors, not as
system failures:

```go
// Reject anything non-positive before it reaches a builder.
if amountShors <= 0 {
    return errors.New("amount must be positive")
}
```

A `float64` holds 53 bits of mantissa, about 90,071,992 SOQ in shors. Above that
it rounds silently: `strconv.ParseFloat` on a user-supplied amount can produce a
value that differs from what was typed, and through v0.3.5 the RPC client read the
node's output values that way. `types.ParseSOQ` is exact for every value the node
can print.

### Fee rate is per vByte

`feeRate` in `tx.BuildAndSign` and `tx.BuildSendTransaction` is shors per
vByte, not a flat fee. A single-input ML-DSA payment is 1,073 vB. Use
`types.RecommendedFeeRate` (1,000); below it a default miner does not include the
transaction. The builders cap the fee at `tx.MaxFeeShors` and the rate at
`tx.MaxFeeRateShorsPerVB`, both adjustable, so a typo cannot burn the hot wallet.
Validate before you rely on a broadcast:

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
```

See [Exchange Integration](EXCHANGE_INTEGRATION.md#fee-estimation) for converting
a node fee estimate into a per-vByte rate.

---

## Reporting vulnerabilities

Please do not open a public issue for a security defect.

Email **[security@soqu.org](mailto:security@soqu.org)** with a
description, reproduction steps, and your assessment of the impact. Include a
suggested fix if you have one. The mailbox is monitored and was confirmed
receiving on 2026-09-03; `https://soqu.org/.well-known/security.txt` carries the
same contact. If you have not received an acknowledgement within two business
days, open a private vulnerability report on the GitHub repository so the report
is not lost to a mail problem.

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
  git tag -v v0.4.0
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
| This SDK's construction and client layer | Verified against the node's own format vectors, against addresses produced by the node's own encoder, and by confirmed on-chain transactions; an internal adversarial review in September 2026 found and fixed defects in every package (see the release notes); no separate external engagement |
| ML-DSA-44 implementation used here | [Cloudflare CIRCL](https://github.com/cloudflare/circl). The node verifies with the pq-crystals reference C implementation, so two independent implementations meet on every signature; their agreement is pinned by signatures verified across the pair and by the confirmed transaction |

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
