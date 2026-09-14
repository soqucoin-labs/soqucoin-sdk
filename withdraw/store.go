package withdraw

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
)

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
func (m *MemStore) Get(id string) (*Intent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.intents[id]
	if !ok {
		return nil, false, nil
	}
	cp := *in
	return &cp, true, nil
}

// Put implements Store.
func (m *MemStore) Put(in *Intent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *in
	m.intents[in.ID] = &cp
	return nil
}

// List implements Store.
func (m *MemStore) List(states ...State) ([]*Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return filterSorted(m.intents, states), nil
}

// FileStore keeps every intent in one JSON file, rewritten on each Put: a
// temporary file in the same directory is written, synced and renamed into
// place, then the directory is synced so the rename survives a power loss.
// Suitable for a single process with modest volume; an exchange with a
// database should implement Store over it instead and keep the same "durable
// before broadcast" rule.
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
func (fs *FileStore) Get(id string) (*Intent, bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	in, ok := fs.intents[id]
	if !ok {
		return nil, false, nil
	}
	cp := *in
	return &cp, true, nil
}

// Put implements Store. It returns only after the new file is on disk and
// renamed into place, and the directory entry is synced.
func (fs *FileStore) Put(in *Intent) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	cp := *in
	fs.intents[in.ID] = &cp

	all := filterSorted(fs.intents, nil)
	buf, err := json.MarshalIndent(fileStoreFile{Version: 1, Updated: time.Now().UTC(), Intents: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal intents: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(fs.path), 0700); err != nil {
		return fmt.Errorf("create intents dir: %w", err)
	}
	if err := atomicfile.WriteFile(fs.path, buf, 0600); err != nil {
		return fmt.Errorf("write intents: %w", err)
	}
	return nil
}

// List implements Store.
func (fs *FileStore) List(states ...State) ([]*Intent, error) {
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
