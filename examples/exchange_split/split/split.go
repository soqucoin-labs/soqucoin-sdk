// Package split holds what the three binaries of examples/exchange_split
// share: the directory that is their only channel, an intent store more than
// one process may write, and the snapshot the signer selects from.
//
// Nothing here is a network protocol. The processes never speak to each other;
// each one reads the files the others have left and writes only the files it
// owns. That is deliberate: the signer has no listening socket and no client
// of its own, so the only way to reach it is the directory, and the only thing
// it will act on is a withdrawal the store already holds.
//
// The directory is the trust boundary. Whoever can write it can offer the
// signer inputs that do not exist, which the node then refuses at the
// broadcast; they cannot redirect a payment, because the destination and the
// amount come from the intent file and the signer signs for one address only.
// Give it mode 0700 and one owner per host.
package split

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// Dir is the shared directory. The three processes are given the same value.
type Dir string

// The layout. Each process writes only what it owns: the watcher the requests
// it has accepted and the snapshot, the signer and the broadcaster the intent
// files of the states they own.
func (d Dir) Intents() string  { return filepath.Join(string(d), "intents") }
func (d Dir) Requests() string { return filepath.Join(string(d), "requests") }
func (d Dir) Accepted() string { return filepath.Join(string(d), "requests", "accepted") }
func (d Dir) Snapshot() string { return filepath.Join(string(d), snapshotName) }

// snapshotName is the snapshot file name inside the shared directory.
const snapshotName = "snapshot.json"

// EnsureDir creates a directory with mode 0700 and refuses one that is
// readable or writable by anyone else. The snapshot tells the signer what to
// spend and the intent files tell it what to sign, so another account with
// write access to this directory chooses both.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%s has mode %#o: the shared directory must not be readable or writable by any other account", path, mode)
	}
	return nil
}

// idPattern is what may become a file name. The intent id is the exchange's
// own idempotency key and it is used as a path element, so it is checked
// before it reaches the filesystem: a leading dot would collide with the
// temporary files a replace writes, and a separator or a parent reference
// would put the file outside the store.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ErrBadID is returned for an id that cannot be a file name in the store.
var ErrBadID = errors.New("split: intent id is not usable as a file name")

// ErrIDMismatch is returned when a file's name and the id inside it disagree,
// which a case-folding filesystem produces from two ids that differ only in
// case.
var ErrIDMismatch = errors.New("split: the file name and the record's id disagree")

// CheckID returns nil when id may be used as a store file name.
func CheckID(id string) error {
	if !idPattern.MatchString(id) || id == ".." {
		return fmt.Errorf("%w: %q (want 1 to 64 characters of letters, digits, dot, dash or underscore, not starting with a dot)", ErrBadID, id)
	}
	return nil
}

// DirStore is a withdraw.Store that keeps one intent per file, so the three
// processes can share it.
//
// withdraw.FileStore cannot be shared: it holds every intent in memory and
// writes the whole file from that map on each write, so a second process does
// not merge with the first, it overwrites it. An intent the signer saved as
// Built would come back as Created the next time the watcher wrote, with its
// signed transaction already in a mempool and its inputs free for the next
// withdrawal to take. Here a write replaces one file and reads none, and every
// read comes from the disk rather than from a cache, so no process holds a
// view that another can invalidate.
//
// What it does not do is lock across processes. Create is atomic on the
// filesystem: the record is linked into place under a name that must not
// exist yet, so two processes creating one id cannot both succeed. Update
// reads the record, compares its state and replaces the file under this
// process's lock only, so two processes updating one id at the same moment
// can still interleave; what stops them is that each state has one owner (the
// watcher creates, the signer builds, the broadcaster sends and confirms) and
// each process lists only the states it owns. An exchange that wants the
// guarantee rather than the convention puts the intents in a database and
// makes Update a conditional update on the state column, which is what
// withdraw.Store is an interface for.
type DirStore struct {
	dir string
	// mu orders the writes of this process. The file is the shared truth; this
	// only keeps one process's own goroutines from interleaving a read and a
	// write of the same record.
	mu sync.Mutex
}

// OpenDirStore creates or opens the store under dir. Prefer Dir.Open, which
// checks the shared directory as well as the store inside it.
func OpenDirStore(dir string) (*DirStore, error) {
	if err := EnsureDir(dir); err != nil {
		return nil, err
	}
	return &DirStore{dir: dir}, nil
}

// Open checks the shared directory and opens the intent store inside it. It is
// the one call each of the three processes makes, because checking the store
// and not its parent is no check at all: an account that can write the shared
// directory can rename the intents directory aside and put its own in place,
// and the process that opened only the inner path would never see it. The
// watcher did check the root and the other two did not, which is the kind of
// gap an enumerated site list exists to close.
func (d Dir) Open() (*DirStore, error) {
	if err := EnsureDir(string(d)); err != nil {
		return nil, err
	}
	return OpenDirStore(d.Intents())
}

func (s *DirStore) file(id string) string { return id + ".json" }

// Get implements withdraw.Store. It reads the file every time: another process
// may have written it since the last call.
func (s *DirStore) Get(_ context.Context, id string) (*withdraw.Intent, bool, error) {
	if err := CheckID(id); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// get is Get under a lock the caller holds.
func (s *DirStore) get(id string) (*withdraw.Intent, bool, error) {
	// Read through io/fs rooted at the store: a name that tries to leave the
	// directory is refused by the filesystem package as well as by CheckID.
	data, err := fs.ReadFile(os.DirFS(s.dir), s.file(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read intent %s: %w", id, err)
	}
	var in withdraw.Intent
	if err := json.Unmarshal(data, &in); err != nil {
		// A file is replaced by a rename, so a half-written record cannot be
		// read. One that will not parse is damaged, and a damaged withdrawal
		// record is never treated as "no such withdrawal".
		return nil, false, fmt.Errorf("parse intent %s: %w", id, err)
	}
	if in.ID != id {
		// The file name and the record disagree. APFS, NTFS and most SMB
		// shares fold case, so a read of W1.json can answer with w1.json, and
		// a Store that returned it would report one withdrawal as another:
		// Submit would take the second as already submitted and never pay it.
		return nil, false, fmt.Errorf("%w: %s holds the record of %s", ErrIDMismatch, s.file(id), in.ID)
	}
	return &in, true, nil
}

// Create implements withdraw.Store. The record is written to a temporary file
// and linked into place under the id's name; the link fails when that name
// exists, whichever process made it, so exactly one Create of an id succeeds
// and the other receives withdraw.ErrExists. The record is on disk, and its
// directory entry synced, before it returns nil.
//
// The intents directory therefore has to be on a filesystem that supports
// hard links. A share that does not fails every Create with the link error
// at the first withdrawal, which is loud and immediate; the alternative, an
// exclusive create written in place, would leave a half-written record
// visible to every reader after a crash. A crash between the link and the
// removal of the temporary file leaves that file behind; List skips it.
func (s *DirStore) Create(_ context.Context, in *withdraw.Intent) error {
	data, err := s.encode(in)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = createFile(filepath.Join(s.dir, s.file(in.ID)), data, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s", withdraw.ErrExists, in.ID)
	}
	return err
}

// Update implements withdraw.Store. The stored record is read and its state
// compared with from under this process's lock, then the file is replaced.
// The record is on disk, and its directory entry synced, before it returns
// nil.
//
// Unlike withdraw.FileStore there is nothing to roll back, because this store
// keeps no map. A write that failed after the rename leaves the file holding
// the new record, and the error wraps withdraw.ErrWrittenNotDurable to say so:
// the record is in the store and only its durability is unconfirmed. A Built
// intent then keeps its inputs reserved, because the broadcaster reads the
// same file and will send it.
func (s *DirStore) Update(_ context.Context, in *withdraw.Intent, from withdraw.State) error {
	data, err := s.encode(in)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok, err := s.get(in.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s is not in the store", withdraw.ErrStale, in.ID)
	}
	if stored.State != from {
		return fmt.Errorf("%w: %s is %s, the write expected %s", withdraw.ErrStale, in.ID, stored.State, from)
	}
	return replaceFile(filepath.Join(s.dir, s.file(in.ID)), data, 0o600)
}

// encode checks the id and renders the record.
func (s *DirStore) encode(in *withdraw.Intent) ([]byte, error) {
	if err := CheckID(in.ID); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal intent %s: %w", in.ID, err)
	}
	return data, nil
}

// List implements withdraw.Store, in the order the engine expects: oldest
// first, ties by id.
func (s *DirStore) List(_ context.Context, states ...withdraw.State) ([]*withdraw.Intent, error) {
	want := map[withdraw.State]bool{}
	for _, st := range states {
		want[st] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list intents: %w", err)
	}
	var out []*withdraw.Intent
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" || name[0] == '.' {
			continue // a temporary file of a replace in flight, or not a record
		}
		data, err := fs.ReadFile(os.DirFS(s.dir), name)
		if err != nil {
			return nil, fmt.Errorf("read intent file %s: %w", name, err)
		}
		var in withdraw.Intent
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, fmt.Errorf("parse intent file %s: %w", name, err)
		}
		if err := CheckID(in.ID); err != nil {
			// The name and the record can agree and both still be unusable,
			// as "bad id.json" holding the id "bad id" does. Get, Create and
			// Update refuse that id, so the engine would receive a withdrawal
			// this store can neither read back nor persist on its next
			// transition.
			return nil, fmt.Errorf("intent file %s: %w", name, err)
		}
		if name != s.file(in.ID) {
			// The same disagreement Get refuses. Here it would hand the engine
			// two entries for one record, or one record under a name no Get
			// will ever resolve to it.
			return nil, fmt.Errorf("%w: %s holds the record of %s", ErrIDMismatch, name, in.ID)
		}
		if len(want) == 0 || want[in.State] {
			cp := in
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// replaceFile puts data at path so that a crash leaves either the old content
// or the new, and the new content is on disk when it returns nil. The SDK has
// the same step in an internal package; the copy here keeps this example
// compilable when it is lifted out of the module, which is what an exchange
// does with it.
func replaceFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return syncDir(path)
}

// createFile puts data at path as replaceFile does, except that a path that
// already exists is refused with fs.ErrExist and left as it was. The
// temporary file is linked into place rather than renamed: a link onto an
// existing name fails atomically where hard links are supported, which is
// what makes one Create of an id succeed and the rest fail.
func createFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}
	linkErr := os.Link(tmp, path)
	_ = os.Remove(tmp)
	if linkErr != nil {
		if errors.Is(linkErr, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", path, fs.ErrExist)
		}
		return fmt.Errorf("link %s: %w", path, linkErr)
	}
	return syncDir(path)
}

// writeTemp writes data to a new temporary file beside path, synced and
// closed, and returns its name. The name starts with a dot, which List skips.
func writeTemp(path string, data []byte, perm os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmp := f.Name()
	fail := func(step string, err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("%s %s: %w", step, path, err)
	}
	if err := f.Chmod(perm); err != nil {
		return "", fail("chmod", err)
	}
	if _, err := f.Write(data); err != nil {
		return "", fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return "", fail("sync", err)
	}
	if err := f.Close(); err != nil {
		return "", fail("close", err)
	}
	return tmp, nil
}

// syncDir syncs the directory entry of path, so the rename or link that put
// the file there survives a power loss.
func syncDir(path string) error {
	dir := filepath.Dir(path)
	if runtime.GOOS == "windows" {
		// Syncing a directory handle fails there; the file sync above has run.
		// The SDK's own replace step makes the same exception.
		return nil
	}
	// filepath.Clean, although filepath.Dir above already returned a clean
	// path: it is what the static-analysis floor reads to see that the name
	// opened here is the directory of the file just written.
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return fmt.Errorf("open directory of %s: %w: %w", path, withdraw.ErrWrittenNotDurable, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory of %s: %w: %w", path, withdraw.ErrWrittenNotDurable, err)
	}
	return nil
}

// Snapshot is the watcher's answer to "what may be spent right now". It is the
// signer's whole view of the chain: the signer has no node and no indexer of
// its own.
//
// Every output in it was read back from the node with gettxout after the
// indexer offered it, so the indexer alone cannot put an output here. Tip is
// the node's height at that moment and At is when the node answered, both of
// which the signer uses to decide whether to act on it at all.
type Snapshot struct {
	At         time.Time `json:"at"`
	Tip        int64     `json:"tip"`
	HotAddress string    `json:"hot_address"`
	Outputs    []Output  `json:"outputs"`
}

// Output is one spendable output as the snapshot carries it.
//
// It is not types.UTXO. That type is shaped for the indexer's wire format,
// where the address, the asset type and the spend-pending flag carry `json:"-"`
// because the indexer does not send them; writing it to a file directly drops
// exactly the three fields this file exists to state. The conversions below
// are the only place the two meet.
type Output struct {
	TxID      string `json:"txid"`
	Vout      uint32 `json:"vout"`
	Value     int64  `json:"value_shors"`
	Height    int64  `json:"height"`
	Address   string `json:"address"`
	AssetType uint8  `json:"asset_type"`
}

// OutputsFrom converts what the indexer and the node agreed on into snapshot
// records.
func OutputsFrom(utxos []types.UTXO) []Output {
	out := make([]Output, 0, len(utxos))
	for _, u := range utxos {
		out = append(out, Output{
			TxID: u.TxID, Vout: u.Vout, Value: u.Value,
			Height: u.Height, Address: u.Address, AssetType: u.AssetType,
		})
	}
	return out
}

// Spendable returns the snapshot's outputs in the form the coin selector
// takes. ReadSnapshot has already refused a snapshot with an output of
// another address, without a height, or of another asset, so everything
// returned here is a confirmed native SOQ output of the hot address.
func (s Snapshot) Spendable() []types.UTXO {
	out := make([]types.UTXO, 0, len(s.Outputs))
	for _, o := range s.Outputs {
		out = append(out, types.UTXO{
			TxID: o.TxID, Vout: o.Vout, Value: o.Value,
			Height: o.Height, Address: o.Address, AssetType: o.AssetType,
		})
	}
	return out
}

var (
	// ErrNoSnapshot is returned before the watcher has written one.
	ErrNoSnapshot = errors.New("split: no snapshot yet")
	// ErrSnapshotStale is returned for a snapshot too old to spend from, or
	// one stamped in the future. Either way the signer builds nothing: the
	// outputs in it may already be spent, and spending one twice is the
	// failure this example is arranged to prevent.
	ErrSnapshotStale = errors.New("split: snapshot is not current")
	// ErrSnapshotInvalid is returned for a snapshot that contradicts itself:
	// no height, an output of another address, a non-positive value.
	ErrSnapshotInvalid = errors.New("split: snapshot is not self-consistent")
)

// WriteSnapshot replaces the snapshot file.
func WriteSnapshot(d Dir, s Snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	return replaceFile(d.Snapshot(), data, 0o600)
}

// ReadSnapshot reads the snapshot and refuses it unless it is current and
// self-consistent. maxAge is how old the signer will tolerate; skew is how far
// ahead of now a stamp may be before the file is refused, since a snapshot
// stamped in the future would otherwise stay "fresh" indefinitely.
func ReadSnapshot(d Dir, maxAge, skew time.Duration, now time.Time) (Snapshot, error) {
	var s Snapshot
	// Read through io/fs rooted at the shared directory, as the store does.
	data, err := fs.ReadFile(os.DirFS(string(d)), snapshotName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, ErrNoSnapshot
		}
		return s, fmt.Errorf("read snapshot: %w", err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse snapshot: %w", err)
	}
	if s.At.IsZero() {
		return s, fmt.Errorf("%w: no timestamp", ErrSnapshotInvalid)
	}
	if age := now.Sub(s.At); age > maxAge {
		return s, fmt.Errorf("%w: written %s ago, the limit is %s", ErrSnapshotStale, age.Round(time.Second), maxAge)
	} else if age < -skew {
		return s, fmt.Errorf("%w: stamped %s in the future", ErrSnapshotStale, (-age).Round(time.Second))
	}
	if s.Tip <= 0 {
		return s, fmt.Errorf("%w: tip height %d", ErrSnapshotInvalid, s.Tip)
	}
	if s.HotAddress == "" {
		return s, fmt.Errorf("%w: no hot address", ErrSnapshotInvalid)
	}
	for _, o := range s.Outputs {
		switch {
		case o.Address != s.HotAddress:
			// The signer signs for one address. An output of any other is
			// either a mistake or an attempt to have it sign what it should
			// not, and there is no reason to sort out which.
			return s, fmt.Errorf("%w: %s:%d pays %s, not the hot address %s", ErrSnapshotInvalid, o.TxID, o.Vout, o.Address, s.HotAddress)
		case o.Height <= 0:
			return s, fmt.Errorf("%w: %s:%d has height %d, so it is not a confirmed output", ErrSnapshotInvalid, o.TxID, o.Vout, o.Height)
		case o.Height > s.Tip:
			return s, fmt.Errorf("%w: %s:%d is at height %d, above the tip %d it was read at", ErrSnapshotInvalid, o.TxID, o.Vout, o.Height, s.Tip)
		case o.Value <= 0:
			return s, fmt.Errorf("%w: %s:%d has value %d", ErrSnapshotInvalid, o.TxID, o.Vout, o.Value)
		case o.AssetType != types.AssetTypeSOQ:
			// A USDSOQ output is real and is not SOQ. Selecting one to pay a
			// SOQ withdrawal would spend the wrong asset.
			return s, fmt.Errorf("%w: %s:%d is asset type %d, not native SOQ", ErrSnapshotInvalid, o.TxID, o.Vout, o.AssetType)
		}
	}
	return s, nil
}

// Request is one withdrawal the exchange's own system asks for, dropped into
// requests/ as a JSON file. It is the only thing that enters the directory
// from outside, and the watcher validates it before it becomes an intent.
type Request struct {
	ID          string `json:"id"`           // the idempotency key
	Address     string `json:"address"`      // destination
	AmountShors int64  `json:"amount_shors"` // never SOQ, never a float
	FeeRate     int64  `json:"fee_rate"`     // shors per vB
}

// ReadRequests reads every request file in requests/, oldest name first. A
// file that will not parse or that asks for something impossible is returned
// as an error naming it, so one bad file stops nothing but itself.
func ReadRequests(d Dir) (reqs []Request, errs []error) {
	entries, err := os.ReadDir(d.Requests())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("list requests: %w", err)}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" && e.Name()[0] != '.' {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := fs.ReadFile(os.DirFS(d.Requests()), name)
		if err != nil {
			errs = append(errs, fmt.Errorf("read request %s: %w", name, err))
			continue
		}
		var r Request
		if err := json.Unmarshal(data, &r); err != nil {
			errs = append(errs, fmt.Errorf("parse request %s: %w", name, err))
			continue
		}
		if err := r.check(name); err != nil {
			errs = append(errs, err)
			continue
		}
		reqs = append(reqs, r)
	}
	return reqs, errs
}

// check refuses a request the store or the engine would refuse later, where
// the file name can still be named in the error.
func (r Request) check(name string) error {
	if err := CheckID(r.ID); err != nil {
		return fmt.Errorf("request %s: %w", name, err)
	}
	if r.ID+".json" != name {
		// The file name is the id. Two names for one id would submit the same
		// withdrawal twice on a store that has lost the first file.
		return fmt.Errorf("request %s: the file name must be the id (%s.json)", name, r.ID)
	}
	if r.Address == "" || r.AmountShors <= 0 || r.FeeRate <= 0 {
		return fmt.Errorf("request %s: address, amount_shors and fee_rate are all required and the two numbers must be positive", name)
	}
	return nil
}

// Accept moves a request that became an intent into requests/accepted, so the
// watcher's next pass is quiet. It is a rename, so nothing is lost if it fails
// and the request is simply submitted again, which Submit answers with the
// intent that already exists.
func Accept(d Dir, id string) error {
	if err := EnsureDir(d.Accepted()); err != nil {
		return err
	}
	from := filepath.Join(d.Requests(), id+".json")
	to := filepath.Join(d.Accepted(), id+".json")
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("accept request %s: %w", id, err)
	}
	return nil
}

// NetworkFor maps the -network flag the three binaries share to a network.
// There is no default: the prefix decides which chain's addresses the
// processes will accept, and a wrong guess here is a payment to an address on
// another chain.
func NetworkFor(name string) (types.Network, error) {
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		if name == n.Name {
			return n, nil
		}
	}
	return types.Network{}, fmt.Errorf("unknown network %q: want mainnet, stagenet or regtest", name)
}

// LoopbackHost reports whether hostport is on this machine. The indexer sees
// every address tracked and can alter what it reports, so off this machine the
// connection is TLS or nothing.
func LoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
