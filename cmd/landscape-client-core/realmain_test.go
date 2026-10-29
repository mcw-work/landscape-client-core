package main

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// installSnapctlConfig fakes snapctl so `get <key>` prints values[key] and
// `set ...` exits with setCode.
func installSnapctlConfig(t *testing.T, values map[string]string, setCode int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\nif [ \"$1\" = set ]; then exit " + strconv.Itoa(setCode) + "; fi\ncase \"$2\" in\n")
	for _, k := range slices.Sorted(maps.Keys(values)) {
		b.WriteString(k + ") printf '%s' '" + values[k] + "' ;;\n")
	}
	b.WriteString("esac\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapctl"), []byte(b.String()), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// restoreDefaultLogger undoes the slog.SetDefault performed by realMain.
func restoreDefaultLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func validConfig(url string) map[string]string {
	return map[string]string{
		"url":               url,
		"account-name":      "acct",
		"computer-title":    "host",
		"exchange-interval": "3600",
		"ping-interval":     "3600",
	}
}

func TestRealMain_Flags(t *testing.T) {
	var stderr strings.Builder
	if code := realMain(context.Background(), []string{"-h"}, &stderr, ""); code != 0 {
		t.Errorf("-h exit code = %d, want 0", code)
	}
	if code := realMain(context.Background(), []string{"--no-such-flag"}, &stderr, ""); code != 2 {
		t.Errorf("unknown flag exit code = %d, want 2", code)
	}
}

func TestRealMain_ValidateConfig(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   int
	}{
		{"fresh install", map[string]string{}, 0},
		{"valid", validConfig("https://ls.example.com/message-system"), 0},
		{"bad scheme", validConfig("ftp://ls.example.com"), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installSnapctlConfig(t, tc.values, 0)
			var stderr strings.Builder
			if code := realMain(context.Background(), []string{"--validate-config"}, &stderr, ""); code != tc.want {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tc.want, stderr.String())
			}
			if tc.want != 0 && !strings.Contains(stderr.String(), "config error") {
				t.Errorf("stderr = %q, want a config error", stderr.String())
			}
		})
	}
}

func TestRealMain_SyncConfDB(t *testing.T) {
	for _, tc := range []struct {
		setCode int
		want    int
	}{{0, 0}, {1, 1}} {
		installSnapctlConfig(t, validConfig("https://ls.example.com/message-system"), tc.setCode)
		var stderr strings.Builder
		if code := realMain(context.Background(), []string{"--sync-confdb"}, &stderr, ""); code != tc.want {
			t.Errorf("snapctl set exit %d: code = %d, want %d (stderr %q)", tc.setCode, code, tc.want, stderr.String())
		}
	}
}

func TestRealMain_MissingConfigFails(t *testing.T) {
	installSnapctlConfig(t, map[string]string{}, 0)
	var stderr strings.Builder
	if code := realMain(context.Background(), nil, &stderr, ""); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing required config") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRealMain_BadSSLKeyFails(t *testing.T) {
	restoreDefaultLogger(t)
	values := validConfig("https://ls.example.com/message-system")
	values["ssl-public-key"] = filepath.Join(t.TempDir(), "missing.pem")
	installSnapctlConfig(t, values, 0)
	t.Setenv("SNAP_COMMON", "")

	var stderr strings.Builder
	if code := realMain(context.Background(), nil, &stderr, ""); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "failed to create transport client") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRealMain_DaemonFailureExitsNonZero(t *testing.T) {
	restoreDefaultLogger(t)
	installSnapctlConfig(t, validConfig("http://127.0.0.1:1/message-system"), 0)
	snapCommon := t.TempDir()
	t.Setenv("SNAP_COMMON", snapCommon)
	if err := os.WriteFile(filepath.Join(snapCommon, "state.json"), []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stderr strings.Builder
	if code := realMain(ctx, nil, &syncBuilder{b: &stderr}, filepath.Join(snapCommon, "no-snapd.socket")); code != 1 {
		t.Errorf("exit code = %d, want 1 so snapd's on-failure restart applies", code)
	}
	if ctx.Err() != nil {
		t.Error("daemon only stopped at the test deadline")
	}
}

func TestRealMain_RunsDaemonUntilCancelled(t *testing.T) {
	var mu sync.Mutex
	var exchanges int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		exchanges++
		mu.Unlock()
		_, _ = w.Write([]byte("d;"))
	}))
	defer srv.Close()

	for _, level := range []string{"debug", "info", "warn", "warning", "error"} {
		t.Run(level, func(t *testing.T) {
			restoreDefaultLogger(t)
			values := validConfig(srv.URL + "/message-system")
			values["log-level"] = level
			installSnapctlConfig(t, values, 0)
			snapCommon := t.TempDir()
			t.Setenv("SNAP_COMMON", snapCommon)

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			var stderr strings.Builder
			code := realMain(ctx, []string{"--handler-concurrency", "2"}, &syncBuilder{b: &stderr}, filepath.Join(snapCommon, "no-snapd.socket"))
			if code != 0 {
				t.Fatalf("exit code = %d", code)
			}
			if _, err := os.Stat(filepath.Join(snapCommon, "state.json")); err != nil {
				t.Errorf("daemon did not persist state under SNAP_COMMON: %v", err)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges == 0 {
		t.Error("daemon never contacted the server")
	}
}

// A runner failure must stop the daemon without waiting for a signal, and be
// returned so the process exits non-zero and snapd restarts it.
func TestRun_RunnerFailureStopsDaemon(t *testing.T) {
	snapCommon := t.TempDir()
	statePath := filepath.Join(snapCommon, "state.json")
	if err := os.WriteFile(statePath, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.URL = "http://127.0.0.1:1/message-system"
	cfg.PingInterval = time.Hour

	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), deps{
			cfg:                cfg,
			store:              persist.New(statePath),
			transport:          tc,
			snapd:              &snapd.MockClient{},
			snapCommon:         snapCommon,
			handlerConcurrency: 1,
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exchange runner") {
			t.Errorf("run error = %v, want the exchange runner failure", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run kept going after the exchange runner failed")
	}
}

func TestRun_WatchdogTicksWithoutFalsePositive(t *testing.T) {
	orig := watchdogInterval
	watchdogInterval = 5 * time.Millisecond
	t.Cleanup(func() { watchdogInterval = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("d;"))
	}))
	defer srv.Close()
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	snapCommon := t.TempDir()
	cfg := config.Defaults()
	cfg.URL = srv.URL
	cfg.PingInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := run(ctx, deps{
		cfg:                cfg,
		store:              persist.New(filepath.Join(snapCommon, "state.json")),
		transport:          tc,
		snapd:              &snapd.MockClient{},
		snapCommon:         snapCommon,
		handlerConcurrency: 1,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// A false positive would have stopped the daemon well before the deadline.
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("run stopped after %v; the watchdog fired on healthy plugins", elapsed)
	}
}

type syncBuilder struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
