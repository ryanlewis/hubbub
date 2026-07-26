package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Store holds the live keyring and channel set, hot-reloading both when their
// content changes. Reload trigger is a poll (stdlib-only; an atomic rename swaps
// the inode out from under a kernel watch anyway). A failed parse keeps the old
// set — validate-before-swap.
type Store struct {
	cfg *Config

	keys     atomic.Pointer[Keyring]
	channels atomic.Pointer[ChannelSet]

	// Content hashes of the last *successful* load, and a token for the last
	// failure already reported. A file is retried every poll until it loads, so
	// the second pair keeps a permanently-broken file from repeating one error
	// line forever.
	keysHash         string
	channelsHash     string
	keysErrToken     string
	channelsErrToken string

	// OnChannelsChange is invoked (from the watch goroutine) after a
	// successful channels reload, so the outbox can rebuild its workers.
	OnChannelsChange func(*ChannelSet)

	// reload wakes the watcher early. Buffered at 1 and sent to without
	// blocking: a pending wake-up already covers any number of callers.
	reload chan struct{}
}

func NewStore(cfg *Config) (*Store, error) {
	s := &Store{cfg: cfg, reload: make(chan struct{}, 1)}

	keySrc, keyHash, err := readHashed(cfg.KeysFile)
	if err != nil {
		return nil, err
	}
	ring, err := ParseKeys(keySrc, cfg.KeysFile)
	if err != nil {
		return nil, err
	}
	chSrc, chHash, err := readHashed(cfg.ChannelsFile)
	if err != nil {
		return nil, err
	}
	chs, err := ParseChannels(chSrc, cfg.ChannelsFile)
	if err != nil {
		return nil, err
	}

	s.keys.Store(ring)
	s.channels.Store(chs)
	// Stamped from the bytes that were actually parsed, so the first poll cannot
	// see a spurious change.
	s.keysHash = keyHash
	s.channelsHash = chHash
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
// It signals rather than reloading inline, deliberately. Every hash and token
// field on Store is unsynchronised, and safe only because exactly one goroutine
// — the watcher — ever touches them; calling pollOnce from an HTTP handler would
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

// pollOnce reloads either file whose content has changed since it last loaded.
//
// The hash is stamped only on success. Consuming the change up front means a
// single transient failure — fd exhaustion, EIO, a read that caught a
// non-atomic writer mid-write — permanently swallows that edit: no later poll
// sees a difference, so a revoked key keeps authenticating until someone
// touches the file again. The spool guards this same transient-vs-permanent
// distinction; the config watcher has to as well.
func (s *Store) pollOnce() {
	s.reloadKeys()
	s.reloadChannels()
}

func (s *Store) reloadKeys() {
	src, hash, err := readHashed(s.cfg.KeysFile)
	if err != nil {
		// A file that will not read is not a change. Parsing what we did not get
		// would hand ParseKeys an empty document, which is a *valid* keys file
		// with no callers in it — every key revoked, from one EIO.
		logReloadFailure(&s.keysErrToken, "unread:"+err.Error(),
			"keys unreadable; keeping previous keyring, will retry", err)
		return
	}
	if hash == s.keysHash {
		return
	}
	ring, err := ParseKeys(src, s.cfg.KeysFile)
	if err != nil {
		logReloadFailure(&s.keysErrToken, hash, "keys reload failed; keeping previous keyring, will retry", err)
		return
	}
	s.keys.Store(ring)
	s.keysHash = hash
	s.keysErrToken = ""
	slog.Info("keys reloaded", "file", s.cfg.KeysFile)
}

func (s *Store) reloadChannels() {
	src, hash, err := readHashed(s.cfg.ChannelsFile)
	if err != nil {
		logReloadFailure(&s.channelsErrToken, "unread:"+err.Error(),
			"channels unreadable; keeping previous set, will retry", err)
		return
	}
	if hash == s.channelsHash {
		return
	}
	chs, err := ParseChannels(src, s.cfg.ChannelsFile)
	if err != nil {
		logReloadFailure(&s.channelsErrToken, hash, "channels reload failed; keeping previous set, will retry", err)
		return
	}
	s.channels.Store(chs)
	s.channelsHash = hash
	s.channelsErrToken = ""
	slog.Info("channels reloaded", "file", s.cfg.ChannelsFile, "channels", chs.IDs())
	if s.OnChannelsChange != nil {
		s.OnChannelsChange(chs)
	}
}

// logReloadFailure reports a failed reload once per distinct token. The retry
// runs every poll regardless — this only stops a file left broken from filling
// the journal with the same line every few seconds.
func logReloadFailure(reported *string, token, msg string, err error) {
	if *reported == token {
		return
	}
	*reported = token
	slog.Error(msg, "err", err)
}

// readHashed reads a config file and hashes what it read.
//
// Content, not mtime. Two writes inside one filesystem timestamp tick — the
// dashboard saving twice in a second, an editor that puts the mtime back — are
// indistinguishable by timestamp, and because the comparison is against the
// last *loaded* state, missing one misses it permanently: no later poll sees a
// difference either, so a key revoked by the second write goes on
// authenticating until something unrelated touches the file. These files are a
// few kilobytes and the poll is seconds apart, so hashing every one of them
// costs nothing worth measuring.
func readHashed(path string) (src []byte, hash string, err error) {
	src, err = os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(src)
	return src, hex.EncodeToString(sum[:]), nil
}
