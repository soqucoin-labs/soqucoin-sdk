package utxo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// A Reserve whose write reached the file and was not confirmed durable is
// rolled back like any other failed write: a reservation not known to survive
// a restart is not held, the error names both ErrPersist and
// ErrWrittenNotDurable, and the entry the file holds is an orphan the next
// write or Recover removes.
func TestReserveRollsBackAWriteThatIsNotDurable(t *testing.T) {
	real := writeFile
	writeFile = func(path string, data []byte, perm os.FileMode) error {
		if err := real(path, data, perm); err != nil {
			return err
		}
		return fmt.Errorf("sync directory of %s: %w: disk gone", path, atomicfile.ErrWrittenNotDurable)
	}
	t.Cleanup(func() { writeFile = real })
	path := filepath.Join(t.TempDir(), "spent.json")
	ss := NewSpentSet(path, nil)
	in := []types.UTXO{{TxID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Vout: 1, Value: 1}}
	err := ss.Reserve(in, "w1", time.Hour)
	if !errors.Is(err, ErrPersist) || !errors.Is(err, atomicfile.ErrWrittenNotDurable) {
		t.Fatalf("Reserve: %v, want ErrPersist wrapping ErrWrittenNotDurable", err)
	}
	if ss.IsSpent(in[0].TxID, in[0].Vout) {
		t.Fatal("a reservation not known to be durable was kept in memory")
	}
	writeFile = real
	if len(ss.ReservedIntents()) != 0 {
		t.Fatal("the rolled-back reservation is still owned in memory")
	}
	if err := ss.Reserve(in, "w2", time.Hour); err != nil {
		t.Fatalf("the input must be free after the rollback: %v", err)
	}
}

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
