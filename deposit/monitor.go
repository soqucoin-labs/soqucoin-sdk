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
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Cache is the indexer-side view: what the electrumx.Client provides.
type Cache interface {
	GetUTXOs(addr string) []types.UTXO
	LastRefresh() (time.Time, error)
}

// AddressFreshness is the optional part of Cache that reports, per tracked
// address, when it last refreshed successfully; *electrumx.Client implements
// it. When the Cache has it, Scan judges staleness address by address: the
// addresses the indexer has not answered for within MaxCacheAge are skipped
// and alarmed, the others are credited, and Scan pauses only when every
// address is stale. Without it one failed address in a pass makes the whole
// pass stale and nothing is credited until a pass succeeds for all of them.
type AddressFreshness interface {
	LastRefreshOf(addr string) (time.Time, error)
}

// Node is the exchange's own soqucoind. *rpc.Client satisfies it.
type Node interface {
	RequireSynced(ctx context.Context) error
	GetBlockCount(ctx context.Context) (int64, error)
	GetTxOut(ctx context.Context, txid string, vout uint32, includeMempool bool) (*rpc.TxOut, error)
}

// Ledger is the exchange's book. Credit must be idempotent on (txid, vout):
// the same outpoint will be presented on every scan until it is final, and
// after a restart. Pending returns credited outpoints that have not yet been
// reported final, so Monitor can re-verify them.
//
// Pending must leave out outputs the exchange has spent itself, such as a
// deposit swept to the hot wallet before it was final. Monitor asks the node
// for each pending output, and the node answers the same for an output the
// exchange spent and for one a reorganisation removed: it is gone. Monitor
// cannot tell the two apart, so a swept output still in Pending raises
// AlertDepositVanished on every scan. Record the sweep in the ledger and
// exclude the output, or mark it final when the sweep transaction is final.
//
// IsCredited and Pending receive Scan's context. Credit and MarkFinal receive
// one that does not end when Scan's does (context.WithoutCancel, which also
// carries no deadline): they record what the node has already confirmed, and
// a record cut short by the caller is a deposit the book has that no Scan
// will return. A database-backed ledger bounds every method with its own
// timeout and does not rely on the context for that.
type Ledger interface {
	Credit(ctx context.Context, d Deposit) error
	IsCredited(ctx context.Context, txid string, vout uint32) (bool, error)
	Pending(ctx context.Context) ([]Deposit, error)
	MarkFinal(ctx context.Context, txid string, vout uint32) error
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
	Addresses func(ctx context.Context) []string // the deposit addresses to scan
	Required  Policy

	// Network supplies the chain's coinbase maturity: a mined-to deposit is
	// credited only once consensus lets it be spent. The zero value is
	// types.Mainnet. When it is set and Node can report its chain (as
	// *rpc.Client does through RequireChain), Scan refuses to credit anything
	// while the node serves another chain, so the maturity applied and the
	// node consulted cannot drift apart.
	Network types.Network

	// MaxCacheAge bounds how stale the indexer cache may be before a scan is
	// skipped entirely (default 5 minutes). A stale cache is an outage, not
	// "no deposits".
	MaxCacheAge time.Duration

	// OnAlert receives every condition a human should see: indexer and node
	// disagreeing, a credited deposit that vanished, a syncing node. It is
	// never optional in production; a nil OnAlert only logs, at warn, through
	// Logger.
	OnAlert func(kind AlertKind, msg string)

	// Logger receives the alerts when OnAlert is nil. nil discards.
	Logger *slog.Logger

	now func() time.Time
}

// AlertKind classifies alerts.
type AlertKind string

const (
	AlertNodeSyncing     AlertKind = "node_syncing"     // crediting paused; node not caught up
	AlertNodeWrongChain  AlertKind = "node_wrong_chain" // node serves another chain than Network; crediting refused until the deployment is fixed
	AlertCacheStale      AlertKind = "cache_stale"      // indexer has not refreshed; crediting paused, or skipped for the stale addresses
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
		return
	}
	logutil.Or(m.Logger).Warn("deposit alert", "kind", string(kind), "msg", msg)
}

func (m *Monitor) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// network returns the chain parameters in force: Network when set, else
// mainnet. A hand-built Network without a maturity gets mainnet's, the
// largest, rather than zero, which would credit every coinbase at once.
func (m *Monitor) network() types.Network {
	n := m.Network
	if n.ChainID == "" {
		return types.Mainnet
	}
	if n.CoinbaseMaturity <= 0 {
		n.CoinbaseMaturity = types.Mainnet.CoinbaseMaturity
	}
	return n
}

// ended reports whether err is the context's own doing: the context has ended,
// or err is or wraps a context error. Such an error is returned from Scan as
// it is; it is never an alert, because the node and the ledger have said
// nothing about themselves.
func ended(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (m *Monitor) maxCacheAge() time.Duration {
	if m.MaxCacheAge > 0 {
		return m.MaxCacheAge
	}
	return 5 * time.Minute
}

// chainChecker is the optional part of Node that reports which chain the node
// serves; *rpc.Client implements it.
type chainChecker interface {
	RequireChain(ctx context.Context, chainID string) error
}

// Scan performs one pass. It returns the deposits credited in this pass, or
// ErrPaused (wrapped with the reason) when crediting was not safe. A node on
// the wrong chain is returned as rpc.ErrWrongChain, a permanent error, not as
// a pause. Other errors from the node are returned as-is; the pass credits
// nothing in that case. A context that ends mid-pass is returned as its own
// error, never as a pause and never as an alert: the deposits credited
// before it are credited and returned with it, the rest wait for the next
// pass.
func (m *Monitor) Scan(ctx context.Context) ([]Deposit, error) {
	// 1. The node must serve this Monitor's chain and must have caught up.
	//    During initial block download the finality horizon is not enforced
	//    and gettxout is incomplete.
	if err := m.Node.RequireSynced(ctx); err != nil {
		if errors.Is(err, rpc.ErrWrongChain) {
			m.alert(AlertNodeWrongChain, "%v", err)
			return nil, err
		}
		if ended(ctx, err) {
			// The caller ended the pass; the node has said nothing about
			// itself, so this is neither a pause nor an alert.
			return nil, err
		}
		m.alert(AlertNodeSyncing, "%v", err)
		return nil, fmt.Errorf("%w: %v", ErrPaused, err)
	}
	if want := m.Network.ChainID; want != "" {
		if cc, ok := m.Node.(chainChecker); ok {
			if err := cc.RequireChain(ctx, want); err != nil {
				if errors.Is(err, rpc.ErrWrongChain) {
					m.alert(AlertNodeWrongChain, "%v", err)
				}
				return nil, err
			}
		}
	}
	// 2. The indexer must be fresh. A cache that stopped refreshing looks
	//    exactly like "no new deposits". A cache that reports freshness per
	//    address is judged address by address in step 4 instead.
	perAddress, _ := m.Cache.(AddressFreshness)
	if perAddress == nil {
		at, refreshErr := m.Cache.LastRefresh()
		if refreshErr != nil || at.IsZero() || m.clock().Sub(at) > m.maxCacheAge() {
			m.alert(AlertCacheStale, "indexer last refreshed %v, error %v", at, refreshErr)
			return nil, fmt.Errorf("%w: indexer cache stale (last %v, err %v)", ErrPaused, at, refreshErr)
		}
	}
	tip, err := m.Node.GetBlockCount(ctx)
	if err != nil {
		return nil, err
	}

	// 3. Re-verify everything credited but not yet final.
	if err := m.recheckPending(ctx, tip); err != nil {
		return nil, err
	}

	// 4. Credit new deposits the node agrees with, skipping the addresses the
	//    indexer has not refreshed within MaxCacheAge when it reports that.
	var credited []Deposit
	addrs := m.Addresses(ctx)
	if err := ctx.Err(); err != nil {
		// An address provider that answers an ended context with nothing must
		// not turn a cancelled pass into a clean "no deposits".
		return nil, err
	}
	var stale []string
	var staleErr error
	for _, addr := range addrs {
		if perAddress != nil {
			at, err := perAddress.LastRefreshOf(addr)
			if at.IsZero() || m.clock().Sub(at) > m.maxCacheAge() {
				if staleErr == nil {
					staleErr = err
				}
				stale = append(stale, addr)
				continue
			}
		}
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
			done, err := m.Ledger.IsCredited(ctx, u.TxID, u.Vout)
			if err != nil {
				if !ended(ctx, err) {
					m.alert(AlertLedgerError, "IsCredited %s:%d: %v", u.TxID, u.Vout, err)
				}
				return credited, err
			}
			if done {
				continue
			}
			d, ok, err := m.verifyWithNode(ctx, addr, wantHex, u, confs)
			if err != nil {
				return credited, err
			}
			if !ok {
				continue
			}
			// The node has confirmed the output; the record lands whether or
			// not the caller is still waiting, and a failure to record it is
			// a ledger alert whatever the caller's context says.
			if err := m.Ledger.Credit(context.WithoutCancel(ctx), d); err != nil {
				m.alert(AlertLedgerError, "Credit %s:%d: %v", u.TxID, u.Vout, err)
				return credited, err
			}
			credited = append(credited, d)
		}
	}
	if len(stale) > 0 {
		m.alert(AlertCacheStale, "indexer has not refreshed %d of %d addresses within %v (first %s, its last refresh error %v); their deposits wait",
			len(stale), len(addrs), m.maxCacheAge(), stale[0], staleErr)
		if len(stale) == len(addrs) {
			return nil, fmt.Errorf("%w: indexer cache stale for every address", ErrPaused)
		}
	}
	return credited, nil
}

// verifyWithNode asks the exchange's own node for the exact output and
// requires agreement on existence, value, destination script, confirmation
// depth and coinbase maturity. Any disagreement is alarmed and not credited.
// A lookup ended by the context is returned as that error, unalarmed: the
// node has not disagreed, it has not been asked.
func (m *Monitor) verifyWithNode(ctx context.Context, addr, wantHex string, u types.UTXO, confs int64) (Deposit, bool, error) {
	out, err := m.Node.GetTxOut(ctx, u.TxID, u.Vout, false)
	if err != nil {
		if ended(ctx, err) {
			return Deposit{}, false, err
		}
		m.alert(AlertIndexerMismatch, "%s:%d for %s: node lookup failed: %v", u.TxID, u.Vout, addr, err)
		return Deposit{}, false, nil
	}
	switch {
	case out == nil:
		m.alert(AlertIndexerMismatch, "%s:%d for %s: indexer reports a confirmed output the node does not have (or it is already spent)", u.TxID, u.Vout, addr)
	case out.Value != u.Value:
		m.alert(AlertIndexerMismatch, "%s:%d for %s: indexer value %d, node value %d", u.TxID, u.Vout, addr, u.Value, out.Value)
	case !strings.EqualFold(out.ScriptPubKey.Hex, wantHex):
		m.alert(AlertIndexerMismatch, "%s:%d: indexer attributes it to %s but the node's script is %s", u.TxID, u.Vout, addr, out.ScriptPubKey.Hex)
	case out.Confirmations < m.Required(u.Value):
		m.alert(AlertIndexerMismatch, "%s:%d for %s: indexer depth %d, node depth %d, required %d", u.TxID, u.Vout, addr, confs, out.Confirmations, m.Required(u.Value))
	case out.Coinbase && out.Confirmations < m.network().CoinbaseMaturity:
		// Real, but not spendable yet; credit when mature. Not an alarm.
	default:
		return Deposit{
			TxID: u.TxID, Vout: u.Vout, Address: addr, Value: u.Value, Height: u.Height,
			Confirmations: out.Confirmations, CreditedAt: m.clock(),
		}, true, nil
	}
	return Deposit{}, false, nil
}

// recheckPending confirms every credited, non-final deposit still exists on
// the node with at least its credited depth, and marks it final past the
// horizon. A vanished output is a reorg or a lie; either way the exchange's
// book now holds a credit with nothing behind it.
func (m *Monitor) recheckPending(ctx context.Context, tip int64) error {
	pending, err := m.Ledger.Pending(ctx)
	if err != nil {
		if !ended(ctx, err) {
			m.alert(AlertLedgerError, "Pending: %v", err)
		}
		return err
	}
	for _, d := range pending {
		out, err := m.Node.GetTxOut(ctx, d.TxID, d.Vout, false)
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
			if err := m.Ledger.MarkFinal(context.WithoutCancel(ctx), d.TxID, d.Vout); err != nil {
				m.alert(AlertLedgerError, "MarkFinal %s:%d: %v", d.TxID, d.Vout, err)
				return err
			}
		}
	}
	return nil
}
