//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// batchEngine is the fixture's engine with the batch signer a pool wires:
// tx.BuildAndSignBatch over the payees' scripts, change to the hot wallet.
func (f *fixture) batchEngine(t *testing.T, store withdraw.Store, spent *utxo.SpentSet, bc withdraw.Broadcaster) *withdraw.Engine {
	t.Helper()
	e := f.engine(t, store, spent, bc)
	changeSPK, err := address.ScriptFor(f.hot)
	if err != nil {
		t.Fatal(err)
	}
	e.BuildSignBatch = func(_ context.Context, inputs []types.UTXO, payees []withdraw.Payee, feeRate int64) (string, string, error) {
		outputs := make([]tx.TxOutput, len(payees))
		for i, p := range payees {
			spk, err := address.ScriptFor(p.Address)
			if err != nil {
				return "", "", err
			}
			outputs[i] = tx.TxOutput{Value: p.Amount, ScriptPubKey: spk}
		}
		return tx.BuildAndSignBatch(inputs, outputs, changeSPK, feeRate, f.hotKeys)
	}
	return e
}

func batchPayees(t *testing.T, n int) []withdraw.Payee {
	t.Helper()
	out := make([]withdraw.Payee, n)
	for i := range out {
		_, addr := newKey(t)
		out[i] = withdraw.Payee{Address: addr, Amount: (1_000+int64(i))*types.ShorsPerSOQ + int64(i)}
	}
	return out
}

// requireBatchOnChain reads every recipient output of txid from the node and
// requires output i to pay payee i the recorded amount.
func requireBatchOnChain(t *testing.T, f *fixture, txid string, payees []withdraw.Payee) {
	t.Helper()
	for i, p := range payees {
		out, err := f.n.rpc.GetTxOut(context.Background(), txid, uint32(i), true)
		if err != nil || out == nil {
			t.Fatalf("output %d of %s: %v", i, txid, err)
		}
		spk, _ := address.ScriptFor(p.Address)
		if out.Value != p.Amount || out.ScriptPubKey.Hex != hex.EncodeToString(spk) {
			t.Fatalf("output %d pays %d to %s, want %d to payee %d", i, out.Value, out.ScriptPubKey.Hex, p.Amount, i)
		}
	}
}

// A batch of 120 payees is one transaction the node accepts and mines, and
// every payee's output carries the recorded amount. Processing the same id
// again sends nothing.
func TestBatchPayoutEndToEnd(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	spent := openSpent(t, filepath.Join(t.TempDir(), "spent.json"))
	e := f.batchEngine(t, withdraw.NewMemStore(), spent, f.n.rpc)
	payees := batchPayees(t, 120)

	if _, _, err := e.SubmitBatch(ctx, "run-1", payees, types.RecommendedFeeRate); err != nil {
		t.Fatal(err)
	}
	in, err := e.Process(ctx, "run-1")
	if err != nil {
		t.Fatalf("process: %v (state %s, last error %s)", err, in.State, in.LastError)
	}
	if in.State != withdraw.StateBroadcast {
		t.Fatalf("state %s", in.State)
	}
	if _, err := f.n.rpc.Call(ctx, "getmempoolentry", in.TxID); err != nil {
		t.Fatalf("node does not have %s in its mempool: %v", in.TxID, err)
	}
	requireBatchOnChain(t, f, in.TxID, payees)

	before := f.n.height()
	again, err := e.Process(ctx, "run-1")
	if err != nil || again.TxID != in.TxID {
		t.Fatalf("second process: %v, txid %s, want %s", err, again.TxID, in.TxID)
	}
	if _, created, err := e.SubmitBatch(ctx, "run-1", payees, types.RecommendedFeeRate); err != nil || created {
		t.Fatalf("the same batch submitted again: %v created=%v", err, created)
	}

	f.n.mine(f.hot, 3)
	if f.n.height() != before+3 {
		t.Fatal("the node did not mine")
	}
	if err := e.UpdateConfirmations(ctx, in); err != nil {
		t.Fatal(err)
	}
	if in.State != withdraw.StateConfirmed || in.Confirmations < 3 {
		t.Fatalf("after 3 blocks: state %s, %d confirmations", in.State, in.Confirmations)
	}
	requireBatchOnChain(t, f, in.TxID, payees)

	// Each payee holds one output, so the batch was mined once.
	rs := newScanner(f.n, payees[0].Address, payees[119].Address)
	if err := rs.RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []withdraw.Payee{payees[0], payees[119]} {
		got := rs.GetUTXOs(p.Address)
		if len(got) != 1 || got[0].Value != p.Amount {
			t.Fatalf("payee %s holds %+v, want one output of %d", p.Address, got, p.Amount)
		}
	}
}

// The reply to a batch's broadcast is lost. The retry sends the same bytes,
// and each payee is paid once.
func TestBatchLostBroadcastReplyNeverPaysTwice(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	spent := openSpent(t, filepath.Join(t.TempDir(), "spent.json"))
	lossy := &lossyBroadcaster{inner: f.n.rpc, drop: true}
	e := f.batchEngine(t, withdraw.NewMemStore(), spent, lossy)
	payees := batchPayees(t, 30)

	if _, _, err := e.SubmitBatch(ctx, "run-2", payees, types.RecommendedFeeRate); err != nil {
		t.Fatal(err)
	}
	in, err := e.Process(ctx, "run-2")
	if err == nil || in.State != withdraw.StateBuilt {
		t.Fatalf("after the lost reply: err %v, state %s; want an error and Built", err, in.State)
	}
	first := in.TxID
	in, err = e.Process(ctx, "run-2")
	if err != nil || in.State != withdraw.StateBroadcast || in.TxID != first {
		t.Fatalf("retry: %v, state %s, txid %s, want Broadcast under %s", err, in.State, in.TxID, first)
	}
	if lossy.sent != 2 {
		t.Fatalf("sent %d times, want 2 sends of the same bytes", lossy.sent)
	}
	f.n.mine(f.hot, 1)
	rs := newScanner(f.n, payees[0].Address, payees[29].Address)
	if err := rs.RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []withdraw.Payee{payees[0], payees[29]} {
		got := rs.GetUTXOs(p.Address)
		if len(got) != 1 || got[0].Value != p.Amount {
			t.Fatalf("payee %s holds %+v, want one output of %d", p.Address, got, p.Amount)
		}
	}
}
