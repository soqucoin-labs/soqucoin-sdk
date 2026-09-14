# Exchange Integration Guide

Step-by-step guide for listing Soqucoin (SOQ) on your exchange.

---

## Overview

To support SOQ deposits and withdrawals, your exchange needs to:

1. **Generate deposit addresses**: one unique address per user
2. **Monitor deposits**: track UTXOs via ElectrumX polling
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
| ❌ **You do not use** | the wallet subsystem *inside* that node. It ships disabled (`disablewallet=1`), so `listunspent`, `getbalance` and `sendtoaddress` are not part of the integration surface |
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

> **https://github.com/soqucoin-labs/electrumx**, branch `soqucoin`

Upstream ElectrumX ships no Soqucoin coin definition, so that fork is what you need. It is based on
a pinned upstream commit rather than tracking `master`, so the entire Soqucoin delta is reviewable
in one command:

```bash
git diff 24865dc..soqucoin -- src/
```

Roughly 242 lines across five files: the coin definitions (AuxPoW-aware headers), the AuxPoW
transaction deserializer, asset and visibility derivation from the witness version, UTXO records
carrying that metadata, and one protocol extension (`get_multi_balance`) for per-asset balances.

You can confirm a clean checkout works before committing any effort to it:

```bash
git clone -b soqucoin https://github.com/soqucoin-labs/electrumx && cd electrumx
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
| Software | `soqucoind` from [github.com/soqucoin/soqucoin](https://github.com/soqucoin/soqucoin) at tag **`v2.5.0`**, built per its `INSTALL.md` or `Dockerfile`. Every node we operate runs this tag. The golden transaction vector in the tests is the v2.3.0 node's decode; the integration harness ran against a v2.5.0 build before the v0.3.5 tag, and the release notes record that binary's digest. |
| `txindex=1` | **Required, and set before the first start**; adding it later means rebuilding the chainstate with `-reindex-chainstate`. `withdraw.RPCConfirmer` and the lost-reply check in `rpc.Client` ask the node for a transaction by id with `getrawtransaction`. Without the index the node finds a mined transaction only through its UTXO set (`src/validation.cpp`, `GetTransaction` with `fAllowSlow`), so once every output has been spent, the recipient's and then your change, the transaction reads as unknown and the withdrawal never settles in the engine's view. |
| `disablewallet=1` | The node's wallet is not part of this integration (see [Integration model](#integration-model-read-this-first)). Every node we operate runs with it. |
| `server=1` plus `rpcauth` (or `rpcuser` and `rpcpassword`) | The node's RPC has no TLS. `rpc.Client` takes a URL, so either bind RPC to localhost or your private network, or terminate TLS in front of it ([Security Guide](SECURITY.md#network-security)). |
| `stagenet=1` | For the staging network. Omit it for mainnet. |
| `dbcache` | Our nodes run 512 MB; the node's default is 450. |

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
branch `soqucoin`, tag `mainnet-genesis-2026-09-03`. Configuration is in its `SOQUCOIN.md`:
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

Stagenet on 2026-09-14 UTC at height 82,061, read from our nodes (`getblockchaininfo`,
`systemctl show -p MemoryCurrent`, `du`):

| | |
|---|---|
| Node data directory with `txindex=1` | 425 MB (`size_on_disk`), of which the chainstate is 532 KB |
| `soqucoind` resident memory | 110 MB on a node serving RPC only, 340 MB on one also feeding an indexer, both at `dbcache=512` |
| ElectrumX database (LevelDB) | 64 MB |
| ElectrumX resident memory | 110 MB |
| Hosts these were read from | 4 vCPU with 8 GB running node plus indexer; 8 vCPU with 16 GB |

Blocks arrive one a minute (`nPowTargetSpacing = 60`). Mainnet starts from its genesis block at
launch, so it stays smaller than stagenet for months; plan storage from the stagenet figures and
the growth you observe. We have not timed an initial sync on a reference host.

---

## Before you write code: run the harness

Fifteen minutes, one command, and you have seen every flow in this guide work against a real node:

```bash
git clone https://github.com/soqucoin-labs/soqucoin-sdk && cd soqucoin-sdk
SOQUCOIND=/path/to/soqucoind make integration   # a soqucoind build, v2.3.0 or later
```

The harness starts a throwaway regtest node, mines to SDK-generated addresses, and drives the same
`deposit` and `withdraw` packages this guide uses through six scenarios: a deposit credited only
after the node confirms it, a withdrawal built, signed, broadcast, mined and confirmed, a lost
broadcast reply survived with exactly one payment, two withdrawals that cannot share an input,
refused inputs (a USDSOQ-form destination, an amount below the relay floor, a fee-rate typo) that
never reach the node, and a reorganisation that removes a credited deposit and raises the alarm.
The indexer role is played by an in-test block scanner, so the harness needs no ElectrumX; the
ElectrumX client is exercised by its own protocol tests against a scripted server.

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
`Load` refuses a record whose address does not belong to its key. Key handling in detail:
[Security Guide](SECURITY.md).

---

## Step 2: Monitor Deposits

Two sources, two roles. The ElectrumX indexer **discovers** candidate deposits quickly across
thousands of addresses. Your own `soqucoind` gives the **verdict**: nothing is credited unless your
node confirms the exact output, its value, its destination script and its depth. That is what
`deposit.Monitor` does, and it also refuses to credit while your node is still syncing (the finality
horizon is not enforced during initial download) or while the indexer has stopped refreshing (an
outage looks exactly like "no deposits"), and it re-verifies every credited deposit until it passes
the horizon so a reorganisation that removes one is alarmed rather than missed.

```go
package main

import (
	"log"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/deposit"
	"github.com/soqucoin-labs/soqucoin-sdk/electrumx"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// requiredConfirmations is the policy from Step 4: deeper for larger values.
func requiredConfirmations(value int64) int64 {
	switch {
	case value <= 100*types.ShorsPerSOQ:
		return 30
	case value <= 10_000*types.ShorsPerSOQ:
		return 120
	default:
		return types.MaxReorgDepth // the chain's own finality horizon
	}
}

// memLedger stands in for your database. Credit must be idempotent on the
// outpoint, and in production the credit and the balance change belong in the
// same database transaction.
type memLedger struct {
	mu       sync.Mutex
	credited map[string]deposit.Deposit
	final    map[string]bool
}

func key(txid string, vout uint32) string { return txid + ":" + string(rune('0'+vout)) }

func (l *memLedger) Credit(d deposit.Deposit) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credited[key(d.TxID, d.Vout)] = d
	log.Printf("CREDIT %s:%d %d shors to %s at %d confirmations", d.TxID, d.Vout, d.Value, d.Address, d.Confirmations)
	return nil
}
func (l *memLedger) IsCredited(txid string, vout uint32) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.credited[key(txid, vout)]
	return ok, nil
}
func (l *memLedger) Pending() ([]deposit.Deposit, error) {
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
func (l *memLedger) MarkFinal(txid string, vout uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.final[key(txid, vout)] = true
	return nil
}

func main() {
	depositAddresses := []string{"sq1p...", "sq1p..."} // one per user, from your database

	// The indexer. Over anything but a private network, use TLS: the server
	// sees every address you track. The network is inferred from the
	// addresses; mixed or undecodable addresses are refused.
	elx := electrumx.NewClient("electrumx.example.com:50002", 15*time.Second)
	elx.UseTLS()
	if err := elx.TrackAddresses(depositAddresses); err != nil {
		log.Fatalf("track addresses: %v", err)
	}
	if err := elx.Connect(); err != nil { // also verifies the server's genesis hash
		log.Fatalf("connect: %v", err)
	}
	defer elx.Stop()
	elx.StartPolling()

	// Your own node. Nothing is credited on the indexer's word alone.
	node := rpc.NewClient("http://127.0.0.1:33389", "rpcuser", "rpcpass")
	node.Network = types.Mainnet // RequireSynced refuses a node on any other chain

	ledger := &memLedger{credited: map[string]deposit.Deposit{}, final: map[string]bool{}}
	monitor := &deposit.Monitor{
		Cache:     elx,
		Node:      node,
		Network:   types.Mainnet, // coinbase maturity 288; Scan refuses a node on any other chain
		Ledger:    ledger,
		Addresses: func() []string { return depositAddresses },
		Required:  requiredConfirmations,
		OnAlert: func(kind deposit.AlertKind, msg string) {
			// Route to your paging. Every alert here is a human's problem:
			// indexer and node disagreeing, a credited deposit that vanished,
			// crediting paused because the node or indexer is not current.
			log.Printf("ALERT %s: %s", kind, msg)
		},
	}

	for range time.Tick(30 * time.Second) {
		credited, err := monitor.Scan()
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

---

## Step 3: Process Withdrawals

A withdrawal is a state machine, not a call. `withdraw.Engine` makes the two failures an exchange
cannot afford impossible by construction:

- **No double payment.** Each withdrawal is an `Intent` under your idempotency key. The signed
  transaction is persisted **before** it is broadcast, and a broadcast whose reply was lost
  (`rpc.ErrUnknownOutcome`) is retried with the **same bytes**, never rebuilt. `rpc.Broadcast`
  resolves that case against your node and treats "already in chain" as success.
- **No own double-spend.** Inputs are reserved in the spent set when the transaction is built,
  all-or-nothing, and unconfirmed spends survive restarts however long confirmation takes. Every
  unsettled broadcast attempt renews the reservation, so retry Built intents at an interval
  shorter than `ReservationTTL` (default 15 minutes).
- **A node that accepts the bytes under a different txid** (`rpc.ErrTxIDMismatch`) is neither a
  rejection nor a retry: the payment is in the mempool. The inputs are marked spent under the
  node's txid, the intent stays Built with `NodeTxID` recorded, and `Broadcast` and `Recover`
  refuse to send it again (`withdraw.ErrHeld`); stop withdrawals and investigate before anything
  is rebuilt.

If `Process` returns an error that is `utxo.ErrPersist`, the node accepted the payment and the
spent set could not be written: the intent is saved as `Broadcast`, the process still refuses those
inputs, and `Recover` re-marks every Broadcast intent's inputs from the intent store at startup, so
a restart does not re-expose them. That guarantee holds only if you stop when `Recover` returns an
error: a failed `List` means nothing was re-marked. Open the spent set with `utxo.OpenSpentSet`,
which refuses a file that exists but cannot be read; an empty set in that case would forget every
unconfirmed spend.

`Recover` at startup re-sends anything persisted but not yet acknowledged. The circuit breaker is
fed through `RecordResult`, which never counts a per-request error (a bad address, an amount below
the floor, insufficient funds, a node rejection of one transaction), so a user cannot halt every
withdrawal with three malformed requests.

```go
package main

import (
	"log"
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

	// Keys. The manager refuses records the node could not spend from and
	// records whose address does not belong to their key.
	keystore := keys.NewManager("/var/lib/exchange/keys.enc", os.Getenv("SOQ_KEYSTORE_PASSPHRASE"))
	if err := keystore.Load(); err != nil {
		log.Fatalf("load keystore: %v", err)
	}

	// Indexer and node.
	elx := electrumx.NewClient("electrumx.example.com:50002", 15*time.Second)
	elx.UseTLS()
	if err := elx.TrackAddresses([]string{hotWallet}); err != nil {
		log.Fatalf("track: %v", err)
	}
	if err := elx.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer elx.Stop()
	elx.StartPolling()
	node := rpc.NewClient("http://127.0.0.1:33389", "rpcuser", "rpcpass")
	node.Network = types.Mainnet

	// Halts only for systemic failures (RecordResult never counts a bad
	// request), and the reconciler trips it when the book and the chain
	// disagree.
	cb := resilience.NewCircuitBreaker(3, 15*time.Minute)

	// Inputs are reserved here at build time, and unconfirmed spends survive
	// a restart however long confirmation takes.
	spent, err := utxo.OpenSpentSet("/var/lib/exchange/spent_set.json")
	if err != nil {
		log.Fatal(err) // a file that exists but cannot be read must not start empty
	}
	selector := utxo.NewCoinSelector(spent)

	// Durable intents: persisted before anything is broadcast. An exchange
	// with a database implements withdraw.Store over it and keeps the same
	// rule that Put is durable before it returns.
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
		Broadcaster:           node, // rpc.Broadcast resolves lost replies against the node
		Confirmer:             withdraw.RPCConfirmer{Client: node},
		RequiredConfirmations: types.MaxReorgDepth,
		ReservationTTL:        15 * time.Minute,
		Select: func(amount, feeRate int64) ([]types.UTXO, error) {
			if err := node.RequireSynced(); err != nil {
				return nil, err
			}
			tip, err := node.GetBlockCount()
			if err != nil {
				return nil, err
			}
			// Budget the fee against vsize: a one-input, two-output payment is about
			// 1,073 vB and each further ML-DSA-44 input adds about 976 vB.
			budget := amount + (1100+950*int64(utxo.MaxInputsPerTX))*feeRate
			selected, _, err := selector.SelectUTXOs(elx.GetAllUTXOs(), budget, 1, tip, []string{hotWallet})
			if err != nil {
				return nil, err
			}
			// Defense 11: the node must still have every input. Refuses to
			// evict on a syncing node; skips immature coinbase.
			return node.VerifyAndFilterUTXOs(selected, elx.EvictUTXO, elx.SetAssetType)
		},
		BuildSign: func(inputs []types.UTXO, to string, amount, feeRate int64) (string, string, error) {
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
	if err := engine.Recover(); err != nil {
		log.Fatalf("recover: %v", err)
	}

	// A withdrawal request. The id is your idempotency key: the same id never
	// produces a second transaction, and a different amount under the same id
	// is refused.
	requestID, toAddress, amount := "wd-000123", "sq1p...", 25*types.ShorsPerSOQ
	if err := address.Validate(types.Mainnet.HRP, toAddress); err != nil {
		log.Printf("refuse %s: %v", requestID, err) // per-request; do not feed the breaker
		return
	}
	if err := cb.Allow(); err != nil {
		log.Printf("withdrawals halted: %v", err)
		return
	}
	if _, _, err := engine.Submit(requestID, toAddress, amount, types.RecommendedFeeRate); err != nil {
		log.Printf("submit %s: %v", requestID, err)
		return
	}
	intent, err := engine.Process(requestID)
	cb.RecordResult(err) // nil = success; per-request errors are ignored; systemic ones count
	if err != nil {
		// rpc.ErrUnknownOutcome or rpc.ErrTransient: the intent stays Built.
		// Call Process again later; the same bytes go out.
		// rpc.ErrTxIDMismatch: also Built, inputs held, intent.NodeTxID set;
		// the breaker counts it, and every later Process of that intent
		// returns withdraw.ErrHeld, which also counts. Stop and investigate,
		// never rebuild.
		log.Printf("process %s: state %s: %v", requestID, intent.State, err)
		return
	}
	log.Printf("%s broadcast as %s", requestID, intent.TxID)

	// Later, from a ticker: confirmations complete the intent.
	if err := engine.UpdateConfirmations(intent); err != nil {
		log.Printf("confirmations %s: %v", requestID, err)
	}
}
```

`tx.BuildAndSign` measures the transaction's weight on its real serialized form, refuses recipient
amounts below the node's relay floor (`tx.MinOutputValue`, 279,500 shors for a normal output),
refuses amounts outside the node's range, and refuses fees above `tx.MaxFeeShors` or rates above
`tx.MaxFeeRateShorsPerVB`. Change below the floor is left to the miner rather than emitted as an
output the node would reject.

---

## Step 4: Confirm Transactions

SOQ uses a **1-minute block target**. Anchor your thresholds to two consensus parameters rather
than to a rule of thumb carried over from another chain:

| Parameter | Value | Meaning |
|-----------|:-----:|---------|
| `nMaxReorgDepth` | **288 blocks** (~4.8 h) | The chain's own finality horizon, exposed as `types.MaxReorgDepth`. Nodes reject headers building on a fork deeper than this, once they have finished initial download |
| `nCoinbaseMaturity` | **288 blocks** (~4.8 h) | Newly mined coins are unspendable until this depth, enforced by consensus. Exposed per network as `types.Network.CoinbaseMaturity` (mainnet 288, stagenet 288, regtest 60); `rpc.Client` and `deposit.Monitor` apply it from their `Network` field, mainnet when unset. With `Network` set, `Monitor.Scan` refuses a node that reports another chain; if you wrap `*rpc.Client` behind your own `deposit.Node`, forward `RequireChain` so that check still reaches the node |

Recommended thresholds:

| Use Case | Confirmations | Approximate Time | Rationale |
|----------|:-------------:|:----------------:|-----------|
| Zero-confirmation display | 0 | instant | Show as *pending only*. Never credit |
| Small deposits | 30 | ~30 min | Bounded, recoverable loss if reorganised |
| Medium deposits | 120 | ~2 h | Materially past any plausible reorg depth |
| Large deposits | **288** | ~4.8 h | Matches the chain's own finality horizon |
| Mining / coinbase payouts | **288 minimum** | ~4.8 h | Consensus-enforced; the output cannot be spent earlier regardless of policy |
| Withdrawal release | 288 | ~4.8 h | Do not release outbound value against inbound funds the chain does not yet treat as final |

**Set your own thresholds against value at risk. The table is a floor, not a ceiling.**

### Why these are higher than a typical Bitcoin-style table

Two reasons, both worth understanding before tuning them down:

1. **The chain declares its own finality at 288 blocks.** Crediting a large deposit at 6
   confirmations means accepting reorganisation risk the protocol itself does not consider settled.
   Below 288 you are taking a position the chain has not taken.
2. **Absolute hashrate matters more than block interval on a young chain.** Confirmation count is a
   proxy for accumulated work. Early in a chain's life, and while hashrate is concentrated among a
   small number of mining participants, producing a competing chain of N blocks costs far less than
   the same N blocks would cost on a mature network. Prefer depth over speed until sustained
   third-party hashrate exists.

If low-latency credit matters to your product, credit small amounts quickly against your own risk
budget and hold larger amounts to 288, rather than lowering the threshold uniformly.

---

## Verification: a real confirmed transaction

Rather than asking you to trust that the signing path works, there is a
[verification record](VERIFICATION.md) for two stagenet transactions **built, signed,
serialized, broadcast and confirmed entirely by this SDK**:

| | Single input, `tx.BuildAndSign` | Three inputs, `withdraw.Engine` |
|---|---|---|
| Transaction id | `99fd147aaa4d575ee8f6266acfda4b09a5b0dc730d964294efded2cf3cd2eae7` | `13568c1a34416618fc5d160385643304230062bb6c53023522ec9e7f08fb67be` |
| Block | `ad12368c1e083a6f0efe8da7cc65b52613b05d3301f0e609ba4660fdcffcf380` | `823edee8e491e706b16f38923cfc30767516fadca9d3247d7dd72a1ed13d5e42` |
| Witness stack per input | `[2421, 1313]` bytes | `[2421, 1313]` bytes |

The transaction id the SDK computed matches the one the node assigned in both
cases, which independently confirms that serialization agrees with consensus byte
for byte. The second was produced by [`examples/stagenet_withdrawal`](../examples/stagenet_withdrawal):
intent persisted, inputs reserved, broadcast and confirmed through the engine.

That document also gives the exact witness format consensus requires, a table
mapping `testmempoolaccept` rejections to their causes, and the steps to reproduce
the transaction against your own node. Worth reading before you scope the
integration.

---

## Test Coverage, current status

Every package now carries unit tests. Measured with `go test -cover ./...`:

| Package | Coverage | What is covered |
|---------|:--------:|-----------------|
| `address` | **92.4%** | Bech32m encoding, checksum, v1/32-byte destination rule, network detection, node-derived vectors |
| `utxo` | **88.1%** | Coin selection, persistent spent set, reservations, restart survival of unconfirmed spends |
| `client` | **86.8%** | soq-signer auth, error propagation, SOQ-to-shor conversion |
| `rpc` | **78.3%** | Error kinds, outcome-resolving broadcast, synced-node gate, stale-UTXO filtering |
| `deposit` | **77.3%** | Node cross-check before credit, pause conditions, vanished-credit alarm |
| `electrumx` | **76.2%** | Id-matched replies, notification routing, merge, refresh failures, network inference, genesis check, TLS |
| `tx` | **76.1%** | Serialized weight, output floor, amount checks, fee caps, txid byte order, BIP143 sighash, witness format, consensus format vectors |
| `keys` | **75.3%** | Keypair generation with the 0xFF guard, record consistency, keystore encryption, node-derived vectors |
| `withdraw` | **73.1%** | Idempotency, reservation, same-bytes retry, recovery, persist-before-broadcast |
| `resilience` | **62.1%** | Circuit breaker transitions and classification, reconciler against the node |

Also passes under the race detector (`go test -race`), which matters for `electrumx` because its
UTXO cache is shared between the polling goroutine and caller threads.

**Where the coverage is thin, and why.** These numbers are reported rather than rounded up:

- **`resilience` (62.1%)**: the breaker and the reconciler are covered against fakes; the Slack
  alerter's HTTP path is not, and it is an operational convenience rather than part of the money path.
- **`electrumx` (76.2%)**: the protocol path is driven by a scripted fake server, including the
  notification-in-front-of-reply case. The long-running polling loop's timing is not unit-tested.

**What the tests deliberately target.** Rather than chasing a percentage, they pin the invariants
whose failure is silent: transaction ID byte-order reversal, per-input BIP143 sighash separation,
exclusion of witness data from the txid, USDSOQ never counted as native SOQ, spent-pending UTXOs
never double-selected, a stale UTXO both dropped *and* evicted from cache, and RPC errors never
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
// Query the node for a dynamic estimate. It returns SOQ per kB.
soqPerKB, err := rpcClient.EstimateSmartFee(6) // target: 6 blocks
if err != nil {
    return err
}
feeRate := int64(soqPerKB * float64(types.ShorsPerSOQ) / 1000) // shors per vByte
if feeRate < 1000 {
    feeRate = 1000 // floor: below this the node rate-limits as free
}
```

`EstimateSmartFee` falls back to 0.01 SOQ/kB when the node has no estimate, which
is exactly 1,000 shors per vByte, so the floor above and the fallback agree.

Always confirm before you rely on a broadcast:

```bash
soqucoin-cli testmempoolaccept '["<rawHex>"]'
```

### UTXO Consolidation

Exchanges accumulate many small UTXOs from deposits. Periodically consolidate them to avoid hitting the 80-input limit during large withdrawals:

```go
// Select the smallest UTXOs for consolidation. Consolidate only outputs that
// are final (past the reorg horizon); a consolidation that spends a shallow
// output is undone with it.
smallUTXOs, total, err := selector.SelectSmallestUTXOs(allUTXOs, 50, int(types.MaxReorgDepth)+1, tipHeight, nil)
// Build a single TX that merges them into one output to your hot wallet
```

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
