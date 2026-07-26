// Package config loads the hub config, the keys file and the channels file.
// All three are TOML: they are hand-edited operational files, and being able
// to leave a comment next to a parked channel or a rotating key is worth the
// one dependency.
//
// Keys and channels hot-reload on mtime change with validate-before-swap: a
// broken hand-edit must never blank credentials or crash the process.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ryanlewis/hubbub/internal/adminauth"
)

// Duration is a time.Duration written as a string: interval = "2.5s". TOML has
// no duration type, and encoding.TextUnmarshaler is what the decoder reaches
// for, so this works whatever the surrounding format.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("durations are strings like \"30s\": %w", err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

type OpsConfig struct {
	Port int `toml:"port"`
}

type HeartbeatConfig struct {
	URL      string   `toml:"url"`
	Interval Duration `toml:"interval"`
}

// AdminConfig enables the admin dashboard. It has no port of its own: the
// dashboard rides the public listener, because that is the only one the
// deployment's proxy puts an identity in front of.
type AdminConfig struct {
	Auth          string   `toml:"auth"`
	AllowedEmails []string `toml:"allowed_emails"`
}

type Config struct {
	PublicPort         int              `toml:"public_port"`
	Ops                *OpsConfig       `toml:"ops"`   // presence-enabled
	Admin              *AdminConfig     `toml:"admin"` // presence-enabled
	RateCapPerHour     int              `toml:"rate_cap_per_hour"`
	ResponseWindow     Duration         `toml:"response_window"`
	QueueTTL           Duration         `toml:"queue_ttl"`
	SpoolDir           string           `toml:"spool_dir"`
	SpoolCapPerChannel int              `toml:"spool_cap_per_channel"`
	DrainPace          Duration         `toml:"drain_pace"`
	AttemptTimeout     Duration         `toml:"attempt_timeout"`
	Heartbeat          *HeartbeatConfig `toml:"heartbeat"` // presence-enabled
	DeliveryLog        string           `toml:"delivery_log"`
	KeysFile           string           `toml:"keys_file"`
	ChannelsFile       string           `toml:"channels_file"`
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{ // defaults; all implementation-tuned
		PublicPort:         8080,
		RateCapPerHour:     60,
		ResponseWindow:     Duration{2500 * time.Millisecond},
		QueueTTL:           Duration{6 * time.Hour},
		SpoolDir:           "spool",
		SpoolCapPerChannel: 100,
		DrainPace:          Duration{2 * time.Second},
		AttemptTimeout:     Duration{10 * time.Second},
		DeliveryLog:        "delivery.log",
		KeysFile:           "keys.toml",
		ChannelsFile:       "channels.toml",
	}
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := rejectUnknown(path, md); err != nil {
		return nil, err
	}

	if cfg.PublicPort <= 0 || cfg.PublicPort > 65535 {
		return nil, fmt.Errorf("%s: public_port out of range", path)
	}
	if cfg.Ops != nil && (cfg.Ops.Port <= 0 || cfg.Ops.Port > 65535) {
		return nil, fmt.Errorf("%s: ops.port out of range", path)
	}
	// Every remaining knob is a positive quantity. The defaults above only
	// apply to *absent* fields, so an explicit 0 reaches the runtime as-is —
	// and a zero rate cap, attempt timeout or spool cap each break the hub in
	// a way nothing alerts on. Reject at load instead.
	if cfg.RateCapPerHour < 1 {
		return nil, fmt.Errorf("%s: rate_cap_per_hour must be at least 1 (the global cap is the only blast-radius guard; there is no \"unlimited\" setting)", path)
	}
	if cfg.SpoolCapPerChannel < 1 {
		return nil, fmt.Errorf("%s: spool_cap_per_channel must be at least 1", path)
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"response_window", cfg.ResponseWindow.Duration},
		{"queue_ttl", cfg.QueueTTL.Duration},
		{"drain_pace", cfg.DrainPace.Duration},
		{"attempt_timeout", cfg.AttemptTimeout.Duration},
	} {
		if d.val <= 0 {
			return nil, fmt.Errorf("%s: %s must be greater than zero", path, d.name)
		}
	}
	if cfg.SpoolDir == "" {
		return nil, fmt.Errorf("%s: spool_dir is required", path)
	}
	if cfg.Heartbeat != nil {
		if cfg.Heartbeat.URL == "" {
			return nil, fmt.Errorf("%s: heartbeat.url is required when [heartbeat] is set", path)
		}
		if cfg.Heartbeat.Interval.Duration <= 0 {
			cfg.Heartbeat.Interval = Duration{60 * time.Second}
		}
	}
	if cfg.Admin != nil {
		if cfg.Admin.Auth == "" {
			return nil, fmt.Errorf("%s: admin.auth is required when [admin] is set (known: %s)", path, strings.Join(adminauth.Names(), ", "))
		}
		if _, err := adminauth.New(cfg.Admin.Auth); err != nil {
			return nil, fmt.Errorf("%s: admin: %w", path, err)
		}
		// The allowlist is checked here as well as in adminauth.NewGuard so
		// the failure is a refusal to start rather than a dashboard that
		// 403s everyone — an empty list is a half-written config, and the
		// operator needs to hear about it at the same moment they made it.
		if len(cfg.Admin.AllowedEmails) == 0 {
			return nil, fmt.Errorf("%s: admin.allowed_emails must list at least one address; there is deliberately no \"allow everyone\"", path)
		}
	}
	return cfg, nil
}

// rejectUnknown turns anything the decoder didn't consume into an error, so a
// typo'd key is loud rather than silently ignored — the TOML equivalent of
// DisallowUnknownFields. (Duplicate keys need no guard of their own: TOML
// rejects them at the parser, where JSON silently kept the last one.)
func rejectUnknown(path string, md toml.MetaData) error {
	und := md.Undecoded()
	if len(und) == 0 {
		return nil
	}
	keys := make([]string, 0, len(und))
	for _, k := range und {
		keys = append(keys, k.String())
	}
	return fmt.Errorf("%s: unknown setting(s): %s", path, strings.Join(keys, ", "))
}
