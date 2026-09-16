// Package atomicfile replaces a file so that a crash or power loss at any
// point leaves either the old content or the new content at the path, never
// a partial file, and so that the new content is on disk when WriteFile
// returns without error.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrWrittenNotDurable reports a failure after the rename: the new content is
// at the path and synced as a file, and only the directory entry was not
// forced out. Every error from WriteFile means "not known to be durable", but
// this one also means "written", and the difference matters to a caller that
// keeps a copy in memory. Rolling that copy back to the old record on this
// error leaves memory contradicting the file, and the next successful write,
// which writes the whole file from memory, puts the old record back over the
// new one. Keep the new record and treat durability as unconfirmed.
var ErrWrittenNotDurable = errors.New("atomicfile: content written, durability not confirmed")

// fsync is the sync step, a variable so a test can observe the order of the
// steps and make one fail.
var fsync = func(f *os.File) error { return f.Sync() }

// WriteFile writes data to path with mode perm. It writes to a temporary file
// in the same directory, syncs it, renames it over path, then syncs the
// directory so the rename itself survives a power loss.
//
// A failure before the rename removes the temporary file and leaves path
// unchanged. A failure after the rename (opening or syncing the directory) is
// reported with path holding the new content, synced to disk as a file but
// with the directory entry not yet forced out, and wraps
// ErrWrittenNotDurable so a caller can tell the two apart. Treat a returned
// error as "not known to be durable", never as "undone". The directory sync is skipped on
// Windows, where syncing a directory handle fails; the file sync still runs
// there.
//
// A crash between the create and the rename can leave a ".<name>.<random>.tmp"
// file in the directory. It holds nothing the target does not and may be
// deleted.
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
		return fail("close", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail("rename", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory of %s: %w: %w", path, ErrWrittenNotDurable, err)
	}
	defer d.Close()
	if err := fsync(d); err != nil {
		return fmt.Errorf("sync directory of %s: %w: %w", path, ErrWrittenNotDurable, err)
	}
	return nil
}
