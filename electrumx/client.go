// Package electrumx provides a production-hardened TCP client for ElectrumX servers.
//
// This client was extracted from the canonical soq-signer service (v1.0.0-alpha)
// which has been running in production since May 2026. It incorporates all
// battle-tested fixes:
//
//   - PF-018: 4MB read buffer for addresses with 18,000+ UTXOs
//   - F5: TCP keepalive at 30s to survive NAT/firewall timeouts
//   - PF-018b: Serialised writes, one reader, replies paired with calls by id
//   - Defense 12: Merge-based UTXO refresh that preserves the AssetType stamp
//   - Panic recovery: the refresher goroutine auto-restarts after crashes
//
// Usage:
//
//	client := electrumx.NewClient("host:50001", 10*time.Minute, logger)
//	if err := client.Connect(ctx); err != nil {
//	    return err
//	}
//	defer client.Stop()
//
//	client.TrackAddresses([]string{"ssq1abc..."})
//	client.Start(ctx)
//
//	utxos := client.GetUTXOs("ssq1abc...")
//	balance := client.GetBalance(1, tipHeight)
//
// The client subscribes to every tracked address and refreshes an address
// when the server reports its history changed; a full pass over every address
// runs on the reconcile interval given to NewClient as the safety net for a
// notification the server never sent; a ping on PingInterval keeps the session
// alive and advances the freshness of every subscribed address with no change
// pending. Freshness is read per address through LastRefreshOf, which
// deposit.Monitor uses to skip only the addresses the indexer has not
// answered for.
//
// Every method that reaches the server takes a context.Context first. Calls
// run concurrently on the one connection; a call whose context ends returns
// ctx.Err() at once, and its reply, if it arrives later, finds no waiter and
// is dropped. A call cut short while writing closes the connection, since the
// stream is no longer known to be at a line boundary, and the next call
// returns ErrNotConnected until Reconnect or Start restores it. Methods that
// read the cache (GetUTXOs, GetBalance, LastRefresh and the rest) take no
// context; they never block on the network.
//
// Copyright (c) 2025-2026 Soqucoin Labs Inc. MIT License.
package electrumx

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Client is a production-hardened TCP JSON-RPC client for ElectrumX.
//
// It maintains a persistent connection, tracks addresses by subscription,
// and provides battle-tested UTXO caching with merge-based refresh
// (Defense 12) that preserves the asset type stamped after node verification.
type Client struct {
	mu    sync.RWMutex
	utxos map[string][]types.UTXO // address -> UTXOs
	host  string

	// The connection: conn, reader and gen are guarded by connSem, a one-slot
	// semaphore rather than a mutex so that a caller waiting for it leaves
	// the queue when its own context ends. gen counts connections; a reader
	// goroutine and every pending call belong to one generation. liveGen
	// mirrors gen for readers that hold mu, not connSem.
	conn     net.Conn
	reader   *bufio.Reader
	gen      uint64
	connDone chan struct{} // closed by the reader when this connection is lost
	connSem  chan struct{}
	liveGen  atomic.Uint64

	// The genesis verification that has been done, as the pair it is true of:
	// the connection generation it was done on and the network prefix it was
	// done against. Guarded by connSem, like gen itself, and read by the gate
	// in callGen. A reconnect moves gen and a SetHRP or TrackAddresses moves
	// the prefix, and either makes the recorded pair stale, which is what
	// makes the check run again rather than once per process.
	genesisGen uint64
	genesisHRP string

	// pending pairs each in-flight request id with its waiting call.
	pendMu  sync.Mutex
	pending map[int64]pendingCall

	reqID   atomic.Int64
	lastTip atomic.Int64 // latest height seen in a headers.subscribe reply or notification

	// refreshTicket orders overlapping refreshes of one address. Taken before
	// the call, compared at the commit: see refreshRecord.commit.
	refreshTicket atomic.Uint64

	addresses         []string
	reconcileInterval time.Duration
	stopCh            chan struct{}
	stopOnce          sync.Once
	kickCh            chan struct{} // wakes the refresher; one slot
	log               *slog.Logger

	lastRefreshAt  time.Time                // guarded by mu: last time EVERY tracked address refreshed
	lastRefreshErr error                    // guarded by mu: error of the last full pass, nil on success
	refreshed      map[string]refreshRecord // guarded by mu: per tracked address, see LastRefreshOf
	trackedSet     map[string]bool          // guarded by mu: the addresses list as a set
	byScriptHash   map[string]string        // guarded by mu: scripthash -> tracked address
	subscribed     map[string]uint64        // guarded by mu: address -> generation the subscription was acknowledged on
	status         map[string]string        // guarded by mu: address -> last status seen (reply or notification)
	changed        map[string]bool          // guarded by mu: addresses with a change pending

	// networkHRP is the network prefix the tracked addresses must carry,
	// guarded by mu. Read it through hrp(), set it through SetHRP: it was an
	// exported field, and a caller assigning to it while the refresher was
	// running was the same data race from outside the package that hrp()
	// closes inside it.
	networkHRP string

	// PingInterval is how often Start pings the server: it keeps the server's
	// idle timer from closing the session and advances the freshness of every
	// subscribed address with no change pending. Zero means 60 seconds.
	// deposit.Monitor.MaxCacheAge must exceed it, or every address reads stale
	// between pings.
	PingInterval time.Duration

	// OnRefresh is called after each successful UTXO refresh with the address
	// and current UTXO count. Useful for monitoring/logging.
	OnRefresh func(address string, utxoCount int)

	// TLSConfig, when non-nil, wraps the connection in TLS. It applies to
	// Reconnect as well, so a connection cannot silently downgrade to plaintext
	// after a reconnect.
	//
	// Leave it nil only when the transport is already private: ElectrumX on
	// localhost, or over a tunnel you control. An ElectrumX server sees every
	// address you track, so over the public internet a plaintext connection
	// discloses your entire deposit set to anyone on the path, and lets them
	// alter the balances and UTXOs you act on.
	//
	// ServerName is filled in from the host when empty. The zero value of
	// tls.Config verifies the server certificate against the system roots,
	// which is what you want; see UseTLS.
	TLSConfig *tls.Config
}

// UseTLS enables TLS with certificate verification against the system roots.
// This is the setting an exchange should use for any ElectrumX server it does
// not reach over a private network.
//
//	client := electrumx.NewClient("electrum.example.org:50002", 10*time.Minute, logger)
//	client.UseTLS()
//	client.Connect(ctx)
//
// For a private CA or a pinned certificate, set TLSConfig directly instead.
func (c *Client) UseTLS() {
	c.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
}

// refreshRecord is one address's freshness state.
//
// at is the last moment the address's UTXO set was known current: a
// listunspent reply landed, a subscribe reply carried the status last seen,
// or a ping was answered while the address was subscribed with no change
// pending. err is the last attempt's error, nil on success. gen is the
// connection at was established on. dirty is set by a notification and
// cleared by the listunspent that answers it, so no ping advances an address
// with a change in flight; seq counts notifications so a reply that predates
// one leaves dirty set.
type refreshRecord struct {
	at    time.Time
	err   error
	gen   uint64
	dirty bool
	seq   uint64
	// commit is the ticket of the last refresh of this address that reported.
	// Overlapping refreshes commit in ticket order and an older one is
	// dropped, whatever order the replies arrive in.
	commit uint64
}

// request is a JSON-RPC request to ElectrumX.
type request struct {
	ID     int64       `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// incoming is any line the server sends: a reply (id set) or a notification
// (method set, no id). Replies are paired with calls by id, never by position:
// a reader that took "the next line" as "the reply" went off by one at every
// notification and stored address A's UTXOs under address B.
type incoming struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error,omitempty"`
}

var (
	// ErrNotConnected is returned by every call made before Connect or after
	// Stop, by a call whose connection was lost before its reply, and by
	// every call after a lost connection until Reconnect or Start restores it.
	ErrNotConnected = errors.New("electrumx: not connected")
	// ErrNetworkMismatch is returned when a tracked address is on a different
	// network than the client's HRP, or when addresses in one call disagree.
	ErrNetworkMismatch = errors.New("electrumx: address network does not match the client's")
	// ErrGenesisMismatch is returned when the server's reported genesis hash
	// is not one of the chains the client's HRP belongs to: by Connect, and by
	// any later call, before the requested method is sent. The prefix can be
	// set after the connection, so the first verification of a chain can fall
	// on an ordinary call rather than on Connect.
	ErrGenesisMismatch = errors.New("electrumx: server is indexing a different chain")
)

// NewClient creates a new ElectrumX client.
//
// Parameters:
//   - host: ElectrumX TCP address (e.g., "localhost:50001")
//   - reconcileInterval: how often Start makes a full listunspent pass over
//     every tracked address, the safety net for a notification the server
//     never sent. It must exceed the pass itself (one round trip per
//     address); zero or negative means 10 minutes.
//   - logger: where connection events, reorgs and refresh errors go; nil discards
func NewClient(host string, reconcileInterval time.Duration, logger *slog.Logger) *Client {
	if reconcileInterval <= 0 {
		reconcileInterval = defaultReconcileInterval
	}
	return &Client{
		log:               logutil.Or(logger),
		utxos:             make(map[string][]types.UTXO),
		refreshed:         make(map[string]refreshRecord),
		trackedSet:        make(map[string]bool),
		byScriptHash:      make(map[string]string),
		subscribed:        make(map[string]uint64),
		status:            make(map[string]string),
		changed:           make(map[string]bool),
		pending:           make(map[int64]pendingCall),
		host:              host,
		reconcileInterval: reconcileInterval,
		stopCh:            make(chan struct{}),
		kickCh:            make(chan struct{}, 1),
		connSem:           make(chan struct{}, 1),
	}
}

// pruneTo deletes from m every address not in keep. Caller holds mu.
func pruneTo[V any](m map[string]V, keep map[string]bool) {
	for a := range m {
		if !keep[a] {
			delete(m, a)
		}
	}
}

// TrackAddresses registers the addresses to keep fresh, replacing the previous
// list. Start subscribes the new ones on its next pass.
//
// Every address must decode on one network. If HRP is unset it is inferred
// from the first address; if it is set, an address on another network is an
// error (ErrNetworkMismatch). Nothing is registered when any address fails, so
// a mainnet deployment that forgets to set HRP gets an error at startup rather
// than a cache that refreshes nothing for the life of the process.
func (c *Client) TrackAddresses(addresses []string) error {
	hrp := c.hrp()
	for _, a := range addresses {
		n, err := address.NetworkOf(a)
		if err != nil {
			return fmt.Errorf("track %s: %w", a, err)
		}
		if hrp == "" {
			hrp = n.HRP
		} else if n.HRP != hrp {
			return fmt.Errorf("%w: %s is %s, client is %s", ErrNetworkMismatch, a, n.HRP, hrp)
		}
		if _, _, err := address.Decode(hrp, a); err != nil {
			return fmt.Errorf("track %s: %w", a, err)
		}
	}
	byHash := make(map[string]string, len(addresses))
	for _, a := range addresses {
		sh, err := address.AddressToScriptHash(hrp, a)
		if err != nil {
			return fmt.Errorf("track %s: %w", a, err)
		}
		byHash[sh] = a
	}
	beforeTrackCommit()
	c.mu.Lock()
	if c.networkHRP != "" && c.networkHRP != hrp {
		// SetHRP pinned another network between the read at the top of this
		// function and this commit. Writing the prefix here would drop that
		// pin while installing addresses derived under the old one, and both
		// calls would return nil: the client would then ask one network's
		// server about another network's keys and answer "no coins" for every
		// address, which is what pinning exists to prevent.
		pinned := c.networkHRP
		c.mu.Unlock()
		return fmt.Errorf("%w: the client was pinned to %s while %s addresses were being prepared",
			ErrNetworkMismatch, pinned, hrp)
	}
	c.networkHRP = hrp
	c.addresses = append([]string(nil), addresses...)
	c.trackedSet = make(map[string]bool, len(addresses))
	for _, a := range addresses {
		c.trackedSet[a] = true
	}
	c.byScriptHash = byHash
	// Every per-address map is pruned to the new set, the UTXO cache included.
	// It is the one that holds money: an address no longer tracked must not be
	// counted by GetBalance, returned by GetAllUTXOs or offered to the
	// selector. Pruning it was missing, and iterating only c.refreshed missed
	// any address that had a cache entry and no freshness record.
	pruneTo(c.refreshed, c.trackedSet)
	pruneTo(c.subscribed, c.trackedSet)
	pruneTo(c.status, c.trackedSet)
	pruneTo(c.changed, c.trackedSet)
	pruneTo(c.utxos, c.trackedSet)
	c.mu.Unlock()
	c.kick()
	return nil
}

// beforeTrackCommit runs between the validation a TrackAddresses call does and
// the commit of its result, a variable so a test can occupy that window. The
// prefix is read before the validation and written after it, and this is the
// only place another goroutine can get between the two.
var beforeTrackCommit = func() {}

// hrp reads the network prefix under the lock TrackAddresses writes it under,
// since TrackAddresses may run while Start's refresher is making calls.
func (c *Client) hrp() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.networkHRP
}

// SetHRP pins the network prefix the tracked addresses must carry: "sq" for
// mainnet and regtest, "ssq" for stagenet. Leave it unset and TrackAddresses
// infers the prefix from the addresses it is given; pin it and TrackAddresses
// refuses an address on any other network. There is deliberately no default: a
// silent stagenet default on a mainnet deployment refreshed nothing, ever.
//
// The write is under the lock every read of the prefix takes, so this is safe
// on a running client. Two things are refused. A prefix no supported network
// uses, because it can only ever produce script hashes no server knows. And a
// change of prefix once addresses are tracked, because the UTXO cache, the
// subscriptions and the script hashes were all derived under the old one, so
// the client would be reading one network's server with another network's
// keys: call TrackAddresses(nil) first if that is really the intent.
func (c *Client) SetHRP(hrp string) error {
	if len(types.GenesisHashesForHRP(hrp)) == 0 {
		return fmt.Errorf("%w: %q is not the prefix of a supported network", ErrNetworkMismatch, hrp)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.networkHRP == hrp {
		return nil
	}
	if c.networkHRP != "" && len(c.addresses) > 0 {
		return fmt.Errorf("%w: client is %s with %d addresses tracked, cannot become %s",
			ErrNetworkMismatch, c.networkHRP, len(c.addresses), hrp)
	}
	c.networkHRP = hrp
	return nil
}

// RefreshAll fetches UTXOs for all tracked addresses: one
// blockchain.scripthash.listunspent per address, in sequence, on the one
// connection, each under the 30-second call deadline. Start makes this pass
// on the reconcile interval; resilience.Reconciler makes it before comparing
// the cache with the node.
//
// One failing address does not stop the others: every address is attempted
// and the returned error joins the failures, naming each address. The result
// is recorded for LastRefresh, which callers must consult before treating an
// empty UTXO set as "no deposits", and per address for LastRefreshOf.
//
// When ctx ends mid-pass, or the connection is lost, the pass stops there:
// the addresses reached keep their new records, the rest keep their old ones,
// and the error names one address, the one whose call was cut short or,
// between calls, the first not attempted.
func (c *Client) RefreshAll(ctx context.Context) error {
	c.mu.RLock()
	addrs := append([]string(nil), c.addresses...)
	c.mu.RUnlock()
	_, err := c.refresh(ctx, addrs, true)
	return err
}

// refresh makes one listunspent per address in addrs and returns the
// addresses not refreshed: those whose call failed and those not attempted
// because the pass stopped. When full, the result is recorded as the last
// full pass for LastRefresh.
func (c *Client) refresh(ctx context.Context, addrs []string, full bool) ([]string, error) {
	var errs []error
	var failed []string
	for i, addr := range addrs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", addr, err))
			failed = append(failed, addrs[i:]...)
			break
		}
		err := c.refreshAddress(ctx, addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", addr, err))
			failed = append(failed, addr)
			// A context that ended during this address's call is already
			// named through its error; so is a lost connection, which every
			// later call would report the same way. The pass stops here.
			if ctx.Err() != nil || errIsConnection(err) {
				failed = append(failed, addrs[i+1:]...)
				break
			}
		}
	}
	err := errors.Join(errs...)
	if full {
		c.mu.Lock()
		c.lastRefreshErr = err
		if err == nil {
			c.lastRefreshAt = time.Now()
		}
		c.mu.Unlock()
	}
	return failed, err
}

// LastRefresh reports when every tracked address last refreshed successfully
// in one full pass and the error of the most recent full pass (nil on
// success). A zero time or a non-nil error means the cache may be stale:
// GetUTXOs and GetBalance answer from the cache regardless, so this is how an
// indexer outage is told apart from "no deposits". deposit.Monitor reads the
// per-address LastRefreshOf instead, which a subscription keeps current
// between full passes.
func (c *Client) LastRefresh() (time.Time, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefreshAt, c.lastRefreshErr
}

// LastRefreshOf reports the last moment one tracked address's UTXO set was
// known current, and the error of the most recent attempt for it, nil on
// success. The zero time means never: the address is not tracked, or no call
// for it has succeeded.
//
// The moment advances when a listunspent reply lands, when a subscribe reply
// on a new connection carries the status last seen (the history did not
// change while the client was away), and when the server answers a ping
// while the address is subscribed with no change pending. It does not
// advance on a notification, or while the listunspent a notification asked
// for is still in flight. deposit.Monitor uses it to skip only the addresses
// the indexer has not answered for instead of pausing every credit when one
// of thousands fails.
func (c *Client) LastRefreshOf(addr string) (time.Time, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rec := c.refreshed[addr]
	return rec.at, rec.err
}

// refreshAddress fetches UTXOs for a single address via ElectrumX and records
// the attempt in the address's freshness record.
//
// Defense 12 (Merge Refresh): Uses a MERGE strategy instead of full replacement.
// The indexer does not send an asset type; SetAssetType stamps it after node
// verification, and a replacement would drop the stamp on every poll. The merge:
//  1. Preserves the AssetType stamp on UTXOs that still appear
//  2. Removes UTXOs that ElectrumX no longer reports (confirmed spent)
//  3. Adds new UTXOs that appeared since last refresh (change outputs, new coinbases)
func (c *Client) refreshAddress(ctx context.Context, addr string) error {
	scriptHash, err := address.AddressToScriptHash(c.hrp(), addr)
	if err != nil {
		return fmt.Errorf("address to script hash: %w", err)
	}

	// One ticket per listunspent, taken before the call goes out. Two
	// refreshes of the same address can be in flight at once (the refresher's
	// pass and a caller's RefreshAll), and their replies can arrive in either
	// order. A commit whose ticket is older than the last one committed for
	// the address is dropped, so a late reply cannot put an older set back
	// into the cache and cannot claim its freshness. seq does not cover this:
	// it counts notifications, and neither of two overlapping calls has one.
	ticket := c.refreshTicket.Add(1)

	c.mu.RLock()
	seqBefore := c.refreshed[addr].seq
	c.mu.RUnlock()

	result, gen, err := c.callGen(ctx, "blockchain.scripthash.listunspent", []interface{}{scriptHash})
	if err != nil {
		c.recordRefresh(addr, gen, seqBefore, ticket, fmt.Errorf("listunspent: %w", err))
		return fmt.Errorf("listunspent: %w", err)
	}

	var freshUTXOs []types.UTXO
	if err := json.Unmarshal(result, &freshUTXOs); err != nil {
		c.recordRefresh(addr, gen, seqBefore, ticket, fmt.Errorf("parse utxos: %w", err))
		return fmt.Errorf("parse utxos: %w", err)
	}

	count, committed := c.commitRefresh(addr, gen, seqBefore, ticket, freshUTXOs)
	if committed && c.OnRefresh != nil {
		// Outside the lock: a slow OnRefresh must not hold the reader, which
		// takes mu to note a change, and with it every reply on the
		// connection. The commit is finished, so a panic here unwinds through
		// the refresher's recovery with no lock held.
		c.OnRefresh(addr, count)
	}
	return nil
}

// commitRefresh merges one listunspent reply into the cache under mu and
// reports the size of the merged set and whether it was committed. A merge
// rather than a replacement, so the AssetType stamp survives the pass.
// Outputs the reply no longer lists are dropped as spent, and outputs it
// lists for the first time are added.
func (c *Client) commitRefresh(addr string, gen, seqBefore, ticket uint64, freshUTXOs []types.UTXO) (int, bool) {
	type utxoKey struct {
		TxID string
		Vout uint32
	}
	freshSet := make(map[utxoKey]types.UTXO, len(freshUTXOs))
	for i := range freshUTXOs {
		freshUTXOs[i].Address = addr
		key := utxoKey{freshUTXOs[i].TxID, freshUTXOs[i].Vout}
		freshSet[key] = freshUTXOs[i]
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	existing := c.utxos[addr]

	// Step 1: Walk existing UTXOs — keep ones still in freshSet (preserving flags)
	var merged []types.UTXO
	kept := make(map[utxoKey]bool)

	for _, u := range existing {
		key := utxoKey{u.TxID, u.Vout}
		if fresh, stillExists := freshSet[key]; stillExists {
			// Preserve our AssetType stamp but take the server's height and
			// value on every pass. Height must be allowed to fall
			// back to 0 (a reorg returned the tx to the mempool) or move (it
			// was re-mined elsewhere); value must be allowed to correct a
			// wrongly injected change output, because the amount is committed
			// into the sighash and a stale one makes every spend fail.
			if u.Height > 0 && fresh.Height == 0 {
				c.log.Warn("reorg: a confirmed output is back in the mempool", "txid", u.TxID, "vout", u.Vout, "height", u.Height)
			}
			u.Height = fresh.Height
			u.Value = fresh.Value
			merged = append(merged, u)
			kept[key] = true
		}
		// else: UTXO disappeared → confirmed spent, drop it
	}

	// Step 2: Add new UTXOs
	for key, u := range freshSet {
		if !kept[key] {
			merged = append(merged, u)
		}
	}

	if !c.trackedSet[addr] {
		// TrackAddresses dropped the address while this listunspent was in
		// flight; committing now would put an untracked address's outputs
		// back into the cache the balance and the selector read.
		return 0, false
	}
	if ticket < c.refreshed[addr].commit {
		// A refresh that started later has already committed; this reply is
		// older than what the cache holds.
		return 0, false
	}
	c.utxos[addr] = merged
	c.recordRefreshLocked(addr, gen, seqBefore, ticket, nil)
	return len(merged), true
}

func (c *Client) recordRefresh(addr string, gen, seqBefore, ticket uint64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordRefreshLocked(addr, gen, seqBefore, ticket, err)
}

// recordRefreshLocked records one listunspent attempt. Caller holds mu. A
// success makes the set known current as of now on connection gen and clears
// the change flag, unless a notification arrived while the call was in
// flight: then the reply may predate the change, so neither the moment nor
// the flag moves, and the next pass refreshes the address again.
// TrackAddresses may have dropped the address during the call; then nothing
// is recorded.
func (c *Client) recordRefreshLocked(addr string, gen, seqBefore, ticket uint64, err error) {
	if !c.trackedSet[addr] {
		return
	}
	rec := c.refreshed[addr]
	if ticket < rec.commit {
		return // a later refresh of this address has already reported
	}
	rec.commit = ticket
	if !endedErr(err) {
		// A context that ended says nothing about the indexer; the previous
		// verdict stands and no alert follows from a shutdown.
		rec.err = err
	}
	if err == nil && rec.seq == seqBefore {
		rec.at = time.Now()
		rec.gen = gen
		rec.dirty = false
	}
	c.refreshed[addr] = rec
}

// endedErr reports an error that is, or wraps, a context ending.
func endedErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// GetBalance returns the total confirmed and unconfirmed balance across all
// tracked addresses, as the indexer reports it. Only native SOQ UTXOs
// (AssetType=0) are counted; USDSOQ and future types have separate accounting.
// It is the indexer's view: an input this process has reserved or sent is
// counted until the indexer sees the spend. What can be spent is what
// utxo.CoinSelector accepts against the spent set, and a payout budget reads
// that, never this figure.
func (c *Client) GetBalance(minConf int, tipHeight int64) (confirmed, unconfirmed int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, utxos := range c.utxos {
		for _, u := range utxos {
			if u.AssetType != types.AssetTypeSOQ {
				continue
			}
			if u.Height > 0 && (tipHeight-u.Height+1) >= int64(minConf) {
				confirmed += u.Value
			} else {
				unconfirmed += u.Value
			}
		}
	}
	return
}

// GetUTXOs returns a copy of all UTXOs for the given address.
func (c *Client) GetUTXOs(addr string) []types.UTXO {
	c.mu.RLock()
	defer c.mu.RUnlock()

	utxos := c.utxos[addr]
	result := make([]types.UTXO, len(utxos))
	copy(result, utxos)
	return result
}

// GetAllUTXOs returns a copy of all UTXOs across all tracked addresses.
func (c *Client) GetAllUTXOs() []types.UTXO {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var all []types.UTXO
	for _, utxos := range c.utxos {
		all = append(all, utxos...)
	}
	return all
}

// EvictUTXO permanently removes a UTXO from the in-memory cache.
// Called by Defense 11 (gettxout pre-verification) when a UTXO is confirmed
// spent on-chain but ElectrumX still returns it. The UTXO will be re-added
// on the next refresh ONLY if ElectrumX still reports it.
func (c *Client) EvictUTXO(txid string, vout uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for addr, utxos := range c.utxos {
		for i, u := range utxos {
			if u.TxID == txid && u.Vout == vout {
				c.utxos[addr] = append(utxos[:i], utxos[i+1:]...)
				c.log.Info("evicted a stale output from the cache", "txid", txid, "vout", vout)
				return
			}
		}
	}
}

// SetAssetType stamps the asset type on a cached UTXO. Called by Defense 11
// (gettxout verification) after reading the "assettype" field from RC7+
// gettxout responses.
func (c *Client) SetAssetType(txid string, vout uint32, assetType uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for addr, utxos := range c.utxos {
		for i, u := range utxos {
			if u.TxID == txid && u.Vout == vout {
				c.utxos[addr][i].AssetType = assetType
				return
			}
		}
	}
}

// UTXOCount returns the number of native SOQ UTXOs in the cache, as the
// indexer reports them; inputs this process has reserved or sent are
// included until the indexer sees the spend (see GetBalance).
func (c *Client) UTXOCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	count := 0
	for _, utxos := range c.utxos {
		for _, u := range utxos {
			if u.AssetType == types.AssetTypeSOQ {
				count++
			}
		}
	}
	return count
}

// GetTip fetches the current chain tip height from ElectrumX.
func (c *Client) GetTip(ctx context.Context) (int64, error) {
	result, err := c.call(ctx, "blockchain.headers.subscribe", []interface{}{})
	if err != nil {
		return 0, fmt.Errorf("get tip: %w", err)
	}

	var header struct {
		Height int64 `json:"height"`
	}
	if err := json.Unmarshal(result, &header); err != nil {
		return 0, fmt.Errorf("parse tip: %w", err)
	}
	return header.Height, nil
}

// GetHistory fetches transaction history for an address.
func (c *Client) GetHistory(ctx context.Context, addr string) ([]TxHistoryEntry, error) {
	scriptHash, err := address.AddressToScriptHash(c.hrp(), addr)
	if err != nil {
		return nil, fmt.Errorf("address to script hash: %w", err)
	}

	result, err := c.call(ctx, "blockchain.scripthash.get_history", []interface{}{scriptHash})
	if err != nil {
		return nil, fmt.Errorf("get_history: %w", err)
	}

	var entries []TxHistoryEntry
	if err := json.Unmarshal(result, &entries); err != nil {
		return nil, fmt.Errorf("parse history: %w", err)
	}
	return entries, nil
}

// TxHistoryEntry represents a single transaction in an address's history.
type TxHistoryEntry struct {
	TxHash string `json:"tx_hash"`
	Height int64  `json:"height"` // 0 = unconfirmed
}

// BroadcastTx broadcasts a raw transaction hex via ElectrumX.
func (c *Client) BroadcastTx(ctx context.Context, rawTxHex string) (string, error) {
	result, err := c.call(ctx, "blockchain.transaction.broadcast", []interface{}{rawTxHex})
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}

	var txid string
	if err := json.Unmarshal(result, &txid); err != nil {
		return "", fmt.Errorf("parse broadcast result: %w", err)
	}
	return txid, nil
}
