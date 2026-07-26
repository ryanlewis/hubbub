package config

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Store holds the live keyring and channel set, hot-reloading both on mtime
// change. Reload trigger is a poll (stdlib-only; an atomic rename swaps the
// inode out from under a kernel watch anyway). A failed parse keeps the old
// set — validate-before-swap.
type Store struct {
	cfg *Config

	keys     atomic.Pointer[Keyring]
	channels atomic.Pointer[ChannelSet]

	// mtimes of the last *successful* load, and of the last failure already
	// reported. A file is retried every poll until it loads, so the second
	// pair keeps a permanently-broken file from repeating one error line
	// forever.
	keysMtime        time.Time
	channelsMtime    time.Time
	keysErrMtime     time.Time
	channelsErrMtime time.Time

	// OnChannelsChange is invoked (from the watch goroutine) after a
	// successful channels reload, so the outbox can rebuild its workers.
	OnChannelsChange func(*ChannelSet)

	// reload wakes the watcher early. Buffered at 1 and sent to without
	// blocking: a pending wake-up already covers any number of callers.
	reload chan struct{}
}

func NewStore(cfg *Config) (*Store, error) {
	s := &Store{cfg: cfg, reload: make(chan struct{}, 1)}
	ring, err := LoadKeys(cfg.KeysFile)
	if err != nil {
		return nil, err
	}
	chs, err := LoadChannels(cfg.ChannelsFile)
	if err != nil {
		return nil, err
	}
	s.keys.Store(ring)
	s.channels.Store(chs)
	s.keysMtime = mtime(cfg.KeysFile)
	s.channelsMtime = mtime(cfg.ChannelsFile)
	return s, nil
}

func (s *Store) Keyring() *Keyring     { return s.keys.Load() }
func (s *Store) Channels() *ChannelSet { return s.channels.Load() }

// Watch polls both files until ctx is done. Poll interval bounds revocation
// latency; a few seconds keeps "revoke in under a minute" honest.
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollOnce()
		case <-s.reload:
			s.pollOnce()
		}
	}
}

// ReloadNow asks the watcher to poll immediately, so a change this process
// just wrote is live in milliseconds rather than up to one poll interval
// later.
//
// It signals rather than reloading inline, deliberately. Every mtime field on
// Store is unsynchronised, and safe only because exactly one goroutine — the
// watcher — ever touches them; calling pollOnce from an HTTP handler would
// race those fields and could run two OnChannelsChange callbacks (and so two
// Engine.SetChannels reconciles) concurrently, which today cannot happen.
// Waking the owner keeps that property for free.
//
// Non-blocking: a wake-up already queued covers this caller too, and the
// ticker is the backstop if no watcher is running at all.
func (s *Store) ReloadNow() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// pollOnce reloads either file whose mtime has moved since it last loaded.
//
// The mtime is stamped only on success. Consuming the change up front means a
// single transient failure — fd exhaustion, EIO, a read that caught a
// non-atomic writer mid-write — permanently swallows that edit: no later poll
// sees a difference, so a revoked key keeps authenticating until someone
// touches the file again. The spool guards this same transient-vs-permanent
// distinction; the config watcher has to as well.
func (s *Store) pollOnce() {
	if m := mtime(s.cfg.KeysFile); !m.Equal(s.keysMtime) {
		if ring, err := LoadKeys(s.cfg.KeysFile); err != nil {
			logReloadFailure(&s.keysErrMtime, m, "keys reload failed; keeping previous keyring, will retry", err)
		} else {
			s.keys.Store(ring)
			s.keysMtime = m
			slog.Info("keys reloaded", "file", s.cfg.KeysFile)
		}
	}
	if m := mtime(s.cfg.ChannelsFile); !m.Equal(s.channelsMtime) {
		if chs, err := LoadChannels(s.cfg.ChannelsFile); err != nil {
			logReloadFailure(&s.channelsErrMtime, m, "channels reload failed; keeping previous set, will retry", err)
		} else {
			s.channels.Store(chs)
			s.channelsMtime = m
			slog.Info("channels reloaded", "file", s.cfg.ChannelsFile, "channels", chs.IDs())
			if s.OnChannelsChange != nil {
				s.OnChannelsChange(chs)
			}
		}
	}
}

// logReloadFailure reports a failed reload once per distinct mtime. The retry
// runs every poll regardless — this only stops a file left broken from filling
// the journal with the same line every few seconds.
func logReloadFailure(reported *time.Time, m time.Time, msg string, err error) {
	if reported.Equal(m) {
		return
	}
	*reported = m
	slog.Error(msg, "err", err)
}

func mtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
