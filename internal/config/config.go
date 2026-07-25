// Package config loads the hub config, the keys file and the channels file.
// Keys and channels hot-reload on mtime change with validate-before-swap: a
// broken hand-edit must never blank credentials or crash the process.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration is a time.Duration that unmarshals from strings like "2.5s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("durations are strings like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

type OpsConfig struct {
	Port int `json:"port"`
}

type HeartbeatConfig struct {
	URL      string   `json:"url"`
	Interval Duration `json:"interval"`
}

type Config struct {
	PublicPort         int              `json:"publicPort"`
	Ops                *OpsConfig       `json:"ops,omitempty"` // presence-enabled
	RateCapPerHour     int              `json:"rateCapPerHour"`
	ResponseWindow     Duration         `json:"responseWindow"`
	QueueTTL           Duration         `json:"queueTTL"`
	SpoolDir           string           `json:"spoolDir"`
	SpoolCapPerChannel int              `json:"spoolCapPerChannel"`
	DrainPace          Duration         `json:"drainPace"`
	AttemptTimeout     Duration         `json:"attemptTimeout"`
	Heartbeat          *HeartbeatConfig `json:"heartbeat,omitempty"`
	DeliveryLog        string           `json:"deliveryLog"`
	KeysFile           string           `json:"keysFile"`
	ChannelsFile       string           `json:"channelsFile"`
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
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
		KeysFile:           "keys.json",
		ChannelsFile:       "channels.json",
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.PublicPort <= 0 || cfg.PublicPort > 65535 {
		return nil, fmt.Errorf("%s: publicPort out of range", path)
	}
	if cfg.Ops != nil && (cfg.Ops.Port <= 0 || cfg.Ops.Port > 65535) {
		return nil, fmt.Errorf("%s: ops.port out of range", path)
	}
	// Every remaining knob is a positive quantity. The defaults above only
	// apply to *absent* fields, so an explicit 0 reaches the runtime as-is —
	// and a zero rate cap, attempt timeout or spool cap each break the hub in
	// a way nothing alerts on. Reject at load instead.
	if cfg.RateCapPerHour < 1 {
		return nil, fmt.Errorf("%s: rateCapPerHour must be at least 1 (the global cap is the only blast-radius guard; there is no \"unlimited\" setting)", path)
	}
	if cfg.SpoolCapPerChannel < 1 {
		return nil, fmt.Errorf("%s: spoolCapPerChannel must be at least 1", path)
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"responseWindow", cfg.ResponseWindow.Duration},
		{"queueTTL", cfg.QueueTTL.Duration},
		{"drainPace", cfg.DrainPace.Duration},
		{"attemptTimeout", cfg.AttemptTimeout.Duration},
	} {
		if d.val <= 0 {
			return nil, fmt.Errorf("%s: %s must be greater than zero", path, d.name)
		}
	}
	if cfg.SpoolDir == "" {
		return nil, fmt.Errorf("%s: spoolDir is required", path)
	}
	if cfg.Heartbeat != nil {
		if cfg.Heartbeat.URL == "" {
			return nil, fmt.Errorf("%s: heartbeat.url is required when heartbeat is set", path)
		}
		if cfg.Heartbeat.Interval.Duration <= 0 {
			cfg.Heartbeat.Interval = Duration{60 * time.Second}
		}
	}
	return cfg, nil
}
