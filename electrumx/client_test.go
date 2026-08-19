// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// These tests cover the UTXO cache, which is the part of this client that holds
// state and therefore the part that can be wrong without any network error being
// raised. The TCP transport is exercised in production and is not simulated here.

package electrumx

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func init() { log.SetOutput(io.Discard) }

const (
	txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	adr = "ssq1pqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
)

// newTestClient returns a client with a pre-seeded cache and no network.
func newTestClient(seed map[string][]types.UTXO) *Client {
	c := NewClient("127.0.0.1:1", time.Second)
	if seed != nil {
		c.utxos = seed
	}
	return c
}

func utxo(txid string, vout uint32, value int64, height int64, asset uint8) types.UTXO {
	return types.UTXO{TxID: txid, Vout: vout, Value: value, Height: height, Address: adr, AssetType: asset}
}

// ── shortID: the panic regression ──────────────────────────────────────────
//
// EvictUTXO used to format its log line with txid[:12] and no length guard, so a
// UTXO with a short identifier crashed the process. That call sits on the
// withdrawal path — VerifyAndFilterUTXOs passes EvictUTXO as its eviction
// callback — so the panic would land mid-payout, and it would replace a handled
// stale-UTXO condition with process death.

func TestShortIDNeverPanics(t *testing.T) {
	for _, s := range []string{"", "a", "abc", "0123456789", "0123456789ab", txA} {
		got := shortID(s, 12)
		if len(got) > 12 {
			t.Errorf("shortID(%q, 12) = %q, longer than 12", s, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Errorf("shortID(%q, 12) = %q, not a prefix", s, got)
		}
	}
}

func TestEvictUTXODoesNotPanicOnShortTxID(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EvictUTXO panicked on a short txid: %v", r)
		}
	}()
	c := newTestClient(map[string][]types.UTXO{
		adr: {utxo("abc", 0, 1000, 100, types.AssetTypeSOQ)},
	})
	c.EvictUTXO("abc", 0)
	if n := len(c.utxos[adr]); n != 0 {
		t.Errorf("UTXO not evicted: %d remain", n)
	}
}

func TestAddChangeUTXODoesNotPanicOnShortIdentifiers(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AddChangeUTXO panicked: %v", r)
		}
	}()
	c := newTestClient(nil)
	c.AddChangeUTXO("ab", 0, 500, "s")
	if n := len(c.utxos["s"]); n != 1 {
		t.Errorf("change UTXO not added: %d entries", n)
	}
}

// ── EvictUTXO ──────────────────────────────────────────────────────────────

func TestEvictUTXORemovesOnlyTheMatchingOutpoint(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {
			utxo(txA, 0, 100, 10, types.AssetTypeSOQ),
			utxo(txA, 1, 200, 10, types.AssetTypeSOQ), // same txid, different vout
			utxo(txB, 0, 300, 10, types.AssetTypeSOQ),
		},
	})
	c.EvictUTXO(txA, 1)

	remaining := c.GetUTXOs(adr)
	if len(remaining) != 2 {
		t.Fatalf("remaining = %d, want 2", len(remaining))
	}
	for _, u := range remaining {
		if u.TxID == txA && u.Vout == 1 {
			t.Error("the targeted outpoint survived eviction")
		}
	}
	// The same txid at a different vout is a different UTXO and must be kept.
	var keptA0 bool
	for _, u := range remaining {
		if u.TxID == txA && u.Vout == 0 {
			keptA0 = true
		}
	}
	if !keptA0 {
		t.Error("evicting txA:1 also removed txA:0")
	}
}

func TestEvictUTXOUnknownOutpointIsANoOp(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {utxo(txA, 0, 100, 10, types.AssetTypeSOQ)},
	})
	c.EvictUTXO(txB, 9)
	if n := len(c.GetUTXOs(adr)); n != 1 {
		t.Errorf("cache size = %d, want 1 unchanged", n)
	}
}

// ── SpentPending (Defense 12) ──────────────────────────────────────────────

func TestMarkAndUnmarkSpentPending(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {utxo(txA, 0, 100, 10, types.AssetTypeSOQ)},
	})
	c.MarkSpentPending(txA, 0)
	if !c.GetUTXOs(adr)[0].SpentPending {
		t.Fatal("MarkSpentPending did not set the flag")
	}
	c.UnmarkSpentPending(txA, 0)
	if c.GetUTXOs(adr)[0].SpentPending {
		t.Error("UnmarkSpentPending did not clear the flag")
	}
}

// A UTXO marked spent-pending must not be counted as spendable balance, or a
// second payment selects an input the first payment already consumed.
func TestGetBalanceExcludesSpentPending(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {
			utxo(txA, 0, 100, 10, types.AssetTypeSOQ),
			utxo(txB, 0, 400, 10, types.AssetTypeSOQ),
		},
	})
	confirmed, _ := c.GetBalance(1, 100)
	if confirmed != 500 {
		t.Fatalf("confirmed = %d, want 500", confirmed)
	}
	c.MarkSpentPending(txB, 0)
	confirmed, _ = c.GetBalance(1, 100)
	if confirmed != 100 {
		t.Errorf("confirmed after marking = %d, want 100", confirmed)
	}
}

// ── GetBalance: asset and confirmation filtering ───────────────────────────

// USDSOQ must never be counted as native SOQ. Mixing them would let a caller
// spend a stablecoin output as if it were the base asset.
func TestGetBalanceCountsOnlyNativeSOQ(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {
			utxo(txA, 0, 100, 10, types.AssetTypeSOQ),
			utxo(txB, 0, 900, 10, 1), // USDSOQ
		},
	})
	confirmed, unconfirmed := c.GetBalance(1, 100)
	if confirmed != 100 {
		t.Errorf("confirmed = %d, want 100 (USDSOQ excluded)", confirmed)
	}
	if unconfirmed != 0 {
		t.Errorf("unconfirmed = %d, want 0", unconfirmed)
	}
}

func TestGetBalanceSplitsOnConfirmationDepth(t *testing.T) {
	const tip = 1000
	c := newTestClient(map[string][]types.UTXO{
		adr: {
			utxo(txA, 0, 100, 0, types.AssetTypeSOQ),    // in mempool
			utxo(txA, 1, 200, 1000, types.AssetTypeSOQ), // 1 confirmation
			utxo(txB, 0, 400, 701, types.AssetTypeSOQ),  // 300 confirmations
		},
	})
	// At a 288-confirmation threshold only the deep one counts as confirmed.
	confirmed, unconfirmed := c.GetBalance(288, tip)
	if confirmed != 400 {
		t.Errorf("confirmed = %d, want 400 (only the 300-conf UTXO)", confirmed)
	}
	if unconfirmed != 300 {
		t.Errorf("unconfirmed = %d, want 300 (mempool + 1-conf)", unconfirmed)
	}
	// At a 1-confirmation threshold everything mined counts.
	confirmed, unconfirmed = c.GetBalance(1, tip)
	if confirmed != 600 {
		t.Errorf("confirmed = %d, want 600", confirmed)
	}
	if unconfirmed != 100 {
		t.Errorf("unconfirmed = %d, want 100", unconfirmed)
	}
}

// ── SetAssetType (Defense 11 stamping) ─────────────────────────────────────

func TestSetAssetTypeStampsTheCache(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {utxo(txA, 0, 100, 10, types.AssetTypeSOQ)},
	})
	c.SetAssetType(txA, 0, 1)
	if got := c.GetUTXOs(adr)[0].AssetType; got != 1 {
		t.Errorf("AssetType = %d, want 1", got)
	}
	// Stamping makes it non-native, so it drops out of the SOQ balance.
	if confirmed, _ := c.GetBalance(1, 100); confirmed != 0 {
		t.Errorf("confirmed = %d, want 0 after restamping as USDSOQ", confirmed)
	}
}

func TestSetAssetTypeUnknownOutpointIsANoOp(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {utxo(txA, 0, 100, 10, types.AssetTypeSOQ)},
	})
	c.SetAssetType(txB, 0, 1)
	if got := c.GetUTXOs(adr)[0].AssetType; got != types.AssetTypeSOQ {
		t.Errorf("an unrelated UTXO was restamped to %d", got)
	}
}

// ── AddChangeUTXO (Defense 13) ─────────────────────────────────────────────

// Change must be spendable immediately, before ElectrumX has seen the
// transaction, or back-to-back payments fail for lack of inputs.
func TestAddChangeUTXOIsImmediatelySpendableAsUnconfirmed(t *testing.T) {
	c := newTestClient(nil)
	c.AddChangeUTXO(txA, 1, 750, adr)

	got := c.GetUTXOs(adr)
	if len(got) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(got))
	}
	if got[0].Height != 0 {
		t.Errorf("Height = %d, want 0 (unconfirmed)", got[0].Height)
	}
	if got[0].Value != 750 || got[0].Vout != 1 {
		t.Errorf("change UTXO = %+v, want value 750 vout 1", got[0])
	}
	if got[0].AssetType != types.AssetTypeSOQ {
		t.Errorf("AssetType = %d, want native SOQ", got[0].AssetType)
	}
	if got[0].SpentPending {
		t.Error("fresh change must not be marked spent-pending")
	}
	// Unconfirmed, so it must not appear in the confirmed balance.
	confirmed, unconfirmed := c.GetBalance(1, 100)
	if confirmed != 0 {
		t.Errorf("confirmed = %d, want 0", confirmed)
	}
	if unconfirmed != 750 {
		t.Errorf("unconfirmed = %d, want 750", unconfirmed)
	}
}

// ── Cache accessors ────────────────────────────────────────────────────────

// GetUTXOs must hand back a copy. If it returned the backing slice, a caller
// mutating the result would corrupt the cache without touching any setter.
func TestGetUTXOsReturnsACopy(t *testing.T) {
	c := newTestClient(map[string][]types.UTXO{
		adr: {utxo(txA, 0, 100, 10, types.AssetTypeSOQ)},
	})
	got := c.GetUTXOs(adr)
	got[0].Value = 999_999

	if inCache := c.GetUTXOs(adr)[0].Value; inCache != 100 {
		t.Errorf("cache was mutated through the returned slice: value = %d", inCache)
	}
}

func TestGetUTXOsUnknownAddressReturnsEmpty(t *testing.T) {
	c := newTestClient(nil)
	if got := c.GetUTXOs("ssq1punknown"); len(got) != 0 {
		t.Errorf("got %d UTXOs for an untracked address", len(got))
	}
}

func TestGetAllUTXOsSpansAddresses(t *testing.T) {
	const other = "ssq1pother"
	c := newTestClient(map[string][]types.UTXO{
		adr:   {utxo(txA, 0, 100, 10, types.AssetTypeSOQ)},
		other: {utxo(txB, 0, 200, 10, types.AssetTypeSOQ)},
	})
	if n := len(c.GetAllUTXOs()); n != 2 {
		t.Errorf("GetAllUTXOs returned %d, want 2 across both addresses", n)
	}
}

func TestTrackAddressesRegistersForPolling(t *testing.T) {
	c := newTestClient(nil)
	c.TrackAddresses([]string{adr, "ssq1psecond"})
	// Tracking alone must not invent UTXOs.
	if n := len(c.GetAllUTXOs()); n != 0 {
		t.Errorf("tracking created %d phantom UTXOs", n)
	}
}
