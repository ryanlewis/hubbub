// hubbub: a tiny self-hosted notification fan-out hub.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ryanlewis/hubbub/internal/adminauth"
	"github.com/ryanlewis/hubbub/internal/confedit"
	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/heartbeat"
	"github.com/ryanlewis/hubbub/internal/httpapi"
	"github.com/ryanlewis/hubbub/internal/metrics"
	"github.com/ryanlewis/hubbub/internal/outbox"
)

func main() {
	configPath := flag.String("config", "hubbub.toml", "path to the config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// buildAdmin assembles the dashboard's dependencies.
//
// The two editable files are handed the *real* loaders as validators, so a
// candidate edit is checked by exactly the code that will have to load it
// afterwards. Anything the dashboard accepts, the hub can read.
func buildAdmin(cfg *config.Config) (*httpapi.Admin, error) {
	provider, err := adminauth.New(cfg.Admin.Auth)
	if err != nil {
		return nil, err
	}
	guard, err := adminauth.NewGuard(provider, cfg.Admin.AllowedEmails)
	if err != nil {
		return nil, err
	}
	return &httpapi.Admin{
		Guard: guard,
		Keys: &confedit.File{Path: cfg.KeysFile, Validate: func(src []byte, name string) error {
			_, err := config.ParseKeys(src, name)
			return err
		}},
		Channels: &confedit.File{Path: cfg.ChannelsFile, Validate: func(src []byte, name string) error {
			_, err := config.ParseChannels(src, name)
			return err
		}},
	}, nil
}

func run(configPath string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}

	logger, err := dlog.Open(cfg.DeliveryLog)
	if err != nil {
		return fmt.Errorf("delivery log: %w", err)
	}
	defer logger.Close()

	store, err := config.NewStore(cfg)
	if err != nil {
		return err
	}

	m := metrics.New()
	reg := outbox.NewRegistry()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine := outbox.NewEngine(ctx, outbox.Options{
		SpoolDir:       cfg.SpoolDir,
		TTL:            cfg.QueueTTL.Duration,
		CapPerChannel:  cfg.SpoolCapPerChannel,
		DrainPace:      cfg.DrainPace.Duration,
		AttemptTimeout: cfg.AttemptTimeout.Duration,
	}, reg, logger, m)
	// Deferred, not called at the bottom: an early return via errCh has to
	// drain the workers too. LIFO puts this ahead of logger.Close() above, so
	// a delivery settling in the shutdown window still gets its terminal line.
	defer engine.Shutdown(5 * time.Second)

	// Every channel goes to the engine, disabled ones included: it has to tell
	// a paused channel (keep its spool) from a removed one (settle the backlog
	// and clean up).
	syncWorkers := func(chs *config.ChannelSet) error {
		var rts []outbox.ChannelRuntime
		for _, ch := range chs.All() {
			rts = append(rts, outbox.ChannelRuntime{ID: ch.ID, Enabled: ch.Enabled, Adapter: ch.Adapter})
		}
		return engine.SetChannels(rts)
	}
	store.OnChannelsChange = func(chs *config.ChannelSet) {
		if err := syncWorkers(chs); err != nil {
			slog.Error("channel reload left channels inactive", "err", err)
		}
	}
	// At boot a spool that won't initialise is fatal: running on with a
	// channel that silently fails every send is worse than not starting, and
	// the supervisor plus the dead-man switch make the failure loud.
	if err := syncWorkers(store.Channels()); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	go store.Watch(ctx, 2*time.Second)

	srv := &httpapi.Server{
		Store:   store,
		Engine:  engine,
		Reg:     reg,
		Log:     logger,
		Metrics: m,
		Rate:    httpapi.NewRateLimiter(cfg.RateCapPerHour, time.Hour),
		Window:  cfg.ResponseWindow.Duration,
	}

	if cfg.Admin != nil {
		admin, err := buildAdmin(cfg)
		if err != nil {
			return fmt.Errorf("admin: %w", err)
		}
		srv.Admin = admin
		// A crashed write can strand a temp file holding a bearer key or an
		// SMTP password in the config directory, where nothing else would
		// ever notice it.
		for _, p := range confedit.SweepTemps(cfg.KeysFile, cfg.ChannelsFile) {
			slog.Warn("removed a leftover config temp file from an interrupted admin write", "file", p)
		}
		slog.Info("admin dashboard enabled", "auth", cfg.Admin.Auth, "allowed", len(cfg.Admin.AllowedEmails))
		// The provider trusts something it cannot verify by itself. Say so at
		// every boot rather than leaving the assumption in a doc.
		if w := admin.Guard.Provider.Warning(); w != "" {
			slog.Warn(w)
		}
	}

	// Explicit timeouts on both listeners: Go's zero-values are unlimited,
	// and slow-connection fd exhaustion is exactly the alive-but-not-serving
	// state the heartbeat self-probe watches for.
	newServer := func(port int, h http.Handler) *http.Server {
		return &http.Server{
			Addr:              fmt.Sprintf(":%d", port),
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		}
	}

	public := newServer(cfg.PublicPort, srv.PublicMux())
	errCh := make(chan error, 2)
	go func() {
		slog.Info("public listener up", "addr", public.Addr, "version", httpapi.Version)
		if err := public.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("public listener: %w", err)
		}
	}()

	var ops *http.Server
	if cfg.Ops != nil {
		ops = newServer(cfg.Ops.Port, srv.OpsMux())
		go func() {
			slog.Info("ops listener up", "addr", ops.Addr)
			if err := ops.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("ops listener: %w", err)
			}
		}()
	}

	if cfg.Heartbeat != nil {
		p := &heartbeat.Pinger{
			TargetURL: cfg.Heartbeat.URL,
			SelfURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.PublicPort),
			Interval:  cfg.Heartbeat.Interval.Duration,
		}
		go p.Run(ctx)
		slog.Info("dead-man heartbeat armed", "interval", cfg.Heartbeat.Interval.String())
	} else {
		// Absent must not be silent. The heartbeat is the only thing that
		// notices process-dead, VM-down or outbound-dead, and a boot log that
		// looks entirely healthy without it is the exact failure it exists to
		// prevent.
		slog.Warn("no dead-man heartbeat configured: nothing will notice if this hub, its VM or its outbound path dies — set heartbeat.url")
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = public.Shutdown(shutCtx)
	if ops != nil {
		_ = ops.Shutdown(shutCtx)
	}
	return nil
}
