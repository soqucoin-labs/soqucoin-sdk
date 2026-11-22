package electrumx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// MaxBoundedLimit is the most outputs blockchain.scripthash.listunspent_bounded
// answers in one reply (BOUND_MAX in the indexer's server/utxo_cache.py).
const MaxBoundedLimit = 1000

// ErrReplyTooLarge matches, with errors.Is, the refusal a server gives when a
// reply would pass its size limit (MAX_SEND, 10,000,000 bytes by default on
// the Soqucoin indexer). A listunspent of an address that holds more than
// about 60,000 outputs is refused this way on every attempt, and the error
// sits on the address's record (LastRefreshOf) until the address holds fewer.
// ListUnspentBounded reads such an address.
var ErrReplyTooLarge = errors.New("electrumx: the server refused a reply over its size limit")

// tooLargePrefix opens the message of that refusal. The server's RPC library
// words it "response too large (over 10,000,000 bytes".
const tooLargePrefix = "response too large"

// ServerError is the error member of a server's reply to one request. Error
// prints the member as the server sent it.
type ServerError struct {
	// Code and Message are the member's fields, zero and empty when the
	// member is not an object that carries them.
	Code    int
	Message string
	raw     string
}

func newServerError(raw json.RawMessage) *ServerError {
	e := &ServerError{raw: string(raw)}
	var member struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &member); err == nil {
		e.Code, e.Message = member.Code, member.Message
	}
	return e
}

func (e *ServerError) Error() string { return "electrumx error: " + e.raw }

// Is reports a size refusal as ErrReplyTooLarge.
func (e *ServerError) Is(target error) bool {
	return target == ErrReplyTooLarge && strings.HasPrefix(e.Message, tooLargePrefix)
}

// BoundedUnspent is one answer of blockchain.scripthash.listunspent_bounded.
type BoundedUnspent struct {
	// Outputs holds every unspent output of the address when it has no more
	// than the limit asked for, and exactly the limit when it has more. The
	// indexer then chooses them (bounded_utxos in its server/utxo_cache.py).
	// It walks the confirmed outputs largest first, then the pending ones
	// (height 0) largest first, and takes a transaction's outputs together
	// when all of them still fit. A transaction with more outputs than the
	// places left is passed over, and a later one that fits takes them, a
	// pending one included. Places still free go to the outputs left, in
	// the same order. The list comes back largest first whatever the
	// height, so confirmed outputs have no absolute priority: read Height
	// on each entry. Address is set on each entry. AssetType stays 0
	// whatever the output holds: as in the client's cache, the indexer's
	// label is not read, and the node's answer tells the asset.
	Outputs []types.UTXO
	// Total and TotalValue count every unspent output of the address at the
	// moment of the read, in outputs and in shors: pending ones included,
	// outputs spent in the mempool left out.
	Total      int64
	TotalValue int64
}

// ListUnspentBounded asks the indexer for at most limit unspent outputs of
// addr, with the count and the value of all of them. limit is 1 to
// MaxBoundedLimit. The indexer serves the method from tag soqucoin-1.19.0-3;
// an older one answers a ServerError for an unknown method.
//
// The answer goes to the caller and nowhere else. The client's cache holds
// whole sets (GetBalance and the selector read it), so a bounded list never
// enters it, and addr does not have to be tracked. The client's network
// prefix does have to be set, by SetHRP or by TrackAddresses, and addr has to
// carry it. Otherwise the call is refused and no request goes out, because
// the prefix is what the genesis check compares the server's chain with.
//
// The figures are the indexer's. As with every output the indexer reports,
// confirm an output with the node (gettxout) before crediting or spending it.
// A reply that does not add up is refused whole: an entry parseUnspent would
// refuse, a list that is not min(total, limit) long, or a total value the
// listed outputs and the node's per-output ceiling cannot account for.
//
// The indexer can report one outpoint twice for a moment after a block (at
// its height and as pending, until its mempool refresh ends). A reply whose
// list holds both is refused, and the next read answers. When the list does
// not hold both, Total and TotalValue count that output twice until then.
func (c *Client) ListUnspentBounded(ctx context.Context, addr string, limit int) (BoundedUnspent, error) {
	if limit < 1 || limit > MaxBoundedLimit {
		return BoundedUnspent{}, fmt.Errorf("electrumx: limit %d is outside 1 to %d", limit, MaxBoundedLimit)
	}
	scriptHash, err := address.AddressToScriptHash(c.hrp(), addr)
	if err != nil {
		return BoundedUnspent{}, fmt.Errorf("address to script hash: %w", err)
	}
	raw, err := c.call(ctx, "blockchain.scripthash.listunspent_bounded", []interface{}{scriptHash, limit})
	if err != nil {
		return BoundedUnspent{}, fmt.Errorf("listunspent_bounded: %w", err)
	}
	answer, err := parseBounded(raw, limit)
	if err != nil {
		return BoundedUnspent{}, fmt.Errorf("listunspent_bounded: parse: %w", err)
	}
	for i := range answer.Outputs {
		answer.Outputs[i].Address = addr
	}
	return answer, nil
}

// parseBounded is the one reader of a listunspent_bounded result.
func parseBounded(raw json.RawMessage, limit int) (BoundedUnspent, error) {
	var reply struct {
		Outputs    json.RawMessage `json:"outputs"`
		Total      *int64          `json:"total"`
		TotalValue *int64          `json:"total_value"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return BoundedUnspent{}, err
	}
	// A null result leaves all three unset.
	if reply.Outputs == nil || reply.Total == nil || reply.TotalValue == nil {
		return BoundedUnspent{}, errors.New("outputs, total or total_value is missing")
	}
	outputs, err := parseUnspent(reply.Outputs)
	if err != nil {
		return BoundedUnspent{}, fmt.Errorf("outputs: %w", err)
	}
	total, value := *reply.Total, *reply.TotalValue
	listed := int64(len(outputs))
	// A negative total fails here too: no list is that long.
	if want := min(total, int64(limit)); listed != want {
		return BoundedUnspent{}, fmt.Errorf("%d outputs listed for a total of %d and a limit of %d", listed, total, limit)
	}
	// parseUnspent has refused a list whose values pass an int64.
	var sum int64
	for i := range outputs {
		sum += outputs[i].Value
	}
	// Each output left out of the list is worth 0 to the per-output ceiling,
	// so the value past the listed sum is 0 to that many ceilings. The count
	// is compared by division: five ceilings pass an int64.
	rest, extra := total-listed, value-sum
	if value < sum || ceilings(extra) > rest {
		return BoundedUnspent{}, fmt.Errorf("total_value %d does not fit %d listed outputs worth %d and %d more", value, listed, sum, rest)
	}
	return BoundedUnspent{Outputs: outputs, Total: total, TotalValue: value}, nil
}

// ceilings returns the fewest outputs that can hold v shors, v at least 0.
func ceilings(v int64) int64 {
	n := v / types.MaxMoney
	if v%types.MaxMoney > 0 {
		n++
	}
	return n
}
