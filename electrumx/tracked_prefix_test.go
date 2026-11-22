package electrumx

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A refresh and a subscribe of a tracked address derive the script hash under
// one reading of the prefix, and the gate in callGen reads the prefix again.
// SetHRP refuses a change while addresses are tracked, and a commit is dropped
// for an address no longer tracked, so the two readings differ only when the
// tracked set is emptied, the prefix changed and changed back, and the address
// tracked again while the call is in flight. The gate then verifies the server
// for the other network, and that server's answer for the script would be
// committed under the address of the first network.
//
// refreshAddressUnder and subscribeUnder take the prefix the address was read
// under, so the tests below stand in that state without racing for it: the
// client is on the stagenet prefix and connected to a stagenet server when the
// call goes out for a mainnet address, the server holds its reply, and the
// client is on the mainnet prefix with the mainnet address tracked when the
// reply arrives.

// heldStub is a stagenet server that holds its reply to one method for one
// script hash until the test sends it.
func heldStub(t *testing.T, fill byte, method string) (mainnet string, stub *scriptedStub, held chan int64) {
	t.Helper()
	sh := scripthashOf(t, craftAddr(t, fill))
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = fill
	}
	mainnet, err := address.Encode(types.Mainnet.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	held = make(chan int64, 1)
	stub = newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == method && firstParam(req) == sh {
			held <- req.ID
			return nil // held until the test pushes it
		}
		return []string{reply(req.ID, `null`)}
	})
	return mainnet, stub, held
}

// trackOnMainnetWhileHeld waits for the held request, then moves the client to
// the mainnet prefix and tracks the mainnet address, as the sequence above
// leaves it, and returns the id the reply must carry.
func trackOnMainnetWhileHeld(t *testing.T, c *Client, mainnet string, held chan int64) int64 {
	t.Helper()
	var id int64
	select {
	case id = <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the server")
	}
	if err := c.SetHRP(types.Mainnet.HRP); err != nil {
		t.Fatalf("SetHRP on a client that tracks nothing: %v", err)
	}
	if err := c.TrackAddresses([]string{mainnet}); err != nil {
		t.Fatalf("TrackAddresses: %v", err)
	}
	return id
}

func TestARefreshOfATrackedAddressTakesNoSetFromAServerCheckedAgainstAnotherPrefix(t *testing.T) {
	mainnet, stub, held := heldStub(t, 0x71, "blockchain.scripthash.listunspent")
	c := connect(t, stub)
	var commits atomic.Int64
	c.OnRefresh = func(string, int) { commits.Add(1) }
	ctx := context.Background()

	done := make(chan error, 1)
	go func() { done <- c.refreshAddressUnder(ctx, types.Mainnet.HRP, mainnet) }()
	id := trackOnMainnetWhileHeld(t, c, mainnet, held)
	stub.push(reply(id, `[{"tx_hash":"`+txA+`","tx_pos":0,"value":100,"height":10}]`))

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh did not return")
	}
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a stagenet server's set for a mainnet address: %v", err)
	}
	if got := c.GetAllUTXOs(); len(got) != 0 {
		t.Fatalf("the refused reply left %+v in the cache", got)
	}
	if n := commits.Load(); n != 0 {
		t.Fatalf("OnRefresh ran %d times for a refused reply", n)
	}
	// The refusal is on the address's record, so the deposit monitor skips the
	// address until a refresh succeeds.
	if at, rerr := c.LastRefreshOf(mainnet); !at.IsZero() || !errors.Is(rerr, ErrNetworkMismatch) {
		t.Fatalf("the address's record after the refusal: %v, %v", at, rerr)
	}
}

func TestASubscribeOfATrackedAddressTakesNoStatusFromAServerCheckedAgainstAnotherPrefix(t *testing.T) {
	mainnet, stub, held := heldStub(t, 0x72, "blockchain.scripthash.subscribe")
	c := connect(t, stub)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() { done <- c.subscribeUnder(ctx, types.Mainnet.HRP, mainnet) }()
	id := trackOnMainnetWhileHeld(t, c, mainnet, held)
	stub.push(reply(id, `"a-status-of-the-stagenet-script"`))

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscribe did not return")
	}
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a stagenet server's status for a mainnet address: %v", err)
	}
	if c.isSubscribed(mainnet) {
		t.Fatal("the mainnet address reads as subscribed on a stagenet server's connection")
	}
	c.mu.RLock()
	_, seen := c.status[mainnet]
	changed := c.changed[mainnet]
	c.mu.RUnlock()
	if seen || changed {
		t.Fatalf("the refused reply left a status (%v) or a pending change (%v) for the address", seen, changed)
	}
	if _, rerr := c.LastRefreshOf(mainnet); !errors.Is(rerr, ErrNetworkMismatch) {
		t.Fatalf("the address's record after the refusal: %v", rerr)
	}
}

// The same state reached the way a deployment could reach it: one goroutine
// empties the tracked set, changes the prefix, changes it back and tracks the
// mainnet address again, while another refreshes. The server is on stagenet
// and answers every listunspent with one output, so no set may ever be
// committed for the mainnet address, wherever the changes land.
func TestNoSetIsCommittedForAMainnetAddressFromAStagenetServerWhileThePrefixAndTheTrackedSetChange(t *testing.T) {
	sh := scripthashOf(t, craftAddr(t, 0x73))
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = 0x73
	}
	mainnet, err := address.Encode(types.Mainnet.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		switch {
		case req.Method == "blockchain.scripthash.listunspent" && firstParam(req) == sh:
			return []string{reply(req.ID, `[{"tx_hash":"`+txA+`","tx_pos":0,"value":100,"height":10}]`)}
		case req.Method == "blockchain.scripthash.subscribe" && firstParam(req) == sh:
			return []string{reply(req.ID, `"a-status"`)}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	var wrong atomic.Int64
	c.OnRefresh = func(addr string, n int) {
		if addr == mainnet && n > 0 {
			wrong.Add(1)
		}
	}
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
			_ = c.TrackAddresses(nil)
			_ = c.SetHRP(types.Stagenet.HRP)
			runtime.Gosched()
			_ = c.SetHRP(types.Mainnet.HRP)
			_ = c.TrackAddresses([]string{mainnet})
			runtime.Gosched()
		}
	}()
	defer func() {
		close(stop)
		<-flipped
	}()

	for i := 0; i < 300; i++ {
		_ = c.refreshAddress(ctx, mainnet)
		_ = c.subscribe(ctx, mainnet)
		if n := wrong.Load(); n != 0 {
			t.Fatalf("pass %d: %d sets of a stagenet server were committed for a mainnet address", i, n)
		}
		if c.isSubscribed(mainnet) {
			c.mu.RLock()
			hrp := c.networkHRP
			c.mu.RUnlock()
			// A subscription of the mainnet address can only be recorded on a
			// connection checked against the mainnet prefix, and this server
			// fails that check.
			t.Fatalf("pass %d: the mainnet address reads as subscribed on a stagenet server (client prefix %q)", i, hrp)
		}
	}
}

// replacedWhileHeld waits for the held request and then leaves the client as a
// reconnect during the call leaves it, with the old socket still open so the
// held reply can be delivered: a newer generation is live when live is true,
// and the genesis check on record is that generation's.
func replacedWhileHeld(t *testing.T, c *Client, held chan int64, live bool) int64 {
	t.Helper()
	var id int64
	select {
	case id = <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the server")
	}
	c.lockConnBlocking()
	c.genesisGen = c.gen + 1
	if live {
		c.setLiveGen(c.gen + 1)
	} else {
		c.setLiveGen(0)
	}
	c.unlockConn()
	return id
}

// stagenetHeldStub is heldStub for an address of the server's own network.
func stagenetHeldStub(t *testing.T, fill byte, method string) (a string, stub *scriptedStub, held chan int64) {
	t.Helper()
	a = craftAddr(t, fill)
	sh := scripthashOf(t, a)
	held = make(chan int64, 1)
	stub = newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == method && firstParam(req) == sh {
			held <- req.ID
			return nil
		}
		return []string{reply(req.ID, `null`)}
	})
	return a, stub, held
}

// The check of the reply runs before the commit, so a reply whose connection
// was replaced and checked again meets it first. It is handled as the commit
// handles it: a listunspent reply is recorded as the replacement with a
// refresh left pending, and a subscribe reply commits nothing and is no error.
func TestAReplyOfAReplacedConnectionIsHandledAsTheCommitHandlesIt(t *testing.T) {
	ctx := context.Background()

	a, stub, held := stagenetHeldStub(t, 0x74, "blockchain.scripthash.listunspent")
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.refreshAddressUnder(ctx, types.Stagenet.HRP, a) }()
	id := replacedWhileHeld(t, c, held, true)
	stub.push(reply(id, `[{"tx_hash":"`+txA+`","tx_pos":0,"value":100,"height":10}]`))
	err := <-done
	if !errors.Is(err, ErrNotConnected) || errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("a listunspent reply of a replaced connection: %v", err)
	}
	if got := c.GetUTXOs(a); len(got) != 0 {
		t.Fatalf("a reply of a replaced connection reached the cache: %+v", got)
	}
	at, rerr := c.LastRefreshOf(a)
	if !at.IsZero() || !errors.Is(rerr, ErrNotConnected) {
		t.Fatalf("the record after the stale reply: %v, %v", at, rerr)
	}
	c.mu.RLock()
	pending := c.changed[a]
	c.mu.RUnlock()
	if !pending {
		t.Fatal("the stale reply left no refresh pending")
	}

	b, stubB, heldB := stagenetHeldStub(t, 0x75, "blockchain.scripthash.subscribe")
	d := connect(t, stubB)
	if err := d.TrackAddresses([]string{b}); err != nil {
		t.Fatal(err)
	}
	go func() { done <- d.subscribeUnder(ctx, types.Stagenet.HRP, b) }()
	id = replacedWhileHeld(t, d, heldB, true)
	stubB.push(reply(id, `"the status the old connection reported"`))
	if err := <-done; err != nil {
		t.Fatalf("a subscribe reply of a replaced connection is an error: %v", err)
	}
	d.mu.RLock()
	_, subscribed := d.subscribed[b]
	_, seen := d.status[b]
	rec := d.refreshed[b]
	d.mu.RUnlock()
	if subscribed || seen || rec.err != nil || rec.dirty || rec.seq != 0 {
		t.Fatalf("a subscribe reply of a replaced connection wrote subscribed=%v status=%v record=%+v", subscribed, seen, rec)
	}
}

// A listunspent reply whose connection was replaced and checked again is
// refused when the replacement is lost too. Nothing is live then, and the
// commit's own test reads that as the address's own answer; the check on
// record says another connection has been verified since the reply's.
func TestAReplyIsRefusedWhenItsConnectionWasReplacedAndTheReplacementLost(t *testing.T) {
	a, stub, held := stagenetHeldStub(t, 0x76, "blockchain.scripthash.listunspent")
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.refreshAddressUnder(context.Background(), types.Stagenet.HRP, a) }()
	id := replacedWhileHeld(t, c, held, false)
	stub.push(reply(id, `[{"tx_hash":"`+txA+`","tx_pos":0,"value":100,"height":10}]`))
	if err := <-done; !errors.Is(err, ErrNotConnected) {
		t.Fatalf("the reply of a connection replaced twice over: %v", err)
	}
	if got := c.GetUTXOs(a); len(got) != 0 {
		t.Fatalf("the refused reply reached the cache: %+v", got)
	}
	if at, rerr := c.LastRefreshOf(a); !at.IsZero() || !errors.Is(rerr, ErrNotConnected) {
		t.Fatalf("the record after the refusal: %v, %v", at, rerr)
	}
}

// A context that ends while the check of the reply waits for the connection
// lock says nothing about the indexer. The caller gets its own context's
// error and the address keeps the verdict it had, also when the reply's
// connection was replaced meanwhile: recorded as a refusal, the replacement
// would put a lost-connection error on an address the server had answered
// clean, for a caller that only stopped waiting.
func TestAContextThatEndsAtTheReplyCheckKeepsTheVerdictOfTheAddress(t *testing.T) {
	a := craftAddr(t, 0x77)
	sh := scripthashOf(t, a)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		if req.Method == "blockchain.scripthash.listunspent" && firstParam(req) == sh {
			return []string{reply(req.ID, `[]`)}
		}
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}
	if err := c.refreshAddressUnder(context.Background(), types.Stagenet.HRP, a); err != nil {
		t.Fatal(err)
	}
	clean, rerr := c.LastRefreshOf(a)
	if clean.IsZero() || rerr != nil {
		t.Fatalf("the record after a clean refresh: %v, %v", clean, rerr)
	}

	// The reply's connection is replaced and a newer one is live, as
	// replacedWhileHeld leaves the client.
	c.lockConnBlocking()
	gen := c.gen
	c.genesisGen = gen + 1
	c.setLiveGen(gen + 1)
	c.unlockConn()
	c.mu.RLock()
	seq := c.refreshed[a].seq
	c.mu.RUnlock()

	for _, ended := range []error{context.Canceled, context.DeadlineExceeded} {
		err := c.refusedReply(a, gen, seq, c.refreshTicket.Add(1), ended)
		if !errors.Is(err, ended) || errors.Is(err, ErrNotConnected) {
			t.Fatalf("a check that ended with %v returned %v", ended, err)
		}
		at, rerr := c.LastRefreshOf(a)
		if !at.Equal(clean) || rerr != nil {
			t.Fatalf("after a check that ended with %v the record is %v, %v; it was %v, nil", ended, at, rerr, clean)
		}
		c.mu.RLock()
		pending := c.changed[a]
		c.mu.RUnlock()
		if pending {
			t.Fatalf("a check that ended with %v left a refresh pending", ended)
		}
	}

	// A refusal in the same state is recorded as the replacement.
	err := c.refusedReply(a, gen, seq, c.refreshTicket.Add(1), ErrNetworkMismatch)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a refused reply of a replaced connection: %v", err)
	}
	if _, rerr := c.LastRefreshOf(a); !errors.Is(rerr, ErrNotConnected) {
		t.Fatalf("the record after the refusal: %v", rerr)
	}
}
