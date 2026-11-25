# Exchange Integration Guide

Step-by-step guide for listing Soqucoin (SOQ) on your exchange.

---

## Overview

To support SOQ deposits and withdrawals, your exchange needs to:

1. **Generate deposit addresses**: one unique address per user
2. **Monitor deposits**: track UTXOs through ElectrumX subscriptions, credit through your node
3. **Process withdrawals**: build, sign, and broadcast transactions
4. **Confirm transactions**: wait for sufficient block confirmations

All four run against a node you operate with `txindex=1` and an ElectrumX indexer; see
[What you run](#what-you-run) for the configuration, ports and measured sizing.

SOQ uses **NIST FIPS 204 ML-DSA-44** (Dilithium) for all signatures. Transaction structure is similar to Bitcoin/Dogecoin (UTXO model), but witness data contains Dilithium signatures (~2,420 bytes) and public keys (~1,312 bytes).

---

## Integration model, read this first

**You run our node. You do not use the node's built-in wallet.** Those are different things and the
distinction is the whole integration:

| | |
|---|---|
| ✅ **You run** | `soqucoind`, our node, this is how you see the chain and broadcast |
| ❌ **You do not use** | the wallet subsystem *inside* that node. Turn it off with `disablewallet=1`, as every mainnet node we operate does (the Linux and Windows release builds include the wallet and start it by default; the macOS builds leave it out), so `listunspent`, `getbalance` and `sendtoaddress` are not part of the integration surface |
| ✅ **You still need, and still do** | wallet *functions*, address derivation, key custody, signing. Those run in **your** infrastructure via this SDK |

In other words the node is a chain reader and broadcaster; your key vault stays your key vault.
Exchanges already work this way for other UTXO coins rather than keeping customer keys inside
`bitcoind`.

This is the same pattern exchanges use to integrate any UTXO chain at scale, deposit addresses
derived from your own key store, chain watched by an indexer, withdrawals signed in your own signing
infrastructure, and the node used only to read the chain and broadcast. **It is not a
Soqucoin-specific arrangement**, and if you already integrate Bitcoin or Litecoin this way, the
shape will be familiar.

| Component | What you use | SDK package |
|-----------|--------------|-------------|
| **Deposit addresses** | Derive per-user keypairs in your own key store | [`keys`](../keys), [`address`](../address) |
| **Deposit monitoring** | ElectrumX indexer, polled for UTXO changes | [`electrumx`](../electrumx) |
| **Withdrawal signing** | Your signing infrastructure, offline or HSM-backed | [`keys`](../keys), [`tx`](../tx) |
| **Broadcast + chain reads** | Node JSON-RPC (`sendrawtransaction`, `gettxout`, `getblock`) | [`rpc`](../rpc) |

**Why the node wallet is not used:** ML-DSA-44 keys are roughly 60x larger than the ECDSA keys a
Bitcoin-derived wallet was designed around (2,560-byte private, 1,312-byte public, versus ~32 and
~33). At exchange scale, one deposit address per user, that key material belongs in your own
storage rather than in the node's embedded wallet database. Nothing about this reduces what you can
do; it moves key custody to where you want it anyway.

**What this means for your integration estimate:** the node is a standard Bitcoin-style JSON-RPC
daemon for every call in this guide, and the post-quantum specifics are confined to signature
construction, which this SDK handles. Be aware, however, that this is **not** the templated
`getnewaddress` / `sendtoaddress` integration path, it requires wiring this SDK into your deposit
and withdrawal flow, plus an ElectrumX indexer. See the next section, which is the item most likely
to affect your estimate.

### ⚠️ ElectrumX indexer, a decision we need from you

Because the node's wallet is disabled, there is no address index to query for deposits. The SDK
reads UTXOs from an **ElectrumX indexer**, and one has to exist. There are two ways to arrange it,
and we would rather you choose than discover it later:

| Option | What it means | Trade-off |
|--------|---------------|-----------|
| **A. You run your own** | You operate an ElectrumX instance alongside your node, as you may already do for other UTXO coins | ✅ No dependency on our infrastructure, no shared point of failure |
| **B. You connect to ours** | You point the SDK at an ElectrumX endpoint we operate | ✅ Nothing extra to run. ⚠️ Creates a hard dependency on our availability for your deposit crediting, which most exchanges reasonably refuse |

**We recommend Option A, and the software is published:**

> **https://github.com/soqucoin-labs/electrumx**, branch `soqucoin`, tag **`soqucoin-1.19.0-3`** (22 November 2026)

Upstream ElectrumX ships no Soqucoin coin definition, so that fork is what you need. It is based on
a pinned upstream commit rather than tracking `master`, so the entire Soqucoin delta is reviewable
in one command:

```bash
git diff 24865dc..soqucoin-1.19.0-3 -- src/
```

Roughly 550 lines across eight files: the coin definitions (AuxPoW-aware headers), the AuxPoW
transaction deserializer, asset and visibility derivation from the witness version, UTXO records
carrying that metadata, and three protocol extensions. `get_multi_balance` answers per-asset balances
and `listunspent_by_asset` answers the unspent outputs of one asset. `listunspent_bounded` answers at
most 1,000 outputs of an address that holds many, with a cache of its answers.

You can confirm a clean checkout works before committing any effort to it:

```bash
git clone -b soqucoin-1.19.0-3 https://github.com/soqucoin-labs/electrumx && cd electrumx
pip install .
cd /tmp && python -c "
from electrumx.lib.coins import Coin
print(Coin.lookup_coin_class('Soqucoin', 'stagenet').DESERIALIZER.__name__)
"
```

CI runs that on every push, on Python 3.10 and 3.12, installing from a fresh checkout so only
committed files can satisfy it. [`SOQUCOIN.md`](https://github.com/soqucoin-labs/electrumx/blob/soqucoin/SOQUCOIN.md)
in that repository documents the configuration. The mainnet coin class carries the ceremony genesis
(`0d828600…86a8`, 2026-09-02); a checkout from before that fix refuses to sync mainnet, and the SDK's
ElectrumX client refuses a server whose reported genesis is not the chain its addresses belong to.
Upstream `LICENCE` is unchanged.

Tags name the revisions our own instances run. `soqucoin-1.19.0-3` (22 November 2026) adds
`blockchain.scripthash.listunspent_bounded`. It answers at most 1,000 of an address's unspent outputs,
with the count and value of all of them. When the address holds more than the limit asked for, it
chooses in a fixed order: confirmed outputs largest first, then pending ones largest first. It takes
a transaction's outputs together when all of them still fit. A transaction with more outputs than the
places left is passed over, so the list can hold pending outputs while confirmed ones are left out.
Read `height` on each entry. For an address of 1,000 outputs or more it keeps an answer until the
next block is indexed or the address changes in the mempool. It may drop an answer sooner to stay
within its memory.

By default the indexer refuses a reply over 10,000,000 bytes (`MAX_SEND`). An output takes about 160
bytes of a reply, so it refuses a `listunspent` of more than about 60,000 outputs. The SDK's client
refreshes a tracked address with `listunspent`, so keep an address under that count by consolidating
its outputs ([UTXO Consolidation](#utxo-consolidation)). An address over it is refused on every
refresh. The error matches `electrumx.ErrReplyTooLarge` under `errors.Is`, from `RefreshAll` and on
the address's record (`LastRefreshOf`). `Client.ListUnspentBounded` reads such an address through
`listunspent_bounded`: at most 1,000 outputs, with the count and the value of all of them. Its answer
stays out of the client's cache, so the balance and the coin selector do not see it.

`soqucoin-1.19.0-2` (18 October 2026) carries the
fix for the reorg undo path: a checkout from before it stops at the first mainnet reorg, height
7,434, and recovers on restart at the tag with no reindex. A running instance moves to a newer tag
with a fetch of the tags, a checkout of the tag, a reinstall of the package into the environment its
service runs from (`pip install .` from the checkout: a non-editable install keeps its own copy of the
code, so a checkout alone changes nothing for the service) and a restart.

**So the question we need answered:** do you run your own indexer for UTXO-model coins, or would
you expect to connect to one we operate? That answer changes both the integration effort and where
the operational risk sits, so it is worth settling before scoping.

If neither option suits your architecture, tell us, we would rather adapt than have you commit to
something that does not fit your operations.

---

## What you run

Two processes: the node, and an ElectrumX indexer, yours under option A above. Everything the
SDK reads or broadcasts goes through them.

### The node

| | |
|---|---|
| Software | `soqucoind` from [github.com/soqucoin/soqucoin](https://github.com/soqucoin/soqucoin) at tag **`v2.6.1`**, the current release (published 29 October 2026). The release carries one consensus rule on mainnet, scheduled in 2.6.0: from block 67,000, expected about 28 November 2026 at the block rate of the two weeks to 22 November, every block must carry the chain id 0x5351 in its version. The rule changes nothing for a current node: every block in mainnet history from block 1 carries the id and every template a Soqucoin Core node builds carries it. A node still on 2.5.x at the height follows a block the upgraded network refuses, if such a block appears, for as long as that branch is the heavier one. Once that node's own chain is 288 or more blocks past the fork, every release refuses the network's headers however heavy the network's chain becomes, and a late upgrade alone does not recover the node. The operator runs `invalidateblock` on the hash of the block without the chain id, or replays the block files with `-reindex`; rebuilding the chainstate alone does neither. Upgrade every mainnet node before the height. The height can move later once, by a point release published no later than 16 November 2026; install it the day it appears. The peer-to-peer protocol version and the on-disk formats are those of 2.5.3, and the RPC interface gains one field (`chainid_required_height` in `getblockchaininfo`, 67000 on mainnet), so the upgrade is a replacement of the two programs with no reindex. A node on 2.6.0 moves to 2.6.1 by the same replacement. An explicit `blockmaxsize=750000`, the old default, should be removed: release 2.6.0 raised the default to 3000000 and an explicit value wins (the 2.6.0 release notes, "The default block template"). The release page carries Linux x64 and arm64 archives with `SHA256SUMS`; they bundle every library except glibc (2.35 or newer, per its `INSTALL.md`; on an older distribution, build from source). Install an archive after checking it against `SHA256SUMS`, or build the `v2.6.1` tag yourself per its `INSTALL.md` or `Dockerfile`. Every mainnet node we operate has run 2.6.1 since 31 October 2026. The golden transaction vector in the tests is the v2.3.0 node's decode; the integration harness ran against a build of the `v2.5.0` tag commit (`9d3ceb4f2d0fe41`) before this SDK tag. Our own builds carry no published digest; one would identify our build machine as much as the software. |
| `txindex=1` | **Required, and set before the first start**; adding it later means rebuilding the chainstate with `-reindex-chainstate`. `withdraw.RPCConfirmer`, `rpc.Client.TxConfirmations` and the lost-reply check in `rpc.Client` ask the node for a transaction by id with `getrawtransaction`. Without the index the node finds a mined transaction only through its UTXO set (`src/validation.cpp`, `GetTransaction` with `fAllowSlow`), so once every output has been spent, the recipient's and then your change, the transaction reads as unknown and the withdrawal never settles in the engine's view. |
| `disablewallet=1` | The node's wallet is not part of this integration (see [Integration model](#integration-model-read-this-first)). Every mainnet node we operate runs with it. |
| `server=1` plus `rpcauth` (or `rpcuser` and `rpcpassword`) | The node's RPC is plaintext. Bind it to localhost, where `rpc.Client` connects with no further setting. For a node on another host the client refuses the URL until `AllowRemote` is set (`rpc.ErrRemoteNode`), and with the flag set refuses `http://` to it (`rpc.ErrPlaintextRemote`); the route is a tunnel that ends on this machine or an `https://` TLS terminator in front of the node ([Security Guide](SECURITY.md#network-security)). Loopback is read from the URL as written, the name `localhost` or a dotted-quad or bracketed IPv6 literal in `127.0.0.0/8` or `::1`, and nothing is resolved: a Docker service name, `host.docker.internal` or a Kubernetes service is a remote host to the client and needs the same route. |
| `stagenet=1` | For the staging network. Omit it for mainnet. |
| `dbcache` | The nodes in [Sizing, measured](#sizing-measured) run 512 MB; the node's default is 450. |

Ports, as `types.Mainnet`, `types.Stagenet` and `types.Regtest` carry them (node
`src/chainparams.cpp` and `src/chainparamsbase.cpp`):

| Network | Address prefix | P2P | JSON-RPC |
|---|---|---|---|
| mainnet | `sq1p` | 33388 | 33389 |
| stagenet | `ssq1p` | 28333 | 28332 |
| regtest | `sq1p` | 18444 | 18332 |

Set `Client.Network` (`c.Network = types.Mainnet`). `RequireSynced`, which `deposit.Monitor`, the
reconciler and `VerifyAndFilterUTXOs` call, then compares the node's `chain` with `Network.ChainID`
and refuses a mismatch with `rpc.ErrWrongChain`, so a mainnet deployment pointed at a stagenet node
fails before it credits a deposit or signs a withdrawal. Left unset, the client applies mainnet
rules without the check.

### The indexer

The ElectrumX fork at [github.com/soqucoin-labs/electrumx](https://github.com/soqucoin-labs/electrumx),
branch `soqucoin`, tag `soqucoin-1.19.0-3`, the revision our own instances run (the indexer section
above). Configuration is in its `SOQUCOIN.md`:
`COIN=Soqucoin`, `NET=stagenet` or `mainnet`, `DAEMON_URL` at your node's RPC. The default TCP
service port is 50001 (`types.Network.ElectrumPort`); TLS is whatever port you terminate it on.

One operational note from running it. ElectrumX stores its history flush counter in 16 bits
(`src/electrumx/server/history.py`, `flush_id = pack_be_uint16(self.flush_count)`). The counter
advances on every flush, and a caught-up server flushes after each block it processes
(`server/block_processor.py`, `_maybe_flush`), so at one block a minute an indexer uses the 65,535
ids in about 45 days whether or not it restarts; a start or a clean shutdown flushes only when the
height moved since the last flush (`server/db.py`, `flush_dbs`). At the limit every flush fails
with `struct.error: 'H' format requires 0 <= number <= 65535`, the process exits, a supervisor
restarts it, and the index stays frozen at one height. Watch the `flush #N` line the server logs
on each flush (`flush count: N` at startup) and run `electrumx_compact_history` with the server
stopped well before N reaches 65,535; compaction resets the counter to about the row count of
the largest address history.

### Sizing, measured

Mainnet on 2026-11-22 UTC at height 58,347, 39 days after block 1. The figures are read from the
two hosts of ours that each run a mainnet node and an indexer (`getblockchaininfo`, `VmRSS` and
`VmHWM` in `/proc/<pid>/status`, `du`). The indexers ran `soqucoin-1.19.0-2` at that reading. The
cache that `soqucoin-1.19.0-3` adds holds at most 50,000 outputs, about 25 MB:

| | |
|---|---|
| Node data directory with `txindex=1` | 1,532 MiB (`du`). Block and undo files are 1,463 MiB of it (`size_on_disk`), the block and transaction index 27 MiB and the chainstate 28 MiB |
| `soqucoind` resident memory | 1,001 MiB on one host and 1,439 MiB on the other. Each node had run 12 days at `dbcache=512`, feeding an indexer and serving RPC. The highest either reached is 1,983 MiB |
| ElectrumX database (LevelDB) | 85 MiB |
| ElectrumX resident memory | 124 MiB and 361 MiB. The highest either reached is 448 MiB |
| Hosts these were read from | 4 vCPU with 8 GB running two nodes and two indexers, for mainnet and stagenet; 8 vCPU with 16 GB |

Blocks arrive one a minute (`nPowTargetSpacing = 60`). Mainnet's block files grew by about 37 MiB
a day on average over those 39 days. The rate follows transaction volume: a block that carries
only its coinbase is about 1 KB. Plan storage from the growth you observe. We have not timed an
initial sync on a reference host.

### Signing, measured

One run of the benchmarks in the repository, 2026-09-14, Apple M4, Go 1.26
(`go test ./keys ./tx -run '^$' -bench . -benchmem`). Run them on the host that will sign; these
give the shape of the cost, not a figure for your hardware.

| Operation | Benchmark | Time | Memory per operation |
|---|---|---|---|
| One ML-DSA-44 signature, `keys.Manager.Sign` | `BenchmarkManagerSign` (`keys/bench_test.go`) | 178 µs | 3.1 KB, 4 allocations |
| One verification, `keys.Verify` | `BenchmarkVerify` (`keys/bench_test.go`) | 51 µs | 16.8 KB, 4 allocations |
| Signing an 80-input payout, `SignAll` | `BenchmarkSignAll80Inputs` (`tx/bench_test.go`) | 14.4 ms | 1.6 MB |
| Verifying an 80-input payout, `VerifyAll` | `BenchmarkVerifyAll80Inputs` (`tx/verify_test.go`) | 4.4 ms | 2.2 MB |
| Writing the keystore, `keys.Manager.Save` (Argon2id, AES-256-GCM, durable write) | `BenchmarkManagerSave` (`keys/bench_test.go`) | 39 ms | 67 MB, the Argon2id working memory |
| Generating a key, `keys.GenerateKeyForNetwork` | `BenchmarkGenerateKeyForNetwork` (`keys/bench_test.go`) | 62 µs | 63 KB |

Signing is not where ML-DSA-44 costs an exchange. The size is: 3,782 bytes per input on the wire
([Transaction Size](#transaction-size)), and on the deposit side one `listunspent` round trip per
tracked address per poll ([Step 2](#step-2-monitor-deposits)).

---

## Before you write code: run the harness

Fifteen minutes, one command, and you have seen every flow in this guide work against a real node:

```bash
git clone https://github.com/soqucoin-labs/soqucoin-sdk && cd soqucoin-sdk
SOQUCOIND=/path/to/soqucoind make integration   # a soqucoind build, v2.3.0 or later
```

The harness starts a throwaway regtest node, mines to SDK-generated addresses, and drives the same
`deposit` and `withdraw` packages this guide uses through seventeen scenarios. Ten cover deposits
and withdrawals. A deposit is credited only after the node confirms it. A withdrawal is built,
signed, broadcast, mined and confirmed. A lost broadcast reply is survived with exactly one payment.
Two withdrawals cannot share an input. Refused inputs (a USDSOQ-form destination, an amount below
the relay floor, a fee-rate typo) never reach the node. A reorganisation that removes a credited
deposit raises the alarm. Retries past the reservation TTL keep their inputs. A node that accepts
the bytes under a different txid leaves the inputs held. A broadcast whose context is cancelled
while the node holds the reply is recovered after a restart with exactly one payment. A swept
deposit whose outputs are all spent is counted past the horizon with `TxConfirmations`.

Four more cover the push model the `electrumx` client uses: a deposit the indexer pushes, credited
after the node check with exactly one `listunspent` behind it; a notification the indexer never
sends, covered by the reconcile pass; an indexer outage across which a deposit is mined, picked up
from the status in the re-subscribe reply; and one address the indexer fails, skipped and alarmed
while the other is credited.

Three more cover batch payments. A 120-payee batch is mined as one transaction and every output is
read back from the node. A batch whose broadcast reply is lost is sent again with the same bytes,
and each payee holds one output. The third builds `examples/pool_payout` and runs it as a process:
a dry run, a run of three batches, the same command again, and a changed file under the same run
id. [Pool Payouts](POOL_PAYOUTS.md) is the guide to that path.

The indexer role is played by an in-test block scanner, and for the push scenarios by an in-process
fake ElectrumX over that scanner, so the harness needs no ElectrumX build. The methods that fake
answers are checked against the client's own call sites, because a double that has drifted from its
client is believed rather than noticed. The ElectrumX client's wire behaviour is exercised by its
own protocol tests against a scripted server.

The harness also holds ten cases for the swap contract of the `htlc` package, which this guide does
not cover. They need a node that reports the activation heights of witness version 6. A node does
that from commit `8de57e851` of `main`, and no release carries that commit yet. On a release the
harness skips the ten and prints the reason.

---

## Context and logging

Every method that reaches the network, or your storage, takes a `context.Context` first:
`rpc.Client`, `electrumx.Client`, `deposit.Monitor.Scan`, every `withdraw.Engine` method, and
the interfaces you implement for them (`withdraw.Store`, `withdraw.Broadcaster`,
`withdraw.Confirmer`, `withdraw.Selector`, `withdraw.BuildSigner`, `deposit.Ledger`,
`deposit.Node`). Methods that only read a cache (`GetUTXOs`, `GetBalance`, `LastRefresh`) take
none; they never block. The one network call without a context is the webhook post of
`resilience.Alerter`, which is fire-and-forget on its own goroutine with a ten-second timeout.
The context is how a process bounds a call and how it shuts down:
`electrumx.Client.Start` and `resilience.Reconciler.Start` end with it.

What a context that ends does to a withdrawal, state by state
(`withdraw/engine.go`, `rpc/client.go`):

| Where it ends | What happens | Why |
|---|---|---|
| In the selector or the signer, intent Created | Stays Created, attempt recorded, nothing reserved or built | The node has said nothing about the payment; an interrupted attempt is not a verdict |
| In `Broadcast`, intent Built | Stays Built, reservation renewed, `rpc.ErrUnknownOutcome` carrying the context's error | The node may hold the transaction; only the same bytes may go out again |
| In `UpdateConfirmations` or `Abandon` | Error returned, no state change | |
| In `Rebroadcast`, intent Broadcast | Stays Broadcast, attempt recorded, `SentAt` moved | The node may hold the transaction again |
| In `Recover` | The passes that repair the spent set run to the end whatever the context says; every Built intent is re-reserved, none is sent, and each is reported as not sent with the context's error | Every Built intent is sent by the next `Recover` or `Broadcast` |

`rpc.Client.Broadcast` treats a context ending during the send as a lost reply: the resolution
lookup runs on the same, ended, context and reports nothing, and the outcome stays unknown. It is
never a rejection. Nothing a context can do releases a Built intent's inputs, fails an intent, or
builds a second transaction. The harness proves it against the node: scenario 9 hands the
broadcast to the node through a proxy that never returns the reply, cancels, restarts, and pays once.

The context governs what the engine asks of the network and how long it waits for the store
reads of `Submit` and `Process`. It does not govern the writes that record what the network did,
nor the reads that decide what the engine may do: every `withdraw.Store.Update` is made under
`context.WithoutCancel(ctx)`, because the record (a broadcast accepted, a txid the node disagreed
on, a lost reply's attempt) must land whether or not the caller is still waiting; so is the `Get`
each of `Build`, `Broadcast`, `UpdateConfirmations`, `Rebroadcast` and `Abandon` makes under the
engine's lock to re-read the record it acts on, and so are the `Get` and `List` calls `Recover` makes to repair the spent set.
`Submit`'s `Create` is bound to the caller's context, since nothing has happened yet and an
abandoned registration must not become a payment. `WithoutCancel` carries no deadline, so a
database-backed `Store` bounds every method with its own timeout and does not rely on the
context for that; the re-reads run with the engine's lock held, so a `Get` that hangs holds every
transition in the process. `deposit.Ledger` follows the same rule: `IsCredited` and `Pending` receive
`Scan`'s context, `Credit` and `MarkFinal` one the caller cannot end. `deposit.Monitor.Scan`
returns a context error plainly, from whichever node or ledger read it ended, never as
`ErrPaused` and never as an alert: a shutdown is not an indexer lying.

`electrumx.Client`: calls run concurrently on the one connection, and a call whose context
ends returns `ctx.Err()` at once, whether it is waiting to write or waiting for its reply. Its
reply, if it arrives later, finds no waiter and is dropped; replies are paired with calls by id,
never by position. A call cut short while writing closes the connection, since part of a line
may be on the wire, and the next call returns `ErrNotConnected` until `Reconnect` or `Start`'s
refresher restores it. A context deadline shorter than the 30-second call deadline bounds the
exchange.

`resilience`: a reconciliation run the context ends before it found anything is `Incomplete`
and reported, but it neither alerts nor trips the breaker; a run that had already recorded a
mismatch when the context ended alerts and trips like any other. `CircuitBreaker.RecordResult`
treats `context.Canceled` as a per-request error that does not count; `context.DeadlineExceeded`
counts. A trip puts the breaker in `HALTED`: no cooldown and no later success clears it,
`Allow` returns `resilience.ErrHalted` with the reason, and `CircuitBreaker.Reset` is the exit,
reported through `OnStateChange` as the halt was. Give the reconciler the engine's spent set
(`Reconciler.Spent`): an output the node no longer has because this process spent it is
counted in `Report.OwnSpends` and is not a finding; without the set every own spend halts payouts.
Start the reconciler after `Recover`, which re-marks into the set the spends the intent store
knows. The exclusion covers reservations as well as sends, so a spend of a reserved input by
another holder of the key is not a finding for that run; the engine reports it itself, as a
rejection at the send or a held intent. The breaker is memory, so a restart closes it and the first run after `InitialDelay` halts it
again while the book still disagrees; a restart is not the response to a halt. The webhook the
alerter posts names the transition and the failure count; the reason, which can carry
transaction ids and amounts, stays in the process log.

Every component takes a `*log/slog.Logger` in its constructor (`rpc.NewClient`,
`electrumx.NewClient`, `utxo.OpenSpentSet`, `resilience.NewCircuitBreaker`, `NewAlerter`,
`NewReconciler`) or as a `Logger` field (`withdraw.Engine`, `deposit.Monitor`). Nil discards
everything; the SDK writes nothing to the process's default logger. Levels: `Info` for
connections, cache evictions, spent-set writes and clean reconciliations; `Warn` for a reorg seen in
the cache, a refresh that failed, a reservation that could not be renewed, a reconciliation the
context ended, a webhook the server did not accept, and, on `deposit.Monitor`, every alert when
`OnAlert` is nil; `Error` for a circuit breaker opening or halting, a reconciliation that found a mismatch
or could not complete, a webhook that could not be sent, and a panic in the refresher goroutine.
Identifiers are logged whole, as attributes or in the message, never truncated.

---

## Step 1: Generate Deposit Addresses

Derive one deposit address per user from a master secret that stays in your key-management
system. The derivation is FIPS 204's own: `keys.FromSeed` runs ML-DSA-44 KeyGen on a 32-byte seed,
and `keys.DeriveSeed` produces that seed from your master secret and a per-user index. The master
and the index each user was given are the only recovery material.

```go
import (
	"errors"
	"math"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// DepositAddress derives a deposit address starting at derivation index i and
// returns the index it used. Store that index with the user record; the key is
// re-derived from it at sweep time. hrp is types.Mainnet.HRP in production and
// types.Stagenet.HRP on a test host, which has a master of its own.
func DepositAddress(hrp string, master []byte, i uint32) (string, uint32, error) {
	for {
		seed, err := keys.DeriveSeed(master, hrp, i)
		if err != nil {
			return "", 0, err // keys.ErrShortMaster and keys.ErrUnknownHRP are configuration faults, not indices to skip
		}
		kp, err := keys.FromSeed(hrp, seed)
		for j := range seed {
			seed[j] = 0 // FromSeed zeroed its own copy; this is the caller's
		}
		if errors.Is(err, keys.ErrInvalidPublicKey) {
			// About 1 index in 256 derives a key the node can never spend from; leave it unused.
			if i == math.MaxUint32 {
				return "", 0, errors.New("derivation index space exhausted")
			}
			i++
			continue
		}
		if err != nil {
			return "", 0, err
		}
		return kp.Address, i, nil
	}
}
```

The scheme, so that your own key-management system can implement it and land on the same
addresses:

```
seed    = HMAC-SHA256(key = master, message = "soqucoin-sdk/keys/seed/v2/" || hrp || "/" || index as 4 bytes big-endian)
key     = ML-DSA-44 KeyGen(seed)                       (FIPS 204, deterministic)
address = bech32m(hrp, witness version 1, SHA-256(public key))
```

The master must be at least 32 bytes of secret random data (`keys.MinMasterSize`). The network
prefix is part of the message, so one master derives different keys for `sq` and `ssq`: a
production master that reaches a stagenet host yields stagenet keys there, not keys that also
spend mainnet funds. Regtest shares `sq` with mainnet and gets no such separation. Give every test
host a master of its own all the same. The v1 scheme of v0.3.5, which
had no network in the message, was replaced before any integrator derived an address under it.
Vectors for the whole chain are in `keys/seed_test.go`; the address encoding is pinned to addresses
produced by the node's own encoder in `keys/node_vectors_test.go`.

**Two key stores, two jobs.** Derived keys are for deposit addresses: nothing is stored per user
except the index, and at sweep time you re-derive the key and hand it to an in-memory `keys.Manager`
through `ImportPrivateKey` (no `Save`) to sign. `keys.Manager` with `GenerateKeyForNetwork` is the
hot-wallet store: randomly generated keys in a file encrypted with AES-256-GCM under an
Argon2id-derived key, loaded and rewritten as one document, sized for a hot wallet's few addresses
rather than one per user. Back that file up; a generated key has no seed. Neither `FromSeed` nor the
generator returns a key the node treats as invalid (a public key whose first byte is `0xFF`), and
`Load` refuses a record whose address does not belong to its key or whose private key does not sign
for its public key, and a passphrase manager refuses an empty passphrase wherever it would be used:
`Load`, `LoadOrCreate` and `Save`. Key handling in detail: [Security Guide](SECURITY.md).

---

## Step 2: Monitor Deposits

Two sources, two roles. The ElectrumX indexer **discovers** candidate deposits quickly across
thousands of addresses. Your own `soqucoind` gives the **verdict**: nothing is credited unless your
node confirms the exact output, its value, its destination script and its depth. That is what
`deposit.Monitor` does, and it also refuses to credit while your node is still syncing (the finality
horizon is not enforced during initial download) or while the indexer has stopped refreshing (an
outage looks exactly like "no deposits"), and it re-verifies every credited deposit until it passes
the horizon so a reorganisation that removes one is alarmed rather than missed.

**How the indexer client keeps addresses fresh.** `electrumx.Client.Start` subscribes to every
tracked address (`blockchain.scripthash.subscribe`, `electrumx/subscribe.go`); when the server
reports an address's history changed, the client makes one `blockchain.scripthash.listunspent` for
that address and merges the reply into the cache. A notification writes nothing to the cache by
itself: only a `listunspent` reply does, and credit still needs your node's `gettxout`, so what a
faulty or lying indexer can do with a notification is what it could do with a poll reply. The
reply is validated as a whole before it is merged (`electrumx/client.go`, `parseUnspent`): a
malformed one is refused, the cache keeps its set, and the address records the error so the
Monitor skips and alarms it. Two timers back the subscriptions. The reconcile interval given to `electrumx.NewClient` is a full
`listunspent` pass over every address whatever the statuses say: the safety net for a notification
the server never sent, so a dropped notification delays a deposit by at most that interval plus
your scan period. `PingInterval` (60 seconds by default) is a `server.ping` that keeps the server
from closing an idle connection and, on your side, advances the freshness of every subscribed address
with no change pending. The ping runs on its own goroutine beside any pass in progress
(`electrumx/subscribe.go`, `pingLoop`), so a long reconcile does not age the addresses it has not
reached. A quiet address reads fresh for as long as the server answers pings; when the connection is
lost, no ping is answered and every address ages out within `MaxCacheAge`. `MaxCacheAge` on
`deposit.Monitor` (5 minutes by default) must hold two `PingInterval`s, one of them missed: `Scan`
reads the interval in force (`electrumx.Client.FreshnessInterval`) and refuses a shorter window
with `deposit.ErrCacheAgeBelowPings` before it asks the node or the indexer anything.

`deposit.Monitor` judges freshness per address (`LastRefreshOf`, `deposit/monitor.go`,
`AddressFreshness`): an address the indexer has not answered for within `MaxCacheAge` is skipped and
alarmed once per pass with the count, its deposits wait for the pass that reaches it, and the
Monitor pauses only when every address is stale. The costs, each cited to the code that pays it:

- Steady state: one `listunspent` per changed address (`subscribe.go`, `pass`). A block that pays N
  tracked addresses costs N calls, in sequence, on one connection.
- Reconnect: one `subscribe` per address (`pass`), plus one `listunspent` per address whose status
  changed while the client was away; an unchanged address whose record is clean costs the
  subscribe call alone (`subscribe`, the status comparison). A connection that dies after every
  handshake is redialled on a backoff from one second to a minute (`subscribe.go`, `run`; the
  schedule is `refreshpolicy.go`).
- Failure: a call whose reply does not arrive within the 30-second call deadline ends the pass at
  that call (`conn.go`, `endsPass`); the retry's pass asks a pending address first, against a
  stalled server that call times out too, and the second timeout rebuilds the connection
  (`refreshpolicy.go`, `onResult`), so a server that has stopped answering costs two deadlines
  plus the backoff in force whatever the address count. An address the server refuses is asked
  again on the same backoff ladder, on every pass another event runs and on the ladder's timer,
  and holds nothing else: a notification for another address is refreshed at once (`onEvent`;
  the refusal sets no `paced`). The ladder resets only on a clean pass, so while any address is
  refused it sits at its one-minute ceiling and that is the backoff a later timeout waits out.
- Reconcile: one `listunspent` per address (`subscribe.go`, `pass` with `full` set, through
  `client.go`, `refresh`); the interval must exceed the pass.
- The client's own cost per address, over loopback against a server that answers at once, is
  44 µs for one subscribe and one listunspent (about 22,700 addresses per second), measured by
  `BenchmarkSubscribeAndRefreshPerAddress` in `electrumx/bench_test.go`; from a notification to the
  `listunspent` it asks for, between 26 and 46 µs across runs at the benchtime the file names
  (`BenchmarkNotificationToRefresh`, `-benchtime 1000x`). Your indexer's round trip is
  added to each call, so a reconnect over 60,000 addresses at a 5 ms round trip is about five minutes
  of subscribe calls, during which the addresses not yet re-subscribed keep their last records and
  age toward `MaxCacheAge`. Run one client per block of addresses if that ramp is too long.

Before v0.4 the client polled every address every 15 seconds, so one client kept at most
`MaxCacheAge` divided by the round trip addresses fresh; in v0.3.5 one failed call marked the whole
pass stale and paused every credit until a pass succeeded for every address.

```go
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// lines are the two value lines and the ceiling from Step 4, in shors. The
// figures are placeholders: set them to the amounts of SOQ worth about 100 USD,
// about 1,000 USD and about 10,000 USD at your own price, and move them with
// Tiers.SetLines when the price moves. A deposit below the first line is
// credited at 10 confirmations, one below the second at 30, and one at or
// above it at types.MaxReorgDepth (288).
var lines = deposit.TierLines{
	SmallBelow: 500_000 * types.ShorsPerSOQ,
	LargeFrom:  5_000_000 * types.ShorsPerSOQ,
	Ceiling:    50_000_000 * types.ShorsPerSOQ,
}

// memLedger stands in for your database. Credit must be idempotent on the
// outpoint, and in production the credit and the balance change belong in the
// same database transaction. The context is Scan's; pass it to every query.
type memLedger struct {
	mu       sync.Mutex
	credited map[string]deposit.Deposit
	final    map[string]bool
}

func key(txid string, vout uint32) string { return txid + ":" + string(rune('0'+vout)) }

func (l *memLedger) Credit(_ context.Context, d deposit.Deposit) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credited[key(d.TxID, d.Vout)] = d
	log.Printf("CREDIT %s:%d %d shors to %s at %d confirmations", d.TxID, d.Vout, d.Value, d.Address, d.Confirmations)
	return nil
}
func (l *memLedger) IsCredited(_ context.Context, txid string, vout uint32) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.credited[key(txid, vout)]
	return ok, nil
}
// Pending leaves out outputs the exchange spent itself; see below.
func (l *memLedger) Pending(context.Context) ([]deposit.Deposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []deposit.Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}
func (l *memLedger) MarkFinal(_ context.Context, txid string, vout uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.final[key(txid, vout)] = true
	return nil
}
// NotFinal is what the ceiling counts: every credited deposit that is not
// final yet, whether or not it is still in Pending. A ledger that sweeps a
// deposit early takes it out of Pending and keeps it here; see below.
func (l *memLedger) NotFinal(context.Context) ([]deposit.Deposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []deposit.Deposit
	for k, d := range l.credited {
		if !l.final[k] {
			out = append(out, d)
		}
	}
	return out, nil
}

func main() {
	depositAddresses := []string{"sq1p...", "sq1p..."} // one per user, from your database

	// One context for the process, one logger for every component. The SDK
	// logs nothing unless it is given a logger.
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// The indexer. Over anything but a private network, use TLS: the server
	// sees every address you track, and plaintext to a host that is not
	// loopback is refused unless AllowPlaintext says the path is private. The
	// network is inferred from the addresses; mixed or undecodable addresses
	// are refused.
	elx := electrumx.NewClient("electrumx.example.com:50002", 10*time.Minute, logger) // the reconcile interval
	elx.UseTLS()
	if err := elx.TrackAddresses(depositAddresses); err != nil {
		log.Fatalf("track addresses: %v", err)
	}
	if err := elx.Connect(ctx); err != nil { // also verifies the server's genesis hash
		log.Fatalf("connect: %v", err)
	}
	defer elx.Stop()
	elx.Start(ctx) // subscribes, refreshes on push, reconciles, pings; ends with ctx or Stop

	// Your own node. Nothing is credited on the indexer's word alone.
	node := rpc.NewClient("http://127.0.0.1:33389", "rpcuser", "rpcpass", logger)
	node.Network = types.Mainnet // RequireSynced refuses a node on any other chain

	ledger := &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{}}
	// The three tiers of Step 4 and their safeguards. Set both fields below:
	// Required alone is the tiers without the safeguards.
	tiers, err := deposit.NewTiers(node, ledger, lines)
	if err != nil {
		log.Fatalf("deposit tiers: %v", err)
	}
	monitor := &deposit.Monitor{
		Cache:     elx,
		Node:      node,
		Network:   types.Mainnet, // coinbase maturity 288; Scan refuses a node on any other chain
		Ledger:    ledger,
		Addresses: func(context.Context) []string { return depositAddresses },
		Required:  tiers.Required,
		Safeguard: tiers,
		OnAlert: func(kind deposit.AlertKind, msg string) {
			// Route to your paging. Every alert here is a human's problem:
			// indexer and node disagreeing, a credited deposit that vanished,
			// crediting paused because the node or indexer is not current.
			log.Printf("ALERT %s: %s", kind, msg)
		},
		Logger: logger, // receives the alerts when OnAlert is nil
	}

	var ahead, unsettled bool // what the last page said is held back
	for range time.Tick(30 * time.Second) {
		// Bound each pass so one stalled node call cannot outlive the interval.
		passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		credited, err := monitor.Scan(passCtx)
		cancel()
		// Page when either hold begins and when it ends, each on its own: a
		// branch with more work can appear while the chain is already unsettled.
		if st := tiers.Status(); (st.Ahead != "") != ahead || (st.Unsettled != "") != unsettled {
			ahead, unsettled = st.Ahead != "", st.Unsettled != ""
			log.Printf("ALERT deposit_safeguard: every deposit waits: %q; the lower tiers wait for 288: %q", st.Ahead, st.Unsettled)
		}
		if err != nil {
			log.Printf("scan: %v", err) // deposit.ErrPaused while the node or indexer is not current
			continue
		}
		if len(credited) > 0 {
			log.Printf("credited %d deposits", len(credited))
		}
	}
}
```

The `Ledger` is your database. `Credit` must be idempotent on the outpoint, and the credit and the
balance change belong in the same database transaction. Every `OnAlert` is a condition a human
should see: indexer and node disagreeing, a credited deposit that vanished, crediting paused.

`Pending` must not return an output you spent yourself. A deposit swept to the hot wallet before it
was final looks to the node exactly like one a reorganisation removed, gone from the UTXO set, and
`Monitor` cannot tell the two apart: a swept output still in `Pending` raises `AlertDepositVanished`
on every scan. Record the sweep in the ledger and leave the output out of `Pending`, or mark it
final once the sweep transaction is.

`deposit.Tiers` is the confirmation policy of Step 4 together with its safeguards. `Tiers.Required`
gives the tier of a value: 10, 30 or 288 confirmations. As the Monitor's `Safeguard`, the same
`Tiers` reads your node and your records once in every pass. It then holds back a deposit that has
reached its tier in five cases:

- **The ceiling.** A deposit below 288 confirmations waits when it would take the value credited
  below 288 over `TierLines.Ceiling`. It is credited at 288. A credited deposit counts until it is
  final. The Monitor marks it final when your node has it more than 288 deep and `Tiers` does not
  hold the mark, as described below the list.
- **An unsettled chain.** Every deposit below 288 confirmations waits while the chain is unsettled.
  It is unsettled while your node's chain tips (`getchaintips`) show a branch of three or more
  blocks beside the active chain, ending within the last 288 blocks. It is also unsettled while the
  hashrate of the last 120 blocks is below half that of the last 1,440, or the node reports none.
- **A branch with more work.** Every deposit waits, at any depth, while your node holds the headers
  of a branch with more work than its chain. The branch is one that leaves the chain below the tip,
  and its work is the `chainwork` of `getblockheader`. Your node follows such a branch once it has
  the blocks.
- **A reading of another tip.** A deposit waits when your node reported its depth at another tip
  than the one the reading was taken at (`Deposit.BestBlock`, the `bestblock` of `gettxout`). A
  block arrived between the two, and a branch that came with it is in no reading yet. Keep a pass
  shorter than the time between two blocks.
- **No reading.** Every deposit waits while the node or your records cannot be read, or the last
  reading is older than `Tiers.MaxAge` (5 minutes by default). The age is counted from the moment
  the reading began.

A held deposit raises no alert and is asked about again on the next pass. `Tiers.Status` says what
is held back and why. Page when it changes, as the loop above does: a branch with more work that
stays is a condition a human should see.

The same reading decides when a credited deposit is final. The Monitor asks `Tiers.HoldFinal`
before it marks one. The mark waits in three of the five cases: a branch with more work, a reading
of another tip, and no reading. A count of 289 alone would end the watch on a deposit beside a
branch your node already holds the headers of. Until the mark the deposit stays in `Pending`. It is
rechecked on every pass, alarmed if your node no longer has it, and counted against the ceiling. A
deposit marked final in one pass leaves the ceiling's count at the next.

The ceiling reads your records through `deposit.Book`, which has one method. `NotFinal` returns
every credited deposit that is not final yet, as it was credited. If you spend no deposit before it
is final, the list holds what `Pending` holds. A deposit you sweep earlier leaves `Pending` and
stays in this list: a reorganisation that removes the deposit takes the sweep with it. Take it off
when the sweep transaction has more than 288 confirmations on your node and `Tiers.HoldFinal` does
not hold it. `client.TxConfirmations(ctx, sweepTxID)` returns the count and the best block your
node counted it at, whether or not the outputs of the sweep are spent. For a sweep whose outputs
are all spent the node needs `txindex=1`, which this guide requires. Ask with that best block:
`tiers.HoldFinal(deposit.Deposit{BestBlock: best})`. Read the counts before `monitor.Scan`, which
refreshes the reading, and ask only after a Scan that returns no error. The reading then lists
every branch your node held while it counted. A sweep in the mempool or in a block off your node's
active chain gives a count of 0 and no best block. So does a read that ends at another best block
than it began at. Ask again on the next pass. With no best block `HoldFinal` holds. Build the list
on its own and not from `Pending`, so a swept deposit you forget keeps counting. The count is read
from your records on every pass, so a restart loses nothing.

The height in a record is not read. A reorganisation can move a credited deposit to a later block,
and the Monitor marks it final from your node's own count of its depth. A deposit your node no
longer has keeps counting until you settle its record.

---

## Step 3: Process Withdrawals

A withdrawal is a state machine, not a call. `withdraw.Engine` makes the two failures an exchange
cannot afford impossible by construction:

- **No double payment.** Each withdrawal is an `Intent` under your idempotency key. The signed
  transaction is persisted **before** it is broadcast, and a broadcast whose reply was lost
  (`rpc.ErrUnknownOutcome`) is retried with the **same bytes**, never rebuilt. `rpc.Broadcast`
  resolves that case against your node and treats "already in chain" as success.
- **No own double-spend.** Inputs are reserved in the spent set when the transaction is built,
  all-or-nothing, and unconfirmed spends survive restarts however long confirmation takes. The
  spent set and the intent file are synced to disk before the call that wrote them returns, and
  the directory after the rename, so a power loss right after `Broadcast` returns does not lose
  the record of it. Every unsettled broadcast attempt renews the reservation, so retry Built intents
  at an interval shorter than `ReservationTTL` (default `withdraw.DefaultReservationTTL`, 4 hours).
  The TTL is a backstop, not the cleanup: `Recover` releases every reservation held for an intent
  the store knows as Created or Failed, so a crash between reserving and persisting frees the
  coins at the next start, and a Built intent's bytes, which may already be in a mempool, keep
  their inputs for hours rather than minutes. The intent store and the spent set are one unit: a
  reservation under an id the store does not know means the intents file is missing or older than
  the spent set, and `Recover` keeps it and returns `withdraw.ErrUnknownReservation`. Never start
  with an empty intents file next to a populated spent set.
- **A node that is briefly behind does not fail a withdrawal.** A selector error that is
  `rpc.ErrTransient` (`RequireSynced` while the node is one block behind its headers, a node in
  warmup, a transport failure) leaves the intent Created with the attempt in `Attempts` and
  `LastError`; call `Process` again and the same id builds once the node is back. So does a
  context that ends while the selector or the signer is working: nothing is reserved, nothing
  is built, the intent waits. Any other selector error (insufficient funds,
  `rpc.ErrWrongChain`, a permanent node error, your own selector's failure) fails the intent.
- **A context that ends during the broadcast is a lost reply, never a failure.** The intent
  stays Built with its reservation renewed, the error is `rpc.ErrUnknownOutcome` carrying
  `context.Canceled` or `context.DeadlineExceeded`, and the next `Process` or `Recover` sends
  the same bytes. Shutting the process down mid-broadcast leaves exactly the state a crash
  leaves, and the same recovery applies. See [Context and logging](#context-and-logging).
- **A node that accepts the bytes under a different txid** (`rpc.ErrTxIDMismatch`) is neither a
  rejection nor a retry: the payment is in the mempool. The inputs are marked spent under the
  node's txid, the intent stays Built with `NodeTxID` recorded, and `Broadcast` and `Recover`
  refuse to send it again (`withdraw.ErrHeld`); stop withdrawals and investigate before anything
  is rebuilt. The split example's broadcaster does this: its startup `Recover` runs under an
  ended context and only repairs, its send pass is the one sender, and that pass sends nothing
  while the store holds a held intent and ends at a hold that arises inside it.
- **A destination on another network is refused where it enters.** The engine is bound to a
  network (`withdraw.Engine.Network`, mainnet when unset, as for `deposit.Monitor`): `Submit`
  refuses a destination that is not a witness version 1 address on its prefix with
  `withdraw.ErrInvalidIntent` and records nothing, and `Build` applies the same check to the
  stored record before anything is selected and fails an intent that does not pass. The prefix
  is not part of the script, so nothing downstream would refuse it: `address.ScriptFor` builds
  the script for any supported prefix and the node accepts the transaction, so a stagenet
  destination would be paid on mainnet to whoever holds that program there. Regtest shares
  mainnet's prefix; the address alone cannot tell them apart.
- **A rejection after a lost reply holds the intent; it does not fail it.** Before every send the
  record is saved with `MaybeRelayed` set and `SentAt`, and nothing is sent when that save fails;
  the mark is taken back only on an answer that shows the node did not take the bytes on that
  attempt (a rejection, a transient error, refused credentials), so a lost reply, and a save that
  fails after the send, leave it set. A permanent rejection on an intent that carries the mark (the node restarted with a higher minimum fee, or dropped its mempool) then
  holds the intent instead of failing it: peers keep a relayed transaction for the node's mempool
  expiry, 24 hours, and can mine it after the node forgot it, so releasing its inputs would let
  the next withdrawal spend coins the first payment can still take. The intent stays Built with
  `Hold` set and its inputs marked spent under its txid, `Broadcast` and `Recover` refuse it
  (`withdraw.ErrHeld`), and `Abandon` is the way out. Failed therefore means: the intent could not
  be built, or the node refused the bytes and no attempt's outcome was unknown, or an operator
  abandoned the intent.
- **A node that refuses the credentials** (`rpc.ErrUnauthorized`, a 401 or 403 before any handler
  ran) has taken nothing: the intent stays Built with the mark put back to what earlier attempts
  left, and the same bytes go out once the credential or the allowlist is fixed. It is not a lost reply, and it counts for the
  breaker: nothing sent under it can succeed.
- **A Broadcast intent whose transaction the node no longer knows has two exits.**
  `UpdateConfirmations` returns `rpc.ErrUnknownOutcome` for it and changes nothing.
  `Rebroadcast` sends the same bytes again and changes no state on any outcome: success says the
  node has the transaction again, a refusal says the node does not have it now, which is not
  proof that no peer does. `Abandon`, the operator's transition to Failed for a Broadcast intent
  or a held Built one, refuses (`withdraw.ErrNotAbandonable`, nothing changed) unless `AbandonAfter`
  (default `withdraw.DefaultAbandonAfter`, the node's mempool expiry) has passed since the last
  send (`SentAt`, set before every send; a re-send the node refused puts the earlier time back),
  the node does not know the transaction, and every input is unspent at the node; it needs a `withdraw.Chain` (`withdraw.RPCChain` over your node, which refuses to answer
  while the node is behind). On success the intent is Failed with its bytes kept and its
  spent-set entries dropped, so the inputs return to selection; when that drop fails, `Recover`
  drops them at the next start (`AbandonedAt` marks the record; entries owned by any other Failed
  intent are kept and reported). Records written by an earlier release carry no `SentAt` and no
  mark: their attempts count as possibly relayed, and their last write stands for their last send. The residual risk is a peer that kept the bytes longer than the
  node's expiry, which the node applies lazily, on its next mempool acceptance rather than on a
  clock, so a quiet peer keeps them longer; the record keeps the txid so a reconciliation finds
  the transaction if it mines. A `Rebroadcast` the node refused does not restart the wait; one it
  took, or whose outcome is unknown, does.

If `Process` returns an error that is `utxo.ErrPersist`, the node accepted the payment and the
spent set could not be written (an `utxo.ErrAlreadyReserved` on the same path means the set and
the store disagree about who owns an input, which a spent set and intent store that were not
running as a pair produce): the intent is saved as `Broadcast`, the process still refuses those
inputs, and `Recover` re-marks every Broadcast intent's inputs from the intent store at startup, so
a restart does not re-expose them. That guarantee holds only if you stop when `Recover` returns an
error: a failed `List` means nothing was re-marked. Open the spent set with `utxo.OpenSpentSet`,
which refuses a file that exists but cannot be read; an empty set in that case would forget every
unconfirmed spend.

`Recover` at startup re-sends anything persisted but not yet acknowledged. The circuit breaker is
fed through `RecordResult`, which never counts a per-request error (a bad address, an amount below
the floor, insufficient funds, a node rejection of one transaction), so a user cannot halt every
withdrawal with three malformed requests. A withdrawal the engine refuses to act on is also a
per-request error: `withdraw.ErrWrongState` when another worker holds the intent,
`withdraw.ErrInvalidIntent`, `withdraw.ErrConflict` and `withdraw.ErrFailed`. A worker pool loses a
race for the same id as a matter of course, and counting those losses would let three of them halt
every payout. `withdraw.ErrHeld`, `withdraw.ErrReservationLost` and `rpc.ErrUnauthorized` do
count: the first two name a transaction an operator resolves before anything else is sent, the
third a credential nothing can succeed under, and halting is the point. `ErrHeld` carries a
rejection as text only, never wrapped, so the `rpc.ErrPermanent` inside it does not read to the
breaker as one bad request.

```go
package main

import (
	"context"
	"log"
	"log/slog"
	"errors"
	"os"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/resilience"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

func main() {
	hotWallet := "sq1p..." // the address whose key the keystore holds
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Keys. The manager refuses records the node could not spend from,
	// records whose address does not belong to their key, records whose
	// private key does not sign for their public key, and an empty passphrase.
	keystore := keys.NewManager("/var/lib/exchange/keys.enc", os.Getenv("SOQ_KEYSTORE_PASSPHRASE"))
	if err := keystore.Load(); err != nil {
		log.Fatalf("load keystore: %v", err)
	}

	// Indexer and node.
	elx := electrumx.NewClient("electrumx.example.com:50002", 10*time.Minute, logger) // the reconcile interval
	elx.UseTLS()
	if err := elx.TrackAddresses([]string{hotWallet}); err != nil {
		log.Fatalf("track: %v", err)
	}
	if err := elx.Connect(ctx); err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer elx.Stop()
	elx.Start(ctx)
	node := rpc.NewClient("http://127.0.0.1:33389", "rpcuser", "rpcpass", logger)
	node.Network = types.Mainnet

	// Halts only for systemic failures (RecordResult never counts a bad
	// request), and the reconciler trips it when the book and the chain
	// disagree, into a halt that only cb.Reset clears.
	cb := resilience.NewCircuitBreaker(3, 15*time.Minute, logger)

	// Inputs are reserved here at build time, and unconfirmed spends survive
	// a restart however long confirmation takes.
	spent, err := utxo.OpenSpentSet("/var/lib/exchange/spent_set.json", logger)
	if err != nil {
		log.Fatal(err) // a file that exists but cannot be read must not start empty
	}
	selector := utxo.NewCoinSelector(spent)

	// Durable intents: persisted before anything is broadcast. An exchange
	// with a database implements withdraw.Store over it and keeps the same
	// rules: Create and Update are durable before they return, Create fails
	// on an existing id, and Update writes only over the state it names.
	store, err := withdraw.NewFileStore("/var/lib/exchange/withdrawals.json")
	if err != nil {
		log.Fatalf("open intents: %v", err)
	}

	changeSPK, err := address.ScriptFor(hotWallet)
	if err != nil {
		log.Fatalf("hot wallet address: %v", err)
	}

	engine := &withdraw.Engine{
		Store:                 store,
		Spent:                 spent,
		Network:               types.Mainnet, // Submit and Build refuse a destination that is not an address on it
		Broadcaster:           node, // rpc.Broadcast resolves lost replies against the node
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		Chain:                 withdraw.RPCChain{Client: node}, // Abandon's node checks
		RequiredConfirmations: types.MaxReorgDepth,
		Logger:                logger,
		// ReservationTTL left at its default, withdraw.DefaultReservationTTL (4 hours).
		Select: func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error) {
			if err := node.RequireSynced(ctx); err != nil { // transient while behind: the intent stays Created
				return nil, err
			}
			tip, err := node.GetBlockCount(ctx)
			if err != nil {
				return nil, err
			}
			// Budget the fee against the inputs the selection takes, not
			// against the cap. A one-input, two-output payment is about
			// 1,073 vB and each further ML-DSA-44 input adds about 976 vB, so
			// asking for utxo.MaxInputsPerTX inputs' worth demands about
			// 0.78 SOQ of headroom at the recommended rate: a wallet holding
			// 25 SOQ in one output could not then pay 24.99, and the last
			// 0.78 SOQ of any hot wallet would be unspendable. Ask for n
			// inputs' worth and ask again when the answer needs more; n only
			// grows, and MaxInputsPerTX bounds the loop.
			var selected []types.UTXO
			for n := 1; n <= utxo.MaxInputsPerTX; {
				vsize := int64(1100 + 976*(n-1))
				selected, _, err = selector.SelectUTXOs(elx.GetAllUTXOs(), amount+vsize*feeRate, 1, tip, []string{hotWallet})
				if err != nil {
					return nil, err
				}
				if len(selected) <= n {
					break
				}
				n = len(selected)
			}
			// The node must still have every input. Refuses to
			// evict on a syncing node; skips immature coinbase.
			return node.VerifyAndFilterUTXOs(ctx, selected, elx.EvictUTXO, elx.SetAssetType)
		},
		BuildSign: func(_ context.Context, inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
			recipientSPK, err := address.ScriptFor(to) // v1, 32-byte program only
			if err != nil {
				return "", "", err
			}
			return tx.BuildAndSign(inputs, recipientSPK, amount, changeSPK, feeRate, keystore)
		},
	}

	// After a restart: re-mark broadcast spends from the intent store and
	// re-send persisted transactions with the same bytes. Nothing is ever
	// rebuilt. Do not start paying out if this fails: the spent set may be
	// missing spends the store knows about. withdraw.ErrHeld here is an
	// intent an operator must resolve first.
	if err := engine.Recover(ctx); err != nil {
		log.Fatalf("recover: %v", err)
	}

	// The book against the chain, daily by default, started after Recover has
	// re-marked the set. A mismatch halts the breaker until an operator has
	// looked and called cb.Reset. This process's own reservations and
	// unconfirmed spends are read from the spent set and are not findings.
	recon := resilience.NewReconciler(elx, node, cb, resilience.DefaultReconciliationConfig(), logger)
	recon.Spent = spent
	recon.Start(ctx)
	defer recon.Stop()

	// A withdrawal request. The id is your idempotency key: the same id never
	// produces a second transaction, and a different amount under the same id
	// is refused. A destination that is not a mainnet address is refused by
	// Submit (withdraw.ErrInvalidIntent, per-request; the breaker ignores it)
	// and nothing is recorded for it.
	requestID, toAddress, amount := "wd-000123", "sq1p...", 25*types.ShorsPerSOQ
	if err := cb.Allow(); err != nil {
		log.Printf("withdrawals halted: %v", err)
		return
	}
	if _, _, err := engine.Submit(ctx, requestID, toAddress, amount, types.RecommendedFeeRate); err != nil {
		log.Printf("submit %s: %v", requestID, err)
		return
	}
	intent, err := engine.Process(ctx, requestID)
	cb.RecordResult(err) // nil = success; per-request errors are ignored; systemic ones count
	if err != nil {
		// rpc.ErrTransient from the selector: the intent stays Created and the
		// next Process builds it. rpc.ErrUnknownOutcome, rpc.ErrTransient or
		// rpc.ErrUnauthorized from the broadcast, or a context that ended
		// during it: the intent stays Built and the same bytes go out on the
		// next Process. Check intent.State to tell them apart.
		// rpc.ErrTxIDMismatch: also Built, inputs held, intent.NodeTxID set;
		// the breaker counts it, and every later Process of that intent
		// returns withdraw.ErrHeld, which also counts. Stop and investigate,
		// never rebuild. withdraw.ErrHeld on this call: the node rejected
		// bytes an earlier attempt may have relayed; the intent is Built with
		// intent.Hold set and its inputs marked spent. Abandon after the wait.
		// withdraw.ErrWrongState: another worker advanced this intent between
		// this call's read and Build's lock, so the withdrawal belongs to that
		// worker. Nothing is wrong, the breaker does not count it, and a later
		// Process of the same id sends it. The intent returned is this call's
		// own copy, which is out of date whenever this error is the reason, so
		// do not read its State here.
		// withdraw.ErrFailed: the intent was already Failed when this call
		// began. Terminal: it could not be built, the node refused the bytes
		// and no attempt's outcome was unknown, or an operator abandoned it.
		// The breaker does not count it. Resolve the payout; never reuse the id.
		// Two of these returns pair a nil intent with the error: an unknown
		// id, and a store read that failed before there was an intent to
		// return. The state is read only when there is something to read it
		// from.
		if intent == nil {
			log.Printf("process %s: %v", requestID, err)
			return
		}
		log.Printf("process %s: state %s: %v", requestID, intent.State, err)
		return
	}
	log.Printf("%s broadcast as %s", requestID, intent.TxID)

	// Later, from a ticker: confirmations complete the intent. A transaction
	// the node no longer knows is sent again with the same bytes; the record
	// does not change, and an operator calls engine.Abandon once the node has
	// not known it for engine.AbandonAfter.
	if err := engine.UpdateConfirmations(ctx, intent); err != nil {
		log.Printf("confirmations %s: %v", requestID, err)
		if errors.Is(err, rpc.ErrUnknownOutcome) {
			if err := engine.Rebroadcast(ctx, intent); err != nil {
				log.Printf("rebroadcast %s: %v", requestID, err)
			}
		}
	}
}
```

`tx.BuildAndSign` measures the transaction's weight on its real serialized form, refuses recipient
amounts below the node's relay floor (`tx.MinOutputValue`, 279,500 shors for a normal output),
refuses amounts outside the node's range, and refuses fees above `tx.MaxFeeShors` or rates above
`tx.MaxFeeRateShorsPerVB`. Change below the floor is left to the miner rather than emitted as an
output the node would reject. Before returning, it verifies every input as the node will
(`tx.Transaction.VerifyAll`: the hashtype read from the witness, the key hashed against the
output's program, the signature checked over the recomputed sighash), so a signer fault surfaces
as a build error rather than as a rejected broadcast. A payout is pre-flighted with `VerifyAll`,
never with `keys.Verify` alone ([Security Guide](SECURITY.md#a-signature-over-your-own-digest)).

### Running the withdrawal path as more than one process

Everything above is one process holding the node's RPC credential, the indexer's address and the
key. The interfaces do not require that: `Selector`, `BuildSigner`, `Broadcaster` and `Store` are
injected, so the work splits across hosts.
[`examples/exchange_split`](../examples/exchange_split) is that split as three binaries sharing one
directory, and its README is the longer form of what follows.

| Process | Holds | Transitions it owns |
|---|---|---|
| watcher | node RPC credential, indexer address, no key | → `Created`, and it publishes what may be spent |
| signer | the keystore; it opens no socket and holds no credential | `Created` → `Built` |
| broadcaster | node RPC credential, no key | `Built` → `Broadcast` → `Confirmed`, `Built` → `Failed`, `Built` held |

Three things decide whether a split is safe, and they are worth stating before you build your own.

**The node cannot give the signer a safe credential.** It has no per-method access control on its
RPC interface, so an account that may call `gettxout` may also call `sendrawtransaction`. A
"read-only signer account" is not something the node enforces. Either the signer gets a credential
as powerful as the broadcaster's, or it gets none and selects from what another process has already
checked against the node. The example takes the second road: the watcher writes a snapshot of
outputs it read back from the node one at a time, stamped with the node's tip height and the time
it answered, and the signer refuses a snapshot older than a bound it is given. A withheld or stale
snapshot means no withdrawal is built until the watcher publishes a current one.

**`withdraw.FileStore` is for one process.** Every write puts the whole file from that process's
own map, so a second process does not merge with the first, it overwrites it: an intent saved as
`Built` comes back as `Created` while its signed transaction is already in a mempool, and its
inputs are free for the next withdrawal to select. Share a store only through something that
writes one record at a time: a row in your database behind `withdraw.Store`, or the
one-file-per-intent store the example carries.

**One state, one writer, and the store is the truth about spends.** Give each transition to exactly
one process and have each process list only the states it owns. This is also what keeps one intent
to one transaction: `Build`, `Broadcast` and `UpdateConfirmations` each re-read the stored intent
under a lock that is held inside one process, so two goroutines of a worker pool cannot both build
or both send one id, but two processes sharing a store do not share that lock. Each would re-read,
each would find `Created`, and each would build and broadcast from different inputs, which pays the
recipient twice. What a shared store does refuse is a write over a record another process has
moved: `Update` names the state the writer read, and a database store makes it a conditional update
on the state column, so the loser of that race is refused (`withdraw.ErrStale`) rather than putting
the record back. It does not stop the two builds; one writer of `Created` → `Built` does. The spent
set is then per process: the signer holds the reservations because it is the only one
that selects, and it reconciles them against the store before every selection. Every `Built` intent
in the store is re-reserved, inputs of `Broadcast` intents are marked spent, entries the set already
holds for a `Confirmed` intent are marked confirmed so they age out, and a reservation held for
something `Created` or `Failed` is released. In one process `withdraw.Engine.Recover` does this once
at startup, and a `Built` intent whose inputs it cannot re-reserve is reported as
`withdraw.ErrReservationLost` and not sent; `Broadcast` re-reserves before every send and refuses
for the same reason, so the rule holds at every send and not only at startup. In a split the facts change while the signer runs, so
the repair runs every pass; the signer cannot call `Recover` itself, because `Recover` also
re-broadcasts, and re-broadcasting is the broadcaster's job.

```bash
# Three terminals, one directory. Only -dir is shared between hosts.
go run ./examples/exchange_split/watcher      -dir state -network stagenet -hot ssq1p... -electrumx 127.0.0.1:50001
go run ./examples/exchange_split/signer       -dir state -network stagenet -state signer-state
go run ./examples/exchange_split/broadcaster  -dir state -network stagenet -state broadcaster-state -confirmations 1
# -confirmations 1 so the walk-through finishes in a minute; the default is types.MaxReorgDepth (288), Step 4's figure for a withdrawal you have sent
```

---

## Step 4: Confirm Transactions

SOQ uses a **1-minute block target**. Anchor your thresholds to two consensus parameters rather
than to a rule of thumb carried over from another chain:

| Parameter | Value | Meaning |
|-----------|:-----:|---------|
| `nMaxReorgDepth` | **288 blocks** (~4.8 h) | The chain's own finality horizon, exposed as `types.MaxReorgDepth`. Once it has finished initial download, a node refuses a new header on a branch that forks from its chain 288 or more blocks below the tip |
| `nCoinbaseMaturity` | **288 blocks** (~4.8 h) | Newly mined coins are unspendable until this depth, enforced by consensus. Exposed per network as `types.Network.CoinbaseMaturity` (mainnet 288, stagenet 288, regtest 60); `rpc.Client` and `deposit.Monitor` apply it from their `Network` field, mainnet when unset. `Monitor.Scan` asks the node for that chain on every pass and refuses one that reports another, mainnet included when `Network` is unset; `RequireChain` is part of `deposit.Node`, so a wrapper of your own around `*rpc.Client` forwards it or does not compile |

Recommended thresholds:

| Use Case | Confirmations | Approximate Time | Rationale |
|----------|:-------------:|:----------------:|-----------|
| Zero-confirmation display | 0 | instant | Show as *pending only*. Never credit |
| Small deposits, below your first value line | 10 | ~10 min | The loss if one is reorganised is bounded by the line |
| Standard deposits, below your second value line | 30 | ~30 min | The same bound at a higher line and a deeper count |
| Large deposits, at or above your second value line | **288** | ~4.8 h | The chain's finality horizon: a synced node refuses a new header on a branch that would remove the deposit |
| Mining / coinbase payouts | **288 minimum** | ~4.8 h | Consensus-enforced; the output cannot be spent earlier regardless of policy |
| A withdrawal against a deposit | that deposit's tier | | Outbound value is released once the deposit behind it has reached its own tier |
| A withdrawal you have sent | 288 | ~4.8 h | `withdraw.Engine` marks it Confirmed at `RequiredConfirmations`. The split example sets that to `types.MaxReorgDepth` |

The two value lines are yours to set. We suggest the amounts of SOQ worth about 100 USD and about
1,000 USD at your own price, moved when the price moves. State each line by value: a fixed number of
SOQ drifts with the price until every deposit falls in one tier. A system that takes one figure can
use 30 and hold deposits above the second line to 288.

Below 288 confirmations a reorganisation remains possible, and the value lines bound what one could
cost. We suggest three safeguards with the two lower tiers:

1. **A ceiling on the value credited below 288 at one time.** About 10,000 USD across every deposit
   that is credited and not yet 288 deep. A deposit that would pass the ceiling waits for 288.
2. **288 for every deposit while the chain is unsettled.** That is the 288 blocks after a
   reorganisation of three or more blocks. It is also any time the hashrate of the last 120 blocks is
   below half that of the last 1,440 (`getnetworkhashps 120` against `getnetworkhashps 1440`).
3. **A withdrawal waits for the tier of the deposit behind it.**

`deposit.Tiers` applies the three tiers and the first two safeguards (Step 2), and
`examples/exchange_deposit` runs it. It reads a branch of three or more blocks beside the active
chain as a reorganisation of that size, whether or not your node followed the branch. One header
added on top of a branch the node left changes how the node lists it, so the two cannot be told
apart. The third safeguard is in your own books.

These figures are our recommendation for today. They follow how mining is distributed across pools
now. We plan to lower the 288 tier as more pools mine SOQ, and this guide will carry the new figures
when they change.

288 is the credit count for a large deposit. It is not the wait for every deposit. It is the depth
of the consensus rule. A synced node refuses a new header on a branch that forks from its chain 288
or more blocks below the tip (node `src/validation.cpp`, `ContextualCheckBlockHeader`:
`nReorgDepth >= nMaxReorgDepth`). A branch that would remove a deposit with 288 confirmations forks
that far down. The node applies the rule once, when a header is first accepted, and not during
initial block download. A header it accepted while its branch was shallower is not judged again
when its block arrives. 288 confirmations are therefore the depth of this rule. They are not
unconditional finality. `deposit.Tiers` covers that case: it credits nothing while your node holds
the headers of a branch with more work than the chain it follows. It takes that reading at the tip
your node counted the deposit's depth from. `deposit.Monitor` marks a
credited deposit final one block later, at 289 confirmations (`types.MaxReorgDepth + 1`). With
`Tiers` as its safeguard it gives that mark only on such a reading with no branch ahead.

**Set your own thresholds against value at risk. The counts in the table are a floor.**

### Why the tiers are by value

1. **The chain's own reorganisation rule is at 288 blocks.** Below 288 you are taking a position the
   chain has not taken. Crediting a large deposit earlier means accepting reorganisation risk the
   protocol itself does not consider settled.
2. **Below 288, what a count is worth depends on the share of hashrate against it.** The catch-up
   probability of Nakamoto 2008, section 11, gives the figures. With a fifth of the hashrate, the
   chance of overtaking 10 blocks is about 0.1 percent, and for 30 blocks it is about 3 in a billion.
   With two fifths, it is about 36 percent at 10 blocks, 7.6 percent at 30 and 0.04 percent at 100.
   Against a party with most of the hashrate, no count below 288 protects.
3. **So value is what bounds a loss in the two lower tiers.** A count of 10 or 30 does not make a
   deposit safe against a party with a large share. Hashrate is still concentrated among a small
   number of mining participants, and hashrate of the same algorithm exists outside this chain. The
   ladder therefore puts small amounts at small counts, holds large amounts to the chain's own rule,
   and caps the total exposed at one time. An exchange that prefers depth to speed can raise the two
   lower counts. Each added block lowers the odds against any minority share.

---

## Verification: a real confirmed transaction

Rather than asking you to trust that the signing path works, there is a
[verification record](VERIFICATION.md) for four stagenet transactions **built, signed,
serialized, broadcast and confirmed entirely by this SDK**:

| | Single input, `tx.BuildAndSign` | Three inputs, `withdraw.Engine` | v0.4, `withdraw.Engine` | v0.5, `withdraw.Engine` |
|---|---|---|---|---|
| Transaction id | `99fd147aaa4d575ee8f6266acfda4b09a5b0dc730d964294efded2cf3cd2eae7` | `13568c1a34416618fc5d160385643304230062bb6c53023522ec9e7f08fb67be` | `b0c659f7d63fd1346c6d4d95e15cbe465e7464213391654f694a08fa62f8422c` | `ebe41fd8ac7feaafbf2e99bbc9522fdecddb527227fa6a9e7db99c4a7377c9ef` |
| Block | `ad12368c1e083a6f0efe8da7cc65b52613b05d3301f0e609ba4660fdcffcf380` | `823edee8e491e706b16f38923cfc30767516fadca9d3247d7dd72a1ed13d5e42` | `c1c1c95fae85e1fbcc1cf2c7e586f61a3fbc558e083a370583f22e56b74c0a44` | `2541c140fc0b43b44b21a57c402d34ccaa6174193136467ba0a02320dd322d7d` |
| Witness stack per input | `[2421, 1313]` bytes | `[2421, 1313]` bytes | `[2421, 1313]` bytes | `[2421, 1313]` bytes |

The transaction id the SDK computed matches the one the node assigned in all four
cases, which independently confirms that serialization agrees with consensus byte
for byte. The second, third and fourth were produced through the engine: intent persisted,
inputs reserved, broadcast and confirmed. The second is
[`examples/stagenet_withdrawal`](../examples/stagenet_withdrawal); the third is the same path on
the v0.4 tree, where every call takes a context and the amounts are `int64` shors throughout;
the fourth is the committed example on the v0.5 tree against a stagenet node over RPC.

That document also gives the exact witness format consensus requires, a table
mapping `testmempoolaccept` rejections to their causes, and the steps to reproduce
the transaction against your own node. Worth reading before you scope the
integration.

---

## Test Coverage, current status

Every package carries unit tests. The figures below are one run of `go test -cover ./...` on the
commit this document ships with, library packages only: the `examples/` programs, `cmd/soq-htlc` and
`internal/atomicfile` are in that run too and are not part of the API you integrate against.
Re-run the command to check any row. Ten of the eleven reproduce exactly. The `electrumx`
figure moved between 91.9 and 92.4 across fourteen runs on this commit, because several of its tests drive
the reader goroutine, the ping loop and the refresher at once, and which branches run
depends on how those are scheduled.

| Package | Coverage | What is covered |
|---------|:--------:|-----------------|
| `address` | **98.5%** | Bech32m encoding, checksum, v1/32-byte destination rule, network detection, node-derived vectors |
| `utxo` | **91.1%** | Coin selection, smallest-first selection and its named empty result, persistent spent set, reservations and who holds them, restart survival of unconfirmed spends |
| `rpc` | **88.1%** | Error kinds, outcome-resolving broadcast, synced-node gate, stale-UTXO filtering, loopback guard, fee estimate conversion and clamp, exact output values, the chain tips, their work and the hashrate |
| `deposit` | **97.2%** | Node cross-check before credit, pause conditions, per-address staleness, vanished-credit alarm, the three tiers with their ceiling, the unsettled chain, a branch with more work, the tip of the reading and the hold on the final mark |
| `electrumx` | **92.2%** | Id-matched replies, notification routing, merge, refresh failures, a pass ending on a reply timeout, reply validation, per-address freshness, network inference, genesis check, TLS and the plaintext refusal |
| `tx` | **92.0%** | Serialized weight, output floor, amount checks, fee caps, one-output sweep, txid byte order, BIP143 sighash, witness format, consensus format vectors |
| `htlc` | **99.1%** | Swap contract template against the node's vector, lock height encoding, parser refusals, claim and refund, spend verification, two spends a node mined, the funding builder, the activation rule and each refusal of the functions that ask a node |
| `keys` | **89.3%** | Keypair generation with the 0xFF guard, record consistency, keystore encryption under a passphrase and under an external key, the version 2 header's floor and tamper-evidence, a version 1 file read and rewritten, network-bound derivation, fail-closed load, node-derived vectors |
| `withdraw` | **88.6%** | Idempotency, reservation, same-bytes retry, recovery, persist-before-broadcast, transient selector deferral, orphan-reservation release, store state after a failed write |
| `types` | **85.7%** | Amount parsing and formatting exact at every value the node prints, network records, asset constants |
| `resilience` | **81.6%** | Circuit breaker transitions, classification and the halt, reconciler against the node with the spent set, the webhook body |

Also passes under the race detector (`go test -race`), which matters for `electrumx` because its
UTXO cache is shared between the reader goroutine, the refresher and caller threads.

**Where the coverage is thin, and why.** These numbers are reported rather than rounded up:

- **`resilience` (81.6%)**: the breaker and the reconciler are covered against fakes and the
  alerter's post against a local server; the reconciler's timers are exercised at short intervals.
- **`electrumx`**: the protocol path is driven by a scripted fake server, including the
  notification-in-front-of-reply case, pushed changes, a reconnect and a dropped notification
  covered by the reconcile. The refresher's timers are exercised at short intervals, not at the
  production defaults.

**What the tests deliberately target.** Rather than chasing a percentage, they pin the invariants
whose failure is silent: transaction ID byte-order reversal, per-input BIP143 sighash separation,
exclusion of witness data from the txid, USDSOQ never counted as native SOQ, an input in the spent
set never selected again, a stale UTXO both dropped *and* evicted from cache, and RPC errors never
surfacing as usable zero values.

Serialization is additionally pinned to the node's own format vectors, address derivation to
addresses produced by the node's own encoder, and the documentation itself is checked in CI: every
self-contained (`package main`) Go program in these docs is compiled against the checkout, and every
API reference in the fragments is resolved against the real package.

If coverage on a specific path is a gating requirement for your review, tell us which and we will
prioritise it.

---

## Important Notes

### Transaction Size

SOQ transactions are larger than Bitcoin transactions due to Dilithium signatures:

Sizes below are measured by building and signing with this SDK, not estimated; the
vsize is what the node computes, and `tx.Transaction.VSize` returns the same number
before signing because every witness item has a fixed size. Each figure is for a
payment plus a change output, which is what the builders produce whenever a
remainder is left over.

| Component | Size |
|-----------|------|
| ML-DSA-44 public key | 1,312 bytes |
| ML-DSA-44 signature | 2,420 bytes |
| Witness stack per input | 3,734 bytes of items (2,421 + 1,313, including the sighash byte and the `0x00` key prefix); 3,741 bytes serialized with the item count and the two length prefixes |
| 1-in, 2-out | 3,880 bytes, 4,291 WU, 1,073 vB |
| 2-in, 2-out | 7,662 bytes, 8,196 WU, 2,049 vB |
| 10-in, 2-out | 37,918 bytes, 39,436 WU, 9,859 vB |
| 80-in, 2-out | 302,658 bytes, 312,786 WU, 78,197 vB |
| Max inputs per TX | 80, enforced by `utxo.MaxInputsPerTX` |

Roughly 3,782 bytes per additional input, so estimate
`3,880 + 3,782 x (inputs - 1)` bytes.

**The 80-input cap is not the node's weight limit.** `MAX_STANDARD_TX_WEIGHT` is
800,000 WU in every node release from v1.1.0 on (`src/policy/policy.h`), and 80 inputs use
312,786 of it, about 39%. The cap dates
from the period when the limit was 400,000 WU and 200-input transactions were
rejected for size. It is an operational limit in this SDK, not a protocol
constant.

`SelectUTXOs` returns `ErrInputLimitReached` with a partial selection when it hits
the cap before reaching the target. Handle that case: it means the payment needs
consolidation first, and ignoring the error sends less than intended.

### Fee estimation

**`feeRate` is shors per vByte, not a flat fee.** `types.RecommendedFeeRate` (1,000) is the
miner's default inclusion floor; below it a transaction is relayed but a default miner does not
include it, and below `types.MinRelayFeeRate` (100) it is not even relayed. The builders add a
one-vbyte margin so a rounding difference against the node's own vsize can never leave a
transaction one shor short. They also refuse fees above `tx.MaxFeeShors` (2 SOQ) and rates above
`tx.MaxFeeRateShorsPerVB` (100,000), so a fee-rate typo cannot burn the hot wallet up to the
node's own 100 SOQ limit. The [verification record](VERIFICATION.md) maps the rejection you get
if you go lower than the relay floor.

```go
// Ask the node for an estimate at a 6-block target. The result is already in
// shors per vByte, rounded up, and clamped to [types.RecommendedFeeRate,
// tx.MaxFeeRateShorsPerVB], so it is a rate the builders accept.
est, err := rpcClient.FeeRateShorsPerVB(ctx, 6)
if err != nil {
    return err // rpc.ErrTransient, rpc.ErrPermanent, or types.ErrAmountFormat for a reply without a numeric fee rate
}
if est.Fallback {
    log.Printf("node has no fee estimate; using the floor, %d shors/vB", est.Rate)
}
feeRate := est.Rate
```

A node that has not observed enough transactions reports no estimate; `FeeRateShorsPerVB`
then returns the floor with `Fallback` set. Log it: a node that never has an estimate is not
seeing the mempool. `Clamped` is set when the node's rate lay outside the range, and
`NodeRate` carries that rate. The floor is the miner's default inclusion rate, so nothing the
node reports lowers a withdrawal below what a default miner mines.

Always confirm before you rely on a broadcast:

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
```

### UTXO Consolidation

Deposits leave many small outputs, and a withdrawal carries at most 80 inputs
(`utxo.MaxInputsPerTX`), so consolidate on a schedule: the smallest final outputs into one at the
hot wallet. `SelectSmallestUTXOs` picks them, and `tx.BuildSignedSweep` builds the one-output
transaction with the fee measured on that form, signs it and verifies every input. Consolidate only
outputs past the reorganisation horizon (`types.MaxReorgDepth` + 1 confirmations): a consolidation
that spends a shallow output is undone with it.

```go
small, _, err := selector.SelectSmallestUTXOs(elx.GetAllUTXOs(), utxo.MaxInputsPerTX, int(types.MaxReorgDepth)+1, tipHeight, nil)
if errors.Is(err, utxo.ErrNoCandidates) {
    return nil // nothing final to consolidate
}
verified, err := node.VerifyAndFilterUTXOs(ctx, small, elx.EvictUTXO, elx.SetAssetType) // the node must still have each
// ...
sweep, err := tx.BuildSignedSweep(verified, hotWalletSPK, feeRate, keystore)
```

[`examples/consolidate`](../examples/consolidate) is the whole flow against your own node: the fee
from `FeeRateShorsPerVB`, outputs worth less than the fee they add left alone, the input count
capped so the fee stays under `tx.MaxFeeShors` (an 80-input sweep reaches it at about 2,550
shors/vB), the inputs recorded in the spent set after the broadcast, and `-dry-run`.

---

## Security Recommendations

| Concern | Recommendation |
|---------|---------------|
| **Key storage** | Deposit keys: derive them with `keys.DeriveSeed` and `keys.FromSeed` from a master secret in your key-management system, and back up the master and each user's index. Hot wallet: `keys.Manager` (AES-256-GCM, Argon2id) or an HSM; back up the key file, a generated key has no seed. |
| **Key rotation** | Generate fresh deposit addresses periodically. Sweep old addresses to cold storage. |
| **Cold storage** | Keep >95% of funds in air-gapped cold wallets. |
| **Monitoring** | Use the `resilience.Alerter` for Slack notifications on circuit breaker state changes. |
| **Rate limiting** | Enforce withdrawal rate limits and require manual approval above thresholds. |
| **Spent tracking** | Always use `utxo.SpentSet` opened with `utxo.OpenSpentSet` and driven through `withdraw.Engine`, which reserves inputs at build time and re-marks broadcast spends on `Recover`. Never re-select a broadcast UTXO. |
| **Deposit crediting** | Credit only what your own node confirms, through `deposit.Monitor`. Never on the indexer's word alone. |
| **Withdrawal outcome** | Treat `rpc.ErrUnknownOutcome` as "maybe sent": retry the same bytes, never rebuild. `withdraw.Engine` does this for you. |

See the full [Security Guide](SECURITY.md) for detailed recommendations.

---

## API Reference

Full API documentation is available at:

**[pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk](https://pkg.go.dev/github.com/soqucoin-labs/soqucoin-sdk)**

---

## Support

- **Technical questions:** Open an issue on [GitHub](https://github.com/soqucoin-labs/soqucoin-sdk/issues)
- **Security issues:** [security@soqu.org](mailto:security@soqu.org), or a private
  vulnerability report on the GitHub repository if you do not receive an acknowledgement within two
  business days
- **Exchange listing inquiries:** [dev@soqu.org](mailto:dev@soqu.org)
