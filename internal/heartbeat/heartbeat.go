// Package heartbeat pings an external dead-man's-switch service on a timer.
// Each tick is gated on one local check: a GET /health against the hub's own
// public listener — no 200, no ping. A bare timer attests only that the
// process is alive; the self-probe makes the ping attest the hub is serving.
// The outbound ping itself covers process-dead, VM-down and outbound-dead.
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type Pinger struct {
	TargetURL string // the dead-man service (plain config; any provider)
	SelfURL   string // http://127.0.0.1:<publicPort>/health
	Interval  time.Duration
	Client    *http.Client
}

func (p *Pinger) Run(ctx context.Context) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !p.selfProbe(ctx, client) {
				slog.Error("heartbeat self-probe failed; withholding dead-man ping")
				continue
			}
			if err := p.ping(ctx, client); err != nil {
				slog.Error("dead-man ping failed", "err", err)
			}
		}
	}
}

func (p *Pinger) selfProbe(ctx context.Context, client *http.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.SelfURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *Pinger) ping(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.TargetURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// A completed request is not a fed dead-man. A typo'd or rotated check URL
	// answers 404 forever, and treating that as success means the switch fires
	// hours later against a hub that was healthy all along, with nothing in
	// the hub's own logs to explain it.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("dead-man service answered %d", resp.StatusCode)
	}
	return nil
}
