// Package utxo provides UTXO coin selection and persistent spent set tracking.
//
// This package was extracted from the canonical soq-signer service (v1.0.0-alpha)
// which has been running in production since May 2026. It incorporates all
// defenses from DL-SIGNER-SPENT-TRACKING:
//
//   - Defense 11: gettxout pre-verification before signing
//   - Change becomes an input once it has confirmed and the indexer reports it
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
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// MaxInputsPerTX is the hard cap on UTXO inputs per transaction.
//
// Each input carries a 2,420-byte ML-DSA-44 signature and a 1,312-byte public
// key, about 3,900 WU. A signed 80-input, 2-output transaction measures
// 312,786 WU (docs/EXCHANGE_INTEGRATION.md, Transaction Size), 39% of the
// node's MAX_STANDARD_TX_WEIGHT of 800,000 WU (src/policy/policy.h:39, the
// same in every node release from v1.1.0 on). The cap is an operational limit
// kept from the period when the node's limit was 400,000 WU and transactions
// of 200 inputs were rejected; raising it is a policy choice, not a protocol
// change.
const MaxInputsPerTX = 80

// ErrNoCandidates is returned by SelectSmallestUTXOs when no output meets the
// confirmation requirement; there is nothing to consolidate, which a scheduled
// consolidation treats as a normal outcome.
var ErrNoCandidates = errors.New("utxo: no confirmed UTXOs available for consolidation")

// ErrInputLimitReached is returned when SelectUTXOs hits MaxInputsPerTX before
// satisfying the target amount. The caller receives a partial selection and
// can proceed with a reduced payment.
var ErrInputLimitReached = errors.New("input limit reached")

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

// ErrPersist is returned when the spent set could not be written to its file.
// The in-memory set is still correct for this process; a restart would not
// see the change. Reserve rolls its reservation back and reserves nothing;
// MarkBroadcast keeps the entries, because the transaction is already out
// and the process must go on refusing those inputs.
var ErrPersist = errors.New("utxo: spent set could not be written")

// ErrSpentSetUnreadable is returned by OpenSpentSet when the file exists but
// cannot be read or parsed. Starting with an empty set in that case would
// forget every unconfirmed spend and re-expose those inputs to selection.
var ErrSpentSetUnreadable = errors.New("utxo: spent set file exists but cannot be read")

// ReservedPrefix marks reservation entries in SpentInTx.
const ReservedPrefix = "reserved:"

func (e SpentEntry) reserved() bool {
	return !e.ExpiresAt.IsZero() && strings.HasPrefix(e.SpentInTx, ReservedPrefix)
}
func (e SpentEntry) expired(now time.Time) bool {
	return e.reserved() && now.After(e.ExpiresAt)
}

// sentByAnother reports whether e records an unconfirmed send that neither
// intentID nor broadcastTxID owns. An empty intentID owns nothing, as in
// Reserve.
func (e SpentEntry) sentByAnother(broadcastTxID, intentID string) bool {
	if e.reserved() || e.Confirmed || e.SpentInTx == broadcastTxID {
		return false
	}
	return intentID == "" || e.IntentID != intentID
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
	log      *slog.Logger
}

// NewSpentSet creates a spent set. With an empty filePath it is in-memory
// only. With a filePath it loads from disk and persists changes, but a file
// that exists and cannot be read is logged and the set starts EMPTY, which
// forgets every unconfirmed spend. Production code opens a file-backed set
// with OpenSpentSet, which refuses that case. logger receives load and
// persist events; nil discards.
func NewSpentSet(filePath string, logger *slog.Logger) *SpentSet {
	ss := &SpentSet{
		entries:  make(map[SpentKey]SpentEntry),
		filePath: filePath,
		log:      logutil.Or(logger),
	}
	if filePath != "" {
		if err := ss.load(); err != nil {
			ss.log.Warn("spent set unreadable, starting empty", "path", filePath, "err", err)
		}
	}
	return ss
}

// OpenSpentSet opens a file-backed spent set. A missing file is a first run
// and is fine; a file that exists but cannot be read or parsed, or a
// directory that cannot be created, is ErrSpentSetUnreadable, and the caller
// must not start paying out on an empty set. logger receives load and
// persist events; nil discards.
func OpenSpentSet(filePath string, logger *slog.Logger) (*SpentSet, error) {
	if filePath == "" {
		return nil, errors.New("utxo: OpenSpentSet needs a file path; use NewSpentSet(\"\", nil) for an in-memory set")
	}
	ss := &SpentSet{
		entries:  make(map[SpentKey]SpentEntry),
		filePath: filePath,
		log:      logutil.Or(logger),
	}
	if err := ss.load(); err != nil {
		return nil, err
	}
	return ss, nil
}

// Reserve holds the given inputs for a withdrawal that is about to be built,
// so a concurrent withdrawal cannot select them. All-or-nothing: if any input
// is already reserved (and not expired) or spent by another withdrawal,
// nothing is reserved and ErrAlreadyReserved names the input. The reservation
// lasts ttl; MarkBroadcast converts it into a permanent spent entry, Release
// drops it.
//
// Entries the same withdrawal already holds are accepted whatever their kind.
// Its own reservation is renewed. Its own broadcast entry is left as it is:
// the transaction is out, so the entry must not become one that expires. That
// case is a retry after a send whose reply was lost and whose earlier attempt
// had succeeded without the record of it landing; reading it as another
// withdrawal taking the inputs would name a conflict that does not exist.
// An empty intentID owns nothing: entries MarkBroadcast wrote without an
// intent carry an empty id too, and a caller reserving without one must
// still be refused those inputs.
func (ss *SpentSet) Reserve(inputs []types.UTXO, intentID string, ttl time.Duration) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	now := time.Now()
	for _, u := range inputs {
		key := SpentKey{u.TxID, u.Vout}
		if e, exists := ss.entries[key]; exists && !e.expired(now) {
			if intentID != "" && e.IntentID == intentID {
				continue // this withdrawal's own entry: renewed below, or kept if it records a send
			}
			return fmt.Errorf("%w: %s:%d (%s)", ErrAlreadyReserved, u.TxID, u.Vout, e.SpentInTx)
		}
	}
	previous := make(map[SpentKey]*SpentEntry, len(inputs))
	for _, u := range inputs {
		key := SpentKey{u.TxID, u.Vout}
		if _, seen := previous[key]; seen {
			continue // the same outpoint twice: keep the state from before the first write
		}
		if e, exists := ss.entries[key]; exists {
			if intentID != "" && e.IntentID == intentID && !e.reserved() {
				continue // this withdrawal's own record of a send: permanent, never replaced by a reservation
			}
			e := e
			previous[key] = &e
		} else {
			previous[key] = nil
		}
		ss.entries[key] = SpentEntry{
			TxID:      u.TxID,
			Vout:      u.Vout,
			SpentInTx: ReservedPrefix + intentID,
			SpentAt:   now,
			ExpiresAt: now.Add(ttl),
			IntentID:  intentID,
		}
	}
	if len(previous) == 0 {
		return nil // every input is already recorded as sent by this withdrawal
	}
	if err := ss.persist(); err != nil {
		// All-or-nothing includes durability: a reservation this process
		// would forget on restart is not a reservation. Put back what was
		// there (an own expired reservation, or nothing) and report.
		for key, prev := range previous {
			if prev == nil {
				delete(ss.entries, key)
			} else {
				ss.entries[key] = *prev
			}
		}
		return err
	}
	return nil
}

// Release drops the reservations held by intentID. Broadcast entries are
// never released here: once a transaction is out, its inputs are spent until
// the chain says otherwise.
func (ss *SpentSet) Release(intentID string) error {
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
		return ss.persist()
	}
	return nil
}

// Forget drops every entry intentID owns, reservations and broadcast entries
// alike, and writes the file once: the one call that removes a broadcast
// entry before the chain confirms it, for withdraw.Engine.Abandon after its
// node checks, and for Recover when that call did not land. A write that
// fails puts the entries back, as Reserve does, since the file still holds
// them; one that landed without its durability confirmed keeps the drop. An
// empty id is refused; it would drop every entry written without one.
func (ss *SpentSet) Forget(intentID string) error {
	if intentID == "" {
		return errors.New("utxo: Forget needs an intent id")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	dropped := map[SpentKey]SpentEntry{}
	for key, e := range ss.entries {
		if e.IntentID == intentID {
			dropped[key] = e
			delete(ss.entries, key)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	if err := ss.persist(); err != nil && !errors.Is(err, atomicfile.ErrWrittenNotDurable) {
		for key, e := range dropped {
			ss.entries[key] = e
		}
		return err
	} else if err != nil {
		return err
	}
	ss.log.Warn("spent set: entries of an abandoned withdrawal dropped", "intent", intentID, "dropped", len(dropped))
	return nil
}

// SentIntents returns the id of every withdrawal that owns an unconfirmed
// broadcast entry, each once, sorted; entries without an id do not appear.
func (ss *SpentSet) SentIntents() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range ss.entries {
		if !e.reserved() && !e.Confirmed && e.IntentID != "" {
			seen[e.IntentID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ReservedIntents returns the id of every withdrawal that holds a reservation
// in the set, expired or not, each once, sorted. Broadcast entries are not
// reservations and do not appear. withdraw.Engine.Recover uses it to release
// reservations whose intent has nothing built.
func (ss *SpentSet) ReservedIntents() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range ss.entries {
		if e.reserved() && !seen[e.IntentID] {
			seen[e.IntentID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MarkBroadcast records that the given UTXOs were spent in a broadcast TX.
// This is the PRIMARY defense against stale UTXO re-selection. Reservations
// on these inputs become permanent spent entries.
func (ss *SpentSet) MarkBroadcast(inputs []types.UTXO, broadcastTxID string) error {
	return ss.markBroadcast(inputs, broadcastTxID, "")
}

// MarkBroadcastFor is MarkBroadcast that also records the withdrawal id.
func (ss *SpentSet) MarkBroadcastFor(inputs []types.UTXO, broadcastTxID, intentID string) error {
	return ss.markBroadcast(inputs, broadcastTxID, intentID)
}

// markBroadcast is all-or-nothing: an input another withdrawal has recorded
// as spent in an unconfirmed transaction refuses the whole call with
// ErrAlreadyReserved and nothing is written, since writing over it would
// attribute the input to the transaction that cannot confirm.
func (ss *SpentSet) markBroadcast(inputs []types.UTXO, broadcastTxID, intentID string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	for _, u := range inputs {
		key := SpentKey{u.TxID, u.Vout}
		if e, exists := ss.entries[key]; exists && e.sentByAnother(broadcastTxID, intentID) {
			return fmt.Errorf("%w: %s:%d is spent in %s by withdrawal %q", ErrAlreadyReserved, u.TxID, u.Vout, e.SpentInTx, e.IntentID)
		}
	}
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

	ss.log.Info("spent set: inputs marked spent", "inputs", len(inputs), "txid", broadcastTxID, "tracked", len(ss.entries))

	// The entries stay whatever persist says: the transaction is out and
	// this process must keep refusing its inputs. The caller learns that a
	// restart would not.
	return ss.persist()
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
func (ss *SpentSet) ConfirmSpent(txid string, vout uint32) error {
	return ss.ConfirmSpentAll([]types.UTXO{{TxID: txid, Vout: vout}})
}

// ConfirmSpentAll marks every listed input confirmed and writes the file once.
// Use it for an intent's inputs together rather than ConfirmSpent per input.
func (ss *SpentSet) ConfirmSpentAll(inputs []types.UTXO) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	changed := false
	for _, u := range inputs {
		key := SpentKey{u.TxID, u.Vout}
		if entry, exists := ss.entries[key]; exists && !entry.Confirmed {
			entry.Confirmed = true
			ss.entries[key] = entry
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return ss.persist()
}

// Prune removes confirmed entries older than 1 hour and expired
// reservations. Unconfirmed broadcast entries are never pruned by the clock:
// the transaction that spends them is still in flight until the chain
// confirms it, however long that takes.
// Should be called periodically (e.g., after each UTXO refresh).
func (ss *SpentSet) Prune() error {
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
		ss.log.Info("spent set: pruned confirmed entries", "pruned", pruned, "remaining", len(ss.entries))
		return ss.persist()
	}
	return nil
}

// Size returns the current size of the spent set.
func (ss *SpentSet) Size() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return len(ss.entries)
}

// persist writes the spent set to disk atomically.
// writeFile is atomicfile.WriteFile, a variable so a test can make the write
// fail after the rename and see what Reserve does with a reservation that is
// in the file and not known to be durable.
var writeFile = atomicfile.WriteFile

func (ss *SpentSet) persist() error {
	if ss.filePath == "" {
		return nil
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
		return fmt.Errorf("%w: marshal: %v", ErrPersist, err)
	}

	// Written, synced and renamed into place, then the directory synced: a
	// spend marked just before a power loss is on disk when persist returns,
	// so a restart cannot re-select an input of a transaction already sent.
	if err := writeFile(ss.filePath, buf, 0600); err != nil {
		return fmt.Errorf("%w: %w", ErrPersist, err)
	}
	return nil
}

// load reads the spent set from disk on startup.
//
// Confirmed entries older than 2 hours are discarded. Unconfirmed broadcast
// entries are kept whatever their age: an earlier version dropped them after
// 2 hours, so a restart after a slow confirmation re-exposed the inputs of a
// transaction that was still in the mempool, and the next withdrawal
// double-spent them. Reservations are kept whether or not they have expired:
// withdraw.Engine.Recover decides what each one was, renewing a Built
// intent's and releasing an orphan's, and an earlier version dropped the
// expired ones here, so after a forward step of the clock Recover found
// nothing to decide and nothing was logged. Reserve treats an expired entry
// as free and Prune drops it on the periodic path, so keeping it changes
// neither the backstop nor the file's growth.
func (ss *SpentSet) load() error {
	// Ensure directory exists
	dir := filepath.Dir(ss.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("%w: create directory %s: %v", ErrSpentSetUnreadable, dir, err)
	}

	data, err := os.ReadFile(ss.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			ss.log.Info("spent set: no file yet, first run", "path", ss.filePath)
			return nil
		}
		return fmt.Errorf("%w: %v", ErrSpentSetUnreadable, err)
	}

	var file spentSetFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("%w: parse %s: %v", ErrSpentSetUnreadable, ss.filePath, err)
	}

	now := time.Now()
	cutoff := now.Add(-2 * time.Hour)
	loaded := 0
	dropped := 0
	expired := 0

	for _, entry := range file.Entries {
		if entry.Confirmed && entry.SpentAt.Before(cutoff) {
			dropped++
			continue
		}
		if entry.expired(now) {
			expired++ // kept for Recover
		}
		key := SpentKey{entry.TxID, entry.Vout}
		ss.entries[key] = entry
		loaded++
	}

	ss.log.Info("spent set loaded", "path", ss.filePath, "entries", len(file.Entries), "dropped_confirmed", dropped, "expired_reservations_kept", expired, "active", loaded)
	return nil
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

// SelectSmallestUTXOs selects up to maxCount of the smallest native-SOQ
// outputs with at least minConf confirmations at tipHeight, ascending by
// value, for consolidation into one output (tx.BuildSweepTransaction). Unlike
// SelectUTXOs it applies no MinUTXOValue filter: the small outputs are the
// point. Outputs in the spent set are skipped. It
// returns ErrNoCandidates when nothing qualifies.
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
		return nil, 0, ErrNoCandidates
	}

	return candidates, totalValue, nil
}
