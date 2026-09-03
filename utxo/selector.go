// Package utxo provides UTXO coin selection and persistent spent set tracking.
//
// This package was extracted from the canonical soq-signer service (v1.0.0-alpha)
// which has been running in production since May 2026. It incorporates all
// defenses from DL-SIGNER-SPENT-TRACKING:
//
//   - Defense 11: gettxout pre-verification before signing
//   - Defense 12: Merge-based refresh (preserves SpentPending flags)
//   - Defense 13: Change tracking (immediate availability)
//   - Persistent spent set: Survives process restarts
//   - Largest-first coin selection: Minimizes TX weight
//   - Asset-type-aware selection: Separates SOQ from USDSOQ
//   - Input limit enforcement: MaxInputsPerTX = 80
//
// Copyright (c) 2025-2026 Soqucoin Labs Inc. MIT License.
package utxo

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"strings"
)

// MaxInputsPerTX is the hard cap on UTXO inputs per transaction.
// Dilithium signatures are 2,420 bytes each, so each input adds ~3,896 WU.
// 80 inputs × 3,896 WU = 311,680 WU — safely within MAX_STANDARD_TX_WEIGHT
// (400,000 WU) with a 22% safety margin for outputs and overhead.
//
// NOTE: SOQ-ARCH-003 raised the soqucoind limit to 800K WU, but that build
// has NOT been deployed to all production nodes yet. When it is, this can
// be raised to ~200. Reverted from 200 to 80 on May 26, 2026 after
// tx-size rejection in production.
const MaxInputsPerTX = 80

// ErrInputLimitReached is returned when SelectUTXOs hits MaxInputsPerTX before
// satisfying the target amount. The caller receives a partial selection and
// can proceed with a reduced payment.
var ErrInputLimitReached = errors.New("input limit reached")

// MaxUTXOVerifyRetries is the number of times Defense 11 will retry
// gettxout verification before giving up on a UTXO. Each retry waits
// 2 seconds. This handles the race where a block is mined between
// ElectrumX refresh and gettxout call.
const MaxUTXOVerifyRetries = 8

// SpentKey uniquely identifies a UTXO for the persistent spent set.
type SpentKey struct {
	TxID string
	Vout uint32
}

// SpentEntry records that a TX consuming this UTXO was broadcast.
// Persisted to disk so spend tracking survives restarts.
type SpentEntry struct {
	TxID      string    `json:"txid"`        // The UTXO's transaction ID
	Vout      uint32    `json:"vout"`        // The UTXO's output index
	SpentInTx string    `json:"spent_in_tx"` // TX that consumed this UTXO, or "reserved:<intent>"
	SpentAt   time.Time `json:"spent_at"`    // When we broadcast (or reserved)
	Confirmed bool      `json:"confirmed"`   // True once absent from ElectrumX
	// ExpiresAt is set only on reservations: an input held for a withdrawal
	// that has not been broadcast yet. A reservation past this time is treated
	// as free. Broadcast entries never expire by the clock; they leave the set
	// when the spend is confirmed (ConfirmSpent then Prune).
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// IntentID names the withdrawal that reserved or spent this input, so a
	// failed or abandoned withdrawal can release exactly its own inputs.
	IntentID string `json:"intent_id,omitempty"`
}

// ErrAlreadyReserved is returned by Reserve when any requested input is already
// reserved by another withdrawal or already spent. Nothing is reserved in that
// case: reservation is all-or-nothing.
var ErrAlreadyReserved = errors.New("utxo: input already reserved or spent")

// ReservedPrefix marks reservation entries in SpentInTx.
const ReservedPrefix = "reserved:"

func (e SpentEntry) reserved() bool {
	return !e.ExpiresAt.IsZero() && strings.HasPrefix(e.SpentInTx, ReservedPrefix)
}
func (e SpentEntry) expired(now time.Time) bool {
	return e.reserved() && now.After(e.ExpiresAt)
}

// spentSetFile is the JSON structure persisted to disk.
type spentSetFile struct {
	Version int          `json:"version"`
	Updated time.Time    `json:"updated"`
	Entries []SpentEntry `json:"entries"`
}

// SpentSet tracks UTXOs that have been broadcast in transactions.
// These UTXOs are NEVER re-selected by any coin selection method, even if
// ElectrumX still reports them as unspent (the ~2 minute stale window).
//
// This eliminates the ENTIRE class of stale UTXO failures that plagued
// soqupool payouts for 2+ weeks in May 2026.
type SpentSet struct {
	mu       sync.Mutex
	entries  map[SpentKey]SpentEntry
	filePath string
}

// NewSpentSet creates a new persistent spent set.
// If filePath is empty, the spent set is in-memory only.
// If filePath is set, the spent set loads from disk and persists changes.
func NewSpentSet(filePath string) *SpentSet {
	ss := &SpentSet{
		entries:  make(map[SpentKey]SpentEntry),
		filePath: filePath,
	}
	if filePath != "" {
		ss.load()
	}
	return ss
}

// Reserve holds the given inputs for a withdrawal that is about to be built,
// so a concurrent withdrawal cannot select them. All-or-nothing: if any input
// is already reserved (and not expired) or spent, nothing is reserved and
// ErrAlreadyReserved names the input. The reservation lasts ttl; MarkBroadcast
// converts it into a permanent spent entry, Release drops it.
func (ss *SpentSet) Reserve(inputs []types.UTXO, intentID string, ttl time.Duration) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	now := time.Now()
	for _, u := range inputs {
		key := SpentKey{u.TxID, u.Vout}
		if e, exists := ss.entries[key]; exists && !e.expired(now) {
			if e.IntentID == intentID && e.reserved() {
				continue // re-reserving our own inputs is fine (retry after a crash)
			}
			return fmt.Errorf("%w: %s:%d (%s)", ErrAlreadyReserved, u.TxID, u.Vout, e.SpentInTx)
		}
	}
	for _, u := range inputs {
		ss.entries[SpentKey{u.TxID, u.Vout}] = SpentEntry{
			TxID:      u.TxID,
			Vout:      u.Vout,
			SpentInTx: ReservedPrefix + intentID,
			SpentAt:   now,
			ExpiresAt: now.Add(ttl),
			IntentID:  intentID,
		}
	}
	ss.persist()
	return nil
}

// Release drops the reservations held by intentID. Broadcast entries are
// never released here: once a transaction is out, its inputs are spent until
// the chain says otherwise.
func (ss *SpentSet) Release(intentID string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	released := 0
	for key, e := range ss.entries {
		if e.reserved() && e.IntentID == intentID {
			delete(ss.entries, key)
			released++
		}
	}
	if released > 0 {
		ss.persist()
	}
}

// MarkBroadcast records that the given UTXOs were spent in a broadcast TX.
// This is the PRIMARY defense against stale UTXO re-selection. Reservations
// on these inputs become permanent spent entries.
func (ss *SpentSet) MarkBroadcast(inputs []types.UTXO, broadcastTxID string) {
	ss.markBroadcast(inputs, broadcastTxID, "")
}

// MarkBroadcastFor is MarkBroadcast that also records the withdrawal id.
func (ss *SpentSet) MarkBroadcastFor(inputs []types.UTXO, broadcastTxID, intentID string) {
	ss.markBroadcast(inputs, broadcastTxID, intentID)
}

func (ss *SpentSet) markBroadcast(inputs []types.UTXO, broadcastTxID, intentID string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	now := time.Now()
	for _, u := range inputs {
		key := SpentKey{u.TxID, u.Vout}
		ss.entries[key] = SpentEntry{
			TxID:      u.TxID,
			Vout:      u.Vout,
			SpentInTx: broadcastTxID,
			SpentAt:   now,
			Confirmed: false,
			IntentID:  intentID,
		}
	}

	if len(broadcastTxID) >= 12 {
		log.Printf("[utxo] Spent set: added %d UTXOs from TX %s (total tracked: %d)",
			len(inputs), shortID(broadcastTxID, 12), len(ss.entries))
	}

	ss.persist()
}

// IsSpent checks if a UTXO is in the spent set.
func (ss *SpentSet) IsSpent(txid string, vout uint32) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	e, exists := ss.entries[SpentKey{txid, vout}]
	if !exists {
		return false
	}
	if e.expired(time.Now()) {
		delete(ss.entries, SpentKey{txid, vout})
		return false
	}
	return true
}

// ConfirmSpent marks a spent entry as confirmed (UTXO disappeared from ElectrumX).
func (ss *SpentSet) ConfirmSpent(txid string, vout uint32) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	key := SpentKey{txid, vout}
	if entry, exists := ss.entries[key]; exists && !entry.Confirmed {
		entry.Confirmed = true
		ss.entries[key] = entry
	}
}

// Prune removes confirmed entries older than 1 hour and expired
// reservations. Unconfirmed broadcast entries are never pruned by the clock:
// the transaction that spends them is still in flight until the chain
// confirms it, however long that takes.
// Should be called periodically (e.g., after each UTXO refresh).
func (ss *SpentSet) Prune() {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)
	pruned := 0
	for key, entry := range ss.entries {
		if (entry.Confirmed && entry.SpentAt.Before(cutoff)) || entry.expired(now) {
			delete(ss.entries, key)
			pruned++
		}
	}

	if pruned > 0 {
		log.Printf("[utxo] Spent set: pruned %d confirmed entries (remaining: %d)", pruned, len(ss.entries))
		ss.persist()
	}
}

// Size returns the current size of the spent set.
func (ss *SpentSet) Size() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return len(ss.entries)
}

// persist writes the spent set to disk atomically.
func (ss *SpentSet) persist() {
	if ss.filePath == "" {
		return
	}

	entries := make([]SpentEntry, 0, len(ss.entries))
	for _, entry := range ss.entries {
		entries = append(entries, entry)
	}

	data := spentSetFile{
		Version: 1,
		Updated: time.Now(),
		Entries: entries,
	}

	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("[utxo] ERROR: failed to marshal spent set: %v", err)
		return
	}

	// Atomic write: write to temp file, then rename
	tmpFile := ss.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, buf, 0600); err != nil {
		log.Printf("[utxo] ERROR: failed to write spent set temp file: %v", err)
		return
	}

	if err := os.Rename(tmpFile, ss.filePath); err != nil {
		log.Printf("[utxo] ERROR: failed to rename spent set file: %v", err)
		os.Remove(tmpFile)
	}
}

// load reads the spent set from disk on startup.
//
// Confirmed entries older than 2 hours and expired reservations are
// discarded. Unconfirmed broadcast entries are kept whatever their age: an
// earlier version dropped them after 2 hours, so a restart after a slow
// confirmation re-exposed the inputs of a transaction that was still in the
// mempool, and the next withdrawal double-spent them.
func (ss *SpentSet) load() {
	// Ensure directory exists
	dir := filepath.Dir(ss.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[utxo] WARNING: failed to create spent set directory %s: %v", dir, err)
		return
	}

	data, err := os.ReadFile(ss.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[utxo] Spent set file not found (first run) — starting fresh")
		} else {
			log.Printf("[utxo] WARNING: failed to read spent set file: %v", err)
		}
		return
	}

	var file spentSetFile
	if err := json.Unmarshal(data, &file); err != nil {
		log.Printf("[utxo] WARNING: failed to parse spent set file: %v (starting fresh)", err)
		return
	}

	now := time.Now()
	cutoff := now.Add(-2 * time.Hour)
	loaded := 0
	expired := 0

	for _, entry := range file.Entries {
		if (entry.Confirmed && entry.SpentAt.Before(cutoff)) || entry.expired(now) {
			expired++
			continue
		}
		key := SpentKey{entry.TxID, entry.Vout}
		ss.entries[key] = entry
		loaded++
	}

	log.Printf("[utxo] Spent set loaded: %d entries from disk (%d expired, %d active)",
		len(file.Entries), expired, loaded)
}

// CoinSelector provides UTXO coin selection algorithms.
// It works with the SpentSet to prevent re-selection of spent UTXOs.
type CoinSelector struct {
	SpentSet *SpentSet

	// MinUTXOValue is the minimum UTXO value for coin selection.
	// UTXOs below this value are skipped (except in consolidation).
	// SOQ-ARCH-003: Spending sub-threshold UTXOs costs more in TX weight than they're worth.
	MinUTXOValue int64
}

// NewCoinSelector creates a new coin selector with the given spent set.
func NewCoinSelector(spentSet *SpentSet) *CoinSelector {
	return &CoinSelector{
		SpentSet: spentSet,
	}
}

// SelectUTXOs selects UTXOs to cover the target amount using largest-first strategy.
// Returns selected UTXOs and the total value. Filters by the specified addresses.
//
// This implements the same algorithm as soq-signer's production coin selection,
// with all defensive filters:
//   - Skips SpentPending UTXOs
//   - Skips UTXOs in the persistent spent set (DL-SIGNER-SPENT-TRACKING)
//   - Skips non-SOQ asset types (SOQ-ARCH-001)
//   - Skips UTXOs below MinUTXOValue (SOQ-ARCH-003 dust filter)
//   - Enforces MaxInputsPerTX hard cap
func (cs *CoinSelector) SelectUTXOs(
	utxos []types.UTXO,
	targetAmount int64,
	minConf int,
	tipHeight int64,
	allowedAddresses []string,
) ([]types.UTXO, int64, error) {
	return cs.selectByAssetType(utxos, targetAmount, minConf, tipHeight, allowedAddresses, types.AssetTypeSOQ)
}

// SelectUTXOsByAssetType selects UTXOs of a specific asset type.
func (cs *CoinSelector) SelectUTXOsByAssetType(
	utxos []types.UTXO,
	targetAmount int64,
	minConf int,
	tipHeight int64,
	allowedAddresses []string,
	assetType uint8,
) ([]types.UTXO, int64, error) {
	return cs.selectByAssetType(utxos, targetAmount, minConf, tipHeight, allowedAddresses, assetType)
}

func (cs *CoinSelector) selectByAssetType(
	utxos []types.UTXO,
	targetAmount int64,
	minConf int,
	tipHeight int64,
	allowedAddresses []string,
	assetType uint8,
) ([]types.UTXO, int64, error) {
	// Build allowed address set for O(1) lookups
	var allowed map[string]bool
	if len(allowedAddresses) > 0 {
		allowed = make(map[string]bool, len(allowedAddresses))
		for _, a := range allowedAddresses {
			allowed[a] = true
		}
	}

	// Collect all spendable UTXOs matching criteria
	var candidates []types.UTXO
	for _, u := range utxos {
		if u.SpentPending {
			continue
		}
		// DL-SIGNER-SPENT-TRACKING: Skip UTXOs in the persistent spent set.
		if cs.SpentSet != nil && cs.SpentSet.IsSpent(u.TxID, u.Vout) {
			continue
		}
		if u.AssetType != assetType {
			continue
		}
		if allowed != nil && !allowed[u.Address] {
			continue
		}
		if u.Height > 0 && (tipHeight-u.Height+1) >= int64(minConf) {
			// SOQ-ARCH-003: Skip UTXOs below MinUTXOValue (dust filter).
			if cs.MinUTXOValue > 0 && u.Value < cs.MinUTXOValue {
				continue
			}
			candidates = append(candidates, u)
		}
	}

	// Sort by value descending (largest first for fewer inputs = smaller TX)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Value > candidates[j].Value
	})

	var selected []types.UTXO
	var totalValue int64

	for _, u := range candidates {
		selected = append(selected, u)
		totalValue += u.Value

		if totalValue >= targetAmount {
			return selected, totalValue, nil
		}

		// Hard cap: never exceed MaxInputsPerTX
		if len(selected) >= MaxInputsPerTX {
			return selected, totalValue, fmt.Errorf("%w: selected %d UTXOs totaling %d sat, need %d sat",
				ErrInputLimitReached, len(selected), totalValue, targetAmount)
		}
	}

	return nil, totalValue, fmt.Errorf("insufficient funds: have %d, need %d", totalValue, targetAmount)
}

// SelectSmallestUTXOs selects up to maxCount of the smallest confirmed UTXOs.
// Used for UTXO consolidation — merging many small fragments into one large UTXO.
// Unlike SelectUTXOs, this does NOT apply MinUTXOValue filtering (that's the point).
func (cs *CoinSelector) SelectSmallestUTXOs(
	utxos []types.UTXO,
	maxCount int,
	minConf int,
	tipHeight int64,
	allowedAddresses []string,
) ([]types.UTXO, int64, error) {
	var allowed map[string]bool
	if len(allowedAddresses) > 0 {
		allowed = make(map[string]bool, len(allowedAddresses))
		for _, a := range allowedAddresses {
			allowed[a] = true
		}
	}

	var candidates []types.UTXO
	for _, u := range utxos {
		if u.SpentPending {
			continue
		}
		if cs.SpentSet != nil && cs.SpentSet.IsSpent(u.TxID, u.Vout) {
			continue
		}
		if u.AssetType != types.AssetTypeSOQ {
			continue
		}
		if allowed != nil && !allowed[u.Address] {
			continue
		}
		if u.Height > 0 && (tipHeight-u.Height+1) >= int64(minConf) {
			candidates = append(candidates, u)
		}
	}

	// Sort by value ascending (smallest first for consolidation)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Value < candidates[j].Value
	})

	if len(candidates) > maxCount {
		candidates = candidates[:maxCount]
	}

	var totalValue int64
	for _, u := range candidates {
		totalValue += u.Value
	}

	if len(candidates) == 0 {
		return nil, 0, fmt.Errorf("no confirmed UTXOs available for consolidation")
	}

	return candidates, totalValue, nil
}

// shortID truncates an identifier for logging without panicking on short input.
// Log formatting must never be able to crash the caller: these helpers sit on
// error paths, and a panic there replaces a handled error with process death.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
