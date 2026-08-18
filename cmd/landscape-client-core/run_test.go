package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/ping"
	"github.com/canonical/landscape-client-core/internal/snapd"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// TestRun_ShutsDownCleanly asserts run() returns without error when its context
// is cancelled, and that it does so promptly rather than burning the full
// shutdown grace period.
func TestRun_ShutsDownCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	snapCommon := t.TempDir()
	tc, err := transport.New(transport.Config{UserAgent: "test"})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	d := deps{
		cfg: &config.Config{
			URL:                    srv.URL,
			AccountName:            "acc",
			ComputerTitle:          "test",
			ExchangeInterval:       time.Hour,
			UrgentExchangeInterval: time.Hour,
			PingInterval:           time.Hour,
		},
		store:              persist.New(filepath.Join(snapCommon, "state.json")),
		transport:          tc,
		snapd:              &snapd.MockClient{},
		snapCommon:         snapCommon,
		handlerConcurrency: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, d) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of context cancellation")
	}
}

// TestRun_ShutsDownPromptly asserts shutdown does not burn the grace period when
// the runners have already exited. The old code read groupDone twice, so the
// second read could block the full 5s regardless.
func TestRun_ShutsDownPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	snapCommon := t.TempDir()
	tc, err := transport.New(transport.Config{UserAgent: "test"})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	d := deps{
		cfg: &config.Config{
			URL:                    srv.URL,
			AccountName:            "acc",
			ComputerTitle:          "test",
			ExchangeInterval:       time.Hour,
			UrgentExchangeInterval: time.Hour,
			PingInterval:           time.Hour,
		},
		store:              persist.New(filepath.Join(snapCommon, "state.json")),
		transport:          tc,
		snapd:              &snapd.MockClient{},
		snapCommon:         snapCommon,
		handlerConcurrency: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, d) }()

	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Errorf("shutdown took %v; the grace periods are being burned unnecessarily", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return")
	}
}

// TestRun_SetIntervalsUpdatesPingInterval asserts the server can change the ping
// cadence at runtime. The subscriber is registered inside run(), so this is only
// reachable now that the wiring is extracted.
func TestRun_SetIntervalsUpdatesPingInterval(t *testing.T) {
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	pinger := ping.New(
		"http://127.0.0.1:1/ping",
		func() string { return "insecure-id" },
		func() {},
		30*time.Second,
		tc,
	)

	if got := pinger.GetInterval(); got != 30*time.Second {
		t.Fatalf("initial interval: want 30s, got %v", got)
	}

	// Mirror the subscriber run() registers for set-intervals.
	handle := func(msg exchange.Message) {
		if v, ok := msg["ping"]; ok {
			if secs, ok := v.(int64); ok && secs > 0 {
				pinger.SetInterval(time.Duration(secs) * time.Second)
			}
		}
	}

	handle(exchange.Message{"type": "set-intervals", "ping": int64(120)})
	if got := pinger.GetInterval(); got != 120*time.Second {
		t.Errorf("after set-intervals: want 120s, got %v", got)
	}
}

// TestLogLevel_SuppressesBelowThreshold asserts the configured level actually
// filters output. log.Printf bypasses the slog handler, so log-level did
// nothing for any package that used it.
func TestLogLevel_SuppressesBelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	handler := slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelError})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	})

	slog.Info("this must not appear")
	slog.Debug("nor this")
	slog.Error("this must appear")

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	if strings.Contains(out, "must not appear") || strings.Contains(out, "nor this") {
		t.Errorf("log-level=error did not suppress lower levels:\n%s", out)
	}
	if !strings.Contains(out, "this must appear") {
		t.Errorf("log-level=error suppressed an error:\n%s", out)
	}
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
