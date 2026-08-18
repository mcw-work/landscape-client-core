// Package main is the entry point for landscape-client-core.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/manager"
	"github.com/canonical/landscape-client-core/internal/monitor"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/ping"
	"github.com/canonical/landscape-client-core/internal/snapd"
	"github.com/canonical/landscape-client-core/internal/transport"
	"github.com/canonical/landscape-client-core/internal/version"
	"golang.org/x/sync/errgroup"
)

func main() {
	validateOnly := flag.Bool("validate-config", false, "Validate configuration and exit")
	syncConfDB := flag.Bool("sync-confdb", false, "Publish validated snap config to the confdb admin view and exit")
	handlerConcurrency := flag.Int("handler-concurrency", 100, "Maximum number of concurrent handler executions")
	flag.Parse()

	// Handle --validate-config before daemon startup.
	// ValidateForHook tolerates fresh installs (no config) and incremental
	// wizard configuration, erroring only when all required keys are present
	// but something is invalid.
	if *validateOnly {
		if err := config.ValidateForHook(&snapctlLoader{}); err != nil {
			fmt.Fprintf(os.Stderr, "landscape-client-core: config error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *syncConfDB {
		if err := publishConfDB(&snapctlLoader{}); err != nil {
			fmt.Fprintf(os.Stderr, "landscape-client-core: confdb sync error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	snapCommon := os.Getenv("SNAP_COMMON")
	if snapCommon == "" {
		snapCommon = "/var/snap/landscape-client-core/common"
	}

	// Load config via snapctl.
	cfg, err := config.Load(&snapctlLoader{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "landscape-client-core: config error: %v\n", err)
		os.Exit(1)
	}

	// Configure slog logger.
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Open persist store.
	statePath := filepath.Join(snapCommon, "state.json")
	store := persist.New(statePath)

	// Create transport client (URL is provided per-request via exchange).
	tc, err := transport.New(transport.Config{
		// The server extracts the client version from User-Agent: landscape-client/<version>
		// and uses it to check snap monitoring compatibility (requires >= 23.02+git6282).
		UserAgent:    version.UserAgent,
		SSLPublicKey: cfg.SSLPublicKey,
		HTTPProxy:    cfg.HTTPProxy,
		HTTPSProxy:   cfg.HTTPSProxy,
	})
	if err != nil {
		slog.Error("failed to create transport client", "error", err)
		os.Exit(1)
	}

	// Create snapd client.
	snapdClient := snapd.New("/run/snapd.socket")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, deps{
		cfg:                cfg,
		store:              store,
		transport:          tc,
		snapd:              snapdClient,
		snapCommon:         snapCommon,
		handlerConcurrency: *handlerConcurrency,
	}); err != nil {
		slog.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}

// deps holds the constructed collaborators run() needs, so the daemon wiring
// can be exercised in tests without touching snapctl or the real snapd socket.
type deps struct {
	cfg                *config.Config
	store              *persist.Store
	transport          *transport.Client
	snapd              snapd.Client
	snapCommon         string
	handlerConcurrency int
}

// run wires up the exchange, monitor, manager and ping loops and blocks until
// ctx is cancelled or a runner fails, then shuts down gracefully. It contains
// no behaviour that main() did not previously perform inline.
func run(ctx context.Context, d deps) error {
	cfg := d.cfg
	store := d.store
	tc := d.transport
	snapdClient := d.snapd
	snapCommon := d.snapCommon

	// Create exchange.
	exc := exchange.New(cfg, store, tc)

	// Create monitor runner with all plugins.
	snapPackages := monitor.NewSnapPackages(snapdClient)
	plugins := []monitor.Plugin{
		monitor.NewCPUUsage(),
		monitor.NewMemoryInfo(),
		monitor.NewLoadAverage(),
		monitor.NewNetworkActivity(),
		monitor.NewActiveProcessInfo(),
		monitor.NewTemperature(),
		monitor.NewRebootRequired(snapdClient),
		monitor.NewComputerInfo(snapdClient),
		monitor.NewProcessorInfo(),
		monitor.NewNetworkDevice(),
		monitor.NewMountInfo(),
		monitor.NewUsers(),
		monitor.NewHardwareInfo(),
		snapPackages,
		monitor.NewSnapServices(snapdClient),
	}
	monRunner := monitor.New(plugins, exc, store)

	// sendSnapUpdate sends an immediate snaps message after a snap operation.
	// Uses the daemon context so a snapd call cannot outlive shutdown.
	sendSnapUpdate := func() { snapPackages.SendNow(ctx, exc) }

	// Create manager runner with all handlers.
	handlers := []manager.Handler{
		&manager.InstallSnapHandler{Snapd: snapdClient, OnComplete: sendSnapUpdate},
		&manager.RemoveSnapHandler{Snapd: snapdClient, OnComplete: sendSnapUpdate},
		&manager.RefreshSnapHandler{Snapd: snapdClient, OnComplete: sendSnapUpdate},
		&manager.StartServiceHandler{Snapd: snapdClient},
		&manager.StopServiceHandler{Snapd: snapdClient},
		&manager.RestartServiceHandler{Snapd: snapdClient},
		manager.NewShutdownHandler(),
		manager.NewScriptExecHandler(snapCommon, transport.NewAttachmentFetcher(tc, cfg.URL, store)),
	}
	mgRunner := manager.NewRunner(handlers, exc, exc, d.handlerConcurrency)
	mgRunner.Register()

	// Create ping loop. The Pinger periodically POSTs to the ping server and
	// triggers an urgent exchange when the server reports messages are waiting.
	pinger := ping.New(
		cfg.GetPingURL(),
		exc.InsecureID,
		exc.TriggerExchange,
		cfg.PingInterval,
		tc,
	)

	// Handle set-intervals messages from the server: update ping and/or
	// exchange intervals when the server requests it.
	exc.Subscribe("set-intervals", func(_ context.Context, msg exchange.Message) {
		if v, ok := msg["ping"]; ok {
			if secs, ok := v.(int64); ok && secs > 0 {
				pinger.SetInterval(time.Duration(secs) * time.Second)
				slog.Info("ping interval updated", "seconds", secs)
			}
		}
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Run goroutines under a shared errgroup context.
	eg, groupCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		err := exc.Run(groupCtx)
		if err != nil {
			slog.Error("exchange runner failed", "error", err)
			return fmt.Errorf("exchange runner: %w", err)
		}
		if groupCtx.Err() != nil {
			slog.Info("exchange stopped due to context cancellation", "error", groupCtx.Err())
		}
		return nil
	})
	eg.Go(func() error {
		err := monRunner.Run(groupCtx)
		if err != nil {
			slog.Error("monitor runner failed", "error", err)
			return fmt.Errorf("monitor runner: %w", err)
		}
		if groupCtx.Err() != nil {
			slog.Info("monitor stopped due to context cancellation", "error", groupCtx.Err())
		}
		return nil
	})
	eg.Go(func() error {
		err := pinger.Run(groupCtx)
		if err != nil {
			slog.Error("ping runner failed", "error", err)
			return fmt.Errorf("ping runner: %w", err)
		}
		if groupCtx.Err() != nil {
			slog.Info("ping stopped due to context cancellation", "error", groupCtx.Err())
		}
		return nil
	})
	// Watchdog: restart-condition only covers process exit, so a goroutine
	// blocked forever in a syscall keeps the daemon alive while silently
	// reporting nothing. Exiting non-zero lets snapd's restart-condition recover
	// a wedged daemon.
	eg.Go(func() error {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-groupCtx.Done():
				return nil
			case <-ticker.C:
				if stale := monRunner.StaleSources(); len(stale) > 0 {
					slog.Error("watchdog: sources stopped making progress; exiting for restart",
						"sources", stale)
					return fmt.Errorf("watchdog: stale sources: %v", stale)
				}
			}
		}
	})

	groupDone := make(chan error, 1)
	go func() {
		groupDone <- eg.Wait()
	}()

	// Wait for shutdown signal or the first runner error.
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-groupDone:
		if err != nil {
			slog.Error("first runner error", "error", err)
		}
		cancel()
		slog.Info("shutting down")
	}

	if err := mgRunner.WaitWithTimeout(5 * time.Second); err != nil {
		slog.Error("error waiting for manager runner", "error", err)
	}

	// Wait up to 5s for goroutines to finish.
	select {
	case err := <-groupDone:
		if err != nil {
			slog.Error("runner group exited with error", "error", err)
		}
	case <-time.After(5 * time.Second):
		slog.Warn("shutdown timeout, exiting")
	}

	return nil
}

// snapctlLoader implements config.Loader using snapctl.
type snapctlLoader struct{}

func (s *snapctlLoader) Get(key string) (string, error) {
	out, err := exec.Command("snapctl", "get", key).Output()
	if err != nil {
		return "", fmt.Errorf("snapctl get %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func publishConfDB(loader config.Loader) error {
	payload, ok, err := config.ConfDBViewJSON(loader)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	arg := "config=" + payload
	cmd := exec.Command("snapctl", "set", ":landscape-client-admin", "--view", arg)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("snapctl set :landscape-client-admin --view: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("snapctl set :landscape-client-admin --view: %w", err)
	}
	return nil
}
