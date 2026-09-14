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
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

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

// Store persists intents. Put must be durable before it returns: the engine
// relies on "persisted, then broadcast" to make recovery safe.
type Store interface {
	Get(id string) (*Intent, bool, error)
	Put(intent *Intent) error
	List(states ...State) ([]*Intent, error)
}

// Broadcaster sends a signed transaction and reports the outcome using the
// rpc error kinds. *rpc.Client satisfies it.
type Broadcaster interface {
	Broadcast(rawHex, txid string) (string, error)
}

// Confirmer reports how many confirmations a transaction has (0 for mempool).
// It is optional; without it intents stay in StateBroadcast.
type Confirmer interface {
	Confirmations(txid string) (int64, error)
}

// Selector chooses inputs for an amount at a fee rate. It must honour the
// engine's SpentSet (utxo.CoinSelector does) so reserved inputs are skipped.
type Selector func(amount, feeRate int64) ([]types.UTXO, error)

// BuildSigner turns selected inputs into a signed transaction. tx.BuildAndSign
// wrapped with the exchange's scripts and signer is the expected value; it
// verifies every input against the node's rules before returning, so a
// transaction the node would refuse fails Build and never reaches Broadcast.
type BuildSigner func(inputs []types.UTXO, toAddress string, amount, feeRate int64) (rawHex, txid string, err error)

// Engine drives intents through the state machine.
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

	mu sync.Mutex // serialises Build across intents so two cannot pick the same inputs between select and reserve
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
	// ErrReservationLost is returned, wrapped around the broadcast error, when
	// a Built intent's inputs could not be re-reserved because another
	// withdrawal took them after the reservation expired. Both transactions
	// cannot confirm; the operator resolves which one the network took.
	ErrReservationLost = errors.New("withdraw: inputs of a built intent were taken by another withdrawal")
	// ErrUnknownReservation is reported by Recover for a reservation held by
	// an intent id the store does not know. Process builds only intents that
	// Submit persisted, so an unknown id means the intent store and the spent
	// set are not the pair that was running: the intents file is missing or
	// older than the spent set (or Build was called on an intent that was
	// never submitted). The reservation is kept; a Built intent that the lost
	// store knew may have its bytes in a mempool.
	ErrUnknownReservation = errors.New("withdraw: reservation held by an intent the store does not know; intent store and spent set disagree")
)

// DefaultReservationTTL applies when Engine.ReservationTTL is zero.
const DefaultReservationTTL = 4 * time.Hour

func (e *Engine) now() time.Time { return time.Now().UTC() }

func (e *Engine) reservationTTL() time.Duration {
	if e.ReservationTTL > 0 {
		return e.ReservationTTL
	}
	return DefaultReservationTTL
}

func (e *Engine) save(in *Intent) error {
	in.UpdatedAt = e.now()
	return e.Store.Put(in)
}

// Submit registers a withdrawal. Calling it again with the same id returns
// the existing intent (created=false); with the same id and different
// parameters it returns ErrConflict.
func (e *Engine) Submit(id, address string, amount, feeRate int64) (intent *Intent, created bool, err error) {
	if id == "" || address == "" || amount <= 0 || feeRate <= 0 {
		return nil, false, fmt.Errorf("%w: id=%q address=%q amount=%d feeRate=%d", ErrInvalidIntent, id, address, amount, feeRate)
	}
	if existing, ok, err := e.Store.Get(id); err != nil {
		return nil, false, err
	} else if ok {
		if existing.Address != address || existing.Amount != amount || existing.FeeRate != feeRate {
			return existing, false, fmt.Errorf("%w: %s", ErrConflict, id)
		}
		return existing, false, nil
	}
	in := &Intent{ID: id, Address: address, Amount: amount, FeeRate: feeRate, State: StateCreated, CreatedAt: e.now()}
	if err := e.save(in); err != nil {
		return nil, false, err
	}
	return in, true, nil
}

// Build selects and reserves inputs, builds and signs the transaction, and
// persists it. The intent is Built and its inputs are reserved when this
// returns nil. Nothing has touched the network.
//
// A selector error that is rpc.ErrTransient (the node behind its headers,
// warming up, or unreachable) leaves the intent Created with the attempt
// recorded and is returned for a later Build; nothing is reserved. Any other
// selector error (insufficient funds, a wrong chain) fails the intent.
func (e *Engine) Build(in *Intent) error {
	if in.State != StateCreated {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	inputs, err := e.Select(in.Amount, in.FeeRate)
	if err != nil {
		err = fmt.Errorf("select inputs: %w", err)
		if errors.Is(err, rpc.ErrTransient) {
			return e.deferBuild(in, err)
		}
		return e.fail(in, err, false)
	}
	if err := e.Spent.Reserve(inputs, in.ID, e.reservationTTL()); err != nil {
		// Either another intent won the race for one of these inputs
		// (utxo.ErrAlreadyReserved; the caller retries Build and selection
		// skips them now) or the spent set could not be written
		// (utxo.ErrPersist; nothing is reserved and nothing is built until the
		// disk is fixed). Not a failure of this intent.
		return e.deferBuild(in, err)
	}
	rawHex, txid, err := e.BuildSign(inputs, in.Address, in.Amount, in.FeeRate)
	if err != nil {
		e.release(in)
		return e.fail(in, fmt.Errorf("build and sign: %w", err), false)
	}
	in.RawHex, in.TxID = rawHex, txid
	in.Inputs = in.Inputs[:0]
	for _, u := range inputs {
		in.Inputs = append(in.Inputs, Outpoint{TxID: u.TxID, Vout: u.Vout, Value: u.Value, Address: u.Address})
	}
	in.State = StateBuilt
	if err := e.save(in); err != nil {
		// Not durable, so it must not reach the network: release and report.
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
func (e *Engine) Broadcast(in *Intent) error {
	if in.State != StateBuilt {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	if in.NodeTxID != "" {
		return fmt.Errorf("%w: %s computed %s, node accepted %s", ErrHeld, in.ID, in.TxID, in.NodeTxID)
	}
	in.Attempts++
	got, err := e.Broadcaster.Broadcast(in.RawHex, in.TxID)
	switch {
	case err == nil:
		in.State = StateBroadcast
		in.LastError = ""
		perr := e.Spent.MarkBroadcastFor(e.inputs(in), in.TxID, in.ID)
		if perr != nil {
			perr = fmt.Errorf("%s broadcast as %s, spent set not written: %w", in.ID, in.TxID, perr)
			in.LastError = perr.Error()
		}
		if saveErr := e.save(in); saveErr != nil {
			return errors.Join(perr, saveErr)
		}
		return perr
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
		if saveErr := e.save(in); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	case errors.Is(err, rpc.ErrPermanent):
		e.release(in)
		return e.fail(in, err, true)
	default:
		// ErrUnknownOutcome or ErrTransient: the transaction may or may not be
		// out. Keep the reservation, keep the bytes, report, retry later.
		return e.holdBuilt(in, err)
	}
}

// deferBuild keeps a Created intent Created after a Build attempt that did
// not settle it: the attempt and its cause are recorded and the cause is
// returned for the caller to retry Build later. Nothing is reserved or built.
func (e *Engine) deferBuild(in *Intent, cause error) error {
	in.Attempts++
	in.LastError = cause.Error()
	if saveErr := e.save(in); saveErr != nil {
		return errors.Join(cause, saveErr)
	}
	return cause
}

// holdBuilt keeps a Built intent Built after a broadcast attempt that did not
// settle it: the reservation is renewed for another TTL so the inputs cannot
// be selected by a later withdrawal while this one is unresolved, the cause
// is recorded, and the cause is returned.
func (e *Engine) holdBuilt(in *Intent, cause error) error {
	if rerr := e.Spent.Reserve(e.inputs(in), in.ID, e.reservationTTL()); rerr != nil {
		// The reservation had expired and another withdrawal took an input.
		// Do not release anything and do not fail this intent: its bytes may
		// be in the mempool. Report both facts.
		cause = fmt.Errorf("%w: %v: %w", ErrReservationLost, rerr, cause)
	}
	in.LastError = cause.Error()
	if saveErr := e.save(in); saveErr != nil {
		return errors.Join(cause, saveErr)
	}
	return cause
}

// Process drives an intent from wherever it is to Broadcast in one call, or
// returns the error that stopped it. Safe to call repeatedly.
func (e *Engine) Process(id string) (*Intent, error) {
	in, ok, err := e.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: unknown intent %s", ErrInvalidIntent, id)
	}
	if in.State == StateCreated {
		if err := e.Build(in); err != nil {
			return in, err
		}
	}
	if in.State == StateBuilt {
		if err := e.Broadcast(in); err != nil {
			return in, err
		}
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
// rebuilt. Intents held after a txid mismatch are left as they are: their
// inputs are re-marked under the node's txid and nothing may be sent for
// them; each is logged and reported as ErrHeld so startup alerting sees it.
// It attempts every intent and returns every error joined, so errors.Is
// finds each kind.
func (e *Engine) Recover() error {
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	broadcast, err := e.Store.List(StateBroadcast)
	if err != nil {
		return err
	}
	for _, in := range broadcast {
		if err := e.Spent.MarkBroadcastFor(e.inputs(in), in.TxID, in.ID); err != nil {
			note(fmt.Errorf("recover %s: %w", in.ID, err))
		}
	}
	note(e.releaseOrphanReservations())
	built, err := e.Store.List(StateBuilt)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, in := range built {
		if in.NodeTxID != "" {
			log.Printf("[withdraw] recover %s: held, node accepted %s for computed %s; resolve by hand", in.ID, in.NodeTxID, in.TxID)
			if err := e.Spent.MarkBroadcastFor(e.inputs(in), in.NodeTxID, in.ID); err != nil {
				note(fmt.Errorf("recover %s: %w", in.ID, err))
			}
			note(fmt.Errorf("recover %s: %w: node accepted %s for computed %s", in.ID, ErrHeld, in.NodeTxID, in.TxID))
			continue
		}
		if err := e.Spent.Reserve(e.inputs(in), in.ID, e.reservationTTL()); err != nil {
			log.Printf("[withdraw] recover %s: re-reserve: %v", in.ID, err)
		}
		if err := e.Broadcast(in); err != nil {
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
func (e *Engine) releaseOrphanReservations() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for _, id := range e.Spent.ReservedIntents() {
		in, ok, err := e.Store.Get(id)
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
		log.Printf("[withdraw] recover %s: released the reservation of a %s intent, nothing built", id, in.State)
	}
	return errors.Join(errs...)
}

// UpdateConfirmations refreshes a Broadcast intent's confirmation count and
// marks it Confirmed at RequiredConfirmations. Requires a Confirmer.
func (e *Engine) UpdateConfirmations(in *Intent) error {
	if in.State != StateBroadcast {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	if e.Confirmer == nil {
		return errors.New("withdraw: no Confirmer configured")
	}
	n, err := e.Confirmer.Confirmations(in.TxID)
	if err != nil {
		return err
	}
	in.Confirmations = n
	var confirmErr error
	if n >= e.RequiredConfirmations && e.RequiredConfirmations > 0 {
		in.State = StateConfirmed
		// One write for all inputs. On failure the entries stay unconfirmed on
		// disk (spent inputs stay excluded; only pruning is delayed).
		confirmErr = e.Spent.ConfirmSpentAll(e.inputs(in))
	}
	if err := e.save(in); err != nil {
		return errors.Join(confirmErr, err)
	}
	return confirmErr
}

// release drops an intent's reservation. A write failure here leaves a stale
// reservation on disk that expires by its TTL; nothing is at risk, so it is
// logged rather than returned.
func (e *Engine) release(in *Intent) {
	if err := e.Spent.Release(in.ID); err != nil {
		log.Printf("[withdraw] %s: release reservation: %v", in.ID, err)
	}
}

func (e *Engine) fail(in *Intent, cause error, keepTx bool) error {
	in.State = StateFailed
	in.LastError = cause.Error()
	if !keepTx {
		in.RawHex, in.TxID, in.Inputs = "", "", nil
	}
	if err := e.save(in); err != nil {
		return errors.Join(cause, err)
	}
	return cause
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
func (c RPCConfirmer) Confirmations(txid string) (int64, error) {
	raw, err := c.Client.Call("getrawtransaction", txid, true)
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
	out, err := c.Client.GetTxOut(txid, 0, true)
	if err != nil {
		return 0, err
	}
	if out == nil {
		return 0, fmt.Errorf("%w: %s is not known to the node (no txindex and every output spent, or never accepted)", rpc.ErrUnknownOutcome, txid)
	}
	return out.Confirmations, nil
}
