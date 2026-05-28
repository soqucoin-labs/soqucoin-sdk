package utxo

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// ── SpentSet Tests ──

func TestSpentSetMarkAndCheck(t *testing.T) {
	ss := NewSpentSet("")

	utxos := []types.UTXO{
		{TxID: "aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", Vout: 0, Value: 100000},
		{TxID: "1122334455667788aabbccddeeff0011aabbccddee1122334455667788aabbcc", Vout: 1, Value: 200000},
	}

	ss.MarkBroadcast(utxos, "txid_broadcast_0000000000000000000000000000000000000000000000000000")

	if !ss.IsSpent("aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", 0) {
		t.Error("expected UTXO 0 to be in spent set")
	}
	if !ss.IsSpent("1122334455667788aabbccddeeff0011aabbccddee1122334455667788aabbcc", 1) {
		t.Error("expected UTXO 1 to be in spent set")
	}
	if ss.IsSpent("0000000000000000000000000000000000000000000000000000000000000000", 0) {
		t.Error("expected unknown UTXO to NOT be in spent set")
	}
	if ss.Size() != 2 {
		t.Errorf("expected size 2, got %d", ss.Size())
	}
}

func TestSpentSetPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spent_set.json")

	// Create and populate
	ss1 := NewSpentSet(path)
	utxos := []types.UTXO{
		{TxID: "aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", Vout: 0, Value: 100000},
	}
	ss1.MarkBroadcast(utxos, "txid_broadcast_0000000000000000000000000000000000000000000000000000")

	// Verify file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("spent set file should exist after MarkBroadcast")
	}

	// Load from disk in a new instance
	ss2 := NewSpentSet(path)
	if !ss2.IsSpent("aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", 0) {
		t.Error("expected UTXO to persist across restarts")
	}
	if ss2.Size() != 1 {
		t.Errorf("expected size 1 after reload, got %d", ss2.Size())
	}
}

func TestSpentSetPrune(t *testing.T) {
	ss := NewSpentSet("")

	// Manually insert an old confirmed entry
	ss.mu.Lock()
	ss.entries[SpentKey{"old_txid_0000000000000000000000000000000000000000000000000000", 0}] = SpentEntry{
		TxID:      "old_txid_0000000000000000000000000000000000000000000000000000",
		Vout:      0,
		SpentInTx: "old_broadcast",
		SpentAt:   time.Now().Add(-2 * time.Hour), // 2 hours ago
		Confirmed: true,
	}
	ss.mu.Unlock()

	ss.Prune()

	if ss.Size() != 0 {
		t.Errorf("expected old confirmed entry to be pruned, size=%d", ss.Size())
	}
}

func TestSpentSetConfirm(t *testing.T) {
	ss := NewSpentSet("")

	utxos := []types.UTXO{
		{TxID: "aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", Vout: 0, Value: 100000},
	}
	ss.MarkBroadcast(utxos, "txid_broadcast_0000000000000000000000000000000000000000000000000000")

	ss.ConfirmSpent("aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", 0)

	// Should still be in spent set (confirmed but not pruned yet)
	if !ss.IsSpent("aabbccddee112233445566778899aabb0011223344556677aabbccddee112233", 0) {
		t.Error("confirmed entry should still be in spent set until pruned")
	}
}

// ── CoinSelector Tests ──

func TestSelectUTXOsLargestFirst(t *testing.T) {
	ss := NewSpentSet("")
	cs := NewCoinSelector(ss)

	utxos := []types.UTXO{
		{TxID: "tx1_000000000000000000000000000000000000000000000000000000000000", Vout: 0, Value: 100000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx2_000000000000000000000000000000000000000000000000000000000000", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx3_000000000000000000000000000000000000000000000000000000000000", Vout: 0, Value: 300000, Height: 10, AssetType: types.AssetTypeSOQ},
	}

	selected, total, err := cs.SelectUTXOs(utxos, 400000, 1, 20, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should select the 500000 UTXO first (largest-first)
	if len(selected) != 1 {
		t.Errorf("expected 1 UTXO selected, got %d", len(selected))
	}
	if total != 500000 {
		t.Errorf("expected total 500000, got %d", total)
	}
}

func TestSelectUTXOsSkipsSpentSet(t *testing.T) {
	ss := NewSpentSet("")
	cs := NewCoinSelector(ss)

	utxos := []types.UTXO{
		{TxID: "tx1_000000000000000000000000000000000000000000000000000000000000", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx2_000000000000000000000000000000000000000000000000000000000000", Vout: 0, Value: 300000, Height: 10, AssetType: types.AssetTypeSOQ},
	}

	// Mark the larger UTXO as spent
	ss.MarkBroadcast([]types.UTXO{utxos[0]}, "broadcast_txid_000000000000000000000000000000000000000000000000")

	selected, total, err := cs.SelectUTXOs(utxos, 200000, 1, 20, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 1 {
		t.Errorf("expected 1 UTXO (spent one skipped), got %d", len(selected))
	}
	if total != 300000 {
		t.Errorf("expected total 300000 (smaller UTXO), got %d", total)
	}
}

func TestSelectUTXOsSkipsSpentPending(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeSOQ, SpentPending: true},
		{TxID: "tx2", Vout: 0, Value: 300000, Height: 10, AssetType: types.AssetTypeSOQ},
	}

	selected, total, err := cs.SelectUTXOs(utxos, 200000, 1, 20, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 1 {
		t.Errorf("expected 1 UTXO (pending skipped), got %d", len(selected))
	}
	if total != 300000 {
		t.Errorf("expected 300000, got %d", total)
	}
}

func TestSelectUTXOsSkipsWrongAssetType(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeUSDSOQ},
		{TxID: "tx2", Vout: 0, Value: 300000, Height: 10, AssetType: types.AssetTypeSOQ},
	}

	selected, _, err := cs.SelectUTXOs(utxos, 200000, 1, 20, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 1 || selected[0].TxID != "tx2" {
		t.Error("expected only SOQ UTXO to be selected")
	}
}

func TestSelectUTXOsInsufficientFunds(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 100000, Height: 10, AssetType: types.AssetTypeSOQ},
	}

	_, _, err := cs.SelectUTXOs(utxos, 500000, 1, 20, nil)
	if err == nil {
		t.Error("expected insufficient funds error")
	}
}

func TestSelectUTXOsMinConfirmations(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 500000, Height: 18, AssetType: types.AssetTypeSOQ}, // 3 confs at tip 20
		{TxID: "tx2", Vout: 0, Value: 300000, Height: 20, AssetType: types.AssetTypeSOQ}, // 1 conf at tip 20
	}

	// Require 6 confirmations — only tx1 qualifies (height 18, tip 20 → 3 confs... actually that's only 3)
	// Need tip=23 for tx1 to have 6 confs
	_, _, err := cs.SelectUTXOs(utxos, 200000, 6, 20, nil)
	if err == nil {
		t.Error("expected error — no UTXOs have 6 confirmations at tip 20")
	}

	// With enough tip height, tx1 qualifies
	selected, _, err := cs.SelectUTXOs(utxos, 200000, 6, 25, nil)
	if err != nil {
		t.Fatalf("unexpected error with sufficient confs: %v", err)
	}
	if len(selected) != 1 || selected[0].TxID != "tx1" {
		t.Error("expected only tx1 to qualify with 6 confs at tip 25")
	}
}

func TestSelectUTXOsDustFilter(t *testing.T) {
	cs := NewCoinSelector(nil)
	cs.MinUTXOValue = 50000 // 0.0005 SOQ minimum

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 10000, Height: 10, AssetType: types.AssetTypeSOQ},  // Below dust
		{TxID: "tx2", Vout: 0, Value: 100000, Height: 10, AssetType: types.AssetTypeSOQ}, // Above dust
	}

	selected, _, err := cs.SelectUTXOs(utxos, 50000, 1, 20, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 1 || selected[0].TxID != "tx2" {
		t.Error("expected dust UTXO to be skipped")
	}
}

func TestSelectSmallestUTXOs(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 100000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx2", Vout: 0, Value: 50000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx3", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx4", Vout: 0, Value: 25000, Height: 10, AssetType: types.AssetTypeSOQ},
	}

	selected, total, err := cs.SelectSmallestUTXOs(utxos, 2, 1, 20, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 2 {
		t.Errorf("expected 2 UTXOs, got %d", len(selected))
	}
	if total != 75000 { // 25000 + 50000
		t.Errorf("expected total 75000, got %d", total)
	}
	// Should be sorted smallest first
	if selected[0].Value != 25000 || selected[1].Value != 50000 {
		t.Error("expected UTXOs sorted smallest-first")
	}
}

func TestSelectByAssetType(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeSOQ},
		{TxID: "tx2", Vout: 0, Value: 300000, Height: 10, AssetType: types.AssetTypeUSDSOQ},
		{TxID: "tx3", Vout: 0, Value: 200000, Height: 10, AssetType: types.AssetTypeUSDSOQ},
	}

	selected, total, err := cs.SelectUTXOsByAssetType(utxos, 400000, 1, 20, nil, types.AssetTypeUSDSOQ)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 2 {
		t.Errorf("expected 2 USDSOQ UTXOs, got %d", len(selected))
	}
	if total != 500000 {
		t.Errorf("expected total 500000, got %d", total)
	}
}

func TestSelectUTXOsAddressFilter(t *testing.T) {
	cs := NewCoinSelector(nil)

	utxos := []types.UTXO{
		{TxID: "tx1", Vout: 0, Value: 500000, Height: 10, AssetType: types.AssetTypeSOQ, Address: "ssq1addr1"},
		{TxID: "tx2", Vout: 0, Value: 300000, Height: 10, AssetType: types.AssetTypeSOQ, Address: "ssq1addr2"},
	}

	selected, total, err := cs.SelectUTXOs(utxos, 200000, 1, 20, []string{"ssq1addr2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(selected) != 1 || selected[0].Address != "ssq1addr2" {
		t.Error("expected only addr2 UTXO to be selected")
	}
	if total != 300000 {
		t.Errorf("expected 300000, got %d", total)
	}
}
