// Package withdraw is the durable withdrawal state machine an exchange runs
// between "a user asked for a payout" and "the payout is confirmed".
//
// It exists because the two failure modes an exchange cannot afford are both
// created by treating a withdrawal as a single call:
//
//   - Double payment. A broadcast whose reply is lost looks like a failure. If
//     the caller then builds a new transaction, the first one is usually
//     already relayed, so the recipient is paid twice from different inputs.
//     Here every withdrawal is an Intent with a client-chosen idempotency key.
//     The signed transaction is persisted BEFORE it is broadcast, and a
//     broadcast with an unknown outcome is retried with the same bytes only.
//   - Own double-spend. Two withdrawals built concurrently select the same
//     UTXO; the second is rejected by the mempool, or lives in another node's
//     mempool until one confirms. Here inputs are reserved in the spent set
//     when the transaction is built, atomically and all-or-nothing, and the
//     reservation becomes a permanent spent entry at broadcast, or when the
//     intent is held, and is released on a permanent failure.
//
// States and transitions:
//
//	Created ──Build──► Built ──Broadcast──► Broadcast ──Confirm──► Confirmed
//	   │                 │        │ (unknown outcome: stay Built, reservation
//	   │                 │        │  renewed, MaybeRelayed set, retry same hex)
//	   │                 │        │ (node accepted under another txid, or
//	   │                 │        │  rejected after an unknown outcome: stay
//	   │                 │        │  Built, held, inputs marked spent)
//	   │                 │        └──(rejected, no unknown outcome)──► Failed
//	   ├──(selector transient: stay Created, attempt recorded, retry Build)
//	   └──(cannot build)─┴──────────────────────► Failed
//
//	Broadcast ──Rebroadcast (operator, same bytes)──► Broadcast
//	Broadcast, held Built ──Abandon (operator, after the checks)──► Failed
//
// Failed means the intent could not be built, or the node refused the bytes
// on an attempt and no attempt's outcome is unknown, or an operator
// abandoned the intent. A held intent is one the engine will not send again
// and will not fail on its own; Abandon is the way out.
//
// Every method takes a context.Context. A context that ends is never a
// verdict on a withdrawal: during Build it leaves the intent Created with
// nothing reserved; during Broadcast it is a lost reply, so the intent stays
// Built with its reservation renewed and Recover sends the same bytes after
// a restart, exactly as for a timeout. Nothing a context can do releases a
// Built intent's inputs, fails it, or builds a second transaction.
//
// The context governs what the engine asks of the network and how long it
// waits for the store reads of Submit and Process. It does not govern the
// writes that record what the network did, nor the reads that decide what the
// engine may do: those Store.Update calls, the re-read each transition makes
// of the record it is about to act on, and the Get and List calls Recover
// makes to repair the spent set, are made under context.WithoutCancel(ctx),
// because the record must land, the decision must be made on the record, and
// the repair must run, whether or not the caller is still waiting. Submit's Create is
// the one exception: it records nothing the network has done and is bound
// to the caller's context. A Store bounds every call with a timeout of its
// own; the engine's contexts carry no deadline on these paths.
//
// Recover re-drives Built intents after a restart with the same bytes. It
// never rebuilds. It also releases reservations held for intents that have
// nothing built (a crash between the reservation and the Built save), so the
// reservation TTL is a backstop rather than the mechanism that frees them.
// Every non-final broadcast attempt renews the input reservation, so a Built
// intent that is retried at least once per ReservationTTL never loses its
// inputs to another withdrawal. The engine is agnostic about where coins come
// from and how they are signed: those are injected so the exchange can wire
// its own ElectrumX client, key manager and node.
package withdraw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/logutil"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// State of an Intent.
type State string

const (
	StateCreated   State = "created"   // accepted, nothing built
	StateBuilt     State = "built"     // signed transaction persisted, inputs reserved, not yet known to the network
	StateBroadcast State = "broadcast" // accepted by the node, awaiting confirmations
	StateConfirmed State = "confirmed" // reached RequiredConfirmations
	StateFailed    State = "failed"    // rejected, could not be built, or abandoned; inputs released or dropped
)

// Outpoint is a persisted reference to a spent input.
type Outpoint struct {
	TxID    string `json:"txid"`
	Vout    uint32 `json:"vout"`
	Value   int64  `json:"value"`
	Address string `json:"address"`
}

// Intent is one withdrawal. ID is the caller's idempotency key: submitting
// the same ID twice returns the same Intent and never a second transaction.
type Intent struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Amount  int64  `json:"amount"`
	FeeRate int64  `json:"fee_rate"`

	State State  `json:"state"`
	TxID  string `json:"txid,omitempty"`
	// NodeTxID is set when the node accepted the transaction under a txid
	// other than TxID (rpc.ErrTxIDMismatch). The intent stays Built with its
	// inputs marked spent under NodeTxID; an operator resolves it. Empty
	// otherwise.
	NodeTxID string `json:"node_txid,omitempty"`
	// MaybeRelayed says peers may hold the bytes whatever the node says now.
	// Broadcast saves it set before every send and takes it back only on an
	// answer that shows the node did not take the bytes (a rejection, a
	// transient error, refused credentials) when no earlier attempt left it.
	MaybeRelayed bool `json:"maybe_relayed,omitempty"`
	// SentAt is the last time the bytes were handed to the node: set and
	// saved before every send by Broadcast, and by Rebroadcast, which puts
	// the earlier value back when the node refused, so a loop of refused
	// re-sends does not keep Abandon out of reach. Abandon's wait runs from
	// it. Zero on records from before this field existed, which read UpdatedAt.
	SentAt time.Time `json:"sent_at,omitempty"`
	// AbandonedAt is set by Abandon. Recover drops the spent entries only of
	// a Failed intent that carries it; any other Failed owner's are kept.
	AbandonedAt time.Time `json:"abandoned_at,omitempty"`
	// Hold names why a Built intent is held (HoldRejectedAfterUnknown); empty
	// when it is not. NodeTxID set is a hold in its own right.
	Hold          string     `json:"hold,omitempty"`
	RawHex        string     `json:"raw_hex,omitempty"`
	Inputs        []Outpoint `json:"inputs,omitempty"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	Confirmations int64      `json:"confirmations"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists intents. Create and Update must be durable before they
// return: the engine relies on "persisted, then broadcast" to make recovery
// safe.
//
// Create writes a record for an id the store does not hold and returns
// ErrExists, wrapped, for one it does; nothing is written in that case.
// Update replaces the record of intent.ID when the stored record's State is
// from, and returns ErrStale, wrapped, when the record is absent or in any
// other state; nothing is written in that case either. Together they make
// every write conditional on what the writer read, so no caller can move a
// record backwards: a second Submit cannot write Created over a Built intent,
// and a confirmation loop holding a stale copy cannot write Broadcast over
// Confirmed. A database store implements Create as an insert that reports a
// conflict on the primary key without aborting the caller's transaction (in
// PostgreSQL, ON CONFLICT DO NOTHING and a row count), and Update as an
// update whose where clause names the state. The engine also calls Update
// with from equal to the record's own state, to record an attempt that
// settled nothing, so the where clause must not demand that the state change.
//
// Which context each call receives: Create, and the Get calls of Submit and
// Process, receive the caller's context. Every Update, the Get each of Build,
// Broadcast, UpdateConfirmations, Rebroadcast and Abandon makes under the
// engine's lock to re-read the record it acts on, and the Get and List calls
// of Recover's repair passes, receive one that does not end when the caller's
// does (context.WithoutCancel, which also carries no deadline): the engine
// makes them to record what the network has already done, to decide what it
// may do, or to repair the spent set from the record, and a record cut short
// by the caller is a Built intent the store thinks is Created, or a held
// intent it thinks is sendable. A database-backed store bounds every method
// with its own timeout and does not rely on the context for that.
type Store interface {
	Get(ctx context.Context, id string) (*Intent, bool, error)
	Create(ctx context.Context, intent *Intent) error
	Update(ctx context.Context, intent *Intent, from State) error
	List(ctx context.Context, states ...State) ([]*Intent, error)
}

// Broadcaster sends a signed transaction and reports the outcome using the
// rpc error kinds; rpc.ErrAlreadyInChain is read as success. *rpc.Client
// satisfies it. A context that ends during the send, or a connection lost
// after the request was written, must be reported
// as rpc.ErrUnknownOutcome or as the context's error, never as
// rpc.ErrPermanent or rpc.ErrTransient: the bytes may be in a mempool, and
// the engine reads ErrTransient as the node answering without taking them.
// It is called with the engine's lock held and must not call back into the
// Engine.
type Broadcaster interface {
	Broadcast(ctx context.Context, rawHex, txid string) (string, error)
}

// Confirmer reports how many confirmations a transaction has (0 for mempool).
// It is optional; without it intents stay in StateBroadcast. It is called
// with the engine's lock held and must not call back into the Engine.
type Confirmer interface {
	Confirmations(ctx context.Context, txid string) (int64, error)
}

// Chain answers Abandon's two questions from the node: whether it knows a
// transaction, and whether an outpoint is unspent. A "no" is acted on, so an
// implementation refuses to answer while the node is behind, as RPCChain does.
// It is called with the engine's lock held and must not call back into the Engine.
type Chain interface {
	KnowsTransaction(ctx context.Context, txid string) (bool, error)
	Unspent(ctx context.Context, txid string, vout uint32) (bool, error)
}

// Selector chooses inputs for an amount at a fee rate. It must honour the
// engine's SpentSet (utxo.CoinSelector does) so reserved inputs are skipped.
// An error that is rpc.ErrTransient, or the context's own error, leaves the
// intent Created for a later Build; any other error fails it. It is called
// with the engine's lock held and must not call back into the Engine.
type Selector func(ctx context.Context, amount, feeRate int64) ([]types.UTXO, error)

// BuildSigner turns selected inputs into a signed transaction. tx.BuildAndSign
// wrapped with the exchange's scripts and signer is the expected value; it
// verifies every input against the node's rules before returning, so a
// transaction the node would refuse fails Build and never reaches Broadcast.
// A signer that reaches another process honours the context; when it returns
// the context's error nothing signed has reached the engine, the reservation
// is released and the intent stays Created. It is called with the engine's
// lock held and must not call back into the Engine.
type BuildSigner func(ctx context.Context, inputs []types.UTXO, toAddress string, amount, feeRate int64) (rawHex, txid string, err error)

// Engine drives intents through the state machine.
//
// One lock covers every transition after Submit, and it is held across the
// selector, the signer, the Broadcaster and the Confirmer. Builds, sends and
// confirmation lookups are therefore serialised within one process, and a
// node call that does not return holds every transition for as long as its
// context allows, so give the injected clients a deadline.
type Engine struct {
	Store       Store
	Spent       *utxo.SpentSet
	Select      Selector
	BuildSign   BuildSigner
	Broadcaster Broadcaster
	Confirmer   Confirmer // optional
	Chain       Chain     // optional; Abandon refuses without it

	// Network is the chain every destination must be an address on. The zero
	// value is types.Mainnet, as for deposit.Monitor, and so is any value with
	// no ChainID, whatever its other fields. Submit refuses a
	// destination that is not a witness version 1 address on its prefix and
	// records nothing (ErrInvalidIntent); Build applies the same check to the
	// stored record before anything is selected and fails an intent that does
	// not pass, since the record then came from a Submit that did not check.
	// The prefix is not part of the script, so nothing downstream would refuse
	// a destination from another network: address.ScriptFor builds the script
	// for any supported prefix and the node accepts the transaction. Regtest
	// shares mainnet's prefix and its addresses pass a mainnet engine.
	Network types.Network

	// RequiredConfirmations before an intent is Confirmed. The exchange's own
	// policy; docs/EXCHANGE_INTEGRATION.md discusses the horizon.
	RequiredConfirmations int64
	// ReservationTTL bounds how long inputs stay reserved for an intent that
	// is built but not yet broadcast (default DefaultReservationTTL, 4 hours).
	// Every Broadcast attempt that does not end the intent renews it, and
	// Recover re-reserves on restart, so retry Built intents at an interval
	// shorter than this. A Built intent's bytes may already be in a mempool,
	// so an expired reservation is a double-spend window, not a cleanup; the
	// default is long because Recover, not the clock, frees the reservations
	// of intents that have nothing built. Expiry is read from the wall clock,
	// so a forward step of the clock ages every reservation by the size of
	// the step; run Recover after a clock correction, as after a restart. The
	// spent set keeps an expired reservation on disk until Recover or Prune
	// decides it, so Recover sees what the step did.
	ReservationTTL time.Duration
	// AbandonAfter is how long after the last send (Intent.SentAt) Abandon
	// will consider an intent (default DefaultAbandonAfter, 24 hours, the
	// node's own mempool expiry).
	AbandonAfter time.Duration

	// Logger receives the conditions Recover finds and the release failures
	// that are not returned. nil discards.
	Logger *slog.Logger

	mu sync.Mutex // every transition after Submit; see the type doc

}

var (
	// ErrInvalidIntent is returned for a submission with an empty id, a
	// destination that is not an address on Network, or a non-positive amount
	// or fee rate; by Build for a stored intent whose destination is not; and
	// by every transition for an id the store does not hold.
	ErrInvalidIntent = errors.New("withdraw: invalid intent")
	// ErrWrongState is returned when an operation is applied to an intent in a
	// state that does not allow it.
	ErrWrongState = errors.New("withdraw: intent is not in a state that allows this")
	// ErrConflict is returned when an existing intent with the same id has a
	// different address, amount or fee rate: the idempotency key is being
	// reused for a different payment, which is a caller bug worth stopping.
	ErrConflict = errors.New("withdraw: intent id already used for a different withdrawal")
	// ErrHeld is returned by Broadcast, and reported by Recover, for a Built
	// intent the engine will not send again and will not fail: the node
	// accepted it under a different txid (NodeTxID), or rejected it after an
	// attempt whose outcome is unknown (Hold). Abandon is the way out. It never
	// wraps the rejection: rpc.ErrPermanent reads to the breaker as one bad
	// request, and a held intent is a run that must stop.
	ErrHeld = errors.New("withdraw: intent is held; resolve by hand")
	// ErrNotAbandonable is returned by Abandon when a check did not pass: the
	// wait is not over, the node knows the transaction, or an input is spent.
	ErrNotAbandonable = errors.New("withdraw: the checks for abandoning the intent did not pass")
	// ErrReservationLost is returned by Broadcast and by Recover for a Built
	// intent whose inputs could not be re-reserved because another withdrawal
	// took them after the reservation expired. Nothing is sent for it: the
	// intent stays Built with its bytes, which an earlier attempt may have
	// relayed, and both transactions cannot confirm. The operator resolves
	// which one the network took.
	ErrReservationLost = errors.New("withdraw: inputs of a built intent were taken by another withdrawal")
	// ErrExists is returned by Store.Create for an id the store already
	// holds. Submit reads the existing record and answers with it.
	ErrExists = errors.New("withdraw: intent id already exists in the store")
	// ErrStale is returned by Store.Update when the stored record is not in
	// the state the write names, or is absent. Nothing is written. Inside one
	// process the engine re-reads under its lock before every write, so it
	// reaches a caller only when another process moved the record.
	ErrStale = errors.New("withdraw: stored intent is not in the state the write expected")
	// ErrUnknownReservation is reported by Recover for a reservation held by
	// an intent id the store does not know. Process builds only intents that
	// Submit persisted, so an unknown id means the intent store and the spent
	// set are not the pair that was running: the intents file is missing or
	// older than the spent set (or Build was called on an intent that was
	// never submitted). The reservation is kept; a Built intent that the lost
	// store knew may have its bytes in a mempool.
	ErrUnknownReservation = errors.New("withdraw: reservation held by an intent the store does not know; intent store and spent set disagree")
	// ErrFailed is returned by Process for an intent that was already Failed
	// when it was called. Failed is terminal: the intent could not be built,
	// the node refused the bytes and no attempt's outcome was unknown, or an
	// operator abandoned it. Submit
	// answers the same id with no error, so a caller that retries a payout
	// would otherwise read a nil error from Process and record a withdrawal
	// that has no transaction behind it.
	//
	// It wraps rpc.ErrPermanent because one withdrawal that cannot succeed is
	// a fact about that withdrawal and not about the node or the indexer.
	// resilience.CircuitBreaker reads the wrapped sentinel and leaves itself
	// untouched, so a caller retrying one bad payout cannot halt every
	// withdrawal, which is what that breaker's own documentation says it
	// exists to prevent.
	//
	// A Failed intent reached through a rejection at broadcast, or through
	// Abandon, keeps its TxID and RawHex: the bytes may have reached a node.
	ErrFailed = fmt.Errorf("withdraw: intent failed permanently (%w)", rpc.ErrPermanent)
)

// beforeBuild runs between Process's read of the intent and the Build call it
// makes, a variable so a test can occupy that window. Two workers that have
// both read one intent as Created before either builds is the race Build
// settles by re-reading the stored state under its lock, and this is the only
// place another goroutine can get between the read and the build.
var beforeBuild = func() {}

// DefaultReservationTTL applies when Engine.ReservationTTL is zero.
const DefaultReservationTTL = 4 * time.Hour

// DefaultAbandonAfter applies when Engine.AbandonAfter is zero: the node's
// DEFAULT_MEMPOOL_EXPIRY, how long a peer keeps an unmined transaction.
const DefaultAbandonAfter = 24 * time.Hour

// HoldRejectedAfterUnknown is Intent.Hold for a Built intent the node rejected
// on an attempt after one whose outcome was unknown.
const HoldRejectedAfterUnknown = "rejected after an attempt whose outcome is unknown"

// NodeTxIDUnknown is Intent.NodeTxID when a Broadcaster reported
// rpc.ErrTxIDMismatch without the node's txid; it is never sent to the node.
const NodeTxIDUnknown = "unknown"

func (e *Engine) now() time.Time { return time.Now().UTC() }

func (e *Engine) log() *slog.Logger { return logutil.Or(e.Logger) }

// contextEnded reports whether err is, or wraps, a context's own error.
func contextEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (e *Engine) reservationTTL() time.Duration {
	if e.ReservationTTL > 0 {
		return e.ReservationTTL
	}
	return DefaultReservationTTL
}

func (e *Engine) abandonAfter() time.Duration {
	if e.AbandonAfter > 0 {
		return e.AbandonAfter
	}
	return DefaultAbandonAfter
}

// network returns the chain in force: Network when set, else mainnet.
func (e *Engine) network() types.Network {
	if e.Network.ChainID == "" {
		return types.Mainnet
	}
	return e.Network
}

// checkDestination refuses a destination that is not a witness version 1
// address on the engine's network, with ErrInvalidIntent. It is the one
// reading of a destination the engine makes, at Submit and again at Build.
func (e *Engine) checkDestination(id, addr string) error {
	if err := address.Validate(e.network().HRP, addr); err != nil {
		return fmt.Errorf("%w: %s: destination %s is not an address on %s: %v", ErrInvalidIntent, id, addr, e.network().Name, err)
	}
	return nil
}

// heldErr is ErrHeld naming why in is held, or nil for an intent that is not.
func heldErr(in *Intent) error {
	switch {
	case in.NodeTxID != "":
		return fmt.Errorf("%w: %s computed %s, node accepted %s", ErrHeld, in.ID, in.TxID, in.NodeTxID)
	case in.Hold != "":
		return fmt.Errorf("%w: %s: %s", ErrHeld, in.ID, in.Hold)
	}
	return nil
}

// holdTxID is the txid a held intent's inputs are marked spent under.
func holdTxID(in *Intent) string {
	if in.NodeTxID != "" {
		return in.NodeTxID
	}
	return in.TxID
}

// save writes the intent over the stored record it was read from, which is
// in state from. The write is not bound to the caller's context: it records a
// fact the network already knows, or an attempt at one, and must land whether
// or not the caller is still waiting. Values on ctx are kept. Submit does not
// use it; see there.
func (e *Engine) save(ctx context.Context, in *Intent, from State) error {
	in.UpdatedAt = e.now()
	return e.Store.Update(context.WithoutCancel(ctx), in, from)
}

// reread returns the stored record of in.ID when it is in one of the states
// want, or ErrWrongState naming the stored state, or ErrInvalidIntent for an
// id the store does not hold. Every transition after Submit calls it under
// e.mu before acting, because the caller's copy can be behind the store:
// another worker may have advanced the intent between the caller's read and
// the lock.
//
// The read is not bound to the caller's context: it decides what the engine
// may do, not what it asks of the network, and against a store that honours
// the context an ended one would otherwise skip the renewal and the attempt
// record that the same context ending inside the send would produce.
func (e *Engine) reread(ctx context.Context, in *Intent, want ...State) (*Intent, error) {
	stored, ok, err := e.Store.Get(context.WithoutCancel(ctx), in.ID)
	if err != nil {
		return nil, fmt.Errorf("re-read %s: %w", in.ID, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: unknown intent %s", ErrInvalidIntent, in.ID)
	}
	if !slices.Contains(want, stored.State) {
		return nil, fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, stored.State)
	}
	return stored, nil
}

// Submit registers a withdrawal. Calling it again with the same id returns
// the existing intent (created=false); with the same id and different
// parameters it returns ErrConflict. The destination is compared as an
// address: bech32m spells one address all-lower or all-upper, and the other
// spelling of the recorded destination is the same parameters, recorded as
// first written. A destination that is not an address on Network is
// ErrInvalidIntent, and nothing is recorded for it.
//
// The registration is one Store.Create, which the store refuses for an id it
// holds, so a second Submit of one id can never write over the first one's
// record whatever state a worker has driven it to in the meantime. Two
// submits of one id in flight at once, which is what a client retry under an
// idempotency key produces, both reach the store and exactly one creates.
func (e *Engine) Submit(ctx context.Context, id, address string, amount, feeRate int64) (intent *Intent, created bool, err error) {
	if id == "" || address == "" || amount <= 0 || feeRate <= 0 {
		return nil, false, fmt.Errorf("%w: id=%q address=%q amount=%d feeRate=%d", ErrInvalidIntent, id, address, amount, feeRate)
	}
	if err := e.checkDestination(id, address); err != nil {
		return nil, false, err
	}
	now := e.now()
	in := &Intent{ID: id, Address: address, Amount: amount, FeeRate: feeRate, State: StateCreated, CreatedAt: now, UpdatedAt: now}
	// Bound to the caller's context, unlike every later write: nothing has
	// happened on the network, so a registration the caller abandons must not
	// become an intent a worker later pays. The same id submitted again
	// registers it then.
	err = e.Store.Create(ctx, in)
	if err == nil {
		return in, true, nil
	}
	if !errors.Is(err, ErrExists) {
		return nil, false, err
	}
	existing, ok, err := e.Store.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, fmt.Errorf("%w: %s: the store refused to create it and does not hold it", ErrInvalidIntent, id)
	}
	if !strings.EqualFold(existing.Address, address) || existing.Amount != amount || existing.FeeRate != feeRate {
		return existing, false, fmt.Errorf("%w: %s", ErrConflict, id)
	}
	return existing, false, nil
}

// Build selects and reserves inputs, builds and signs the transaction, and
// persists it. The intent is Built and its inputs are reserved when this
// returns nil. Nothing has touched the network.
//
// A selector error that is rpc.ErrTransient (the node behind its headers,
// warming up, or unreachable) leaves the intent Created with the attempt
// recorded and is returned for a later Build; nothing is reserved. So does a
// selector error returned once ctx has ended, whatever it says: the caller
// stopped the attempt, and the intent is not judged on an attempt that was
// stopped. Any other selector error (insufficient funds, a wrong chain) fails
// the intent. A signer error is the same, except that the reservation taken
// before signing is released. A stored destination that is not an address on
// Network fails the intent before anything is selected.
func (e *Engine) Build(ctx context.Context, in *Intent) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The stored state decides, not the caller's copy, which can be behind the
	// store (another worker advanced the intent) or ahead of it (a save failed
	// after this method changed the copy). Without the re-read two workers
	// holding a Created copy of one intent both build, from different inputs,
	// and the recipient is paid twice.
	stored, err := e.reread(ctx, in, StateCreated)
	if err != nil {
		return err
	}
	// From here the stored record is the one worked on, so an attempt count
	// or a last error another worker recorded is carried rather than lost.
	*in = *stored

	// The stored destination is checked here as well as at Submit: this is
	// the transition that signs, and the record may have been created by an
	// engine bound to another network, or by a release that did not check.
	if err := e.checkDestination(in.ID, in.Address); err != nil {
		return e.fail(ctx, in, err, false, StateCreated)
	}
	inputs, err := e.Select(ctx, in.Amount, in.FeeRate)
	if err != nil {
		err = fmt.Errorf("select inputs: %w", err)
		if errors.Is(err, rpc.ErrTransient) || contextEnded(err) || ctx.Err() != nil {
			return e.deferBuild(ctx, in, err)
		}
		return e.fail(ctx, in, err, false, StateCreated)
	}
	if err := e.Spent.Reserve(inputs, in.ID, e.reservationTTL()); err != nil {
		// Either another intent won the race for one of these inputs
		// (utxo.ErrAlreadyReserved; the caller retries Build and selection
		// skips them now) or the spent set could not be written
		// (utxo.ErrPersist; nothing is reserved and nothing is built until the
		// disk is fixed). Not a failure of this intent.
		return e.deferBuild(ctx, in, err)
	}
	rawHex, txid, err := e.BuildSign(ctx, inputs, in.Address, in.Amount, in.FeeRate)
	if err != nil {
		err = fmt.Errorf("build and sign: %w", err)
		if contextEnded(err) || ctx.Err() != nil {
			// Nothing signed reached this process, so nothing can be on the
			// network; the inputs are free again and the intent waits.
			e.release(in)
			return e.deferBuild(ctx, in, err)
		}
		return e.fail(ctx, in, err, false, StateCreated)
	}
	in.RawHex, in.TxID = rawHex, txid
	in.Inputs = in.Inputs[:0]
	for _, u := range inputs {
		in.Inputs = append(in.Inputs, Outpoint{TxID: u.TxID, Vout: u.Vout, Value: u.Value, Address: u.Address})
	}
	in.State = StateBuilt
	if err := e.save(ctx, in, StateCreated); err != nil {
		if errors.Is(err, ErrWrittenNotDurable) {
			// The store has the record and says so: only its durability is
			// unconfirmed. Every other reader of the store, including Recover
			// and any process that broadcasts, will find this intent Built, so
			// releasing its inputs here would leave a signed transaction whose
			// inputs the next withdrawal can select. The intent stays Built
			// with its reservation, and the error is returned so the caller
			// knows the record may not survive a power loss.
			return fmt.Errorf("built intent %s saved, durability not confirmed: %w", in.ID, err)
		}
		// Not saved, so it must not reach the network: release and report.
		e.release(in)
		in.State, in.RawHex, in.TxID, in.Inputs = StateCreated, "", "", nil
		return fmt.Errorf("persist built intent %s: %w", in.ID, err)
	}
	return nil
}

// Broadcast sends a Built intent's transaction. On success the intent is
// Broadcast and its inputs are permanently marked spent. On an unknown
// outcome the intent stays Built with the attempt recorded and the input
// reservation renewed, and the caller calls Broadcast again later: the same
// bytes go out, never a new transaction. On a permanent rejection the intent
// is Failed and its inputs released, unless an earlier attempt's outcome was
// unknown (MaybeRelayed): peers may hold those bytes for the node's mempool
// expiry, so the intent is held instead, Built with Hold set and its inputs
// marked spent under its txid, and ErrHeld is returned. When the node
// accepted the transaction under a different txid (rpc.ErrTxIDMismatch) the
// payment is in the mempool: the inputs are marked spent under the node's
// txid, the intent stays Built with NodeTxID recorded, and the error is
// returned. Neither is a rejection. A held intent is never sent again;
// Abandon is the way out.
//
// If the node accepted the transaction but the spent set could not be
// written (utxo.ErrPersist), the intent is still saved as Broadcast and that
// error is returned: the payment is out, this process refuses the inputs,
// and Recover re-marks them from the intent store after a restart. Check
// in.State when Broadcast returns an error.
//
// A context that ends during the send is a lost reply: the intent stays
// Built, exactly as for an unknown outcome, whatever the Broadcaster wrapped
// the context's error in. The check for it comes before the permanent branch
// so no wrapping can turn a cancel into a failure and a release.
//
// Broadcast runs under the engine's lock and acts on the stored record; the
// caller's copy is only the key. A copy that says Built while the store says
// Broadcast (another worker sent it first) receives ErrWrongState and sends
// nothing; one that says Broadcast while the store says Built (the save after
// a send failed) is sent again from the stored record.
//
// Before the send the inputs are re-reserved for another TTL. When that fails,
// because another withdrawal holds an input (ErrReservationLost) or the spent
// set cannot be written, nothing is sent: the intent stays Built with its
// bytes and the cause recorded. Recover applies the same rule. Then the
// attempt is saved, with MaybeRelayed set and SentAt, and only then sent: a
// save that fails sends nothing, and a save that fails after the send cannot
// lose the mark.
func (e *Engine) Broadcast(ctx context.Context, in *Intent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	stored, err := e.reread(ctx, in, StateBuilt)
	if err != nil {
		return err
	}
	*in = *stored
	if err := heldErr(in); err != nil {
		return err
	}
	// A refused reservation asks nothing of the network, so Attempts does not
	// advance for it; LastError carries the cause.
	if rerr := e.Spent.Reserve(e.inputs(in), in.ID, e.reservationTTL()); rerr != nil {
		return e.holdBuilt(ctx, in, notReserved(rerr))
	}
	// A record from before the mark existed (no SentAt) with attempts on it
	// has outcomes that are not on record; they count as possibly relayed.
	prior := in.MaybeRelayed || (in.Attempts > 0 && in.SentAt.IsZero())
	in.Attempts++
	in.MaybeRelayed, in.SentAt = true, e.now()
	if err := e.save(ctx, in, StateBuilt); err != nil {
		return fmt.Errorf("record attempt %d of %s, nothing sent: %w", in.Attempts, in.ID, err)
	}
	got, err := e.Broadcaster.Broadcast(ctx, in.RawHex, in.TxID)
	switch {
	case err == nil || errors.Is(err, rpc.ErrAlreadyInChain):
		in.State = StateBroadcast
		in.LastError = ""
		perr := e.Spent.MarkBroadcastFor(e.inputs(in), in.TxID, in.ID)
		if perr != nil {
			perr = fmt.Errorf("%s broadcast as %s, spent set not written: %w", in.ID, in.TxID, perr)
			in.LastError = perr.Error()
		}
		if saveErr := e.save(ctx, in, StateBuilt); saveErr != nil {
			return errors.Join(perr, saveErr)
		}
		return perr
	case contextEnded(err) || errors.Is(err, rpc.ErrUnknownOutcome):
		// The transaction may be out. Keep the reservation, keep the bytes
		// and the mark, report, retry later.
		return e.holdBuilt(ctx, in, err)
	case errors.Is(err, rpc.ErrTxIDMismatch):
		if got == "" {
			// A Broadcaster that reports the kind without the node's txid
			// must still arm the hold; the hold is keyed on NodeTxID.
			got = NodeTxIDUnknown
		}
		in.NodeTxID = got
		return e.holdMarked(ctx, in, err)
	case errors.Is(err, rpc.ErrPermanent) && prior:
		in.Hold = HoldRejectedAfterUnknown
		// %v, never %w: the rejection is rpc.ErrPermanent, and a held intent
		// must count for the breaker (see ErrHeld).
		return e.holdMarked(ctx, in, fmt.Errorf("%w: %s: %s: %v", ErrHeld, in.ID, in.Hold, err))
	case errors.Is(err, rpc.ErrPermanent):
		in.MaybeRelayed = false
		return e.fail(ctx, in, err, true, StateBuilt)
	case errors.Is(err, rpc.ErrTransient) || errors.Is(err, rpc.ErrUnauthorized):
		// The node answered and did not take the bytes on this attempt, so
		// the mark goes back to what earlier attempts left. Keep the
		// reservation, keep the bytes, report, retry later.
		in.MaybeRelayed = prior
		return e.holdBuilt(ctx, in, err)
	default:
		// An error outside the kinds, from a Broadcaster of the caller's own:
		// treated as an unknown outcome, mark kept.
		return e.holdBuilt(ctx, in, err)
	}
}

// holdMarked keeps a Built intent Built and held after the hold was set on
// it: its inputs become permanent spent entries under holdTxID, the cause is
// recorded and returned. A spent set that could not be written is joined to
// the cause; the record still lands, and Recover re-marks the inputs from it.
func (e *Engine) holdMarked(ctx context.Context, in *Intent, cause error) error {
	if perr := e.Spent.MarkBroadcastFor(e.inputs(in), holdTxID(in), in.ID); perr != nil {
		cause = errors.Join(cause, perr)
	}
	return e.holdBuilt(ctx, in, cause)
}

// deferBuild keeps a Created intent Created after a Build attempt that did
// not settle it: the attempt and its cause are recorded and the cause is
// returned for the caller to retry Build later. Nothing is reserved or built.
func (e *Engine) deferBuild(ctx context.Context, in *Intent, cause error) error {
	in.Attempts++
	in.LastError = cause.Error()
	if saveErr := e.save(ctx, in, StateCreated); saveErr != nil {
		return errors.Join(cause, saveErr)
	}
	return cause
}

// holdBuilt keeps a Built intent Built after a Broadcast call that did not
// settle it, whether the send happened or was refused: the cause is recorded
// and returned. Nothing is released and the intent is not failed, because
// its bytes may be in a mempool. The reservation was renewed before the
// send, by Broadcast, and stays as it is.
func (e *Engine) holdBuilt(ctx context.Context, in *Intent, cause error) error {
	in.LastError = cause.Error()
	if saveErr := e.save(ctx, in, StateBuilt); saveErr != nil {
		return errors.Join(cause, saveErr)
	}
	return cause
}

// Process drives an intent from wherever it is to Broadcast in one call, or
// returns the error that stopped it. Safe to call repeatedly.
func (e *Engine) Process(ctx context.Context, id string) (*Intent, error) {
	in, ok, err := e.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: unknown intent %s", ErrInvalidIntent, id)
	}
	if in.State == StateCreated {
		beforeBuild()
		if err := e.Build(ctx, in); err != nil {
			// ErrWrongState here means another worker moved this intent
			// between the read above and Build's lock, to any later state. It
			// is returned rather than followed: that worker holds the intent,
			// and a later call reads it fresh.
			return in, err
		}
	}
	if in.State == StateBuilt {
		if err := e.Broadcast(ctx, in); err != nil {
			return in, err
		}
	}
	if in.State == StateFailed {
		// Terminal, and reached only when the intent was already Failed before
		// this call: a Build or Broadcast that fails it here returns its own
		// error above. Submit answers a Failed id with no error, so a caller
		// that retries a payout arrives here, and returning nil would report a
		// withdrawal as sent with an empty txid.
		return in, fmt.Errorf("%w: %s: %s", ErrFailed, in.ID, in.LastError)
	}
	return in, nil
}

// Recover is called once at startup. Broadcast intents have their inputs
// re-marked spent from the intent store, so a spent-set write that failed
// before the restart cannot re-expose them; broadcast entries owned by an
// abandoned intent are dropped, so an Abandon whose Forget failed cannot
// lock them. Reservations held for an intent
// the store knows as Created or Failed are released: the previous process
// stopped between reserving and persisting a Built intent, so nothing signed
// exists for them and the coins are free. A reservation held by an id the
// store does not know is kept and reported as ErrUnknownReservation: the
// store and the spent set are not the pair that was running. Built intents
// are re-reserved and re-broadcast with their persisted bytes; nothing is
// rebuilt, and one whose inputs cannot be re-reserved, because another
// withdrawal holds them or the set cannot be written, is reported
// (ErrReservationLost, or the write error) and not sent. Held intents are
// left as they are: their inputs are re-marked under the txid they are held
// under and nothing may be sent for them; each is logged and reported as
// ErrHeld so startup alerting sees it.
// It attempts every intent and returns every error joined, so errors.Is
// finds each kind.
//
// The passes that repair the spent set never touch the network and are not
// cut short by the caller's context: their store reads run under
// context.WithoutCancel(ctx), so a Recover started with an ended context
// still re-marks every Broadcast intent's inputs, releases the orphan
// reservations, and re-reserves every Built intent. Listing the Built
// intents is one of those reads: their re-reservation depends on it, and a
// store that bounds its queries with the context it is given (which is what
// a database store does, and what the file stores do not) would answer an
// ended context with an error, so nothing would be re-reserved. Only the
// sending runs under ctx. It stops once ctx has ended, and every Built
// intent stays Built, re-reserved, to be sent by the next Recover. The
// context's error is among those returned.
func (e *Engine) Recover(ctx context.Context) error {
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	repair := context.WithoutCancel(ctx)
	broadcast, err := e.Store.List(repair, StateBroadcast)
	if err != nil {
		return err
	}
	for _, in := range broadcast {
		if err := e.Spent.MarkBroadcastFor(e.inputs(in), in.TxID, in.ID); err != nil {
			note(fmt.Errorf("recover %s: %w", in.ID, err))
		}
	}
	// An abandoned intent whose Forget did not land still owns broadcast
	// entries; any other Failed owner's are a store that missed a send, kept.
	for _, id := range e.Spent.SentIntents() {
		in, ok, err := e.Store.Get(repair, id)
		if err != nil {
			note(fmt.Errorf("recover %s: entries kept, store read failed: %w", id, err))
			continue
		}
		if !ok || in.State != StateFailed {
			continue
		}
		if in.AbandonedAt.IsZero() {
			note(fmt.Errorf("recover %s: entries kept: a failed intent that was not abandoned owns spent entries; the store and the spent set disagree", id))
			continue
		}
		e.log().Warn("recover: dropping the spent entries of an abandoned intent", "intent", id)
		if err := e.Spent.Forget(id); err != nil {
			note(fmt.Errorf("recover %s: %w", id, err))
		}
	}
	note(e.releaseOrphanReservations(repair))
	built, err := e.Store.List(repair, StateBuilt)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, in := range built {
		if held := heldErr(in); held != nil {
			e.log().Warn("recover: intent held, resolve by hand", "intent", in.ID, "hold", in.Hold, "node_txid", in.NodeTxID, "txid", in.TxID)
			if err := e.Spent.MarkBroadcastFor(e.inputs(in), holdTxID(in), in.ID); err != nil {
				note(fmt.Errorf("recover %s: %w", in.ID, err))
			}
			note(fmt.Errorf("recover %s: %w", in.ID, held))
			continue
		}
		if err := e.Spent.Reserve(e.inputs(in), in.ID, e.reservationTTL()); err != nil {
			// The inputs are held by another withdrawal, or the set could
			// not be written. Broadcast would refuse this intent for the same
			// reason; refusing it here keeps the re-reservation of every
			// Built intent ahead of any send and reports the cause once.
			e.log().Warn("recover: built intent not sent, inputs not re-reserved", "intent", in.ID, "err", err)
			note(fmt.Errorf("recover %s: not sent: %w", in.ID, notReserved(err)))
			continue
		}
		if err := ctx.Err(); err != nil {
			// The context stops the sending, not the re-reserving. Breaking
			// here left every Built intent after the first one unreserved,
			// against what this method's documentation promises, and their
			// inputs belong to signed transactions that may be in a mempool.
			note(fmt.Errorf("recover %s: not sent: %w", in.ID, err))
			continue
		}
		if err := e.Broadcast(ctx, in); err != nil {
			note(fmt.Errorf("recover %s: %w", in.ID, err))
		}
	}
	return errors.Join(errs...)
}

// releaseOrphanReservations drops every reservation whose intent the store
// knows as Created or Failed: nothing built exists for it. Reservations of
// Built intents are left for Recover to renew, and anything else (Broadcast,
// Confirmed) is left alone; Recover has already re-marked Broadcast inputs
// and a reservation on a Confirmed intent expires by its TTL. A reservation
// whose id the store does not know is kept and reported
// (ErrUnknownReservation), as is one whose store read failed: a stale hold
// is safe, a released input under a Built intent is not. Holds the Build
// lock so an in-flight Build cannot look like an orphan.
func (e *Engine) releaseOrphanReservations(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for _, id := range e.Spent.ReservedIntents() {
		in, ok, err := e.Store.Get(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("recover %s: reservation kept, store read failed: %w", id, err))
			continue
		}
		if !ok {
			errs = append(errs, fmt.Errorf("recover %s: reservation kept: %w", id, ErrUnknownReservation))
			continue
		}
		if in.State != StateCreated && in.State != StateFailed {
			continue
		}
		if err := e.Spent.Release(id); err != nil {
			errs = append(errs, fmt.Errorf("recover %s: release orphan reservation: %w", id, err))
			continue
		}
		e.log().Info("recover: released the reservation of an intent with nothing built", "intent", id, "state", in.State)
	}
	return errors.Join(errs...)
}

// UpdateConfirmations refreshes a Broadcast intent's confirmation count and
// marks it Confirmed at RequiredConfirmations. Requires a Confirmer and a
// positive RequiredConfirmations; with zero nothing would ever confirm and
// the spent set would only grow, so it is refused before the node is asked.
//
// A Confirmer that reports rpc.ErrUnknownOutcome says the node no longer
// knows the transaction; nothing changes, and the caller decides between
// Rebroadcast and, after the wait, Abandon.
//
// It runs under the engine's lock and acts on the stored record. A copy that
// says Broadcast while the store says Confirmed, because another loop
// confirmed it first, receives ErrWrongState and writes nothing: Confirmed is
// terminal, and a ledger acting on that transition must see it once.
func (e *Engine) UpdateConfirmations(ctx context.Context, in *Intent) error {
	if e.Confirmer == nil {
		return errors.New("withdraw: no Confirmer configured")
	}
	if e.RequiredConfirmations <= 0 {
		return fmt.Errorf("withdraw: RequiredConfirmations is %d; nothing would confirm", e.RequiredConfirmations)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	stored, err := e.reread(ctx, in, StateBroadcast)
	if err != nil {
		return err
	}
	n, err := e.Confirmer.Confirmations(ctx, stored.TxID)
	if err != nil {
		return err
	}
	*in = *stored
	in.Confirmations = n
	var confirmErr error
	if n >= e.RequiredConfirmations {
		in.State = StateConfirmed
		// One write for all inputs. On failure the entries stay unconfirmed on
		// disk (spent inputs stay excluded; only pruning is delayed).
		confirmErr = e.Spent.ConfirmSpentAll(e.inputs(in))
	}
	if err := e.save(ctx, in, StateBroadcast); err != nil {
		return errors.Join(confirmErr, err)
	}
	return confirmErr
}

// Rebroadcast sends a Broadcast intent's stored bytes again, for a
// transaction the node no longer knows (UpdateConfirmations returned
// rpc.ErrUnknownOutcome). The same bytes go out and nothing else may. The
// state does not change on any outcome: success says the node has it again,
// a refusal says the node does not have it now, which is not proof that no
// peer does. A refusal is recorded in LastError and returned, and does not
// move SentAt, so it does not restart Abandon's wait; a send the node took,
// or whose outcome is unknown, does. As in Broadcast, the attempt is saved
// before the send and nothing is sent when that save fails; when the save
// after a refusal fails, the pre-send time stays on disk and Abandon waits
// from it, at most one more AbandonAfter. Runs under the engine's lock and
// acts on the stored record.
func (e *Engine) Rebroadcast(ctx context.Context, in *Intent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	record, err := e.reread(ctx, in, StateBroadcast)
	if err != nil {
		return err
	}
	*in = *record
	in.Attempts++
	sentAt := in.SentAt
	if sentAt.IsZero() {
		sentAt = in.UpdatedAt // a record from before SentAt existed
	}
	in.SentAt = e.now()
	if err := e.save(ctx, in, StateBroadcast); err != nil {
		return fmt.Errorf("record re-send %d of %s, nothing sent: %w", in.Attempts, in.ID, err)
	}
	_, err = e.Broadcaster.Broadcast(ctx, in.RawHex, in.TxID)
	if errors.Is(err, rpc.ErrAlreadyInChain) {
		err = nil // the node has these bytes mined
	}
	in.LastError = ""
	if err != nil {
		err = fmt.Errorf("rebroadcast %s: %w", in.ID, err)
		in.LastError = err.Error()
		if errors.Is(err, rpc.ErrPermanent) || errors.Is(err, rpc.ErrTransient) || errors.Is(err, rpc.ErrUnauthorized) {
			in.SentAt = sentAt // the node answered and did not take the bytes
		}
	}
	if saveErr := e.save(ctx, in, StateBroadcast); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return err
}

// Abandon is the operator's transition to Failed for an intent the engine
// will not move on its own: a held Built intent, or a Broadcast intent whose
// transaction the node no longer knows. It requires a Chain and refuses
// (ErrNotAbandonable, nothing changed) unless every check passes: AbandonAfter
// has passed since the last send (SentAt; UpdatedAt on a record without it),
// the node does not know TxID (nor NodeTxID when it is a txid), and every
// input is unspent at the node. A Chain error is returned as it is. On
// success the intent is Failed with its bytes kept, AbandonedAt and
// LastError record it, and every spent-set entry it owns is dropped
// (utxo.SpentSet.Forget), so the inputs return to selection; logged at Warn.
//
// The wait is the node's mempool expiry; the residual risk is a peer that
// kept the bytes longer, and the record keeps the txid for the reconciler.
// A Built intent that is not held is refused (ErrWrongState). Runs under the
// engine's lock, node calls included, on the stored record, so the second of
// two operators abandoning one intent reads Failed and is refused.
func (e *Engine) Abandon(ctx context.Context, in *Intent) error {
	if e.Chain == nil {
		return errors.New("withdraw: no Chain configured")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	stored, err := e.reread(ctx, in, StateBuilt, StateBroadcast)
	if err != nil {
		return err
	}
	if stored.State == StateBuilt && heldErr(stored) == nil {
		return fmt.Errorf("%w: %s is Built and not held", ErrWrongState, in.ID)
	}
	*in = *stored
	lastSent := in.SentAt
	if lastSent.IsZero() {
		lastSent = in.UpdatedAt
	}
	if wait, since := e.abandonAfter(), e.now().Sub(lastSent); since < wait {
		return fmt.Errorf("%w: %s was last sent %s ago; the wait is %s", ErrNotAbandonable, in.ID, since.Round(time.Second), wait)
	}
	for _, txid := range []string{in.TxID, in.NodeTxID} {
		if txid == "" || txid == NodeTxIDUnknown {
			continue
		}
		known, err := e.Chain.KnowsTransaction(ctx, txid)
		if err != nil {
			return fmt.Errorf("abandon %s: %w", in.ID, err)
		}
		if known {
			return fmt.Errorf("%w: %s: the node knows %s", ErrNotAbandonable, in.ID, txid)
		}
	}
	for _, o := range in.Inputs {
		unspent, err := e.Chain.Unspent(ctx, o.TxID, o.Vout)
		if err != nil {
			return fmt.Errorf("abandon %s: %w", in.ID, err)
		}
		if !unspent {
			return fmt.Errorf("%w: %s: input %s:%d is spent", ErrNotAbandonable, in.ID, o.TxID, o.Vout)
		}
	}
	from := in.State
	in.State, in.AbandonedAt = StateFailed, e.now()
	in.LastError = fmt.Sprintf("abandoned at %s from %s after %s: the node does not know the transaction and every input is unspent", in.AbandonedAt.Format(time.RFC3339), from, e.abandonAfter())
	if err := e.save(ctx, in, from); err != nil {
		return err
	}
	e.log().Warn("abandoned: inputs return to selection", "intent", in.ID, "txid", in.TxID, "node_txid", in.NodeTxID, "from", from)
	if err := e.Spent.Forget(in.ID); err != nil {
		return fmt.Errorf("abandon %s: Failed, spent set not written: %w", in.ID, err)
	}
	return nil
}

// release drops an intent's reservation. A write failure here leaves a stale
// reservation on disk that expires by its TTL; nothing is at risk, so it is
// logged rather than returned.
func (e *Engine) release(in *Intent) {
	if err := e.Spent.Release(in.ID); err != nil {
		e.log().Warn("release reservation failed", "intent", in.ID, "err", err)
	}
}

// fail records the intent as Failed over its stored state from, then releases
// its reservation. The verdict lands before the inputs are freed: were the
// release first and the save then failed, the store would hold a Built intent
// with signed bytes whose inputs the next withdrawal could select. With the
// save first, a failed save leaves the intent as it was, inputs held; on the
// Broadcast path the next attempt holds the intent, since the store carries
// the mark set before the send.
func (e *Engine) fail(ctx context.Context, in *Intent, cause error, keepTx bool, from State) error {
	in.State = StateFailed
	in.LastError = cause.Error()
	if !keepTx {
		in.RawHex, in.TxID, in.Inputs = "", "", nil
	}
	if err := e.save(ctx, in, from); err != nil {
		return errors.Join(cause, err)
	}
	e.release(in)
	return cause
}

// notReserved wraps a Reserve failure for a Built intent that is therefore
// not sent: another withdrawal holding an input is ErrReservationLost, and a
// set that could not be written is reported as the write error it is.
func notReserved(err error) error {
	if errors.Is(err, utxo.ErrAlreadyReserved) {
		return fmt.Errorf("%w: %w", ErrReservationLost, err)
	}
	return err
}

func (e *Engine) inputs(in *Intent) []types.UTXO {
	out := make([]types.UTXO, 0, len(in.Inputs))
	for _, o := range in.Inputs {
		out = append(out, types.UTXO{TxID: o.TxID, Vout: o.Vout, Value: o.Value, Address: o.Address})
	}
	return out
}

// RPCConfirmer implements Confirmer over the node: getrawtransaction verbose
// (without -txindex the node serves a mined transaction only while one of its
// outputs is unspent), falling back to gettxout on the first output.
type RPCConfirmer struct{ Client *rpc.Client }

// Confirmations implements Confirmer.
func (c RPCConfirmer) Confirmations(ctx context.Context, txid string) (int64, error) {
	raw, err := c.Client.Call(ctx, "getrawtransaction", txid, true)
	if err == nil {
		var v struct {
			Confirmations int64 `json:"confirmations"`
		}
		if uerr := unmarshal(raw, &v); uerr == nil {
			return v.Confirmations, nil
		}
	} else if errors.Is(err, rpc.ErrTransient) {
		return 0, err
	}
	out, err := c.Client.GetTxOut(ctx, txid, 0, true)
	if err != nil {
		return 0, err
	}
	if out == nil {
		return 0, fmt.Errorf("%w: %s is not known to the node (no txindex and every output spent, or never accepted)", rpc.ErrUnknownOutcome, txid)
	}
	return out.Confirmations, nil
}

// RPCChain implements Chain over the node, each answer after RequireSynced,
// since a "no" from a node behind its headers would let Abandon free the
// inputs of a transaction the network has. KnowsTransaction is
// getrawtransaction, then gettxout on output 0; Unspent is gettxout with the
// mempool included. Without -txindex a spent-out mined transaction is unknown
// to the first; the inputs check refuses the abandon.
type RPCChain struct{ Client *rpc.Client }

// KnowsTransaction implements Chain.
func (c RPCChain) KnowsTransaction(ctx context.Context, txid string) (bool, error) {
	if err := c.Client.RequireSynced(ctx); err != nil {
		return false, err
	}
	return c.Client.KnowsTransaction(ctx, txid)
}

// Unspent implements Chain.
func (c RPCChain) Unspent(ctx context.Context, txid string, vout uint32) (bool, error) {
	if err := c.Client.RequireSynced(ctx); err != nil {
		return false, err
	}
	out, err := c.Client.GetTxOut(ctx, txid, vout, true)
	if err != nil {
		return false, err
	}
	return out != nil, nil
}
