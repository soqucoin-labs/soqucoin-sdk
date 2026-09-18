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
//     reservation becomes a permanent spent entry at broadcast or is released
//     on a permanent failure.
//
// States and transitions:
//
//	Created ──Build──► Built ──Broadcast──► Broadcast ──Confirm──► Confirmed
//	   │                 │        │ (unknown outcome: stay Built, reservation
//	   │                 │        │  renewed, retry same hex)
//	   │                 │        │ (node accepted under another txid: stay
//	   │                 │        │  Built, NodeTxID recorded, inputs held)
//	   │                 │        └──(rejected)──► Failed (inputs released)
//	   ├──(selector transient: stay Created, attempt recorded, retry Build)
//	   └──(cannot build)─┴──────────────────────► Failed
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
	"sync"
	"time"

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
	StateFailed    State = "failed"    // permanently rejected or could not be built; inputs released
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
	// inputs reserved; an operator resolves it. Empty otherwise.
	NodeTxID      string     `json:"node_txid,omitempty"`
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
// Process, receive the caller's context. Every Update, the Get each of Build, Broadcast and
// UpdateConfirmations makes under the engine's lock to re-read the record it
// acts on, and the Get and List calls of Recover's repair passes, receive one
// that does not end when the caller's does (context.WithoutCancel, which also
// carries no deadline): the engine makes them to record what the network has
// already done, to decide what it may do, or to repair the spent set from the
// record, and a record cut short by the caller is a Built intent the store
// thinks is Created, or a held intent it thinks is sendable. A database-backed
// store bounds every method with its own timeout and does not rely on the
// context for that.
type Store interface {
	Get(ctx context.Context, id string) (*Intent, bool, error)
	Create(ctx context.Context, intent *Intent) error
	Update(ctx context.Context, intent *Intent, from State) error
	List(ctx context.Context, states ...State) ([]*Intent, error)
}

// Broadcaster sends a signed transaction and reports the outcome using the
// rpc error kinds. *rpc.Client satisfies it. A context that ends during the
// send must be reported as rpc.ErrUnknownOutcome or as the context's error,
// never as rpc.ErrPermanent: the bytes may be in a mempool. It is called with
// the engine's lock held and must not call back into the Engine.
type Broadcaster interface {
	Broadcast(ctx context.Context, rawHex, txid string) (string, error)
}

// Confirmer reports how many confirmations a transaction has (0 for mempool).
// It is optional; without it intents stay in StateBroadcast. It is called
// with the engine's lock held and must not call back into the Engine.
type Confirmer interface {
	Confirmations(ctx context.Context, txid string) (int64, error)
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
	// of intents that have nothing built.
	ReservationTTL time.Duration

	// Logger receives the conditions Recover finds and the release failures
	// that are not returned. nil discards.
	Logger *slog.Logger

	// mu covers every transition after Submit. Build, Broadcast and
	// UpdateConfirmations each take it, re-read the stored record, act on
	// that record and write it back naming the state they read, so two
	// workers holding copies of one intent cannot both act on it, and the
	// lock also keeps two Builds from picking the same inputs between select
	// and reserve. It is held across the selector, the signer, the
	// Broadcaster and the Confirmer, so none of those may call back into the
	// Engine.
	mu sync.Mutex
}

var (
	// ErrInvalidIntent is returned for a submission with an empty id, an empty
	// address or a non-positive amount.
	ErrInvalidIntent = errors.New("withdraw: invalid intent")
	// ErrWrongState is returned when an operation is applied to an intent in a
	// state that does not allow it.
	ErrWrongState = errors.New("withdraw: intent is not in a state that allows this")
	// ErrConflict is returned when an existing intent with the same id has a
	// different address, amount or fee rate: the idempotency key is being
	// reused for a different payment, which is a caller bug worth stopping.
	ErrConflict = errors.New("withdraw: intent id already used for a different withdrawal")
	// ErrHeld is returned by Broadcast for an intent the node accepted under a
	// different txid (NodeTxID is set). Nothing is sent: once the node mines
	// the payment it would report the same bytes as already in chain, and the
	// intent would silently become a Broadcast intent under a txid the node
	// does not know. An operator resolves it by hand.
	ErrHeld = errors.New("withdraw: intent is held after a txid mismatch; resolve by hand")
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
	// when it was called. Failed is terminal and nothing of it is on the
	// network. Submit answers the same id with no error, so a caller that
	// retries a payout would otherwise read a nil error from Process and
	// record a withdrawal that has no transaction behind it.
	//
	// It wraps rpc.ErrPermanent because one withdrawal that cannot succeed is
	// a fact about that withdrawal and not about the node or the indexer.
	// resilience.CircuitBreaker reads the wrapped sentinel and leaves itself
	// untouched, so a caller retrying one bad payout cannot halt every
	// withdrawal, which is what that breaker's own documentation says it
	// exists to prevent.
	//
	// A Failed intent reached through a permanent rejection at broadcast keeps
	// its TxID and RawHex, so Failed does not mean the bytes never reached a
	// node, only that the node refused them and the inputs were released.
	ErrFailed = fmt.Errorf("withdraw: intent failed permanently and was not sent (%w)", rpc.ErrPermanent)
)

// beforeBuild runs between Process's read of the intent and the Build call it
// makes, a variable so a test can occupy that window. Two workers that have
// both read one intent as Created before either builds is the race Build
// settles by re-reading the stored state under its lock, and this is the only
// place another goroutine can get between the read and the build.
var beforeBuild = func() {}

// DefaultReservationTTL applies when Engine.ReservationTTL is zero.
const DefaultReservationTTL = 4 * time.Hour

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

// save writes the intent over the stored record it was read from, which is
// in state from. The write is not bound to the caller's context: it records a
// fact the network already knows, or an attempt at one, and must land whether
// or not the caller is still waiting. Values on ctx are kept. Submit does not
// use it; see there.
func (e *Engine) save(ctx context.Context, in *Intent, from State) error {
	in.UpdatedAt = e.now()
	return e.Store.Update(context.WithoutCancel(ctx), in, from)
}

// reread returns the stored record of in.ID when it is in state want, or
// ErrWrongState naming the stored state, or ErrInvalidIntent for an id the
// store does not hold. Every transition after Submit calls it under e.mu
// before acting, because the caller's copy can be behind the store: another
// worker may have advanced the intent between the caller's read and the lock.
//
// The read is not bound to the caller's context. It decides what the engine
// may do, not what it asks of the network, and a store that honours the
// context would otherwise turn an ended context into a transition that never
// happened: a Broadcast that renews no reservation and records no attempt,
// where the same context ending a moment later, inside the send, is a lost
// reply that does both.
func (e *Engine) reread(ctx context.Context, in *Intent, want State) (*Intent, error) {
	stored, ok, err := e.Store.Get(context.WithoutCancel(ctx), in.ID)
	if err != nil {
		return nil, fmt.Errorf("re-read %s: %w", in.ID, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: unknown intent %s", ErrInvalidIntent, in.ID)
	}
	if stored.State != want {
		return nil, fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, stored.State)
	}
	return stored, nil
}

// Submit registers a withdrawal. Calling it again with the same id returns
// the existing intent (created=false); with the same id and different
// parameters it returns ErrConflict.
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
	if existing.Address != address || existing.Amount != amount || existing.FeeRate != feeRate {
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
// before signing is released.
func (e *Engine) Build(ctx context.Context, in *Intent) error {
	if in.State != StateCreated {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// The check above reads the caller's copy of the intent, which another
	// worker can have advanced between the caller's read and this lock. The
	// stored state is the one that decides. Without this re-read, two workers
	// each holding a Created copy of one intent both pass the check, and the
	// lock then serialises them into two builds: each selects different inputs
	// because the other's are reserved, each signs, and each broadcasts. That
	// pays the recipient twice from different inputs, which is the first of the
	// two failure modes this package exists to prevent.
	stored, err := e.reread(ctx, in, StateCreated)
	if err != nil {
		return err
	}
	// From here the stored record is the one worked on, so an attempt count
	// or a last error another worker recorded is carried rather than lost.
	*in = *stored

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
// is Failed and its inputs released. When the node accepted the transaction
// under a different txid (rpc.ErrTxIDMismatch) the payment is in the mempool:
// the inputs are marked spent under the node's txid so no later withdrawal
// can select them however long this takes to resolve, the intent stays Built
// with NodeTxID recording what the node said, and the error is returned for
// the operator. It is not a rejection and must not be treated as one.
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
// Broadcast runs under the engine's lock and acts on the stored record, not
// the caller's copy. A worker whose copy says Built while the store says
// Broadcast, because another worker sent it first, receives ErrWrongState
// and sends nothing; without that, the second sender's lost reply would renew
// a reservation over inputs the first had marked spent and save Built over
// the Broadcast the first had recorded.
//
// Before the send the intent's inputs are re-reserved for another TTL, so a
// Built intent retried at least once per ReservationTTL never loses them.
// When that fails, because another withdrawal holds an input after the
// reservation expired (ErrReservationLost) or the spent set cannot be
// written, nothing is sent: the intent stays Built with its bytes and the
// cause recorded, and the operator resolves it. This is the same rule
// Recover applies, at the one place a send can start.
func (e *Engine) Broadcast(ctx context.Context, in *Intent) error {
	if in.State != StateBuilt {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	stored, err := e.reread(ctx, in, StateBuilt)
	if err != nil {
		return err
	}
	*in = *stored
	if in.NodeTxID != "" {
		return fmt.Errorf("%w: %s computed %s, node accepted %s", ErrHeld, in.ID, in.TxID, in.NodeTxID)
	}
	if rerr := e.Spent.Reserve(e.inputs(in), in.ID, e.reservationTTL()); rerr != nil {
		// Nothing was asked of the network, so Attempts does not advance;
		// LastError carries the cause.
		return e.holdBuilt(ctx, in, notReserved(rerr))
	}
	in.Attempts++
	got, err := e.Broadcaster.Broadcast(ctx, in.RawHex, in.TxID)
	switch {
	case err == nil:
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
	case contextEnded(err):
		return e.holdBuilt(ctx, in, err)
	case errors.Is(err, rpc.ErrTxIDMismatch):
		if got == "" {
			// A Broadcaster that reports the kind without the node's txid
			// must still arm the hold; the hold is keyed on NodeTxID.
			got = "unknown"
		}
		in.NodeTxID = got
		in.LastError = err.Error()
		if perr := e.Spent.MarkBroadcastFor(e.inputs(in), got, in.ID); perr != nil {
			err = errors.Join(err, perr)
			in.LastError = err.Error()
		}
		if saveErr := e.save(ctx, in, StateBuilt); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	case errors.Is(err, rpc.ErrPermanent):
		return e.fail(ctx, in, err, true, StateBuilt)
	default:
		// ErrUnknownOutcome or ErrTransient: the transaction may or may not be
		// out. Keep the reservation, keep the bytes, report, retry later.
		return e.holdBuilt(ctx, in, err)
	}
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
// before the restart cannot re-expose them. Reservations held for an intent
// the store knows as Created or Failed are released: the previous process
// stopped between reserving and persisting a Built intent, so nothing signed
// exists for them and the coins are free. A reservation held by an id the
// store does not know is kept and reported as ErrUnknownReservation: the
// store and the spent set are not the pair that was running. Built intents
// are re-reserved and re-broadcast with their persisted bytes; nothing is
// rebuilt, and one whose inputs cannot be re-reserved, because another
// withdrawal holds them or the set cannot be written, is reported
// (ErrReservationLost, or the write error) and not sent. Intents held after
// a txid mismatch are left as they are: their
// inputs are re-marked under the node's txid and nothing may be sent for
// them; each is logged and reported as ErrHeld so startup alerting sees it.
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
	note(e.releaseOrphanReservations(repair))
	built, err := e.Store.List(repair, StateBuilt)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, in := range built {
		if in.NodeTxID != "" {
			e.log().Warn("recover: intent held after a txid mismatch, resolve by hand", "intent", in.ID, "node_txid", in.NodeTxID, "txid", in.TxID)
			if err := e.Spent.MarkBroadcastFor(e.inputs(in), in.NodeTxID, in.ID); err != nil {
				note(fmt.Errorf("recover %s: %w", in.ID, err))
			}
			note(fmt.Errorf("recover %s: %w: node accepted %s for computed %s", in.ID, ErrHeld, in.NodeTxID, in.TxID))
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
// marks it Confirmed at RequiredConfirmations. Requires a Confirmer.
//
// It runs under the engine's lock and acts on the stored record. A copy that
// says Broadcast while the store says Confirmed, because another loop
// confirmed it first, receives ErrWrongState and writes nothing: Confirmed is
// terminal, and a ledger acting on that transition must see it once.
func (e *Engine) UpdateConfirmations(ctx context.Context, in *Intent) error {
	if in.State != StateBroadcast {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	if e.Confirmer == nil {
		return errors.New("withdraw: no Confirmer configured")
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
	if n >= e.RequiredConfirmations && e.RequiredConfirmations > 0 {
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
// save first, a failed save leaves the intent as it was, inputs held, and the
// next attempt reaches the same verdict.
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
