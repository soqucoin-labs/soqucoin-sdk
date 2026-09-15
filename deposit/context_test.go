package deposit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// ctxNode is a Node that honours the context, as *rpc.Client does.
type ctxNode struct{ fakeNode }

func (n *ctxNode) RequireSynced(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return n.fakeNode.RequireSynced(ctx)
}

func (n *ctxNode) GetTxOut(ctx context.Context, txid string, vout uint32, mem bool) (*rpc.TxOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return n.fakeNode.GetTxOut(ctx, txid, vout, mem)
}

// A context that ends before or during a scan credits nothing and marks
// nothing final; the error is the node's, not a pause.
func TestScanUnderAnEndedContextCreditsNothing(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	m.Node = &ctxNode{*node}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := m.Scan(ctx)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("scan under an ended context: %v %+v", err, got)
	}
	if errors.Is(err, ErrPaused) {
		t.Errorf("a context error was reported as a pause: %v", err)
	}
	if len(led.credited) != 0 {
		t.Fatal("a deposit was credited under an ended context")
	}
	_ = al
	// The next scan with a live context credits it once.
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("scan after: %v %+v", err, got)
	}
}

// The field's comment has always said a nil OnAlert logs; now it does.
func TestNilOnAlertLogsThroughTheLogger(t *testing.T) {
	m, cache, node, _, _, a := setup(t)
	var buf bytes.Buffer
	m.OnAlert = nil
	m.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = nil // the node does not have it

	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, string(AlertIndexerMismatch)) {
		t.Fatalf("a nil OnAlert did not log the alert at warn: %q", out)
	}

	// And a nil Logger with a nil OnAlert drops it without panicking.
	m.Logger = nil
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}
