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
//	   │                 │        │ (unknown outcome: stay Built, retry same hex)
//	   │                 │        └──(rejected)──► Failed (inputs released)
//	   └──(cannot build)─┴──────────────────────► Failed
//
// Recover re-drives Built intents after a restart with the same bytes. It
// never rebuilds. The engine is agnostic about where coins come from and how
// they are signed: those are injected so the exchange can wire its own
// ElectrumX client, key manager and node.
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

	State         State      `json:"state"`
	TxID          string     `json:"txid,omitempty"`
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
// wrapped with the exchange's scripts and signer is the expected value.
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
	// is built but not yet broadcast. Recover re-reserves on restart.
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
)

func (e *Engine) now() time.Time { return time.Now().UTC() }

func (e *Engine) reservationTTL() time.Duration {
	if e.ReservationTTL > 0 {
		return e.ReservationTTL
	}
	return 15 * time.Minute
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
func (e *Engine) Build(in *Intent) error {
	if in.State != StateCreated {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	inputs, err := e.Select(in.Amount, in.FeeRate)
	if err != nil {
		return e.fail(in, fmt.Errorf("select inputs: %w", err), false)
	}
	if err := e.Spent.Reserve(inputs, in.ID, e.reservationTTL()); err != nil {
		// Another intent won the race for one of these inputs. Not a failure of
		// this intent; the caller retries Build and selection skips them now.
		in.Attempts++
		in.LastError = err.Error()
		_ = e.save(in)
		return err
	}
	rawHex, txid, err := e.BuildSign(inputs, in.Address, in.Amount, in.FeeRate)
	if err != nil {
		e.Spent.Release(in.ID)
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
		e.Spent.Release(in.ID)
		in.State, in.RawHex, in.TxID, in.Inputs = StateCreated, "", "", nil
		return fmt.Errorf("persist built intent %s: %w", in.ID, err)
	}
	return nil
}

// Broadcast sends a Built intent's transaction. On success the intent is
// Broadcast and its inputs are permanently marked spent. On an unknown
// outcome the intent stays Built with the attempt recorded, and the caller
// calls Broadcast again later: the same bytes go out, never a new transaction.
// On a permanent rejection the intent is Failed and its inputs released.
func (e *Engine) Broadcast(in *Intent) error {
	if in.State != StateBuilt {
		return fmt.Errorf("%w: %s is %s", ErrWrongState, in.ID, in.State)
	}
	in.Attempts++
	_, err := e.Broadcaster.Broadcast(in.RawHex, in.TxID)
	switch {
	case err == nil:
		in.State = StateBroadcast
		in.LastError = ""
		e.Spent.MarkBroadcastFor(e.inputs(in), in.TxID, in.ID)
		return e.save(in)
	case errors.Is(err, rpc.ErrPermanent):
		e.Spent.Release(in.ID)
		return e.fail(in, err, true)
	default:
		// ErrUnknownOutcome or ErrTransient: the transaction may or may not be
		// out. Keep the reservation, keep the bytes, report, retry later.
		in.LastError = err.Error()
		if saveErr := e.save(in); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	}
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

// Recover is called once at startup. Built intents are re-reserved and
// re-broadcast with their persisted bytes; nothing is rebuilt. Broadcast
// intents are left for UpdateConfirmations. It returns the first error but
// attempts every intent.
func (e *Engine) Recover() error {
	built, err := e.Store.List(StateBuilt)
	if err != nil {
		return err
	}
	var firstErr error
	for _, in := range built {
		if err := e.Spent.Reserve(e.inputs(in), in.ID, e.reservationTTL()); err != nil {
			log.Printf("[withdraw] recover %s: re-reserve: %v", in.ID, err)
		}
		if err := e.Broadcast(in); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("recover %s: %w", in.ID, err)
		}
	}
	return firstErr
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
	if n >= e.RequiredConfirmations && e.RequiredConfirmations > 0 {
		in.State = StateConfirmed
		for _, o := range in.Inputs {
			e.Spent.ConfirmSpent(o.TxID, o.Vout)
		}
	}
	return e.save(in)
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
// (requires -txindex for mined transactions), falling back to gettxout on the
// first output.
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
		return 0, fmt.Errorf("%w: %s is not known to the node (no txindex and first output spent, or never accepted)", rpc.ErrUnknownOutcome, txid)
	}
	return out.Confirmations, nil
}
