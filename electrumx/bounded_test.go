package electrumx

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A listunspent of an address that holds more outputs than the server's
// reply limit allows is refused on every attempt, so the address never
// refreshes. ListUnspentBounded reads such an address: a list of bounded
// length with the count and the value of everything behind it. The count and
// the value are two figures a caller is tempted to act on with no list to
// check them against, so the first test here is a server that answers figures
// that do not add up.

const txC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// boundedStub answers listunspent_bounded for one address with whatever the
// test has stored last, and keeps the parameters of the last request.
type boundedStub struct {
	*scriptedStub
	answer atomic.Value // string: the result member
	params atomic.Value // []interface{}: the last request's parameters
	asked  atomic.Int64
}

func newBoundedStub(t *testing.T, sh string) *boundedStub {
	t.Helper()
	b := &boundedStub{}
	b.answer.Store(boundedReply("0", "0"))
	b.scriptedStub = newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "blockchain.scripthash.listunspent_bounded" && firstParam(req) == sh {
			b.asked.Add(1)
			ps, _ := req.Params.([]interface{})
			b.params.Store(ps)
			return []string{reply(req.ID, b.answer.Load().(string))}
		}
		return []string{reply(req.ID, `null`)}
	})
	return b
}

// boundedEntry is one entry as the indexer writes it, labels included.
func boundedEntry(txid string, vout int, value int64, height int) string {
	return fmt.Sprintf(`{"tx_hash":"%s","tx_pos":%d,"height":%d,"value":%d,"n_visibility":0,"n_asset_type":0}`,
		txid, vout, height, value)
}

func boundedReply(total, totalValue string, entries ...string) string {
	return `{"outputs":[` + strings.Join(entries, ",") + `],"total":` + total + `,"total_value":` + totalValue + `}`
}

func TestABoundedReplyThatDoesNotAddUpIsRefused(t *testing.T) {
	a := craftAddr(t, 0x61)
	stub := newBoundedStub(t, scripthashOf(t, a))
	c := connect(t, stub.scriptedStub)

	big := boundedEntry(txA, 0, 100, 10)
	small := boundedEntry(txB, 1, 50, 0)
	third := boundedEntry(txC, 2, 25, 10)
	ceiling := types.MaxMoney
	num := func(v int64) string { return strconv.FormatInt(v, 10) }

	// Every case is asked with a limit of 2.
	cases := []struct {
		name  string
		reply string
		want  string // a part of the refusal, so each case meets the check it names
	}{
		{"more outputs than the limit", boundedReply("3", "175", big, small, third), "3 outputs listed for a total of 3 and a limit of 2"},
		{"more outputs than the total", boundedReply("1", "150", big, small), "2 outputs listed for a total of 1"},
		{"fewer outputs than the limit with more behind", boundedReply("70007", "100", big), "1 outputs listed for a total of 70007"},
		{"fewer outputs than the total under the limit", boundedReply("2", "100", big), "1 outputs listed for a total of 2"},
		{"no output for a total of five", boundedReply("5", "0"), "0 outputs listed for a total of 5"},
		{"a negative total", boundedReply("-1", "0"), "0 outputs listed for a total of -1"},
		{"a total value below the listed outputs", boundedReply("2", "149", big, small), "total_value 149 does not fit"},
		{"a total value above the listed outputs with none left out", boundedReply("2", "151", big, small), "total_value 151 does not fit"},
		{"one shor more than the one output left out can hold", boundedReply("3", num(150+ceiling+1), big, small), "does not fit 2 listed outputs worth 150 and 1 more"},
		{"a negative total value", boundedReply("0", "-1"), "total_value -1 does not fit"},
		{"the lowest total value an int64 holds", boundedReply("2", "-9223372036854775808", big, small), "does not fit"},
		{"no total", `{"outputs":[],"total_value":0}`, "is missing"},
		{"no total value", `{"outputs":[],"total":0}`, "is missing"},
		{"no outputs", `{"total":0,"total_value":0}`, "is missing"},
		{"a null total", `{"outputs":[],"total":null,"total_value":0}`, "is missing"},
		{"null outputs", `{"outputs":null,"total":0,"total_value":0}`, "outputs: result is null"},
		{"outputs that are an object", `{"outputs":{},"total":0,"total_value":0}`, "outputs: "},
		{"a null result", `null`, "is missing"},
		{"a list, as listunspent answers", oneUTXO, "cannot unmarshal"},
		{"a string result", `"ok"`, "cannot unmarshal"},
		{"a fractional total", boundedReply("2.5", "150", big, small), "cannot unmarshal"},
		{"a total written as a string", boundedReply(`"2"`, "150", big, small), "cannot unmarshal"},
		{"a total past an int64", boundedReply("9223372036854775808", "150", big, small), "cannot unmarshal"},
		{"an entry with a bad tx_hash", boundedReply("2", "150", big, boundedEntry("zz", 1, 50, 0)), "outputs: entry 1: tx_hash"},
		{"an entry with a negative value", boundedReply("2", "50", big, boundedEntry(txB, 1, -50, 0)), "outputs: entry 1: value -50"},
		{"an entry above the per-output ceiling", boundedReply("2", "100", big, boundedEntry(txB, 1, ceiling+1, 0)), "outputs: entry 1: value"},
		{"an entry with a negative height", boundedReply("2", "150", big, boundedEntry(txB, 1, 50, -1)), "outputs: entry 1: height -1"},
		{"one outpoint twice", boundedReply("2", "200", big, big), "appears twice"},
	}
	for _, tc := range cases {
		stub.answer.Store(tc.reply)
		got, err := c.ListUnspentBounded(context.Background(), a, 2)
		if err == nil {
			t.Errorf("%s: accepted: %+v", tc.name, got)
			continue
		}
		if !strings.Contains(err.Error(), "listunspent_bounded: parse: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refused by another check: %v", tc.name, err)
		}
		if got.Outputs != nil || got.Total != 0 || got.TotalValue != 0 {
			t.Errorf("%s: a refused reply still returned %+v", tc.name, got)
		}
	}
	if n := stub.asked.Load(); n != int64(len(cases)) {
		t.Fatalf("%d requests for %d cases", n, len(cases))
	}

	// A refusal leaves the client usable: the next good reply is answered.
	stub.answer.Store(boundedReply("3", "175", big, small))
	got, err := c.ListUnspentBounded(context.Background(), a, 2)
	if err != nil {
		t.Fatalf("the good reply after the refusals: %v", err)
	}
	if len(got.Outputs) != 2 || got.Total != 3 || got.TotalValue != 175 {
		t.Fatalf("the good reply after the refusals: %+v", got)
	}
}

func TestListUnspentBoundedAnswersTheListTheCountAndTheValue(t *testing.T) {
	a := craftAddr(t, 0x62)
	sh := scripthashOf(t, a)
	stub := newBoundedStub(t, sh)
	c := connect(t, stub.scriptedStub)
	ceiling := types.MaxMoney
	num := func(v int64) string { return strconv.FormatInt(v, 10) }

	// 70,007 outputs behind a limit of 2: one confirmed, one pending.
	stub.answer.Store(boundedReply("70007", "123456789",
		boundedEntry(txA, 0, 5000, 10), boundedEntry(txB, 1, 3000, 0)))
	got, err := c.ListUnspentBounded(context.Background(), a, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []types.UTXO{
		{TxID: txA, Vout: 0, Value: 5000, Height: 10, Address: a},
		{TxID: txB, Vout: 1, Value: 3000, Height: 0, Address: a},
	}
	if len(got.Outputs) != 2 || got.Outputs[0].Address != a || got.Outputs[1].Address != a || !sameSet(got.Outputs, want) {
		t.Fatalf("outputs: %+v", got.Outputs)
	}
	if got.Total != 70007 || got.TotalValue != 123456789 {
		t.Fatalf("total %d, total value %d", got.Total, got.TotalValue)
	}

	// The request carries the script hash and the limit and nothing else, so
	// the indexer applies no asset filter and the two totals cover the address.
	ps, _ := stub.params.Load().([]interface{})
	if len(ps) != 2 || ps[0] != sh || ps[1] != float64(2) {
		t.Fatalf("request parameters: %v", ps)
	}

	// A bounded list is not a whole set, so it never enters the cache the
	// balance and the selector read, and the address stays untracked.
	if n := c.UTXOCount(); n != 0 {
		t.Fatalf("%d outputs entered the cache", n)
	}
	if set := c.GetUTXOs(a); len(set) != 0 {
		t.Fatalf("the address has a cached set: %+v", set)
	}
	if at, recErr := c.LastRefreshOf(a); !at.IsZero() || recErr != nil {
		t.Fatalf("the address has a refresh record: %v, %v", at, recErr)
	}

	// The most the outputs left out can hold is one ceiling each, to the shor.
	accepted := []struct {
		name       string
		reply      string
		limit      int
		outputs    int
		total      int64
		totalValue int64
	}{
		{"one output left out, worth the ceiling",
			boundedReply("3", num(8000+ceiling), boundedEntry(txA, 0, 5000, 10), boundedEntry(txB, 1, 3000, 0)),
			2, 2, 3, 8000 + ceiling},
		{"two outputs left out, worth two ceilings",
			boundedReply("4", num(8000+2*ceiling), boundedEntry(txA, 0, 5000, 10), boundedEntry(txB, 1, 3000, 0)),
			2, 2, 4, 8000 + 2*ceiling},
		{"outputs left out that are worth nothing",
			boundedReply("9", "8000", boundedEntry(txA, 0, 5000, 10), boundedEntry(txB, 1, 3000, 0)),
			2, 2, 9, 8000},
		{"an address under the limit", boundedReply("1", "5000", boundedEntry(txA, 0, 5000, 10)), 200, 1, 1, 5000},
		{"an address at the limit", boundedReply("1", "5000", boundedEntry(txA, 0, 5000, 10)), 1, 1, 1, 5000},
		{"an address with no output", boundedReply("0", "0"), 200, 0, 0, 0},
	}
	for _, tc := range accepted {
		stub.answer.Store(tc.reply)
		got, err := c.ListUnspentBounded(context.Background(), a, tc.limit)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got.Outputs) != tc.outputs || got.Total != tc.total || got.TotalValue != tc.totalValue {
			t.Errorf("%s: %d outputs, total %d, total value %d", tc.name, len(got.Outputs), got.Total, got.TotalValue)
		}
	}
}

func TestListUnspentBoundedRefusesALimitOutsideOneToAThousand(t *testing.T) {
	a := craftAddr(t, 0x63)
	stub := newBoundedStub(t, scripthashOf(t, a))
	c := connect(t, stub.scriptedStub)

	for _, limit := range []int{0, -1, MaxBoundedLimit + 1} {
		if _, err := c.ListUnspentBounded(context.Background(), a, limit); err == nil || !strings.Contains(err.Error(), "outside 1 to 1000") {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	if _, err := c.ListUnspentBounded(context.Background(), "not-an-address", 200); err == nil {
		t.Error("an address that does not decode was asked for")
	}
	if n := stub.asked.Load(); n != 0 {
		t.Fatalf("%d requests went out for limits and an address the client refuses", n)
	}

	for _, limit := range []int{1, MaxBoundedLimit} {
		if _, err := c.ListUnspentBounded(context.Background(), a, limit); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		ps, _ := stub.params.Load().([]interface{})
		if len(ps) != 2 || ps[1] != float64(limit) {
			t.Fatalf("limit %d was asked as %v", limit, ps)
		}
	}
}

// Every call passes the genesis check, which compares the server's chain with
// the client's prefix and has nothing to compare while no prefix is set. The
// bounded read takes an address the client does not track, so it must not be
// the one call that reaches a server unchecked.
func TestABoundedReadWithNoPrefixOrAnotherNetworksAddressSendsNothing(t *testing.T) {
	a := craftAddr(t, 0x65)
	stub := newBoundedStub(t, scripthashOf(t, a))

	bare := NewClient(stub.addr(), time.Hour, nil)
	if err := bare.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(bare.Stop)
	if _, err := bare.ListUnspentBounded(context.Background(), a, 2); !errors.Is(err, address.ErrInvalidHRP) {
		t.Fatalf("a client with no prefix: %v", err)
	}

	// The same program under the mainnet prefix has the same script hash, so
	// the stub would count the request if one went out.
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = 0x65
	}
	mainnet, err := address.Encode(types.Mainnet.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	pinned := connect(t, stub.scriptedStub)
	if _, err := pinned.ListUnspentBounded(context.Background(), mainnet, 2); !errors.Is(err, address.ErrInvalidHRP) {
		t.Fatalf("a mainnet address on a stagenet client: %v", err)
	}
	if n := stub.asked.Load(); n != 0 {
		t.Fatalf("%d requests went out", n)
	}
	if _, err := pinned.ListUnspentBounded(context.Background(), a, 2); err != nil || stub.asked.Load() != 1 {
		t.Fatalf("the stagenet address on the stagenet client: %v, %d requests", err, stub.asked.Load())
	}

	// A prefix that arrives after the connection is checked on the call: a
	// server on another chain is refused before the bounded request goes out.
	var wrongAsked atomic.Int64
	wrong := newScriptedStub(t, types.Mainnet.GenesisHash, func(req request) []string {
		if req.Method == "blockchain.scripthash.listunspent_bounded" {
			wrongAsked.Add(1)
			return []string{reply(req.ID, boundedReply("0", "0"))}
		}
		return []string{reply(req.ID, `null`)}
	})
	late := NewClient(wrong.addr(), time.Hour, nil)
	if err := late.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(late.Stop)
	setHRP(t, late, types.Stagenet.HRP)
	if _, err := late.ListUnspentBounded(context.Background(), a, 2); !errors.Is(err, ErrGenesisMismatch) {
		t.Fatalf("a server on another chain: %v", err)
	}
	if n := wrongAsked.Load(); n != 0 {
		t.Fatalf("%d bounded requests reached a server on another chain", n)
	}
}

// SetHRP refuses a change of prefix only while addresses are tracked, and the
// bounded read and the history read take an address the client does not
// track. The prefix can therefore become another network's between the check
// of the address and the genesis gate, which then passes the server for the
// new prefix. A script hash carries nothing of the network, so the server's
// answer for the script would come back under an address of the first
// network: a stagenet server's outputs reported for a mainnet address.
//
// The two helpers take the prefix the address was read under, so this test
// stands in that state without racing for it: the client is on the stagenet
// prefix and connected to a stagenet server, and the address was read under
// the mainnet prefix.
func TestAReadUnderOnePrefixTakesNoAnswerFromAServerCheckedAgainstAnother(t *testing.T) {
	a, mainnet, stub := twoNetworkStub(t, 0x66)
	c := connect(t, stub.scriptedStub)
	ctx := context.Background()

	got, err := c.listUnspentBoundedUnder(ctx, types.Mainnet.HRP, mainnet, 2)
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a bounded read of a mainnet address answered by a stagenet server: %v", err)
	}
	if len(got.Outputs) != 0 || got.Total != 0 || got.TotalValue != 0 {
		t.Fatalf("the refused bounded read left %+v with the caller", got)
	}
	entries, err := c.getHistoryUnder(ctx, types.Mainnet.HRP, mainnet)
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a history read of a mainnet address answered by a stagenet server: %v", err)
	}
	if entries != nil {
		t.Fatalf("the refused history read left %+v with the caller", entries)
	}
	// Both requests went out and were answered: what is refused is the reply.
	if b, h := stub.bounded.Load(), stub.history.Load(); b != 1 || h != 1 {
		t.Fatalf("%d bounded and %d history requests reached the server, want one of each", b, h)
	}

	// The stagenet address of the same script is read as before.
	got, err = c.ListUnspentBounded(ctx, a, 2)
	if err != nil || len(got.Outputs) != 1 || got.Outputs[0].Address != a || got.Total != 1 || got.TotalValue != 100 {
		t.Fatalf("the stagenet address on the stagenet client: %+v, %v", got, err)
	}
	entries, err = c.GetHistory(ctx, a)
	if err != nil || len(entries) != 1 || entries[0].TxHash != txA || entries[0].Height != 10 {
		t.Fatalf("the stagenet address's history on the stagenet client: %+v, %v", entries, err)
	}
}

// The same state reached the way a deployment reaches it: one goroutine
// changes the prefix of a client that tracks nothing while another reads. The
// server is on stagenet, so no read of a mainnet address may be answered,
// wherever the change lands.
func TestNoMainnetAddressIsAnsweredByAStagenetServerWhileThePrefixChanges(t *testing.T) {
	_, mainnet, stub := twoNetworkStub(t, 0x67)
	c := connect(t, stub.scriptedStub)
	ctx := context.Background()

	stop := make(chan struct{})
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.SetHRP(types.Mainnet.HRP)
			runtime.Gosched()
			_ = c.SetHRP(types.Stagenet.HRP)
			runtime.Gosched()
		}
	}()
	defer func() {
		close(stop)
		<-flipped
	}()

	for i := 0; i < 300; i++ {
		if got, err := c.ListUnspentBounded(ctx, mainnet, 2); err == nil {
			t.Fatalf("read %d: a stagenet server's outputs came back for a mainnet address: %+v", i, got)
		}
		if entries, err := c.GetHistory(ctx, mainnet); err == nil {
			t.Fatalf("read %d: a stagenet server's history came back for a mainnet address: %+v", i, entries)
		}
	}
}

// The check behind both: a reply is accepted on the record of a genesis check
// of its own connection against the prefix its address was read under, and on
// no other record.
func TestAReplyIsAcceptedOnlyOnTheCheckOfItsOwnConnectionAndPrefix(t *testing.T) {
	ctx := context.Background()

	// A client never checked holds an empty record, which is not a check of
	// generation 0 against no prefix.
	bare := NewClient("127.0.0.1:1", time.Second, nil)
	if err := bare.replyIsFor(ctx, 0, ""); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("the empty record of a client never checked: %v", err)
	}

	_, _, stub := twoNetworkStub(t, 0x68)
	c := connect(t, stub.scriptedStub)
	_, gen, err := c.callGen(ctx, "server.ping", []interface{}{})
	if err != nil || gen == 0 {
		t.Fatalf("a call on the first connection: generation %d, %v", gen, err)
	}
	if err := c.replyIsFor(ctx, gen, types.Stagenet.HRP); err != nil {
		t.Fatalf("the connection's own check: %v", err)
	}
	if err := c.replyIsFor(ctx, gen, types.Mainnet.HRP); !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a prefix the server was not checked against: %v", err)
	}

	// After a reconnect the record is the new connection's. A reply that came
	// over the old one is refused under the right prefix too, and as a lost
	// connection: the next read answers.
	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if err := c.replyIsFor(ctx, gen, types.Stagenet.HRP); !errors.Is(err, ErrNotConnected) || errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a reply of the replaced connection: %v", err)
	}
	_, next, err := c.callGen(ctx, "server.ping", []interface{}{})
	if err != nil || next == gen {
		t.Fatalf("a call on the second connection: generation %d after %d, %v", next, gen, err)
	}
	if err := c.replyIsFor(ctx, next, types.Stagenet.HRP); err != nil {
		t.Fatalf("the second connection's own check: %v", err)
	}

	// A context that has ended returns its own error and accepts nothing.
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.replyIsFor(ended, next, types.Stagenet.HRP); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended context: %v", err)
	}
}

// twoNetworkStub is a stagenet server that holds one output and one history
// entry for a script, with the stagenet and the mainnet address of that
// script. The two addresses have one script hash.
type networkStub struct {
	*scriptedStub
	bounded, history atomic.Int64
}

func twoNetworkStub(t *testing.T, fill byte) (stagenet, mainnet string, stub *networkStub) {
	t.Helper()
	stagenet = craftAddr(t, fill)
	sh := scripthashOf(t, stagenet)
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = fill
	}
	mainnet, err := address.Encode(types.Mainnet.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := address.AddressToScriptHash(types.Mainnet.HRP, mainnet); err != nil || got != sh {
		t.Fatalf("the mainnet address's script hash is %s, %v, and the stagenet one's is %s", got, err, sh)
	}
	stub = &networkStub{}
	stub.scriptedStub = newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if firstParam(req) == sh {
			switch req.Method {
			case "blockchain.scripthash.listunspent_bounded":
				stub.bounded.Add(1)
				return []string{reply(req.ID, boundedReply("1", "100", boundedEntry(txA, 0, 100, 10)))}
			case "blockchain.scripthash.get_history":
				stub.history.Add(1)
				return []string{reply(req.ID, `[{"tx_hash":"`+txA+`","height":10}]`)}
			}
		}
		return []string{reply(req.ID, `null`)}
	})
	return stagenet, mainnet, stub
}

// The refusal an address over the reply limit gets is one an operator has to
// tell from every other failure, because no retry ends it. It reaches the
// caller of RefreshAll and the address's record as ErrReplyTooLarge, with the
// text unchanged, and the bounded read answers for the same address.
func TestTheSizeRefusalIsErrReplyTooLargeOnTheAddressRecord(t *testing.T) {
	a := craftAddr(t, 0x64)
	sh := scripthashOf(t, a)
	const refusal = `{"code":-32600,"message":"response too large (over 10,000,000 bytes"}`
	var member atomic.Value
	member.Store(refusal)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		switch {
		case req.Method == "blockchain.scripthash.listunspent" && firstParam(req) == sh:
			return []string{fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":%s}`, req.ID, member.Load().(string))}
		case req.Method == "blockchain.scripthash.listunspent_bounded" && firstParam(req) == sh:
			return []string{reply(req.ID, boundedReply("70007", "123456789", boundedEntry(txA, 0, 5000, 10)))}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}

	err := c.RefreshAll(context.Background())
	if !errors.Is(err, ErrReplyTooLarge) {
		t.Fatalf("RefreshAll's error is not ErrReplyTooLarge: %v", err)
	}
	if errors.Is(err, ErrNotConnected) {
		t.Fatalf("the size refusal also matches ErrNotConnected: %v", err)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Code != -32600 || se.Message != "response too large (over 10,000,000 bytes" {
		t.Fatalf("the server's error member did not come through: %+v", se)
	}
	if !strings.Contains(err.Error(), "electrumx error: "+refusal) {
		t.Fatalf("the error text changed: %v", err)
	}
	at, recErr := c.LastRefreshOf(a)
	if !at.IsZero() || !errors.Is(recErr, ErrReplyTooLarge) {
		t.Fatalf("the address's record: %v, %v", at, recErr)
	}

	// The bounded read answers for the address listunspent is refused for.
	got, err := c.ListUnspentBounded(context.Background(), a, 1)
	if err != nil || len(got.Outputs) != 1 || got.Total != 70007 {
		t.Fatalf("the bounded read of the same address: %+v, %v", got, err)
	}

	// Any other error member is a ServerError and is not the size refusal.
	for _, other := range []string{
		`{"code":1,"message":"boom"}`,
		`{"code":-32600,"message":"a response too large for the peer"}`,
		`{"code":-32600}`,
		`"response too large (over 10,000,000 bytes"`,
		`7`,
	} {
		member.Store(other)
		err := c.RefreshAll(context.Background())
		if err == nil || errors.Is(err, ErrReplyTooLarge) {
			t.Errorf("member %s: %v", other, err)
			continue
		}
		var se *ServerError
		if !errors.As(err, &se) || !strings.Contains(err.Error(), "electrumx error: "+other) {
			t.Errorf("member %s did not come through as the server's error: %v", other, err)
		}
	}
}
