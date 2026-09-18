package withdraw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
)

// writeFile is the durable replace step, a variable so a test can fail it
// after the content has landed at the path. That is the one failure a write
// keeps its record on, and the one no test can provoke from outside.
var writeFile = atomicfile.WriteFile

// ErrWrittenNotDurable reports a Create or Update whose record reached the
// store but whose durability is unconfirmed: the content is at the path and a reader of the
// store will find it, and only a power loss before the filesystem flushes its
// directory entry would lose it.
//
// A Store that can tell the two apart should return it, wrapped, instead of a
// plain error, and must keep the new record when it does. Engine.Build then
// keeps the intent Built with its inputs reserved and returns the error,
// rather than releasing inputs for a signed transaction the store is holding.
// A Store that cannot tell them apart returns a plain error, and Build treats
// the intent as unsaved, which is the safe reading for a store that may have
// discarded the record.
var ErrWrittenNotDurable = atomicfile.ErrWrittenNotDurable

func unmarshal(raw json.RawMessage, v interface{}) error { return json.Unmarshal(raw, v) }

// MemStore is an in-memory Store for tests and for callers that persist
// elsewhere. It is not durable; do not use it for real withdrawals.
type MemStore struct {
	mu      sync.Mutex
	intents map[string]*Intent
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{intents: map[string]*Intent{}} }

// Get implements Store.
func (m *MemStore) Get(_ context.Context, id string) (*Intent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.intents[id]
	if !ok {
		return nil, false, nil
	}
	cp := *in
	return &cp, true, nil
}

// Create implements Store.
func (m *MemStore) Create(_ context.Context, in *Intent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, had := m.intents[in.ID]; had {
		return fmt.Errorf("%w: %s", ErrExists, in.ID)
	}
	cp := *in
	m.intents[in.ID] = &cp
	return nil
}

// Update implements Store.
func (m *MemStore) Update(_ context.Context, in *Intent, from State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := expectState(m.intents, in.ID, from); err != nil {
		return err
	}
	cp := *in
	m.intents[in.ID] = &cp
	return nil
}

// expectState is the check behind Update in MemStore and FileStore: the
// record must be present and in the state the write names.
func expectState(intents map[string]*Intent, id string, from State) error {
	prev, had := intents[id]
	if !had {
		return fmt.Errorf("%w: %s is not in the store", ErrStale, id)
	}
	if prev.State != from {
		return fmt.Errorf("%w: %s is %s, the write expected %s", ErrStale, id, prev.State, from)
	}
	return nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, states ...State) ([]*Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return filterSorted(m.intents, states), nil
}

// FileStore keeps every intent in one JSON file, rewritten on each write: a
// temporary file in the same directory is written, synced and renamed into
// place, then the directory is synced so the rename survives a power loss.
// Suitable for a single process with modest volume; an exchange with a
// database should implement Store over it instead and keep the same "durable
// before broadcast" rule. The file stores ignore the context; a database
// store bounds its queries with it.
//
// One process, not two. Every write puts the whole file from this process's
// map, so a second process on the same path does not merge with it: it
// overwrites whatever the first one wrote with its own view, and an intent
// saved as Built by one process reappears as Created to the other while its
// signed bytes are in a mempool. Two processes that must share a store need
// one record per file, or a database; examples/exchange_split shows the first.
type FileStore struct {
	mu      sync.Mutex
	path    string
	intents map[string]*Intent
}

type fileStoreFile struct {
	Version int       `json:"version"`
	Updated time.Time `json:"updated"`
	Intents []*Intent `json:"intents"`
}

// NewFileStore opens or creates the store at path (mode 0600).
func NewFileStore(path string) (*FileStore, error) {
	fs := &FileStore{path: path, intents: map[string]*Intent{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fs, nil
		}
		return nil, fmt.Errorf("read intents %s: %w", path, err)
	}
	var f fileStoreFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse intents %s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("intents %s: unsupported version %d", path, f.Version)
	}
	for _, in := range f.Intents {
		fs.intents[in.ID] = in
	}
	return fs, nil
}

// Get implements Store.
func (fs *FileStore) Get(_ context.Context, id string) (*Intent, bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	in, ok := fs.intents[id]
	if !ok {
		return nil, false, nil
	}
	cp := *in
	return &cp, true, nil
}

// Create implements Store. It writes as Update does, once the id is known to
// be absent.
func (fs *FileStore) Create(_ context.Context, in *Intent) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, had := fs.intents[in.ID]; had {
		return fmt.Errorf("%w: %s", ErrExists, in.ID)
	}
	return fs.put(in)
}

// Update implements Store. It writes as Create does, once the stored record
// is known to be in state from.
func (fs *FileStore) Update(_ context.Context, in *Intent, from State) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := expectState(fs.intents, in.ID, from); err != nil {
		return err
	}
	return fs.put(in)
}

// put replaces the record in memory and writes the whole store. It returns
// only after the new file is on disk and renamed into place, and the
// directory entry is synced. When the write fails the store keeps reporting
// the record it held before, so a caller that treats the error as "not saved"
// and Get agree; the next successful write puts the whole store from memory
// on disk again.
//
// The one write failure that is not rolled back is the one that happened after
// the rename (atomicfile.ErrWrittenNotDurable): the file holds the new record
// already. Rolling memory back there would make Get contradict the file, and
// because the next write puts the whole store from memory, it would put the
// older record back over the newer one on disk. For a Built intent that is a
// signed transaction whose inputs the store has forgotten. The error is still
// returned, because durability is unconfirmed; only the rollback is skipped.
func (fs *FileStore) put(in *Intent) error {
	prev, had := fs.intents[in.ID]
	cp := *in
	fs.intents[in.ID] = &cp
	if err := fs.write(); err != nil {
		if errors.Is(err, atomicfile.ErrWrittenNotDurable) {
			return err
		}
		if had {
			fs.intents[in.ID] = prev
		} else {
			delete(fs.intents, in.ID)
		}
		return err
	}
	return nil
}

func (fs *FileStore) write() error {
	all := filterSorted(fs.intents, nil)
	buf, err := json.MarshalIndent(fileStoreFile{Version: 1, Updated: time.Now().UTC(), Intents: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal intents: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(fs.path), 0700); err != nil {
		return fmt.Errorf("create intents dir: %w", err)
	}
	if err := writeFile(fs.path, buf, 0600); err != nil {
		return fmt.Errorf("write intents: %w", err)
	}
	return nil
}

// List implements Store.
func (fs *FileStore) List(_ context.Context, states ...State) ([]*Intent, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return filterSorted(fs.intents, states), nil
}

func filterSorted(m map[string]*Intent, states []State) []*Intent {
	want := map[State]bool{}
	for _, s := range states {
		want[s] = true
	}
	out := make([]*Intent, 0, len(m))
	for _, in := range m {
		if len(want) == 0 || want[in.State] {
			cp := *in
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
