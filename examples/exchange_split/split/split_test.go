package split

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

const hot = "ssq1photwallet"

func openStore(t *testing.T) (*DirStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "intents")
	s, err := OpenDirStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func intent(id string, state withdraw.State) *withdraw.Intent {
	return &withdraw.Intent{
		ID: id, State: state, Address: "ssq1pdestination",
		Amount: 1000, FeeRate: types.RecommendedFeeRate,
		CreatedAt: time.Now().UTC(),
	}
}

// The id is the exchange's idempotency key and it becomes a file name, so it
// is the one value in the store that an outside system chooses. A separator or
// a parent reference in it would write the record outside the store, or read
// something else as a withdrawal.
func TestTheStoreRefusesAnIDThatIsNotAFileNameInIt(t *testing.T) {
	s, dir := openStore(t)
	for _, id := range []string{
		"", "..", ".", "../escape", "a/b", "/abs", ".hidden", "with space",
		"w1\n", "w\x00", "évidence",
	} {
		if err := s.Create(context.Background(), intent(id, withdraw.StateCreated)); !errors.Is(err, ErrBadID) {
			t.Errorf("Create(%q) returned %v, want ErrBadID", id, err)
		}
		if _, _, err := s.Get(context.Background(), id); !errors.Is(err, ErrBadID) {
			t.Errorf("Get(%q) returned %v, want ErrBadID", id, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused ids left %d files in the store", len(entries))
	}
	// 64 characters is the limit, and the 65th is refused rather than
	// truncated into a collision with the 64th.
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	if err := s.Create(context.Background(), intent(string(long), withdraw.StateCreated)); err != nil {
		t.Errorf("a 64-character id was refused: %v", err)
	}
	if err := s.Create(context.Background(), intent(string(long)+"a", withdraw.StateCreated)); !errors.Is(err, ErrBadID) {
		t.Errorf("a 65-character id was accepted: %v", err)
	}
}

// Two processes share the store. Each writes the records of the states it
// owns, and neither has a cache the other can invalidate, so what one writes
// the other reads.
func TestTwoStoresOnOneDirectoryDoNotOverwriteEachOther(t *testing.T) {
	_, dir := openStore(t)
	a, err := OpenDirStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenDirStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	built := intent("w1", withdraw.StateBuilt)
	built.RawHex, built.TxID = "00", "t1"
	if err := a.Create(context.Background(), built); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(context.Background(), intent("w2", withdraw.StateCreated)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := b.Get(context.Background(), "w1")
	if err != nil || !ok || got.State != withdraw.StateBuilt || got.TxID != "t1" {
		t.Fatalf("the second store reads w1 as %+v (ok=%v, err=%v); the first wrote it Built", got, ok, err)
	}
	got, ok, err = a.Get(context.Background(), "w2")
	if err != nil || !ok || got.State != withdraw.StateCreated {
		t.Fatalf("the first store reads w2 as %+v (ok=%v, err=%v)", got, ok, err)
	}
	all, err := a.List(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("List returned %d intents (err=%v), want both", len(all), err)
	}
	if list, err := a.List(context.Background(), withdraw.StateBuilt); err != nil || len(list) != 1 || list[0].ID != "w1" {
		t.Fatalf("List(Built) returned %+v (err=%v), want only w1", list, err)
	}
}

// Why this store exists. withdraw.FileStore writes the whole file from its own
// map, so the same two writes through two of them lose one record: the intent
// the other process saved as Built comes back as absent, and its signed
// transaction with it.
func TestTheSDKFileStoreLosesARecordWhenTwoProcessesShareIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intents.json")
	a, err := withdraw.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := withdraw.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	built := intent("w1", withdraw.StateBuilt)
	built.RawHex, built.TxID = "00", "t1"
	if err := a.Create(context.Background(), built); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(context.Background(), intent("w2", withdraw.StateCreated)); err != nil {
		t.Fatal(err)
	}
	reloaded, err := withdraw.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reloaded.Get(context.Background(), "w1"); ok {
		t.Fatal("FileStore kept both records; if sharing it is safe then DirStore is unnecessary, and this test goes with it")
	}
}

// A snapshot is the signer's whole view of the chain. Every refusal here is a
// pass in which nothing is built. An old snapshot lists outputs that may
// already be spent, and one that contradicts itself does not describe any
// state the chain was in.
func TestASnapshotIsRefusedUnlessItIsCurrentAndConsistent(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	good := Snapshot{
		At: now.Add(-10 * time.Second), Tip: 100, HotAddress: hot,
		Outputs: []Output{{TxID: "aa", Vout: 0, Value: 5000, Height: 90, Address: hot, AssetType: types.AssetTypeSOQ}},
	}
	for _, c := range []struct {
		name string
		snap Snapshot
		want error
	}{
		{"current", good, nil},
		{"older than the limit", with(good, func(s *Snapshot) { s.At = now.Add(-5 * time.Minute) }), ErrSnapshotStale},
		{"stamped in the future", with(good, func(s *Snapshot) { s.At = now.Add(time.Minute) }), ErrSnapshotStale},
		{"no timestamp", with(good, func(s *Snapshot) { s.At = time.Time{} }), ErrSnapshotInvalid},
		{"no tip height", with(good, func(s *Snapshot) { s.Tip = 0 }), ErrSnapshotInvalid},
		{"no hot address", with(good, func(s *Snapshot) { s.HotAddress = "" }), ErrSnapshotInvalid},
		{"an output of another address", with(good, func(s *Snapshot) { s.Outputs[0].Address = "ssq1pelsewhere" }), ErrSnapshotInvalid},
		{"an unconfirmed output", with(good, func(s *Snapshot) { s.Outputs[0].Height = 0 }), ErrSnapshotInvalid},
		{"an output with no value", with(good, func(s *Snapshot) { s.Outputs[0].Value = 0 }), ErrSnapshotInvalid},
		{"an output above the tip it was read at", with(good, func(s *Snapshot) { s.Outputs[0].Height = 101 }), ErrSnapshotInvalid},
		{"a USDSOQ output", with(good, func(s *Snapshot) { s.Outputs[0].AssetType = types.AssetTypeUSDSOQ }), ErrSnapshotInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Dir(t.TempDir())
			if err := WriteSnapshot(d, c.snap); err != nil {
				t.Fatal(err)
			}
			got, err := ReadSnapshot(d, time.Minute, 5*time.Second, now)
			if c.want == nil {
				if err != nil {
					t.Fatalf("refused a current snapshot: %v", err)
				}
				if len(got.Outputs) != 1 || got.Tip != 100 || got.HotAddress != hot {
					t.Fatalf("snapshot read back as %+v", got)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("ReadSnapshot returned %v, want %v", err, c.want)
			}
		})
	}
}

// Before the watcher has written one, the signer must be able to tell "not
// yet" from "too old": the first is a system starting up, the second is a
// watcher that has stopped publishing while the chain moved on.
func TestNoSnapshotIsDistinctFromAStaleOne(t *testing.T) {
	d := Dir(t.TempDir())
	if _, err := ReadSnapshot(d, time.Minute, time.Second, time.Now()); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("ReadSnapshot with no file returned %v, want ErrNoSnapshot", err)
	}
}

// The request file is the only thing that enters the directory from outside.
// Its name must be the id it carries, or a store that has lost the first file
// would accept the same withdrawal twice under two names.
func TestRequestsAreRefusedUnlessTheyNameThemselvesAndAskForSomethingPossible(t *testing.T) {
	d := Dir(t.TempDir())
	if err := EnsureDir(d.Requests()); err != nil {
		t.Fatal(err)
	}
	write := func(name string, r Request) {
		t.Helper()
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.Requests(), name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ok := Request{ID: "w1", Address: "ssq1pdestination", AmountShors: 1000, FeeRate: types.RecommendedFeeRate}
	write("w1.json", ok)
	write("renamed.json", Request{ID: "w2", Address: "ssq1pdestination", AmountShors: 1000, FeeRate: 1000})
	write("w3.json", Request{ID: "w3", Address: "", AmountShors: 1000, FeeRate: 1000})
	write("w4.json", Request{ID: "w4", Address: "ssq1pdestination", AmountShors: 0, FeeRate: 1000})
	write("w5.json", Request{ID: "w5", Address: "ssq1pdestination", AmountShors: 1000, FeeRate: 0})
	write("w6.json", Request{ID: "../w6", Address: "ssq1pdestination", AmountShors: 1000, FeeRate: 1000})
	if err := os.WriteFile(filepath.Join(d.Requests(), "w7.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	reqs, errs := ReadRequests(d)
	if len(reqs) != 1 || reqs[0] != ok {
		t.Fatalf("accepted %+v, want only the well-formed request", reqs)
	}
	if len(errs) != 6 {
		t.Fatalf("reported %d refusals (%v), want one for each bad file", len(errs), errs)
	}

	// Accepting one moves it out of the way and leaves the rest where they are.
	if err := Accept(d, "w1"); err != nil {
		t.Fatal(err)
	}
	reqs, _ = ReadRequests(d)
	for _, r := range reqs {
		if r.ID == "w1" {
			t.Fatal("the accepted request is still in requests/")
		}
	}
	if _, err := os.Stat(filepath.Join(d.Accepted(), "w1.json")); err != nil {
		t.Fatalf("the accepted request was not kept: %v", err)
	}
}

// The directory is the trust boundary: whoever can write it can tell the
// signer what exists. A mode that lets another account in is refused where it
// is noticed rather than trusted because it is convenient.
func TestASharedDirectoryOtherAccountsCanReachIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o777, 0o770, 0o707, 0o750, 0o705} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if err := EnsureDir(dir); err == nil {
			t.Errorf("mode %#o accepted", mode)
		}
		if _, err := OpenDirStore(dir); err == nil {
			t.Errorf("mode %#o accepted by the store", mode)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("mode 0700 refused: %v", err)
	}
}

// A record that will not parse is never reported as "no such withdrawal": the
// files are replaced by rename, so a damaged one is damage and not a partial
// write, and treating it as absent would let the same withdrawal be created
// again.
func TestADamagedRecordIsAnErrorAndNotAnAbsence(t *testing.T) {
	s, dir := openStore(t)
	if err := os.WriteFile(filepath.Join(dir, "w1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(context.Background(), "w1"); err == nil || ok {
		t.Fatalf("Get on a damaged record returned ok=%v, err=%v", ok, err)
	}
	if _, err := s.List(context.Background()); err == nil {
		t.Fatal("List skipped a damaged record")
	}
}

func with(s Snapshot, edit func(*Snapshot)) Snapshot {
	cp := s
	cp.Outputs = append([]Output(nil), s.Outputs...)
	edit(&cp)
	return cp
}

// A case-folding filesystem (APFS, NTFS, most SMB shares) answers a read of
// W1.json with w1.json. A store that returned that record would report one
// withdrawal as another: Submit would take the second as already submitted,
// move its request to accepted/ and never pay it. So the record's own id is
// checked against the name it was read under, in Get and in List.
func TestARecordWhoseIDIsNotTheNameItWasReadUnderIsRefused(t *testing.T) {
	s, dir := openStore(t)
	other := intent("w2", withdraw.StateCreated)
	data, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "w1.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.Get(context.Background(), "w1"); !errors.Is(err, ErrIDMismatch) || ok {
		t.Fatalf("Get returned ok=%v, err=%v; want ErrIDMismatch", ok, err)
	}
	if _, err := s.List(context.Background()); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("List returned %v; want ErrIDMismatch", err)
	}
}

// The engine reads List's order: Recover re-sends built withdrawals in it, so
// the oldest goes first, ties by id. withdraw.FileStore sorts the same way and
// nothing was checking that this store does.
func TestListIsOldestFirstThenByID(t *testing.T) {
	s, _ := openStore(t)
	base := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	write := func(id string, at time.Time) {
		t.Helper()
		in := intent(id, withdraw.StateBuilt)
		in.CreatedAt = at
		if err := s.Create(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	// The oldest record has the lexically last name, so the file order
	// os.ReadDir returns is not the order this test expects: without the sort
	// by CreatedAt it fails.
	write("w3", base.Add(2*time.Minute))
	write("w2b", base.Add(time.Minute))
	write("w2a", base.Add(time.Minute))
	write("w9", base)

	got, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, in := range got {
		ids = append(ids, in.ID)
	}
	want := []string{"w9", "w2a", "w2b", "w3"}
	if len(ids) != len(want) {
		t.Fatalf("List returned %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("List returned %v, want %v", ids, want)
		}
	}
}

// A file name and the record inside it can agree and both still be unusable.
// Get, Create and Update refuse such an id, so List admitting it would hand the engine a
// withdrawal this store can neither read back nor save on its next transition.
func TestListRefusesARecordWhoseIDIsNotUsableAtAll(t *testing.T) {
	s, dir := openStore(t)
	in := intent("bad id", withdraw.StateCreated)
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad id.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(context.Background()); !errors.Is(err, ErrBadID) {
		t.Fatalf("List returned %v, want ErrBadID", err)
	}
}

// Create is what keeps two processes from both registering one id: the second
// link onto the name fails on the filesystem, whichever process made the
// first. Update names the state it read, so a process writing over a record
// another has moved is refused rather than moving it back.
func TestCreateRefusesAnExistingRecordAndUpdateRefusesAnotherState(t *testing.T) {
	_, dir := openStore(t)
	a, err := OpenDirStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenDirStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Create(ctx, intent("w1", withdraw.StateCreated)); err != nil {
		t.Fatal(err)
	}
	again := intent("w1", withdraw.StateCreated)
	again.Amount = 7
	if err := b.Create(ctx, again); !errors.Is(err, withdraw.ErrExists) {
		t.Fatalf("the second process's Create of w1 returned %v, want ErrExists", err)
	}
	if got, _, _ := a.Get(ctx, "w1"); got.Amount != 1000 {
		t.Fatalf("the refused Create replaced the record: amount %d", got.Amount)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the refused Create left %d entries in the store, want the one record", len(entries))
	}

	built := intent("w1", withdraw.StateBuilt)
	built.RawHex, built.TxID = "00", "t1"
	if err := b.Update(ctx, built, withdraw.StateCreated); err != nil {
		t.Fatal(err)
	}
	late := intent("w1", withdraw.StateCreated)
	late.LastError = "late"
	if err := a.Update(ctx, late, withdraw.StateCreated); !errors.Is(err, withdraw.ErrStale) {
		t.Fatalf("an Update naming Created over the Built record returned %v, want ErrStale", err)
	}
	got, _, _ := a.Get(ctx, "w1")
	if got.State != withdraw.StateBuilt || got.TxID != "t1" {
		t.Fatalf("the refused Update replaced the record: %+v", got)
	}
	if err := a.Update(ctx, intent("w2", withdraw.StateBuilt), withdraw.StateCreated); !errors.Is(err, withdraw.ErrStale) {
		t.Fatalf("an Update of an absent id returned %v, want ErrStale", err)
	}
	if _, ok, _ := a.Get(ctx, "w2"); ok {
		t.Fatal("an Update of an absent id created it")
	}
}
