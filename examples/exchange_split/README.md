# exchange_split — the withdrawal path as three processes

Every other example in this repository is one binary that holds everything: the
node's RPC credential, the indexer's address and the key. This one is the same
withdrawal path split across three processes that share one directory and
nothing else.

| Process | Holds | Writes | Transitions it owns |
|---|---|---|---|
| `watcher` | node RPC credential, indexer address. No key | accepted requests, `snapshot.json` | → `Created` |
| `signer` | the keystore. **No network access of any kind** | intent files | `Created` → `Built` |
| `broadcaster` | node RPC credential. No key | intent files | `Built` → `Broadcast` → `Confirmed`, `Built` → `Failed` |

The processes never speak to each other. Each reads the files the others left
and writes only the files it owns, so there is no protocol to get wrong and
nothing listening on the signer's host.

## What the split buys, and what it does not

The key is on a host with no path to the network, and the host that can send
has no key. Compromising the broadcaster spends nothing that is not already
signed and waiting; compromising the watcher offers the signer inputs that do
not exist, which the node then refuses.

It does not separate "may read the node" from "may send to the node". The node
has no per-method access control on its RPC interface, so a credential that can
read can also `sendrawtransaction`: the watcher's credential is as powerful as
the broadcaster's. Splitting that needs a proxy or a second node, which this
example does not build. The signer is given no credential at all instead, and
selects from the snapshot the watcher publishes.

## The directory

```
<dir>/intents/<id>.json     one withdrawal per file: the shared store
<dir>/requests/<id>.json    what the exchange's own system asks for
<dir>/requests/accepted/    requests that became intents
<dir>/snapshot.json         what may be spent, according to the node
```

Mode 0700, one owner. Whoever can write it decides what the signer believes
exists. The processes refuse a directory another account can reach.

`withdraw.FileStore` is not used here and cannot be: it holds every intent in
memory and rewrites the whole file from that map on each `Put`, so a second
process overwrites the first rather than merging with it — an intent saved as
`Built` would come back as `Created` with its signed transaction already in a
mempool. `split.DirStore` keeps one record per file and reads from disk every
time. What keeps two processes off one record is that each state has exactly
one owner, as in the table above; an exchange that wants a lock rather than a
convention implements `withdraw.Store` over its database.

## Running it on stagenet

Three terminals, one directory. The signer's keystore is created by
`examples/stagenet_withdrawal -init`, or by any tool that writes a
`keys.Manager` file; fund the address it prints.

```bash
export SOQ_RPC_USER=... SOQ_RPC_PASSWORD=...
mkdir -p state/requests

go run ./examples/exchange_split/watcher -dir state -network stagenet \
    -hot ssq1p... -electrumx 127.0.0.1:50001

SOQ_KEYSTORE_PASSPHRASE=... go run ./examples/exchange_split/signer \
    -dir state -state signer-state -network stagenet

go run ./examples/exchange_split/broadcaster -dir state \
    -state broadcaster-state -network stagenet -confirmations 1
```

Ask for a withdrawal by writing one file. The name must be the id:

```bash
cat > state/requests/w-0001.json <<'JSON'
{"id": "w-0001", "address": "ssq1p...", "amount_shors": 120000000000, "fee_rate": 1000}
JSON
```

Amounts are `int64` shors, never SOQ and never a float.

`-state` is each process's own directory: its keystore, its spent set. On three
hosts only `-dir` is shared, over a filesystem the three of them can reach;
the two `-state` directories never leave their host.

## Reading it in order

1. `split/split.go` — the directory, the store, the snapshot, the request file.
   The reasoning about what may be trusted is in its comments.
2. `watcher/main.go` — when a snapshot is withheld, which is most of the safety.
3. `signer/main.go` — the reconcile pass, and why a signer cannot call
   `withdraw.Engine.Recover`.
4. `broadcaster/main.go` — `Recover` at startup, then send and confirm.
