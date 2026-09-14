package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// recordSyncs swaps the sync step for one that logs the name of every file
// synced and fails when failFile returns true for it. The real step is
// restored when the test ends.
func recordSyncs(t *testing.T, failFile func(name string) bool) *[]string {
	t.Helper()
	var synced []string
	real := fsync
	fsync = func(f *os.File) error {
		synced = append(synced, f.Name())
		if failFile != nil && failFile(f.Name()) {
			return errors.New("injected sync failure")
		}
		return real(f)
	}
	t.Cleanup(func() { fsync = real })
	return &synced
}

func onlyEntry(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != want {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only %s", names, want)
	}
}

// The file is synced before the rename and the directory after it, the new
// content replaces the old, the mode is the one asked for, and no temporary
// file is left behind.
func TestWriteFileSyncsFileThenDirectoryAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	synced := recordSyncs(t, nil)

	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("content %q, %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st.Mode(), err)
	}
	onlyEntry(t, dir, "state.json")

	wantSyncs := 2
	if runtime.GOOS == "windows" {
		wantSyncs = 1
	}
	if len(*synced) != wantSyncs {
		t.Fatalf("synced %v, want %d syncs", *synced, wantSyncs)
	}
	first := (*synced)[0]
	if filepath.Dir(first) != dir || first == path || filepath.Ext(first) != ".tmp" {
		t.Fatalf("first sync %q is not the temporary file in %s", first, dir)
	}
	if wantSyncs == 2 && (*synced)[1] != dir {
		t.Fatalf("second sync %q is not the directory %s", (*synced)[1], dir)
	}
}

// A file sync that reports failure must not be followed by the rename: the
// old content stays, the error names the step, and the temporary file is
// removed.
func TestWriteFileSyncFailureKeepsOldContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The temporary name is not known in advance: fail every sync that is not
	// the directory, which is only the file sync.
	recordSyncs(t, func(name string) bool { return name != dir })

	err := WriteFile(path, []byte("new"), 0o600)
	if err == nil || err.Error() != "sync "+path+": injected sync failure" {
		t.Fatalf("error %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatalf("old content replaced after a failed sync: %q", got)
	}
	onlyEntry(t, dir, "state.json")
}

// A directory sync that fails is reported so the caller does not treat the
// write as durable. The new content is already at the path by then and stays.
func TestWriteFileDirectorySyncFailureIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no directory sync on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	recordSyncs(t, func(name string) bool { return name == dir })

	err := WriteFile(path, []byte("new"), 0o600)
	if err == nil || err.Error() != "sync directory of "+path+": injected sync failure" {
		t.Fatalf("error %v", err)
	}
	onlyEntry(t, dir, "state.json")
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content after a failed directory sync %q, want the renamed file", got)
	}
}

// A rename that fails leaves the old content and no temporary file. The
// target is a non-empty directory, which a file cannot be renamed over.
func TestWriteFileRenameFailureKeepsOldContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err == nil {
		t.Fatal("renaming over a non-empty directory succeeded")
	}
	onlyEntry(t, dir, "state.json")
	if _, err := os.Stat(filepath.Join(path, "keep")); err != nil {
		t.Fatalf("directory at the target was disturbed: %v", err)
	}
}

// A directory that does not exist is an error, not a silent no-op.
func TestWriteFileMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := WriteFile(path, []byte("new"), 0o600); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file appeared: %v", err)
	}
}

// The mode comes from the call, not from the temporary file's default (0600)
// and not from the old file: a first write with a wider mode gets that mode,
// and a rewrite with a narrower one narrows it.
func TestWriteFileModeComesFromTheCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.json")
	if err := WriteFile(path, []byte("v"), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode after create %v, %v", st.Mode(), err)
	}
	if err := WriteFile(path, []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rewrite %v, %v", st.Mode(), err)
	}
}
