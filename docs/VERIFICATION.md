# Transaction Verification Record

This document lets you verify the signing path independently rather than take our
word for it. It records a transaction **built, signed, serialized, broadcast and
confirmed entirely by the public SDK**, with identifiers you can decode against a
Soqucoin node yourself, and the procedure to reproduce it.

Nothing here requires access to our infrastructure. Every identifier below is
public on-chain data.

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

**The witness stack sizes are 2421 and 1313, not 2420 and 1312.** The extra byte
on each is the sighash type and the FIPS 204 key prefix; see
[the format below](#the-witness-format-consensus-requires).

**The transaction id the SDK computed matches the one the node assigned.** This is
the check worth making your own: it independently confirms that the SDK's
serialization agrees with consensus byte for byte, since any disagreement would
produce a different hash.

---

## The witness format consensus requires

Worth stating explicitly, because it is Soqucoin-specific and the two lengths are
easy to get wrong by one byte each:

```
stack[0] = signature || sighash-type byte    (2421 bytes)
stack[1] = 0x00      || public key           (1313 bytes)
```

The trailing sighash byte follows Bitcoin convention. The leading `0x00` is
required because NIST FIPS 204 Table 3 specifies that ML-DSA-44 public keys begin
with that byte, and the node checks it directly.

`Transaction.SignAll` and `tx.BuildAndSign` assemble this for you and reject
wrong-sized key material. If you implement your own signer instead, these are the
lengths to target.

## Diagnosing a rejected transaction

If you build transactions yourself during onboarding, `testmempoolaccept` will
tell you which check you are failing. These are the responses we have observed and
what each one means:

| Response | Cause |
|---|---|
| `bad-txns-requires-dilithium` | Witness format wrong. Check for the trailing sighash byte and the leading `0x00`, and for the 2421/1313 lengths above |
| `rate limited free transaction` | Fee too low to relay. `feeRate` is per vByte; at 10 a ~1,073 vB transaction pays about 10,700 shors, which the node treats as effectively free |
| `allowed: true` | Ready to broadcast |

Size your fee against `vsize` rather than byte count, and validate with
`testmempoolaccept` before you rely on any broadcast.

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
    Value:   fundingValueShorss,
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

## Scope of this record

Stated precisely, so you can see exactly what has and has not been demonstrated.

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

## Independent validation

If you validate the signing path against your own known-answer vectors during
onboarding, we would like to know what you find, particularly if anything
disagrees with the above. Outside review has already improved this SDK, and we
would rather hear about a discrepancy early than have it surface in production.
