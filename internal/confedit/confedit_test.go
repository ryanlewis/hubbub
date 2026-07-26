package confedit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// tomlish is a stand-in validator: it refuses anything containing "BAD", which
// is enough to exercise the pipeline without dragging the config package in.
func tomlish(src []byte, name string) error {
	if strings.Contains(string(src), "BAD") {
		return fmt.Errorf("%s: contains BAD", name)
	}
	return nil
}

func newFile(t *testing.T, content string) *File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return &File{Path: p, Validate: tomlish}
}

func TestApplyWritesAndPreservesMode(t *testing.T) {
	f := newFile(t, "a = 1\n")
	src, etag, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(src) != "a = 1\n" {
		t.Fatalf("Read = %q", src)
	}

	out, err := f.Apply(etag, func(src []byte) ([]byte, error) {
		return []byte("a = 2\n"), nil
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(out) != "a = 2\n" {
		t.Errorf("Apply returned %q", out)
	}
	onDisk, _ := os.ReadFile(f.Path)
	if string(onDisk) != "a = 2\n" {
		t.Errorf("on disk = %q", onDisk)
	}
	fi, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — these files hold credentials", fi.Mode().Perm())
	}
}

// The stated workflow is dashboard edits *and* SSH hand-edits. An intervening
// hand-edit must not be silently destroyed.
func TestStaleETagIsRefusedAndFileUntouched(t *testing.T) {
	f := newFile(t, "a = 1\n")
	_, etag, _ := f.Read()

	// Somebody edits over SSH while the page is open.
	if err := os.WriteFile(f.Path, []byte("a = 1\nb = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := f.Apply(etag, func([]byte) ([]byte, error) { return []byte("a = 99\n"), nil })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	onDisk, _ := os.ReadFile(f.Path)
	if string(onDisk) != "a = 1\nb = 2\n" {
		t.Errorf("the hand-edit was destroyed: %q", onDisk)
	}
}

func TestEmptyETagSkipsTheCheck(t *testing.T) {
	f := newFile(t, "a = 1\n")
	if _, err := f.Apply("", func([]byte) ([]byte, error) { return []byte("a = 2\n"), nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestInvalidResultIsRefusedAndFileUntouched(t *testing.T) {
	f := newFile(t, "a = 1\n")
	_, etag, _ := f.Read()

	_, err := f.Apply(etag, func([]byte) ([]byte, error) { return []byte("BAD\n"), nil })
	if err == nil {
		t.Fatal("Apply accepted an invalid result")
	}
	if !strings.Contains(err.Error(), "your edit") {
		t.Errorf("err = %v, want it reported against the edit, not a scratch path", err)
	}
	onDisk, _ := os.ReadFile(f.Path)
	if string(onDisk) != "a = 1\n" {
		t.Errorf("file changed despite a refused edit: %q", onDisk)
	}
}

// The store serves the last good config from memory while the file on disk is
// broken, so the dashboard must not splice into rubble.
func TestBrokenFileOnDiskIsRefusedWithAdvice(t *testing.T) {
	f := newFile(t, "BAD\n")
	_, err := f.Apply("", func(src []byte) ([]byte, error) { return append(src, 'x'), nil })
	if err == nil {
		t.Fatal("Apply edited a file that does not parse")
	}
	if !strings.Contains(err.Error(), "SSH") {
		t.Errorf("err = %v, want it to say where to fix this", err)
	}
}

func TestEditErrorIsPassedThrough(t *testing.T) {
	f := newFile(t, "a = 1\n")
	sentinel := errors.New("no such table")
	if _, err := f.Apply("", func([]byte) ([]byte, error) { return nil, sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the edit's own error", err)
	}
}

// The ETag alone leaves a window between re-reading and renaming; the mutex is
// what closes it. Run under -race.
func TestConcurrentAppliesSerialise(t *testing.T) {
	f := newFile(t, "n = 0\n")

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflicts := 0, 0
	for i := range 8 {
		wg.Go(func() {
			_, etag, err := f.Read()
			if err != nil {
				return
			}
			_, err = f.Apply(etag, func([]byte) ([]byte, error) {
				return fmt.Appendf(nil, "n = %d\n", i+1), nil
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if ok == 0 {
		t.Error("every concurrent write lost")
	}
	if ok+conflicts != 8 {
		t.Errorf("ok=%d conflicts=%d, want them to account for all 8", ok, conflicts)
	}
	onDisk, _ := os.ReadFile(f.Path)
	if err := tomlish(onDisk, "result"); err != nil {
		t.Errorf("file left invalid: %v", err)
	}
	if strings.Count(string(onDisk), "n = ") != 1 {
		t.Errorf("file is torn: %q", onDisk)
	}
}

func TestNoTempFileLeftBehind(t *testing.T) {
	f := newFile(t, "a = 1\n")
	dir := filepath.Dir(f.Path)

	if _, err := f.Apply("", func([]byte) ([]byte, error) { return []byte("a = 2\n"), nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := f.Apply("", func([]byte) ([]byte, error) { return []byte("BAD\n"), nil }); err == nil {
		t.Fatal("expected a refusal")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Errorf("temp file left behind: %s (it would contain credentials)", e.Name())
		}
	}
}

func TestSweepTempsRemovesOrphans(t *testing.T) {
	f := newFile(t, "a = 1\n")
	dir := filepath.Dir(f.Path)
	orphan := filepath.Join(dir, tempPrefix+"crashed")
	if err := os.WriteFile(orphan, []byte("secret = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "channels.toml")
	if err := os.WriteFile(keep, []byte("b = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed := SweepTemps(f.Path, keep)
	if len(removed) != 1 || removed[0] != orphan {
		t.Errorf("removed = %v, want just the orphan", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan survived the sweep")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("sweep removed a real config file")
	}
	if _, err := os.Stat(f.Path); err != nil {
		t.Error("sweep removed a real config file")
	}
}

func TestETagChangesWithContent(t *testing.T) {
	a, b := ETag([]byte("a")), ETag([]byte("b"))
	if a == b {
		t.Error("different content produced the same ETag")
	}
	if again := ETag([]byte("a")); a != again {
		t.Errorf("ETag is not stable: %q then %q", a, again)
	}
}

func TestDiff(t *testing.T) {
	before := []byte("[ntfy]\ntype = \"ntfy\"\n\n# docs\n# more docs\n")
	after := []byte("[ntfy]\ntype = \"ntfy\"\ntopic = \"t\"\n")

	d := Diff(before, after)
	if !Changed(d) {
		t.Fatal("Changed = false for a real change")
	}
	added, deleted := Counts(d)
	if added != 1 || deleted != 3 {
		t.Errorf("added=%d deleted=%d, want 1 and 3", added, deleted)
	}

	// The headline case: a documentation deletion has to be visible.
	var dels []string
	for _, l := range d {
		if l.Op == Del {
			dels = append(dels, l.Text)
		}
	}
	if !slices.Contains(dels, "# docs") || !slices.Contains(dels, "# more docs") {
		t.Errorf("deleted comments not surfaced: %v", dels)
	}
}

func TestDiffOfIdenticalFiles(t *testing.T) {
	src := []byte("a = 1\nb = 2\n")
	d := Diff(src, src)
	if Changed(d) {
		t.Error("Changed = true for identical input")
	}
	if a, del := Counts(d); a != 0 || del != 0 {
		t.Errorf("counts = %d/%d, want 0/0", a, del)
	}
}

func TestDiffHandlesEmptyAndCRLF(t *testing.T) {
	if d := Diff(nil, nil); Changed(d) {
		t.Error("empty vs empty reported a change")
	}
	// A textarea submits CRLF; a line that only changed its terminator is not
	// a change worth showing an operator.
	if d := Diff([]byte("a = 1\n"), []byte("a = 1\r\n")); Changed(d) {
		t.Error("a line-ending-only difference was reported as a change")
	}
}
