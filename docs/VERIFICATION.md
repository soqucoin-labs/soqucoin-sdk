# Transaction Verification Record

This document exists so that an exchange evaluating this SDK does not have to take
our word for anything. It records a transaction **built, signed, serialized,
broadcast and confirmed entirely by the public SDK**, with identifiers you can
decode against a Soqucoin node yourself, and the exact procedure to reproduce it.

Nothing here requires access to our infrastructure. Every identifier below is
public on-chain data.

---

## Why this record exists

An exchange reviewing this SDK asked, reasonably, for proof rather than
assurances. Their argument was that our own release notes for `v0.3.1` disclosed
that every pre-Phase-4 transaction signature had been computed over the wrong
message, so "fixed" needed to be demonstrated rather than asserted.

They were right, and running the exercise found **two further defects that
inspection had not**. Both are described below, because the failures are more
useful to a reviewer than the success.

---

## The confirmed transaction

Stagenet, built and signed by the public SDK:

| | |
|---|---|
| **Transaction id** | `99fd147aaa4d575ee8f6266acfda4b09a5b0dc730d964294efded2cf3cd2eae7` |
| **Block** | `ad12368c1e083a6f0efe8da7cc65b52613b05d3301f0e609ba4660fdcffcf380` |
| Funding transaction | `1d9e069a50294a8d0cf6d60e7a29b59ee4e2db4eb99ead71c823874a06037394` |
| Inputs / outputs | 1 in, 2 out (payment plus change) |
| Size / vsize | 3,880 bytes / 1,073 vB |
| Witness stack | `[2421, 1313]` bytes |
| Network | stagenet |
| SDK version | `v0.3.2` and later |

Decode it against any stagenet node:

```bash
soqucoin-cli getrawtransaction \
  99fd147aaa4d575ee8f6266acfda4b09a5b0dc730d964294efded2cf3cd2eae7 1
```

Two things are worth checking specifically.

**The witness stack sizes are 2421 and 1313, not 2420 and 1312.** That is the
consensus-required format and the subject of defect 2 below.

**The transaction id the SDK computed matches the one the node assigned.** That is
an independent confirmation that the SDK's serialization agrees with consensus
byte for byte, which is what the `v0.3.1` CTxOut fix was about.

---

## What the exercise found

### Defect 1: the SDK could not build a mainnet transaction

Every builder called the address decoder with a hardcoded `"ssq"`, the **stagenet**
prefix. Mainnet addresses use `"sq"`, and the decoder rejects a mismatched prefix,
so every builder failed on every mainnet address with `expected ssq, got sq`.

That error message reads like a malformed address rather than a wrong assumption
inside the SDK, which is why it survived. No test caught it because every fixture
in the suite was a stagenet address.

This mattered beyond a failed call: the script derived from the address is what
BIP143 commits to as the `scriptCode`. A wrong network would not merely fail
loudly. Had it ever succeeded, it would have signed over the wrong message.

**Fixed** by deriving the network from the address (`address.HRPOf`), refusing
prefixes that belong to no known network, and refusing transactions that mix
networks. Regression tests build the same transaction on mainnet, stagenet and
regtest.

### Defect 2: the witness format was rejected by consensus

Consensus requires:

```
stack[0] = signature || sighash-type byte    (2421 bytes)
stack[1] = 0x00      || public key           (1313 bytes)
```

The trailing sighash byte follows Bitcoin convention. The leading `0x00` is
required because NIST FIPS 204 Table 3 specifies that ML-DSA-44 public keys begin
with that byte, and the node checks it directly.

The SDK had **no witness-assembly helper at all**. The only signing example
assembled a bare signature and a bare public key, with neither the sighash byte
nor the `0x00` prefix, so every transaction it produced was rejected.

**Fixed** by adding `Transaction.SignInput` and `SignAll`, which install the
correct format and refuse wrong-sized key material, and `tx.BuildAndSign`, a
one-call build-sign-serialize path.

### The rejection sequence, which is the actual evidence

Run in order against a live node. The progression is more informative than the
final success, because it shows which check each fix satisfied.

| Attempt | `testmempoolaccept` result |
|---|---|
| Original witness format (bare signature and public key) | `bad-txns-requires-dilithium` |
| Corrected witness format, `feeRate` 10 | `rate limited free transaction` |
| Corrected witness format, `feeRate` 1000 | **`allowed: true`** |

The middle row is worth noting for your own integration: **the fee rate used in
our own examples was too low for the node to relay.** At 10 sat/vB a ~1,073 vB
transaction pays about 10,700 satoshis, which the node treats as effectively free
and rate-limits. Size your fee against `vsize`, and validate with
`testmempoolaccept` before broadcasting.

---

## Reproducing this yourself

You need a synced Soqucoin node and a funded address on the network you are
testing. No access to our systems is required.

### 1. Generate a keypair with the SDK

```go
kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP) // or types.Mainnet.HRP
```

Fund `kp.Address` by any means available to you.

### 2. Build, sign and serialize in one call

```go
mgr := keys.NewManager("keystore.enc", passphrase)
if err := mgr.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
    return err
}

witVer, witProg, err := address.Decode(types.Stagenet.HRP, kp.Address)
if err != nil {
    return err
}
spk := address.WitnessProgram(witVer, witProg)

in := []types.UTXO{{
    TxID:    fundingTxID,
    Vout:    fundingVout,
    Value:   fundingValueSats,
    Address: kp.Address,
}}

rawHex, txid, err := tx.BuildAndSign(in, spk, amountSats, spk, feeRate, mgr)
```

### 3. Validate before broadcasting

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
```

Expect `"allowed": true`. If you see `bad-txns-requires-dilithium`, the witness
format is wrong. If you see `rate limited free transaction`, raise the fee rate.

### 4. Broadcast and confirm

```bash
soqucoin-cli sendrawtransaction '<rawHex>'
soqucoin-cli getrawtransaction '<txid>' 1
```

The `txid` returned by `BuildAndSign` must equal the one the node reports. If they
differ, the SDK's serialization disagrees with consensus and you should not
proceed.

---

## What this record does not cover

Stated plainly, because a verification document that overclaims is worse than none.

- **This is a single-input, single-signature payment.** It does not exercise
  multi-input batching, USDSOQ asset transactions, or the authority paths.
- **It was performed on stagenet.** Mainnet construction is covered by unit tests
  across all three networks, but no mainnet transaction has been broadcast,
  because mainnet does not yet exist.
- **It proves the signing and serialization path, not the whole SDK.** The
  deposit-monitoring path depends on an indexer and is not covered here.

Per-package unit test coverage is reported in
[Exchange Integration](EXCHANGE_INTEGRATION.md#test-coverage-current-status).

---

## Independent validation we would welcome

If you validate the signing path against your own known-answer vectors as part of
onboarding, we would like to know what you find, including and especially if it
disagrees with anything above. Two of the defects fixed in the last two releases
were found by an outside reviewer reading this code rather than by us.
