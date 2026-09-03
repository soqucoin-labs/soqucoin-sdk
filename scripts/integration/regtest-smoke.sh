#!/usr/bin/env bash
# regtest-smoke.sh — proves the local node can serve the integration harness:
# starts a throwaway regtest node with the wallet enabled (allowed on a
# developer machine; never on a fleet VPS), mines to an SDK-style v1 address,
# and prints the RPCs the harness relies on.
#
#   SOQ_SRC=~/soqucoin-build/src scripts/integration/regtest-smoke.sh <v1 address>
set -u
SOQ_SRC="${SOQ_SRC:-$HOME/soqucoin-build/src}"
DAEMON="${DAEMON:-$SOQ_SRC/soqucoind}"
CLI="${CLI:-$SOQ_SRC/soqucoin-cli}"
ADDR="${1:?v1 regtest address (sq1p...) required}"
PORT=${PORT:-44444}; RPC=${RPC:-44445}
WORK=$(mktemp -d /tmp/soq-regtest-smoke.XXXXXX)

cleanup() {
    "$CLI" -datadir="$WORK" -rpcuser=it -rpcpassword=it -rpcport=$RPC stop >/dev/null 2>&1 || true
    pid=$(cat "$WORK/pid" 2>/dev/null)
    for i in $(seq 1 60); do { [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; } || break; sleep 1; done
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

args=( -regtest -server -listen=0 -dnsseed=0 -upnp=0 -discover=0 -disablewallet=0 -enablemining=1
       -printtoconsole=0 -rpcuser=it -rpcpassword=it -rpcbind=127.0.0.1 -rpcallowip=127.0.0.1
       -port=$PORT -rpcport=$RPC -txindex=1 -daemon=0 )
"$DAEMON" "${args[@]}" -datadir="$WORK" >"$WORK/stdout.log" 2>&1 &
echo $! >"$WORK/pid"
rpc() { "$CLI" -datadir="$WORK" -rpcuser=it -rpcpassword=it -rpcport=$RPC "$@" 2>&1; }
for i in $(seq 1 60); do rpc getblockcount >/dev/null 2>&1 && break; sleep 1; done
echo "genesis: $(rpc getblockhash 0)"
echo "chain:   $(rpc getblockchaininfo | grep -o '"chain": *"[a-z]*"')"
echo "generatetoaddress 5 -> $(rpc generatetoaddress 5 "$ADDR" | tr -d '\n ' | head -c 80)..."
echo "height:  $(rpc getblockcount)"
bh=$(rpc getblockhash 1)
cb=$(rpc getblock "$bh" 2 | python3 -c "import json,sys;b=json.load(sys.stdin);t=b['tx'][0];print(t['txid'], t['vout'][0]['value'], t['vout'][0]['scriptPubKey'].get('hex'))")
echo "block1 coinbase: $cb"
txid=${cb%% *}
echo "gettxout coinbase: $(rpc gettxout "$txid" 0 true | tr -d '\n ' | head -c 160)"
echo "getrawtransaction verbose confs: $(rpc getrawtransaction "$txid" true | grep -o '"confirmations": *[0-9]*')"
echo "importaddress available: $(rpc help importaddress 2>&1 | head -1 | cut -c1-60)"
echo "listunspent available:   $(rpc help listunspent 2>&1 | head -1 | cut -c1-60)"
echo "maxreorgdepth flag:      $("$DAEMON" -help 2>&1 | grep -o -- '-maxreorgdepth[^ ]*' | head -1)"
