package integration_test

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/monitor"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// -----------------------------------------------------------------------
// fakeServer: records received payloads and returns scripted responses.
// -----------------------------------------------------------------------

type fakeServer struct {
	server    *httptest.Server
	mu        sync.Mutex
	received  []map[string]any
	responses []map[string]any
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.server = httptest.NewServer(http.HandlerFunc(fs.handle))
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	raw, _ := bpickle.Unmarshal(body)
	payload, _ := raw.(map[string]any)

	fs.mu.Lock()
	fs.received = append(fs.received, payload)
	var resp map[string]any
	if len(fs.responses) > 0 {
		resp = fs.responses[0]
		fs.responses = fs.responses[1:]
	} else {
		resp = emptyResponse()
	}
	fs.mu.Unlock()

	data, _ := bpickle.Marshal(resp)
	_, _ = w.Write(data)
}

func emptyResponse() map[string]any {
	return map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(0),
	}
}

func (fs *fakeServer) URL() string { return fs.server.URL }

func (fs *fakeServer) queueResponse(resp map[string]any) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.responses = append(fs.responses, resp)
}

func (fs *fakeServer) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.received)
}

func (fs *fakeServer) get(i int) map[string]any {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.received[i]
}

// waitForCount blocks until at least n exchanges have been received.
func (fs *fakeServer) waitForCount(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fs.count() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d exchanges; have %d", n, fs.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForMessageType blocks until a message with the given type appears in any
// received exchange. Returns the first matching message.
func (fs *fakeServer) waitForMessageType(t *testing.T, msgType string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		fs.mu.Lock()
		for _, payload := range fs.received {
			for _, m := range payloadMessages(payload) {
				if m["type"] == msgType {
					fs.mu.Unlock()
					return m
				}
			}
		}
		fs.mu.Unlock()

		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for message type %q", msgType)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// payloadMessages extracts the decoded messages list from a payload dict.
func payloadMessages(payload map[string]any) []map[string]any {
	v, ok := payload["messages"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var msgs []map[string]any
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// toInt64 converts numeric bpickle values to int64.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// hashTypes returns the MD5 hex digest of comma-joined sorted type names,
// mirroring the exchange package's internal implementation.
func hashTypes(types []string) string {
	sorted := make([]string, len(types))
	copy(sorted, types)
	sort.Strings(sorted)
	joined := strings.Join(sorted, ",")
	h := md5.Sum([]byte(joined))
	return fmt.Sprintf("%x", h)
}

// -----------------------------------------------------------------------
// Test wiring helpers
// -----------------------------------------------------------------------

// newTestExchange creates an Exchange backed by a temp-dir store, optionally
// pre-seeded with initialState, pointing at serverURL.
func newTestExchange(t *testing.T, serverURL string, initialState *persist.State) (*exchange.Exchange, *persist.Store) {
	t.Helper()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))
	if initialState != nil {
		if err := store.Save(initialState); err != nil {
			t.Fatalf("store.Save: %v", err)
		}
	}

	cfg := config.Defaults()
	cfg.URL = serverURL
	cfg.AccountName = "test-account"
	cfg.RegistrationKey = "test-key"
	cfg.ComputerTitle = "test-computer"
	cfg.ExchangeInterval = 100 * time.Millisecond
	cfg.UrgentExchangeInterval = 50 * time.Millisecond

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	return exchange.New(cfg, store, tc), store
}

// startExchange starts ex.Run in a goroutine and registers a t.Cleanup that
// cancels the context and waits for Run to return.
func startExchange(t *testing.T, ex *exchange.Exchange) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ex.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

// -----------------------------------------------------------------------
// Test 1: TestRegistrationFlow
// -----------------------------------------------------------------------

func TestRegistrationFlow(t *testing.T) {
	fs := newFakeServer(t)

	// Script a set-id response for the first exchange.
	fs.queueResponse(map[string]any{
		"messages": []any{
			map[string]any{
				"type":        "set-id",
				"secure-id":   "test-secure-id-123",
				"insecure-id": "test-insecure-id-456",
			},
		},
		"next-expected-sequence": int64(1),
	})

	ex, store := newTestExchange(t, fs.URL(), nil)
	startExchange(t, ex)

	// Wait for the first exchange to arrive at the fake server.
	fs.waitForCount(t, 1, 3*time.Second)

	// The first message in the payload must be type "register".
	payload := fs.get(0)
	msgs := payloadMessages(payload)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message in first exchange payload")
	}
	if got := msgs[0]["type"]; got != "register" {
		t.Errorf("first message type: got %v, want register", got)
	}

	// Poll until the state has been persisted with the SecureID from the set-id response.
	deadline := time.Now().Add(3 * time.Second)
	for {
		state, err := store.Load()
		if err != nil {
			t.Fatalf("store.Load: %v", err)
		}
		if state.SecureID != "" {
			if state.SecureID != "test-secure-id-123" {
				t.Errorf("SecureID: got %q, want test-secure-id-123", state.SecureID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout: SecureID never persisted after set-id response")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// -----------------------------------------------------------------------
// Test 2: TestSequenceTracking
// -----------------------------------------------------------------------

func TestSequenceTracking(t *testing.T) {
	fs := newFakeServer(t)

	initial := &persist.State{
		SecureID:   "sec-123",
		InsecureID: "ins-456",
		// OutboundSequence: 0 (zero value)
	}

	// First response: server acknowledges it expects sequence 1 next.
	fs.queueResponse(map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(1),
	})

	ex, _ := newTestExchange(t, fs.URL(), initial)

	// Queue one message so the first exchange advances OutboundSequence by 1.
	_ = ex.Send(context.Background(), exchange.Message{"type": "test-msg", "data": "hello"})

	startExchange(t, ex)

	// Wait for two exchanges.
	fs.waitForCount(t, 2, 5*time.Second)

	// First exchange: sequence must be 0 (the initial OutboundSequence).
	seq0 := toInt64(fs.get(0)["sequence"])
	if seq0 != 0 {
		t.Errorf("first exchange sequence: got %d, want 0", seq0)
	}

	// Second exchange: OutboundSequence advanced by 1 after the first exchange,
	// so sequence must be 1.
	seq1 := toInt64(fs.get(1)["sequence"])
	if seq1 != 1 {
		t.Errorf("second exchange sequence: got %d, want 1", seq1)
	}
}

// -----------------------------------------------------------------------
// Test 3: TestResynchronize
// -----------------------------------------------------------------------

func TestResynchronize(t *testing.T) {
	fs := newFakeServer(t)

	initial := &persist.State{
		SecureID:         "sec-123",
		InsecureID:       "ins-456",
		OutboundSequence: 5,
	}

	// First response: trigger a resynchronize.
	fs.queueResponse(map[string]any{
		"messages": []any{
			map[string]any{"type": "resynchronize"},
		},
		"next-expected-sequence": int64(5),
	})

	ex, _ := newTestExchange(t, fs.URL(), initial)
	startExchange(t, ex)

	// Wait for two exchanges.
	fs.waitForCount(t, 2, 5*time.Second)

	// First exchange: sequence must be 5 (the pre-seeded OutboundSequence).
	seq0 := toInt64(fs.get(0)["sequence"])
	if seq0 != 5 {
		t.Errorf("first exchange sequence: got %d, want 5", seq0)
	}

	// Second exchange: OutboundSequence was reset to 0 by resynchronize.
	seq1 := toInt64(fs.get(1)["sequence"])
	if seq1 != 0 {
		t.Errorf("second exchange sequence after resync: got %d, want 0", seq1)
	}
}

// -----------------------------------------------------------------------
// Test 4: TestAcceptedTypes
// -----------------------------------------------------------------------

func TestAcceptedTypes(t *testing.T) {
	fs := newFakeServer(t)

	initial := &persist.State{
		SecureID:   "sec-123",
		InsecureID: "ins-456",
		// AcceptedTypesHash intentionally empty — triggers client-accepted-types on first exchange.
	}

	serverTypes := []string{"cpu-usage", "memory-info", "load-average"}
	expectedHash := hashTypes(serverTypes)

	// Build the types list as []any for bpickle encoding.
	serverTypesAny := make([]any, len(serverTypes))
	for i, tp := range serverTypes {
		serverTypesAny[i] = tp
	}

	// First response: accepted-types message from the server.
	fs.queueResponse(map[string]any{
		"messages": []any{
			map[string]any{
				"type":  "accepted-types",
				"types": serverTypesAny,
			},
		},
		"next-expected-sequence": int64(0),
	})

	ex, _ := newTestExchange(t, fs.URL(), initial)
	startExchange(t, ex)

	// Wait for two exchanges.
	fs.waitForCount(t, 2, 5*time.Second)

	// Second exchange: accepted-types field must contain the expected MD5 hash.
	p2 := fs.get(1)
	hash, _ := p2["accepted-types"].(string)
	if hash != expectedHash {
		t.Errorf("second exchange accepted-types hash: got %q, want %q", hash, expectedHash)
	}

	// Second exchange: client-accepted-types must NOT be present once the hash is confirmed.
	if _, ok := p2["client-accepted-types"]; ok {
		t.Error("second exchange should not contain client-accepted-types after hash is confirmed")
	}
}

// -----------------------------------------------------------------------
// Test 5: TestCPUUsageRoundTrip
// -----------------------------------------------------------------------

// fastCPUPlugin implements monitor.Plugin. It immediately sends one cpu-usage
// message, then blocks until the context is cancelled.
type fastCPUPlugin struct{}

func (p *fastCPUPlugin) Name() string { return "cpu-usage" }

func (p *fastCPUPlugin) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	msg := exchange.Message{
		"type":       "cpu-usage",
		"cpu-usages": []any{[]any{time.Now().Unix(), float64(0.42)}},
	}
	_ = sink.Send(ctx, msg)
	<-ctx.Done()
	return nil
}

func TestCPUUsageRoundTrip(t *testing.T) {
	fs := newFakeServer(t)

	initial := &persist.State{
		SecureID:   "sec-123",
		InsecureID: "ins-456",
	}

	ex, store := newTestExchange(t, fs.URL(), initial)

	// Start the monitor runner with the fast CPU plugin.
	runner := monitor.New([]monitor.Plugin{&fastCPUPlugin{}}, ex, store)
	runnerDone := make(chan struct{})
	runnerCtx, runnerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	go func() {
		defer close(runnerDone)
		_ = runner.Run(runnerCtx)
	}()
	t.Cleanup(func() {
		runnerCancel()
		<-runnerDone
	})

	startExchange(t, ex)

	// Wait for a cpu-usage message to appear in the fake server.
	msg := fs.waitForMessageType(t, "cpu-usage", 5*time.Second)

	// Assert the message contains a non-empty cpu-usages list.
	cpuUsages, ok := msg["cpu-usages"]
	if !ok {
		t.Fatal("cpu-usage message missing cpu-usages field")
	}
	list, ok := cpuUsages.([]any)
	if !ok || len(list) == 0 {
		t.Errorf("cpu-usages: got %v (%T), want non-empty list", cpuUsages, cpuUsages)
	}
}
