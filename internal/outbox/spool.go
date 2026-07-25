package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// Item is one spooled message. One file per pending message; the filename
// orders the queue and the atomic rename is the transaction.
type Item struct {
	Notification notify.Notification `json:"notification"`
	Channel      string              `json:"channel"`
	Attempts     int                 `json:"attempts"`
	NotBefore    time.Time           `json:"notBefore"`
	EnqueuedAt   time.Time           `json:"enqueuedAt"`
	Test         bool                `json:"test,omitempty"`
}

// Filenames sort lexicographically into delivery order:
//
//	<class>-<enqueue-nanos, zero-padded>-<requestID>.json
//
// class "0" = test sends (jump the queue), "1" = normal traffic.
func fileName(it *Item) string {
	class := "1"
	if it.Test {
		class = "0"
	}
	return fmt.Sprintf("%s-%020d-%s.json", class, it.EnqueuedAt.UnixNano(), it.Notification.RequestID)
}

type spool struct {
	dir string
}

func newSpool(dir string) (*spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &spool{dir: dir}, nil
}

// put writes the item under a temp name then renames it into place.
func (s *spool) put(it *Item) error {
	data, err := json.Marshal(it)
	if err != nil {
		return err
	}
	name := fileName(it)
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

// rewrite persists updated attempt state under the SAME name, keeping the
// item's queue position (write tmp, rename over).
func (s *spool) rewrite(name string, it *Item) error {
	data, err := json.Marshal(it)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

func (s *spool) remove(name string) error {
	_, err := s.claim(name)
	return err
}

// claim removes name and reports whether this caller is the one that unlinked
// it. A file that was already gone belongs to whoever removed it: two settlers
// racing over the same item — a worker finishing while its channel's spool is
// purged, say — would otherwise both write a terminal line for it.
func (s *spool) claim(name string) (bool, error) {
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// list returns spooled filenames in queue order (test sends first, then
// oldest first — the zero-padded nanos make lexicographic == chronological).
func (s *spool) list() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// errCorrupt marks a spool file whose bytes were read but don't parse — the
// only load failure where deleting the file is the right answer. A failed
// *read* (fd exhaustion, EIO, permissions changed under the process) is
// transient and must leave the message on disk.
var errCorrupt = errors.New("spool item corrupt")

func (s *spool) load(name string) (*Item, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return nil, err
	}
	var it Item
	if err := json.Unmarshal(data, &it); err != nil {
		return nil, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	return &it, nil
}

// requestIDFromName recovers the request id encoded in a spool filename, so a
// corrupt item can still be settled in the delivery log under its own id.
// Returns "" if the name doesn't match the <class>-<nanos>-<reqID>.json shape.
func requestIDFromName(name string) string {
	base := strings.TrimSuffix(name, ".json")
	parts := strings.SplitN(base, "-", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[2]
}

// count returns how many messages are pending.
func (s *spool) count() (int, error) {
	names, err := s.list()
	return len(names), err
}

// oldestEvictable returns the filename of the oldest spooled item ("" if none)
// — the eviction victim when the spool is full. Names in skip are never
// chosen; the caller passes the item currently mid-delivery (settling it as
// "dropped" would report a failure for a message the upstream is about to
// accept) and any it has already delivered but couldn't unlink.
//
// Oldest by *enqueue time*, across both classes: the design's eviction rule is
// plain evict-oldest. Exempting test sends would let the unauthenticated ops
// CTA push out an entire real backlog one fire at a time, and lexicographic
// order can't be used either — that sorts class "0" first, which is delivery
// order, not age.
func (s *spool) oldestEvictable(skip map[string]struct{}) (string, error) {
	names, err := s.list()
	if err != nil {
		return "", err
	}
	var victim string
	var oldest int64
	for _, n := range names {
		if _, skipped := skip[n]; skipped {
			continue
		}
		ns, ok := enqueueNanosFromName(n)
		if !ok {
			// Not a name this hub wrote; the drain loop settles it as corrupt.
			continue
		}
		if victim == "" || ns < oldest {
			victim, oldest = n, ns
		}
	}
	return victim, nil
}

// enqueueNanosFromName recovers the enqueue timestamp encoded in a spool
// filename. Returns false if the name isn't the shape fileName writes.
func enqueueNanosFromName(name string) (int64, bool) {
	base := strings.TrimSuffix(name, ".json")
	parts := strings.SplitN(base, "-", 3)
	if len(parts) != 3 {
		return 0, false
	}
	ns, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return ns, true
}
