package exchange_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/manager"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// dispatchFakeServer is a minimal Landscape server that returns scripted
// bpickle responses. It lives in the external test package because the real
// exchange_test.go fakeServer is unexported and this test must import
// internal/manager (which imports internal/exchange), so it cannot be part of
// package exchange without creating an import cycle.
type dispatchFakeServer struct {
	mu        sync.Mutex
	responses []map[string]any
}

func (f *dispatchFakeServer) push(resp map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, resp)
}

func (f *dispatchFakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)

	f.mu.Lock()
	var resp map[string]any
	if len(f.responses) > 0 {
		resp = f.responses[0]
		f.responses = f.responses[1:]
	} else {
		resp = map[string]any{"messages": []any{}}
	}
	f.mu.Unlock()

	data, _ := bpickle.Marshal(resp)
	w.Write(data) //nolint:errcheck
}

// TestDispatch_LongRunningHandlerSurvivesExchangeCycle drives a real
// manager.Runner through the real Subscribe/dispatch path — not by calling
// Handle directly, which is what let this bug ship. A script that takes longer
// than one exchange cycle must still complete.
func TestDispatch_LongRunningHandlerSurvivesExchangeCycle(t *testing.T) {
	snapCommon := t.TempDir()
	marker := filepath.Join(snapCommon, "marker")

	srv := &dispatchFakeServer{}
	srv.push(map[string]any{
		"next-expected-sequence": int64(0),
		"next-exchange-token":    "tok",
		"messages": []any{
			map[string]any{
				"type":         "execute-script",
				"operation-id": int64(42),
				"code":         "sleep 2; touch " + marker + "\n",
				"interpreter":  "/bin/sh",
			},
		},
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(filepath.Join(snapCommon, "state.json"))
	st, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	st.SecureID = "test-secure-id"
	if err := store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	cfg := &config.Config{
		URL:                    ts.URL,
		AccountName:            "acc",
		ExchangeInterval:       time.Hour,
		UrgentExchangeInterval: time.Hour,
	}
	exc := exchange.New(cfg, store, tc)

	handlers := []manager.Handler{manager.NewScriptExecHandler(snapCommon, nil)}
	mgRunner := manager.NewRunner(handlers, exc, exc, 10)
	mgRunner.Register()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- exc.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			cancel()
			<-runDone
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-runDone
	t.Fatal("script did not complete: marker file was never created, meaning the handler context was cancelled mid-execution")
}
