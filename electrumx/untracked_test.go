package electrumx

import (
	"context"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// An address that leaves the tracked set must leave the money views with it.
// The UTXO cache is the one per-address map that GetBalance, GetAllUTXOs and
// the selector read, and it was the one TrackAddresses did not prune: a
// retired address kept its coins in the balance and kept offering them to the
// selector for the life of the process.
func TestUntrackingAnAddressRemovesItsCoinsFromEveryView(t *testing.T) {
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	c := NewClient("127.0.0.1:1", time.Second, nil)
	c.HRP = types.Stagenet.HRP
	if err := c.TrackAddresses([]string{a1, a2}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.utxos[a1] = []types.UTXO{{TxID: txA, Vout: 0, Value: 100, Height: 10, Address: a1}}
	c.utxos[a2] = []types.UTXO{{TxID: txB, Vout: 0, Value: 900, Height: 10, Address: a2}}
	c.mu.Unlock()

	if err := c.TrackAddresses([]string{a1}); err != nil {
		t.Fatal(err)
	}

	if conf, _ := c.GetBalance(1, 100); conf != 100 {
		t.Errorf("balance is %d: it still counts the untracked address's 900", conf)
	}
	if n := len(c.GetAllUTXOs()); n != 1 {
		t.Errorf("GetAllUTXOs returned %d outputs: the untracked address's output is still selectable", n)
	}
	if n := len(c.GetUTXOs(a2)); n != 0 {
		t.Errorf("GetUTXOs still returns %d outputs for the untracked address", n)
	}
	c.mu.Lock()
	_, stillCached := c.utxos[a2]
	c.mu.Unlock()
	if stillCached {
		t.Error("the untracked address still has an entry in the cache")
	}
}

// A listunspent already in flight when TrackAddresses drops its address must
// not put the reply into the cache: committing it would undo the pruning
// above and make the output selectable again. Calling refreshAddress directly
// for an untracked address stands in for the reply landing late; it pins the
// guard, not the interleaving.
func TestARefreshForAnUntrackedAddressWritesNothing(t *testing.T) {
	stub := newPushStub(t)
	a1, a2 := craftAddr(t, 0x11), craftAddr(t, 0x22)
	stub.set(scripthashOf(t, a2), "", oneUTXO)
	c, _ := startClient(t, stub, a1)

	if err := c.refreshAddress(context.Background(), a2); err != nil {
		t.Fatal(err)
	}
	if n := len(c.GetUTXOs(a2)); n != 0 {
		t.Errorf("an untracked address gained %d cached outputs from a reply that landed after it was dropped", n)
	}
}
