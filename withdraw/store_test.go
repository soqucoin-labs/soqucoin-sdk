package withdraw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
)

// failAfterTheRename makes the next writes land at the path and then report
// that durability is unconfirmed, which is what a failed directory open or
// sync does. Restored by the returned function, or when the test ends.
func failAfterTheRename(t *testing.T) (restore func()) {
	t.Helper()
	real := writeFile
	writeFile = func(path string, data []byte, perm os.FileMode) error {
		if err := real(path, data, perm); err != nil {
			return err
		}
		return fmt.Errorf("sync directory of %s: %w: injected", path, atomicfile.ErrWrittenNotDurable)
	}
	t.Cleanup(func() { writeFile = real })
	return func() { writeFile = real }
}

// The post-rename window (5a0j). A write that failed after the rename left the
// new record in the file and the old one in memory, and the rollback was what
// made them disagree: because every Put writes the whole store from memory,
// the next successful Put put the old record back over the new one on disk.
// For a Built intent that is a signed transaction, with reserved inputs, that
// the store forgets while the file says otherwise — so Recover after a restart
// re-broadcast bytes whose inputs Build had released.
//
// The record now stays. The error is still returned, since nothing here knows
// the write is durable; only the rollback is skipped.
func TestPutKeepsARecordThatLandedButIsNotKnownDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intents.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	created := &Intent{ID: "w1", State: StateCreated, CreatedAt: time.Now().UTC()}
	if err := s.Create(context.Background(), created); err != nil {
		t.Fatal(err)
	}

	restore := failAfterTheRename(t)
	built := *created
	built.State, built.RawHex, built.TxID = StateBuilt, "00", "t"
	built.Inputs = []Outpoint{{TxID: txA, Vout: 0, Value: 1000, Address: "ssq1phot"}}
	err = s.Update(context.Background(), &built, StateCreated)
	if !errors.Is(err, atomicfile.ErrWrittenNotDurable) {
		t.Fatalf("Put returned %v, want an error wrapping atomicfile.ErrWrittenNotDurable", err)
	}

	// Memory agrees with the file: the file holds the Built record, so Get
	// must too.
	got, ok, _ := s.Get(context.Background(), "w1")
	if !ok || got.State != StateBuilt || got.TxID != "t" {
		t.Fatalf("after the post-rename failure Get reports %+v; want the Built record, which is what the file holds", got)
	}
	onDisk, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	fromFile, ok, _ := onDisk.Get(context.Background(), "w1")
	if !ok || fromFile.State != StateBuilt {
		t.Fatalf("the file holds %+v; the write landed before the failure, so it must hold the Built record", fromFile)
	}

	// The defect this pins: the next successful write puts the whole store
	// from memory, so a rolled-back map reverts the Built intent on disk.
	restore()
	if err := s.Create(context.Background(), &Intent{ID: "w2", State: StateCreated, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("the next write, with the write step restored: %v", err)
	}
	reloaded, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	after, ok, _ := reloaded.Get(context.Background(), "w1")
	if !ok || after.State != StateBuilt || after.TxID != "t" || len(after.Inputs) != 1 {
		t.Fatalf("the later write left %+v on disk; the Built record and its inputs were overwritten by the older one", after)
	}
}

// Every write names what it expects to find. Create refuses an id the store
// holds, and Update refuses a record that is absent or in another state, so
// a second Submit cannot write Created over a sent intent and a loop holding
// a copy the store has moved past cannot move it back. Both stores, one test.
func TestStoresWriteOnlyOverTheStateTheyWereTold(t *testing.T) {
	fs, err := NewFileStore(filepath.Join(t.TempDir(), "intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Store{"MemStore": NewMemStore(), "FileStore": fs} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			in := &Intent{ID: "w1", Amount: 1, State: StateCreated, CreatedAt: time.Now().UTC()}
			if err := s.Create(ctx, in); err != nil {
				t.Fatal(err)
			}
			again := *in
			again.Amount = 7
			if err := s.Create(ctx, &again); !errors.Is(err, ErrExists) {
				t.Fatalf("a second Create of w1 returned %v, want ErrExists", err)
			}
			if got, _, _ := s.Get(ctx, "w1"); got.Amount != 1 {
				t.Fatalf("the refused Create wrote its record: amount %d", got.Amount)
			}

			built := *in
			built.State, built.RawHex = StateBuilt, "00"
			if err := s.Update(ctx, &built, StateCreated); err != nil {
				t.Fatal(err)
			}
			late := *in
			late.LastError = "late"
			if err := s.Update(ctx, &late, StateCreated); !errors.Is(err, ErrStale) {
				t.Fatalf("an Update naming Created over a Built record returned %v, want ErrStale", err)
			}
			got, _, _ := s.Get(ctx, "w1")
			if got.State != StateBuilt || got.LastError != "" {
				t.Fatalf("the refused Update wrote its record: %+v", got)
			}
			if err := s.Update(ctx, &Intent{ID: "w2", State: StateBuilt}, StateCreated); !errors.Is(err, ErrStale) {
				t.Fatalf("an Update of an absent id returned %v, want ErrStale", err)
			}
			if _, ok, _ := s.Get(ctx, "w2"); ok {
				t.Fatal("an Update of an absent id created it")
			}
		})
	}
}
