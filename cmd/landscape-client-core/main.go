// Package main is the entry point for landscape-client-core.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
)

func main() {
	validateOnly := flag.Bool("validate-config", false, "Validate configuration and exit")
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
		log.Fatalf("transport: %v", err)
	}

	// Create snapd client.
	snapdClient := snapd.New("/run/snapd.socket")

	// Create exchange.
	exc := exchange.New(cfg, store, tc)

	// Create monitor runner with all plugins.
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
		monitor.NewSnapPackages(snapdClient),
		monitor.NewSnapServices(snapdClient),
	}
	monRunner := monitor.New(plugins, exc, store)

	// Create manager runner with all handlers.
	handlers := []manager.Handler{
		&manager.InstallSnapHandler{Snapd: snapdClient},
		&manager.RemoveSnapHandler{Snapd: snapdClient},
		&manager.RefreshSnapHandler{Snapd: snapdClient},
		&manager.StartServiceHandler{Snapd: snapdClient},
		&manager.StopServiceHandler{Snapd: snapdClient},
		&manager.RestartServiceHandler{Snapd: snapdClient},
		manager.NewShutdownHandler(),
		manager.NewScriptExecHandler(snapCommon, transport.NewAttachmentFetcher(tc, cfg.URL, store)),
	}
	mgRunner := manager.NewRunner(handlers, exc, exc)
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
				log.Printf("ping: interval updated to %ds", secs)
			}
		}
	})

	// Signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Run goroutines.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := exc.Run(ctx); err != nil {
			log.Printf("exchange: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := monRunner.Run(ctx); err != nil {
			log.Printf("monitor: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := pinger.Run(ctx); err != nil {
			log.Printf("ping: %v", err)
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	log.Println("landscape-client-core: shutting down")

	// Wait up to 5s for goroutines to finish.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Println("landscape-client-core: shutdown timeout, exiting")
	}
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
