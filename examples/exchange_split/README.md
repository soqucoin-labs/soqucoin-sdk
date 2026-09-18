# exchange_split: the withdrawal path as three processes

Every other example in this repository is one binary that holds everything: the
node's RPC credential, the indexer's address and the key. This one is the same
withdrawal path split across three processes that share one directory and
nothing else.

| Process | Holds | Writes | Transitions it owns |
|---|---|---|---|
| `watcher` | node RPC credential, indexer address. No key | accepted requests, `snapshot.json` | → `Created` |
| `signer` | the keystore. **No socket, no credential** | intent files | `Created` → `Built` |
| `broadcaster` | node RPC credential. No key | intent files | `Built` → `Broadcast` → `Confirmed`, `Built` → `Failed`, `Built` held |

The processes never speak to each other. Each reads the files the others left
and writes only the files it owns, so there is no protocol to get wrong and
nothing listening on the signer's host.

## What the split buys, and what it does not

The signer opens no socket and holds no credential, and the process that can
send holds no key. The signer's host still reaches the shared directory, over a
mount if the three run on three machines, and that mount is the whole of its
exposure. Compromising the broadcaster spends nothing that is not already
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

Mode 0700, one owner. Whoever can write it decides what the signer treats as spendable, so all
three processes open it through `split.Dir.Open`, which refuses a directory any other account can
reach before it touches the store inside. Checking the store and not its parent is no check at all:
an account that can write the shared directory can rename `intents/` aside and put its own in
place.

Exactly one process per role. Two signers on one directory would both pick up the same `Created`
intent, sign a different transaction over the same inputs and write one over the other: the lost
one's inputs are spent by whichever transaction is broadcast first, and the record of the other is
gone. Nothing here enforces that, because the convention is what the file layout can express; an
implementation of `withdraw.Store` over a database enforces it with a row lock.

`withdraw.FileStore` is not used here and cannot be: it holds every intent in
memory and rewrites the whole file from that map on each write, so a second
process overwrites the first rather than merging with it. An intent saved as
`Built` comes back as `Created` with its signed transaction already in a
mempool. `split.DirStore` keeps one record per file and reads from disk every
time. Its `Create` links the record into place under a name that must not
exist, so two processes cannot both register one id, which needs a filesystem
with hard links under the intents directory; its `Update` compares the stored
state under this process's lock only. What keeps two processes off
one record beyond that is that each state has exactly one owner, as in the
table above; an exchange that wants a lock rather than a convention implements
`withdraw.Store` over its database with a conditional update on the state.

## Running it on stagenet

Three terminals, one directory. The signer's keystore is created by
`examples/stagenet_withdrawal -init`, or by any tool that writes a
`keys.Manager` file; fund the address it prints.

```bash
export SOQ_RPC_USER=... SOQ_RPC_PASSWORD=...

# The processes refuse a shared directory any other account can reach, so
# create it 0700. Without the umask a normal one leaves it 0755 and the
# watcher exits on its first check.
umask 077 && mkdir -p state/requests

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

Amounts are `int64` shors. SOQ figures and floats do not appear at this boundary.

`-state` is each process's own directory: its keystore, its spent set. On three
hosts only `-dir` is shared, over a filesystem the three of them can reach;
the two `-state` directories never leave their host.

Two settings have to agree with each other, or the signer refuses every snapshot and no withdrawal
is built at all:

- the signer's `-max-snapshot-age` (2 minutes) must be comfortably longer than the watcher's
  `-interval` (15 seconds), which is how often the stamp is refreshed;
- the two clocks must agree within the signer's `-clock-skew` (5 seconds), because the stamp is
  written by the watcher's clock and read against the signer's. Run a time daemon on both hosts. A
  snapshot stamped in the future is refused rather than trusted, so a badly set clock stops
  withdrawals rather than ageing out of the check.

## Reading it in order

1. `split/split.go`: the directory, the store, the snapshot, the request file.
   The reasoning about what may be trusted is in its comments.
2. `watcher/main.go`: the conditions under which a snapshot is withheld.
3. `signer/main.go`: the reconcile pass, and why a signer cannot call
   `withdraw.Engine.Recover`.
4. `broadcaster/main.go`: `Recover` at startup, then send and confirm.
