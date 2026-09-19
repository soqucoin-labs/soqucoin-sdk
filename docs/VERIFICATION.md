# Transaction Verification Record

This document lets you verify the signing path independently rather than take our
word for it. It records four transactions **built, signed, serialized, broadcast and
confirmed entirely by the public SDK**, the second, third and fourth through its
withdrawal engine, with identifiers you can decode against a Soqucoin node yourself, and the
procedure to reproduce them.

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

## The confirmed withdrawal through `withdraw.Engine`

Stagenet, three inputs, produced from the v0.3.5 tree by
[`examples/stagenet_withdrawal`](../examples/stagenet_withdrawal): the intent was persisted, the
inputs reserved, the transaction broadcast through `rpc.Client.Broadcast` and confirmed through
`withdraw.RPCConfirmer`, all against a stagenet node with `txindex=1`.

| | |
|---|---|
| **Transaction id** | `13568c1a34416618fc5d160385643304230062bb6c53023522ec9e7f08fb67be` |
| **Block** | `823edee8e491e706b16f38923cfc30767516fadca9d3247d7dd72a1ed13d5e42` (height 82,297) |
| Funding transactions | `c776afdd025599fafa86707d90b55c13ead24cba705ba1950a1c12913478267b`, `43c457ab09f9666ba347663fed80a7236329d79bf07d81e3ce6240d3fe625e0c`, `f9a53afe4399fa8c83885d7f24dfeab1e80f245541fc953ff818826f528ee35c`, output 0 of each |
| Inputs / outputs | 3 in, 2 out (payment plus change) |
| Size / vsize | 11,444 bytes / 3,026 vB |
| Witness stack, each input | `[2421, 1313]` bytes |
| Network | stagenet |
| SDK version | `v0.3.5` and later |

```bash
soqucoin-cli getrawtransaction \
  13568c1a34416618fc5d160385643304230062bb6c53023522ec9e7f08fb67be 1
```

## The confirmed withdrawal from the v0.4 tree

Stagenet, built and signed by the v0.4 tree and driven through `withdraw.Engine`: the intent was
persisted before anything reached the network, the input was reserved in the spent set at build
time, the transaction was broadcast once, and the engine's confirmer moved the intent to confirmed
on the node's word.

| | |
|---|---|
| **Transaction id** | `b0c659f7d63fd1346c6d4d95e15cbe465e7464213391654f694a08fa62f8422c` |
| **Block** | `c1c1c95fae85e1fbcc1cf2c7e586f61a3fbc558e083a370583f22e56b74c0a44` (height 85,205, 2026-09-16T07:46:13Z) |
| Funding transaction | `c4bf3724891b21bbc09c9330092f9b8f328799e7b2d80dc5d5c2778dd7f1b655`, output 0 |
| Inputs / outputs | 1 in, 2 out (payment plus change) |
| Size / vsize | 3,880 bytes / 1,073 vB |
| Witness stack | `[2421, 1313]` bytes |
| Payment / change | 2,497,900,000 / 1,026,000 shors |
| Fee | 1,074,000 shors, 1,001 shors/vB against the 1,000 asked for |
| Network | stagenet |
| SDK version | `v0.4.0` |

```bash
soqucoin-cli getrawtransaction \
  b0c659f7d63fd1346c6d4d95e15cbe465e7464213391654f694a08fa62f8422c 1
```

Two things this record adds to the two above. The amounts are `int64` shors end to end, so the
payment is exactly 2,497,900,000 shors rather than a decimal figure rounded into one. And the
transaction id the SDK computed before the broadcast is the id the node assigned, which is the
check worth making your own: any disagreement in serialization would produce a different hash.

The engine path is the one [`examples/stagenet_withdrawal`](../examples/stagenet_withdrawal) runs.
This particular run reached the node over the public builders gateway (`gettxout`,
`getrawtransaction` and a client-signed broadcast over HTTPS) because the machine that produced it
had no route to an RPC port; the engine, the selector, the keystore and the signing path were the
SDK's own. With node RPC of your own, the example reproduces it directly.

## The confirmed withdrawal from the v0.5 tree

Stagenet, built and signed by the v0.5 tree and driven through `withdraw.Engine` by
[`examples/stagenet_withdrawal`](../examples/stagenet_withdrawal) as committed, against a stagenet
node with `txindex=1` over RPC: the intent was persisted before anything reached the network, the
input was reserved in the spent set at build time, the transaction was broadcast once through
`rpc.Client.Broadcast`, and `withdraw.RPCConfirmer` moved the intent to confirmed on the node's
word.

| | |
|---|---|
| **Transaction id** | `ebe41fd8ac7feaafbf2e99bbc9522fdecddb527227fa6a9e7db99c4a7377c9ef` |
| **Block** | `2541c140fc0b43b44b21a57c402d34ccaa6174193136467ba0a02320dd322d7d` (height 89,196, 2026-09-19T06:17:08Z) |
| Funding transaction | `428c090ada2b8c3db1089f5ea847dda0a3328ccbe3428aa8e1f51cecc9f9f58d`, output 0 |
| Inputs / outputs | 1 in, 2 out (payment plus change) |
| Size / vsize | 3,880 bytes / 1,073 vB |
| Witness stack | `[2421, 1313]` bytes |
| Payment / change | 2,400,000,000 / 98,926,000 shors |
| Fee | 1,074,000 shors, 1,001 shors/vB against the 1,000 asked for |
| Network | stagenet |
| SDK version | `v0.5.0`; no Go source changed between this run and the tag |

```bash
soqucoin-cli getrawtransaction \
  ebe41fd8ac7feaafbf2e99bbc9522fdecddb527227fa6a9e7db99c4a7377c9ef 1
```

The transaction id the SDK computed before the broadcast is the id the node assigned, and the
node's decode of the confirmed transaction gives the sizes and witness lengths above. This record
differs from the v0.4 one in transport only: the example reached the node's RPC port directly, so
the confirmer and the broadcaster are the SDK's `rpc.Client`, as they will be in your deployment.

---

The keys were derived with `keys.DeriveSeed` and `keys.FromSeed` from a master secret created for
the run (under the v1 scheme of v0.3.5; the keystore file holds the keys, so the v2 derivation of
v0.3.6 changes nothing about the transaction); the funding came from the stagenet faucet. To
reproduce, run the example's `-init` step, fund the address it prints, and run it again with the
funding outpoints.

Two things are worth checking specifically on either transaction.

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

- **These are single-signature SOQ payments of one and three inputs.** They do not
  exercise USDSOQ asset transactions or the authority paths.
- **All four were performed on stagenet.** Mainnet construction is covered by unit tests
  across all three networks, by address vectors produced by the node's own
  encoder, and by a sighash and witness vector produced by the node's own
  signing path (below), but no mainnet transaction has been broadcast: the
  mainnet genesis exists (2026-09-02) and the network launches later.
- **They prove the signing, serialization and withdrawal-engine paths, not the
  whole SDK.** The deposit-monitoring path depends on an indexer and is not
  covered here.

## The node-produced signing vector

The stagenet transactions above prove that the node accepts what the SDK signs,
with a node present. The fixture in `tx/testdata/sighash_node_vector.json` proves
the same thing offline, in the direction an integrator can check without any
infrastructure: a fixed two-input transaction was signed by the node's own
signing path (the `sdk_sighash_vector_tests` suite in the node repository, which
asserts the same digests and txid), and the SDK's tests rebuild it from the
recorded fields and check that

- the SDK's BIP 143 digest for each input equals the digest the node handed its signer;
- the node's signatures verify over those digests under the SDK's ML-DSA-44 library
  (a signature verifies over exactly one message, so this is the proof that the two
  preimages are the same bytes);
- the serialized transaction and txid are byte-identical to the node's;
- the key the node signed with is the SDK's own `keys.FromSeed` key for the recorded seed;
- every field the preimage commits to, changed one at a time, fails verification of
  exactly the inputs whose preimage carries it.

Run them with `go test ./tx -run NodeVector`, and with `SOQUCOIN_TX` pointing at a
node's `soqucoin-tx` binary to decode the recorded bytes with the node live. The
witness in the fixture is one run's output, because ML-DSA-44 signing is
randomised; the digests, the key and the txid are deterministic.

Per-package unit test coverage is reported in
[Exchange Integration](EXCHANGE_INTEGRATION.md#test-coverage-current-status).

---

## Independent validation

If you validate the signing path against your own known-answer vectors during
onboarding, we would like to know what you find, particularly if anything
disagrees with the above. Outside review has already improved this SDK, and we
would rather hear about a discrepancy early than have it surface in production.
