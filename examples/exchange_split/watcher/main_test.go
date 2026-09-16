package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const (
	txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hot = "ssq1pe3r0uarwft8pruag9vdvd6e5aa08t94wz86mfsuamh8037cs5wfsyet3p6"
)

// fakeIndexer answers the four calls publish makes and records the evictions,
// which is how a snapshot's absence of an output is told apart from the node
// never having been asked.
type fakeIndexer struct {
	at      time.Time
	err     error
	utxos   []types.UTXO
	evicted []string
	stamped map[string]uint8
}

func (f *fakeIndexer) LastRefreshOf(string) (time.Time, error) { return f.at, f.err }
func (f *fakeIndexer) GetUTXOs(string) []types.UTXO            { return f.utxos }
func (f *fakeIndexer) EvictUTXO(txid string, vout uint32) {
	f.evicted = append(f.evicted, txid)
}
func (f *fakeIndexer) SetAssetType(txid string, vout uint32, assetType uint8) {
	if f.stamped == nil {
		f.stamped = map[string]uint8{}
	}
	f.stamped[txid] = assetType
}

// fakeNode answers the three node calls publish reaches: getblockchaininfo
// through RequireSynced, getblockcount, and gettxout per candidate output.
type fakeNode struct {
	blocks  int64
	headers int64
	syncing bool
	// txout returns the reply for one outpoint: nil means the node does not
	// have it, which is a spent or unknown output.
	txout func(txid string, vout uint32) map[string]any
	// fail names a method the node answers with an error, which is how a
	// call that cannot be made is told apart from one that answers zero.
	fail  string
	calls int
}

func (f *fakeNode) server(t *testing.T) *rpc.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("node request: %v", err)
			return
		}
		f.calls++
		var result any
		if req.Method == f.fail {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": req.ID, "result": nil,
				"error": map[string]any{"code": -1, "message": "injected node failure"},
			})
			return
		}
		switch req.Method {
		case "getblockchaininfo":
			headers := f.headers
			if headers == 0 {
				headers = f.blocks
			}
			result = map[string]any{
				"chain": types.Stagenet.ChainID, "blocks": f.blocks, "headers": headers,
				"initialblockdownload": f.syncing,
			}
		case "getblockcount":
			result = f.blocks
		case "gettxout":
			txid, _ := req.Params[0].(string)
			vout, _ := req.Params[1].(float64)
			if out := f.txout(txid, uint32(vout)); out != nil {
				result = out
			}
		default:
			t.Errorf("unexpected node call %q", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": req.ID, "result": result, "error": nil})
	}))
	t.Cleanup(srv.Close)
	client := rpc.NewClient(srv.URL, "u", "p", nil)
	client.Network = types.Stagenet
	return client
}

func txoutOf(value string, confirmations int64, assetType uint8) map[string]any {
	return map[string]any{
		"bestblock": "00", "confirmations": confirmations, "value": value,
		"scriptPubKey": map[string]any{"hex": "5120aa"}, "coinbase": false,
		"assettype": assetType,
	}
}

func cfgFor(t *testing.T) config {
	t.Helper()
	return config{
		dir: split.Dir(t.TempDir()), network: types.Stagenet, hot: hot,
		maxCacheAge: time.Minute, minConf: 1,
	}
}

func readSnapshot(t *testing.T, d split.Dir) (split.Snapshot, error) {
	t.Helper()
	return split.ReadSnapshot(d, time.Hour, time.Minute, time.Now().UTC())
}

// The snapshot is written only when the indexer's answer is fresh and the node
// confirms the outputs. This is the whole of what the signer relies on, so
// every refusal is a case here.
func TestPublishWithholdsTheSnapshotUnlessTheViewIsCurrentAndConfirmed(t *testing.T) {
	const height = 900
	good := func() *fakeIndexer {
		return &fakeIndexer{
			at: time.Now(),
			utxos: []types.UTXO{
				{TxID: txA, Vout: 0, Value: 2_500_000_000, Height: height, Address: hot},
			},
		}
	}
	node := func(f *fakeNode) *fakeNode {
		if f.txout == nil {
			f.txout = func(string, uint32) map[string]any {
				return txoutOf("25.00000000", 101, types.AssetTypeSOQ)
			}
		}
		if f.blocks == 0 {
			f.blocks = 1000
		}
		return f
	}

	for _, c := range []struct {
		name    string
		idx     *fakeIndexer
		node    *fakeNode
		written bool
	}{
		{"a current view and a confirmed output", good(), node(&fakeNode{}), true},
		{
			"the indexer's last answer was an error",
			&fakeIndexer{at: time.Now(), err: errors.New("refused"), utxos: good().utxos},
			node(&fakeNode{}), false,
		},
		{
			"the indexer's answer is older than the limit",
			&fakeIndexer{at: time.Now().Add(-10 * time.Minute), utxos: good().utxos},
			node(&fakeNode{}), false,
		},
		{"the indexer has never answered", &fakeIndexer{}, node(&fakeNode{}), false},
		{
			"the node is still syncing",
			good(), node(&fakeNode{blocks: 900, headers: 1000, syncing: true}), false,
		},
		{
			"the node reports a tip of zero",
			good(), node(&fakeNode{blocks: -1}), false,
		},
		{
			"the tip call fails",
			good(), node(&fakeNode{fail: "getblockcount"}), false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := cfgFor(t)
			publish(context.Background(), cfg, c.idx, c.node.server(t))
			snap, err := readSnapshot(t, cfg.dir)
			if !c.written {
				if !errors.Is(err, split.ErrNoSnapshot) {
					t.Fatalf("a snapshot was written: %+v (err %v)", snap, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("no snapshot from a current view: %v", err)
			}
			if len(snap.Outputs) != 1 || snap.Outputs[0].TxID != txA || snap.Outputs[0].Value != 2_500_000_000 {
				t.Fatalf("snapshot holds %+v", snap.Outputs)
			}
			if snap.Tip != 1000 || snap.HotAddress != hot {
				t.Fatalf("snapshot tip %d hot %q", snap.Tip, snap.HotAddress)
			}
		})
	}
}

// An output the node no longer has is left out and evicted from the cache, and
// an output of another asset is left out on the node's word rather than on the
// cache's, which does not carry one. A snapshot that kept either would be
// refused wholesale by the signer, stopping every withdrawal.
func TestPublishWritesOnlyWhatTheNodeConfirmsAsNativeSOQ(t *testing.T) {
	cfg := cfgFor(t)
	idx := &fakeIndexer{
		at: time.Now(),
		utxos: []types.UTXO{
			{TxID: txA, Vout: 0, Value: 2_500_000_000, Height: 900, Address: hot},
			{TxID: txB, Vout: 0, Value: 1_000_000_000, Height: 900, Address: hot},
			// Spent since the indexer last looked.
			{TxID: txA, Vout: 1, Value: 500_000_000, Height: 900, Address: hot},
			// Unconfirmed, so no selector could spend it anyway.
			{TxID: txB, Vout: 9, Value: 700_000_000, Height: 0, Address: hot},
		},
	}
	node := &fakeNode{blocks: 1000, txout: func(txid string, vout uint32) map[string]any {
		switch {
		case txid == txA && vout == 1:
			return nil // the node does not have it
		case txid == txB:
			return txoutOf("10.00000000", 101, types.AssetTypeUSDSOQ)
		default:
			return txoutOf("25.00000000", 101, types.AssetTypeSOQ)
		}
	}}

	publish(context.Background(), cfg, idx, node.server(t))

	snap, err := readSnapshot(t, cfg.dir)
	if err != nil {
		t.Fatalf("snapshot refused: %v", err)
	}
	if len(snap.Outputs) != 1 || snap.Outputs[0].TxID != txA || snap.Outputs[0].Vout != 0 {
		t.Fatalf("snapshot holds %+v, want only the native output the node confirmed", snap.Outputs)
	}
	if len(idx.evicted) != 1 || idx.evicted[0] != txA {
		t.Fatalf("evictions %v, want the output the node no longer has", idx.evicted)
	}
	if got := idx.stamped[txB]; got != types.AssetTypeUSDSOQ {
		t.Fatalf("the cache was stamped %d for the other asset, want %d", got, types.AssetTypeUSDSOQ)
	}
}
