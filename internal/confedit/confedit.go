// Package confedit applies edits to a live config file safely enough that a
// web form may drive them.
//
// The files it writes hold every bearer key and every channel credential the
// hub has, and an operator edits the same files by hand over SSH. So an edit
// has to survive three things that a naive read-modify-write does not: a
// concurrent hand-edit (lost update), a concurrent second request (torn
// state), and power loss between rename and writeback (a zero-length keys
// file, which is every caller unauthenticated).
package confedit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrConflict means the file changed between being read and being written —
// almost always an operator editing over SSH while the page was open.
var ErrConflict = errors.New("the file changed since this page was loaded")

// tempPrefix marks this package's temp files so a crashed write can be swept
// up later. They are short-lived but contain credentials, so they must not be
// left to accumulate invisibly in the config directory.
const tempPrefix = ".hubbub-edit-"

// Validator parses a candidate file and returns the loader's own error. The
// real loader is passed in rather than reimplemented, so a file this package
// accepts is by construction a file the hub can load.
type Validator func(src []byte, name string) error

// File is one editable config file.
type File struct {
	Path     string
	Validate Validator

	// mu serialises read-modify-write within this process. The ETag alone
	// leaves a window between re-reading and renaming, which two concurrent
	// requests can both pass through.
	mu sync.Mutex
}

// Read returns the file's current bytes and an ETag over them.
//
// The ETag is a content hash, not an mtime. mtime is what the config watcher
// already polls, and two writes inside one filesystem timestamp tick are
// precisely the racing case an optimistic check has to catch.
func (f *File) Read() (src []byte, etag string, err error) {
	src, err = os.ReadFile(f.Path)
	if err != nil {
		return nil, "", err
	}
	return src, ETag(src), nil
}

// ETag hashes file content.
func ETag(src []byte) string {
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:8])
}

// Edit transforms the current file bytes into the intended new ones.
type Edit func(src []byte) ([]byte, error)

// Apply runs an edit under the whole safety pipeline and returns what was
// written.
//
// Pass the ETag the page was rendered from; an empty one skips the check,
// which is only right for an edit that cannot lose information.
func (f *File) Apply(etag string, edit Edit) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	src, cur, err := f.Read()
	if err != nil {
		return nil, err
	}
	if etag != "" && etag != cur {
		return nil, ErrConflict
	}
	// Never splice into a file that does not currently parse. The store keeps
	// serving the last good config from memory while the file on disk is
	// broken, so the dashboard can otherwise look healthy while editing
	// rubble.
	if err := f.Validate(src, f.Path); err != nil {
		return nil, fmt.Errorf("%s does not currently parse, so it cannot be edited here — fix it over SSH first: %w", f.Path, err)
	}

	out, err := edit(src)
	if err != nil {
		return nil, err
	}
	// The candidate is validated in memory. Doing it through a temp file
	// would leave a 0600 file full of credentials behind on a crash, and
	// would report errors against a scratch path nobody recognises.
	if err := f.Validate(out, "your edit"); err != nil {
		return nil, err
	}
	if err := writeDurable(f.Path, out); err != nil {
		return nil, err
	}
	return out, nil
}

// writeDurable replaces a file's contents atomically *and* durably.
//
// Rename alone gives atomicity: a reader sees the old bytes or the new ones,
// never a mix. It does not give durability — after a power loss the rename can
// be visible while the data blocks are not, which for keys.toml means a
// zero-length file and every caller suddenly unauthenticated. Hence the two
// syncs: one for the data, one for the directory entry that points at it.
func writeDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// Removed on every failure path; the successful rename makes this a no-op.
	defer os.Remove(name)

	// Inherit the real file's mode rather than trusting CreateTemp's 0600 to
	// match: these files carry credentials and the mode is part of that.
	if fi, err := os.Stat(path); err == nil {
		if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
			tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Directory fsync is not supported everywhere; the data sync above is the
	// part that matters most, so a platform that refuses this is not fatal.
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// SweepTemps removes leftover temp files from a write this package did not
// finish. Called at startup: they hold credentials, and nothing else will ever
// notice them.
func SweepTemps(paths ...string) (removed []string) {
	seen := map[string]bool{}
	for _, p := range paths {
		dir := filepath.Dir(p)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), tempPrefix) {
				full := filepath.Join(dir, e.Name())
				if os.Remove(full) == nil {
					removed = append(removed, full)
				}
			}
		}
	}
	return removed
}
