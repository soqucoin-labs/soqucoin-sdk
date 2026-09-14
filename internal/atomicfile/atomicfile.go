// Package atomicfile replaces a file so that a crash or power loss at any
// point leaves either the old content or the new content at the path, never
// a partial file, and so that the new content is on disk when WriteFile
// returns.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// fsync is the sync step, a variable so a test can observe the order of the
// steps and make one fail.
var fsync = func(f *os.File) error { return f.Sync() }

// WriteFile writes data to path with mode perm. It writes to a temporary file
// in the same directory, syncs it, renames it over path, then syncs the
// directory so the rename itself survives a power loss. A plain os.WriteFile
// followed by os.Rename leaves both the data and the directory entry in the
// page cache; a crash after the rename can then yield an empty or truncated
// file at path.
//
// On any failure the temporary file is removed and path is unchanged. The
// directory sync is skipped on Windows, where a directory cannot be opened for
// syncing; the file sync still runs there.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmp := f.Name()
	fail := func(step string, err error) error {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("%s %s: %w", step, path, err)
	}
	if err := f.Chmod(perm); err != nil {
		return fail("chmod", err)
	}
	if _, err := f.Write(data); err != nil {
		return fail("write", err)
	}
	if err := fsync(f); err != nil {
		return fail("sync", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory of %s: %w", path, err)
	}
	defer d.Close()
	if err := fsync(d); err != nil {
		return fmt.Errorf("sync directory of %s: %w", path, err)
	}
	return nil
}
