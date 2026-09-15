package electrumx

import (
	"context"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A reply arriving on one connection never satisfies a call made on another:
// a waiter registered against an earlier generation is left for that
// generation's loss to fail, whatever id the new connection's server replays.
func TestReplyOnANewConnectionDoesNotSatisfyAnOldGenerationsCall(t *testing.T) {
	stub := newScriptedStub(t, types.Stagenet.GenesisHash, func(req request) []string {
		return []string{reply(req.ID, `null`)}
	})
	c := connect(t, stub)
	live := c.liveGen.Load()
	// A waiter from the previous generation, still registered.
	old := pendingCall{ch: make(chan incoming, 1), gen: live - 1, done: make(chan struct{})}
	const id = 424242
	c.pendMu.Lock()
	c.pending[id] = old
	c.pendMu.Unlock()

	stub.push(reply(id, `"replayed"`))
	// The live connection must still work.
	if _, err := c.Call(context.Background(), "x", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case in := <-old.ch:
		t.Fatalf("a reply on the live connection satisfied a call from generation %d: %s", live-1, in.Result)
	case <-time.After(100 * time.Millisecond):
	}
	c.pendMu.Lock()
	_, still := c.pending[id]
	c.pendMu.Unlock()
	if !still {
		t.Fatal("the old generation's waiter was removed by a reply on the new connection")
	}
}
