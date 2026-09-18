package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/examples/exchange_split/split"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// recordingNode accepts every transaction under its own txid, or, for the
// txids named in mismatch, under another one.
type recordingNode struct {
	mu       sync.Mutex
	sent     []string
	mismatch map[string]bool
}

func (n *recordingNode) Broadcast(_ context.Context, _ string, txid string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, txid)
	if n.mismatch[txid] {
		return "node-" + txid, fmt.Errorf("broadcast: %w: node returned node-%s for %s", rpc.ErrTxIDMismatch, txid, txid)
	}
	return txid, nil
}

func (n *recordingNode) count() int { n.mu.Lock(); defer n.mu.Unlock(); return len(n.sent) }

// built writes a Built intent with one input of its own to the store.
func built(t *testing.T, store *split.DirStore, id string, hold func(*withdraw.Intent)) {
	t.Helper()
	in := &withdraw.Intent{
		ID: id, Address: "ssq1pdestination", Amount: 1000, FeeRate: types.RecommendedFeeRate,
		State: withdraw.StateBuilt, TxID: "tx-" + id, RawHex: "hex-" + id,
		Inputs: []withdraw.Outpoint{{TxID: strings.Repeat(id[len(id)-1:], 64), Vout: 0, Value: 5000, Address: "ssq1phot"}},
	}
	if hold != nil {
		hold(in)
	}
	if err := store.Create(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*split.DirStore, *withdraw.Engine, *recordingNode, *bytes.Buffer) {
	t.Helper()
	store, err := split.Dir(filepath.Join(t.TempDir(), "state")).Open()
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	logger = slog.New(slog.NewTextHandler(&log, nil))
	node := &recordingNode{mismatch: map[string]bool{}}
	engine := &withdraw.Engine{
		Store: store, Spent: utxo.NewSpentSet("", nil), Broadcaster: node,
		Network: types.Stagenet, RequiredConfirmations: 1,
	}
	return store, engine, node, &log
}

// A held intent in the store stops every send: the node accepted other bytes
// for it, or rejected bytes an earlier attempt may have relayed, and the
// guide says stop withdrawals and investigate before anything is rebuilt.
// The condition is read from the store on every pass, so it holds after a
// restart and ends when an operator has moved the intent on. The held intent
// sorts after the sendable one here, so only a check made before the loop
// keeps the sendable one back.
func TestNothingIsSentWhileAnIntentIsHeld(t *testing.T) {
	store, engine, node, log := setup(t)
	built(t, store, "w-a", nil)
	built(t, store, "w-b", func(in *withdraw.Intent) { in.NodeTxID = "node-tx-w-b" })

	send(context.Background(), store, engine)
	if node.count() != 0 {
		t.Fatalf("sent %v while w-b is held; want nothing sent", node.sent)
	}
	if !strings.Contains(log.String(), "held") {
		t.Fatalf("the halt was not reported; log:\n%s", log.String())
	}
	// The operator has resolved it: the record is Failed. The next pass sends.
	held, _, _ := store.Get(context.Background(), "w-b")
	held.State = withdraw.StateFailed
	if err := store.Update(context.Background(), held, withdraw.StateBuilt); err != nil {
		t.Fatal(err)
	}
	send(context.Background(), store, engine)
	if node.count() != 1 || node.sent[0] != "tx-w-a" {
		t.Fatalf("sent %v after the hold was resolved; want tx-w-a alone", node.sent)
	}
}

// A hold that arises inside a pass ends the pass: the intent listed after the
// one the node accepted under another txid is not sent in that pass, and the
// store check refuses it in the next.
func TestATxIDMismatchEndsThePassAndTheNextOneSendsNothing(t *testing.T) {
	store, engine, node, _ := setup(t)
	built(t, store, "w-a", nil)
	built(t, store, "w-b", nil)
	node.mismatch["tx-w-a"] = true

	send(context.Background(), store, engine)
	if node.count() != 1 || node.sent[0] != "tx-w-a" {
		t.Fatalf("sent %v; want the pass to end at w-a's mismatch with w-b unsent", node.sent)
	}
	a, _, _ := store.Get(context.Background(), "w-a")
	if a.State != withdraw.StateBuilt || a.NodeTxID != "node-tx-w-a" {
		t.Fatalf("w-a is %s with NodeTxID %q; want Built and held under the node's txid", a.State, a.NodeTxID)
	}
	send(context.Background(), store, engine)
	if node.count() != 1 {
		t.Fatalf("sent %v on the next pass; want nothing while w-a is held", node.sent)
	}
}

// Recover attempts every Built intent it can re-reserve whatever an earlier
// one returned, so at startup it runs as a repair only: every Built intent is
// re-reserved, nothing is sent, and the report carries no entry for the
// intents the send pass will send. The send pass then applies the hold rule,
// so a hold that arises on the first intent keeps the second back.
func TestRecoverAtStartupRepairsAndSendsNothing(t *testing.T) {
	store, engine, node, log := setup(t)
	built(t, store, "w-a", nil)
	built(t, store, "w-b", nil)
	node.mismatch["tx-w-a"] = true

	if err := recoverAtStartup(context.Background(), engine); err != nil {
		t.Fatal(err)
	}
	if node.count() != 0 {
		t.Fatalf("recover sent %v; want nothing sent at startup", node.sent)
	}
	if got := engine.Spent.ReservedIntents(); len(got) != 2 {
		t.Fatalf("reserved %v; want both Built intents re-reserved by the repair pass", got)
	}
	if strings.Contains(log.String(), "context canceled") {
		t.Fatalf("the report names the repair context's own cancel; log:\n%s", log.String())
	}
	send(context.Background(), store, engine)
	if node.count() != 1 || node.sent[0] != "tx-w-a" {
		t.Fatalf("sent %v; want the send pass to end at w-a's mismatch with w-b unsent", node.sent)
	}
}

// The process's own context ending during the repair is returned; a held
// intent found by Recover is reported and does not stop the process.
func TestRecoverAtStartupReportsAHoldAndReturnsTheProcessContext(t *testing.T) {
	store, engine, _, log := setup(t)
	built(t, store, "w-b", func(in *withdraw.Intent) { in.NodeTxID = "node-tx-w-b" })
	if err := recoverAtStartup(context.Background(), engine); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "held") {
		t.Fatalf("the held intent was not reported; log:\n%s", log.String())
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recoverAtStartup(ended, engine); !errors.Is(err, context.Canceled) {
		t.Fatalf("recover under the process's ended context: %v, want context.Canceled", err)
	}
}
