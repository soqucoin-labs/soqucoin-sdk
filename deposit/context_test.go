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

// A context that has ended before the scan credits nothing and marks nothing
// final; the error is the context's, not a pause and not an alert.
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
	if len(al.kinds) != 0 {
		t.Fatalf("an ended context raised alerts %v", al.kinds)
	}
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

// cancellingNode ends the context on its nth GetTxOut and then answers as
// the node would once its context has ended.
type cancellingNode struct {
	fakeNode
	cancel context.CancelFunc
	onCall int
	calls  int
}

func (n *cancellingNode) GetTxOut(ctx context.Context, txid string, vout uint32, mem bool) (*rpc.TxOut, error) {
	n.calls++
	if n.calls == n.onCall {
		n.cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return n.fakeNode.GetTxOut(ctx, txid, vout, mem)
}

// The context ends in the middle of the credit loop, at the node lookup for
// the second of three candidates. The first is credited and returned with
// the error, the other two are not, no alert is raised, and the error is the
// context's: a shutdown or a slow node is not an indexer lying. The next scan
// with a live context credits the rest once.
func TestContextEndingMidScanIsReturnedNotAlarmed(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Node = &cancellingNode{fakeNode: *node, cancel: cancel, onCall: 2}
	for i, tx := range []string{txA, txB, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"} {
		cache.utxos[a] = append(cache.utxos[a], types.UTXO{TxID: tx, Vout: uint32(i), Value: 150_000_000, Height: 900, Address: a})
		node.outs[key(tx, uint32(i))] = txout(t, a, 150_000_000, 101, false)
	}
	got, err := m.Scan(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancelled mid-loop: %v, want the context's error", err)
	}
	if errors.Is(err, ErrPaused) {
		t.Errorf("reported as a pause: %v", err)
	}
	if len(got) != 1 || len(led.credited) != 1 {
		t.Fatalf("credited %d and returned %d, want the one verified before the cancel", len(led.credited), len(got))
	}
	if len(al.kinds) != 0 {
		t.Fatalf("a cancelled lookup raised alerts %v; it is not an indexer mismatch", al.kinds)
	}
	m.Node = node
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 2 || len(led.credited) != 3 {
		t.Fatalf("scan after: %v %d credited now %d", err, len(got), len(led.credited))
	}
}

// ledgerCancel is a Ledger whose IsCredited ends the context and reports it.
type ledgerCancel struct {
	*fakeLedger
	cancel context.CancelFunc
}

func (l *ledgerCancel) IsCredited(ctx context.Context, txid string, vout uint32) (bool, error) {
	l.cancel()
	return false, ctx.Err()
}

// The same for the ledger: a database query ended by the context is the
// context's error, not AlertLedgerError.
func TestLedgerContextErrorIsNotALedgerAlert(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Ledger = &ledgerCancel{fakeLedger: led, cancel: cancel}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)
	_, err := m.Scan(ctx)
	if !errors.Is(err, context.Canceled) || len(al.kinds) != 0 {
		t.Fatalf("ledger cancel: %v alerts %v", err, al.kinds)
	}
}

// ledgerEnds is a Ledger whose named method ends the context and, if the
// context it was given has ended, reports that; otherwise it behaves.
type ledgerEnds struct {
	*fakeLedger
	cancel context.CancelFunc
	method string
}

func (l *ledgerEnds) Pending(ctx context.Context) ([]Deposit, error) {
	if l.method == "Pending" {
		l.cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return l.fakeLedger.Pending(ctx)
}

func (l *ledgerEnds) Credit(ctx context.Context, d Deposit) error {
	if l.method == "Credit" {
		l.cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return l.fakeLedger.Credit(ctx, d)
}

func (l *ledgerEnds) MarkFinal(ctx context.Context, txid string, vout uint32) error {
	if l.method == "MarkFinal" {
		l.cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return l.fakeLedger.MarkFinal(ctx, txid, vout)
}

// Pending, a read, ends with the caller's context and is returned unalarmed.
// Credit and MarkFinal, the writes, are made under a context the caller
// cannot end, so the cancel does not reach them and the record lands.
func TestLedgerWritesLandAfterTheContextEndsAndReadsDoNot(t *testing.T) {
	// Pending: the read fails with the context's error, no alert.
	m, cache, node, led, al, a := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.Ledger = &ledgerEnds{fakeLedger: led, cancel: cancel, method: "Pending"}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)
	if _, err := m.Scan(ctx); !errors.Is(err, context.Canceled) || len(al.kinds) != 0 {
		t.Fatalf("pending cancel: %v alerts %v", err, al.kinds)
	}
	cancel()

	// Credit: the cancel lands during the write; the write is not bound to it.
	m, cache, node, led, al, a = setup(t)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	m.Ledger = &ledgerEnds{fakeLedger: led, cancel: cancel, method: "Credit"}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 150_000_000, Height: 900, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 150_000_000, 101, false)
	got, err := m.Scan(ctx)
	if err != nil || len(got) != 1 || len(led.credited) != 1 || len(al.kinds) != 0 {
		t.Fatalf("credit with a cancel: %v got %d credited %d alerts %v; the write must land", err, len(got), len(led.credited), al.kinds)
	}

	// MarkFinal: the same, past the horizon.
	m, cache, node, led, al, a = setup(t)
	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m.Ledger = &ledgerEnds{fakeLedger: led, cancel: cancel2, method: "MarkFinal"}
	led.credited[key(txA, 0)] = Deposit{TxID: txA, Vout: 0, Address: a, Value: 100}
	node.outs[key(txA, 0)] = txout(t, a, 100, types.MaxReorgDepth+1, false)
	// The write lands; the pass then reports the cancel that followed it.
	if _, err := m.Scan(ctx); (err != nil && !errors.Is(err, context.Canceled)) || !led.final[key(txA, 0)] || len(al.kinds) != 0 {
		t.Fatalf("mark final with a cancel: %v final=%v alerts %v; the write must land", err, led.final[key(txA, 0)], al.kinds)
	}
}
