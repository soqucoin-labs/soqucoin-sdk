package electrumx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The prefix can arrive after the connection, and Connect's verification is
// then long past. A wrong-chain server answers listunspent with a well-formed
// empty list, because a script hash says nothing about which chain it belongs
// to, so the client used to report "no deposits" for the life of the process
// with a freshness record saying every pass succeeded. The gate is on the
// call, so the first call after the prefix is known is refused.
func TestCallIsRefusedWhenTheChainIsWrongAndThePrefixArrivedAfterTheConnection(t *testing.T) {
	stub := newScriptedStub(t, types.Mainnet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `[]`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	t.Cleanup(c.Stop)

	// No prefix yet, so Connect has nothing to verify against and succeeds
	// against an indexer for another chain.
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect with no prefix set: %v", err)
	}
	if n := stub.features.Load(); n != 0 {
		t.Fatalf("server.features was called %d times with no prefix set", n)
	}

	// The prefix arrives, inferred from the addresses themselves, which is the
	// documented shape for a caller that does not pin it.
	addr := craftAddr(t, 0x61)
	if err := c.TrackAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}

	err := c.RefreshAll(context.Background())
	if !errors.Is(err, ErrGenesisMismatch) {
		t.Fatalf("a refresh against a mainnet indexer on a stagenet client returned %v, want ErrGenesisMismatch", err)
	}
	// The refusal has to be a refusal rather than an empty answer recorded as
	// a successful pass: that record is what a caller reads before treating an
	// empty UTXO set as "no deposits".
	if at, err := c.LastRefreshOf(addr); err == nil {
		t.Fatalf("the refused refresh was recorded as successful at %v", at)
	}
	if got := len(c.GetUTXOs(addr)); got != 0 {
		t.Fatalf("GetUTXOs returned %d outputs from a refused refresh", got)
	}
}

// The same order of events against the right chain: the call goes out, and the
// verification happens once for the connection rather than once per call.
func TestThePrefixArrivingAfterTheConnectionVerifiesOnceAndThenCalls(t *testing.T) {
	addr := craftAddr(t, 0x62)
	sh := scripthashOf(t, addr)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `[]`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	t.Cleanup(c.Stop)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.TrackAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.RefreshAll(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if n := stub.features.Load(); n != 1 {
		t.Fatalf("server.features was called %d times over three refreshes, want 1", n)
	}
	if _, err := c.LastRefreshOf(addr); err != nil {
		t.Fatalf("a verified refresh was recorded as a failure: %v", err)
	}
	_ = sh
}

// A reconnect verifies again, through Connect's own check rather than through
// the gate. This pins that behaviour; the test below is the one that pins the
// generation half of the gate's record.
func TestAReconnectVerifiesTheGenesisAgain(t *testing.T) {
	addr := craftAddr(t, 0x63)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `[]`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	t.Cleanup(c.Stop)

	setHRP(t, c, types.Stagenet.HRP)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.TrackAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := stub.features.Load()
	if after != 1 {
		t.Fatalf("server.features was called %d times before the reconnect, want 1", after)
	}
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := stub.features.Load(); n != 2 {
		t.Fatalf("server.features was called %d times across two connections, want 2", n)
	}
}

// With no prefix ever set there is nothing to verify against, and no call that
// reads money can be made either: a script hash needs an address, which needs
// a prefix. This pins that state rather than leaving it to be rediscovered:
// the exported Call still works, and no verification is attempted.
func TestWithNoPrefixTheGateIsSilentAndNoVerificationIsAttempted(t *testing.T) {
	stub := newScriptedStub(t, types.Mainnet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `{"height":9,"hex":"00"}`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	t.Cleanup(c.Stop)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetTip(context.Background()); err != nil {
		t.Fatalf("a call with no prefix set: %v", err)
	}
	if n := stub.features.Load(); n != 0 {
		t.Fatalf("server.features was called %d times with no prefix set, want 0", n)
	}
	if got := c.hrp(); got != "" {
		t.Fatalf("hrp() is %q on a client that was never given one", got)
	}
}

// A broadcast is money leaving. A wrong-chain server takes the transaction and
// does not relay it, so the withdrawal looks sent and never confirms; the gate
// covers this call too because every call passes through the one funnel.
func TestBroadcastIsRefusedWhenTheChainIsWrong(t *testing.T) {
	stub := newScriptedStub(t, types.Mainnet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `"0000000000000000000000000000000000000000000000000000000000000001"`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	t.Cleanup(c.Stop)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	setHRP(t, c, types.Stagenet.HRP)
	_, err := c.BroadcastTx(context.Background(), "00")
	if !errors.Is(err, ErrGenesisMismatch) {
		t.Fatalf("broadcast to a mainnet indexer from a stagenet client returned %v, want ErrGenesisMismatch", err)
	}
}

// The gate skips the check when it has already verified this connection under
// this prefix. The record is keyed by the connection generation as well, so it
// cannot outlive the connection it was made on: every path that replaces a
// connection today runs Connect, which verifies, but a path that did not would
// otherwise inherit a verdict about a different server. Written against the
// fields rather than through a public call, because no public path can replace
// a connection without verifying.
func TestTheRecordedVerificationDoesNotOutliveItsConnection(t *testing.T) {
	addr := craftAddr(t, 0x64)
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `[]`)}
	})
	c := NewClient(stub.addr(), time.Hour, nil)
	t.Cleanup(c.Stop)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.TrackAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := stub.features.Load(); n != 1 {
		t.Fatalf("server.features was called %d times for the first verified call, want 1", n)
	}

	// The record now names an earlier connection, as it would after a
	// connection swap that did not verify.
	c.lockConnBlocking()
	c.genesisGen--
	c.unlockConn()

	if err := c.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := stub.features.Load(); n != 2 {
		t.Fatalf("server.features was called %d times after the record was made stale, want 2", n)
	}
}
