// Package deposit credits incoming payments the way an exchange must: nothing
// is credited on the word of the indexer alone.
//
// ElectrumX is fast and convenient, and it is also a single process the
// exchange does not control. A compromised or mis-pointed indexer, or anyone
// on the path to a plaintext one, can report a confirmed UTXO that does not
// exist, a wrong value, a wrong height, or a different address. So before a
// deposit is credited, Monitor asks the exchange's own node for that exact
// output (gettxout, confirmed only) and requires the value, the destination
// script and the confirmation count to agree. A disagreement is never
// credited and is raised through OnAlert, because it means either the indexer
// is lying or the node is behind, and both need a human.
//
// Monitor also refuses to credit while the node is in initial block download
// (the finality horizon is not enforced then), leaves immature coinbase
// outputs alone, and re-checks every credited-but-not-final outpoint on each
// scan so a reorg that removes a credited deposit is alarmed rather than
// missed.
package deposit

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Cache is the indexer-side view: what the electrumx.Client provides.
type Cache interface {
	GetUTXOs(addr string) []types.UTXO
	LastRefresh() (time.Time, error)
}

// Node is the exchange's own soqucoind. *rpc.Client satisfies it.
type Node interface {
	RequireSynced() error
	GetBlockCount() (int64, error)
	GetTxOut(txid string, vout uint32, includeMempool bool) (*rpc.TxOut, error)
}

// Ledger is the exchange's book. Credit must be idempotent on (txid, vout):
// the same outpoint will be presented on every scan until it is final, and
// after a restart. Pending returns credited outpoints that have not yet been
// reported final, so Monitor can re-verify them.
type Ledger interface {
	Credit(d Deposit) error
	IsCredited(txid string, vout uint32) (bool, error)
	Pending() ([]Deposit, error)
	MarkFinal(txid string, vout uint32) error
}

// Deposit is a credited output.
type Deposit struct {
	TxID          string
	Vout          uint32
	Address       string
	Value         int64 // shors
	Height        int64
	Confirmations int64
	CreditedAt    time.Time
}

// Policy returns the confirmations required before a deposit of this value is
// credited. Exchanges scale it with value; MaxReorgDepth+1 is final.
type Policy func(value int64) int64

// Monitor scans tracked addresses and credits verified deposits.
type Monitor struct {
	Cache     Cache
	Node      Node
	Ledger    Ledger
	Addresses func() []string // the deposit addresses to scan
	Required  Policy

	// MaxCacheAge bounds how stale the indexer cache may be before a scan is
	// skipped entirely (default 5 minutes). A stale cache is an outage, not
	// "no deposits".
	MaxCacheAge time.Duration

	// OnAlert receives every condition a human should see: indexer and node
	// disagreeing, a credited deposit that vanished, a syncing node. It is
	// never optional in production; a nil OnAlert only logs.
	OnAlert func(kind AlertKind, msg string)

	now func() time.Time
}

// AlertKind classifies alerts.
type AlertKind string

const (
	AlertNodeSyncing     AlertKind = "node_syncing"     // crediting paused; node not caught up
	AlertCacheStale      AlertKind = "cache_stale"      // indexer has not refreshed; crediting paused
	AlertIndexerMismatch AlertKind = "indexer_mismatch" // indexer and node disagree on an output; NOT credited
	AlertDepositVanished AlertKind = "deposit_vanished" // a credited, non-final output is gone from the node
	AlertLedgerError     AlertKind = "ledger_error"     // the exchange's own book returned an error
)

var (
	// ErrPaused is returned by Scan when nothing was credited because the node
	// or the indexer is not in a state that allows safe crediting.
	ErrPaused = errors.New("deposit: crediting paused")
)

func (m *Monitor) alert(kind AlertKind, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if m.OnAlert != nil {
		m.OnAlert(kind, msg)
	}
}

func (m *Monitor) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Monitor) maxCacheAge() time.Duration {
	if m.MaxCacheAge > 0 {
		return m.MaxCacheAge
	}
	return 5 * time.Minute
}

// Scan performs one pass. It returns the deposits credited in this pass, or
// ErrPaused (wrapped with the reason) when crediting was not safe. Errors from
// the node are returned as-is; the pass credits nothing in that case.
func (m *Monitor) Scan() ([]Deposit, error) {
	// 1. The node must have caught up. During initial block download the
	//    finality horizon is not enforced and gettxout is incomplete.
	if err := m.Node.RequireSynced(); err != nil {
		m.alert(AlertNodeSyncing, "%v", err)
		return nil, fmt.Errorf("%w: %v", ErrPaused, err)
	}
	// 2. The indexer must be fresh. A cache that stopped refreshing looks
	//    exactly like "no new deposits".
	at, refreshErr := m.Cache.LastRefresh()
	if refreshErr != nil || at.IsZero() || m.clock().Sub(at) > m.maxCacheAge() {
		m.alert(AlertCacheStale, "indexer last refreshed %v, error %v", at, refreshErr)
		return nil, fmt.Errorf("%w: indexer cache stale (last %v, err %v)", ErrPaused, at, refreshErr)
	}
	tip, err := m.Node.GetBlockCount()
	if err != nil {
		return nil, err
	}

	// 3. Re-verify everything credited but not yet final.
	if err := m.recheckPending(tip); err != nil {
		return nil, err
	}

	// 4. Credit new deposits the node agrees with.
	var credited []Deposit
	for _, addr := range m.Addresses() {
		wantScript, err := address.ScriptFor(addr)
		if err != nil {
			// Only v1 addresses are deposit addresses (address.Decode enforces it).
			m.alert(AlertLedgerError, "deposit address %s is not a valid v1 address: %v", addr, err)
			continue
		}
		wantHex := hex.EncodeToString(wantScript)
		for _, u := range m.Cache.GetUTXOs(addr) {
			if u.Height <= 0 || u.AssetType != types.AssetTypeSOQ {
				continue // unconfirmed (or a lying indexer's negative height), or not SOQ
			}
			confs := tip - u.Height + 1
			if confs < m.Required(u.Value) {
				continue
			}
			done, err := m.Ledger.IsCredited(u.TxID, u.Vout)
			if err != nil {
				m.alert(AlertLedgerError, "IsCredited %s:%d: %v", u.TxID, u.Vout, err)
				return credited, err
			}
			if done {
				continue
			}
			d, ok := m.verifyWithNode(addr, wantHex, u, confs)
			if !ok {
				continue
			}
			if err := m.Ledger.Credit(d); err != nil {
				m.alert(AlertLedgerError, "Credit %s:%d: %v", u.TxID, u.Vout, err)
				return credited, err
			}
			credited = append(credited, d)
		}
	}
	return credited, nil
}

// verifyWithNode asks the exchange's own node for the exact output and
// requires agreement on existence, value, destination script, confirmation
// depth and coinbase maturity. Any disagreement is alarmed and not credited.
func (m *Monitor) verifyWithNode(addr, wantHex string, u types.UTXO, confs int64) (Deposit, bool) {
	out, err := m.Node.GetTxOut(u.TxID, u.Vout, false)
	if err != nil {
		m.alert(AlertIndexerMismatch, "%s:%d for %s: node lookup failed: %v", u.TxID, u.Vout, addr, err)
		return Deposit{}, false
	}
	switch {
	case out == nil:
		m.alert(AlertIndexerMismatch, "%s:%d for %s: indexer reports a confirmed output the node does not have (or it is already spent)", u.TxID, u.Vout, addr)
	case out.ValueShors() != u.Value:
		m.alert(AlertIndexerMismatch, "%s:%d for %s: indexer value %d, node value %d", u.TxID, u.Vout, addr, u.Value, out.ValueShors())
	case !strings.EqualFold(out.ScriptPubKey.Hex, wantHex):
		m.alert(AlertIndexerMismatch, "%s:%d: indexer attributes it to %s but the node's script is %s", u.TxID, u.Vout, addr, out.ScriptPubKey.Hex)
	case out.Confirmations < m.Required(u.Value):
		m.alert(AlertIndexerMismatch, "%s:%d for %s: indexer depth %d, node depth %d, required %d", u.TxID, u.Vout, addr, confs, out.Confirmations, m.Required(u.Value))
	case out.Coinbase && out.Confirmations < types.CoinbaseMaturity:
		// Real, but not spendable yet; credit when mature. Not an alarm.
	default:
		return Deposit{
			TxID: u.TxID, Vout: u.Vout, Address: addr, Value: u.Value, Height: u.Height,
			Confirmations: out.Confirmations, CreditedAt: m.clock(),
		}, true
	}
	return Deposit{}, false
}

// recheckPending confirms every credited, non-final deposit still exists on
// the node with at least its credited depth, and marks it final past the
// horizon. A vanished output is a reorg or a lie; either way the exchange's
// book now holds a credit with nothing behind it.
func (m *Monitor) recheckPending(tip int64) error {
	pending, err := m.Ledger.Pending()
	if err != nil {
		m.alert(AlertLedgerError, "Pending: %v", err)
		return err
	}
	for _, d := range pending {
		out, err := m.Node.GetTxOut(d.TxID, d.Vout, false)
		if err != nil {
			return err
		}
		if out == nil {
			// gettxout is nil for a SPENT output too. An exchange sweeping its
			// deposits will hit this; a spend of a real deposit is fine. The
			// ledger decides: it knows whether it spent the output itself.
			m.alert(AlertDepositVanished, "credited deposit %s:%d (%d shors to %s) is no longer in the node's UTXO set; verify it was spent by you and not reorganised away", d.TxID, d.Vout, d.Value, d.Address)
			continue
		}
		if out.Confirmations > types.MaxReorgDepth {
			if err := m.Ledger.MarkFinal(d.TxID, d.Vout); err != nil {
				m.alert(AlertLedgerError, "MarkFinal %s:%d: %v", d.TxID, d.Vout, err)
				return err
			}
		}
	}
	return nil
}
