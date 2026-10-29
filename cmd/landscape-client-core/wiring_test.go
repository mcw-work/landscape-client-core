package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// TestRun_ServerCommandsReachWiredHandlers drives run() through a fake server
// to check the wiring: set-intervals reaches the pinger, and a completed snap
// install pushes an immediate snaps report alongside its operation result.
func TestRun_ServerCommandsReachWiredHandlers(t *testing.T) {
	var mu sync.Mutex
	var pings int
	var sent []map[string]any
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/ping" {
			pings++
			data, _ := bpickle.Marshal(map[string]any{"messages": false})
			_, _ = w.Write(data)
			return
		}
		body, _ := io.ReadAll(r.Body)
		raw, _ := bpickle.Unmarshal(body)
		payload, _ := raw.(map[string]any)
		msgs, _ := payload["messages"].([]any)
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				sent = append(sent, mm)
			}
		}
		resp := map[string]any{"messages": []any{}}
		if first {
			first = false
			resp["messages"] = []any{
				map[string]any{"type": "set-intervals", "ping": int64(3600)},
				map[string]any{
					"type":         "install-snaps",
					"operation-id": int64(5),
					"snaps":        []any{map[string]any{"name": "hello"}},
				},
			}
		}
		data, _ := bpickle.Marshal(resp)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	snapCommon := t.TempDir()
	store := persist.New(filepath.Join(snapCommon, "state.json"))
	if err := store.Save(&persist.State{SecureID: "sec", InsecureID: "ins"}); err != nil {
		t.Fatal(err)
	}
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.URL = srv.URL + "/message-system"
	cfg.ExchangeInterval = 100 * time.Millisecond
	// SetInterval applies from the next timer arm, so start fast and let the
	// server slow the pinger down.
	cfg.PingInterval = 300 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, deps{
			cfg:                cfg,
			store:              store,
			transport:          tc,
			snapd:              &snapd.MockClient{ChangeID: "1", Snaps: []snapd.SnapInfo{{Name: "hello"}}},
			snapCommon:         snapCommon,
			handlerConcurrency: 2,
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		var result map[string]any
		snapsReports := 0
		for _, m := range sent {
			switch m["type"] {
			case "operation-result":
				result = m
			case "snaps":
				snapsReports++
			}
		}
		mu.Unlock()

		// One snaps report comes from plugin start-up; the second from OnComplete.
		if result != nil && snapsReports >= 2 {
			if result["operation-id"] != int64(5) || result["status"] != int64(6) {
				t.Errorf("operation-result = %v, want op 5 succeeded", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: result=%v snaps reports=%d", result, snapsReports)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Unthrottled, a 300ms pinger would ping ~5 times in 1.5s.
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if pings > 2 {
		t.Errorf("pinged %d times after set-intervals ping=3600", pings)
	}
}
