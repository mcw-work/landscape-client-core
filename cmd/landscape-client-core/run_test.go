package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
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
