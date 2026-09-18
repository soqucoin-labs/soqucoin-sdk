package utxo

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A reservation that has expired by the clock is kept across a restart so
// withdraw.Engine.Recover sees it: an orphan is released and logged, a Built
// intent's is renewed. Dropping it at load hid both from Recover, and a
// forward clock step dropped every one. The TTL's backstop is unchanged: the
// input is still free for another withdrawal.
func TestLoadKeepsAnExpiredReservationForRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spent.json")
	in := []types.UTXO{{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, Value: 1}}
	ss := NewSpentSet(path, nil)
	if err := ss.Reserve(in, "w1", -time.Second); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	again := NewSpentSet(path, nil)
	if ids := again.ReservedIntents(); len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("after reload the expired reservation is gone: %v", ids)
	}
	if err := again.Reserve(in, "w2", time.Hour); err != nil {
		t.Fatalf("an expired input must still be free for another withdrawal: %v", err)
	}
}
