package exchange

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// receivedRequest holds a decoded payload and the request headers from one exchange.
type receivedRequest struct {
	payload map[string]any
	headers http.Header
}

// fakeServer records received payloads and returns scripted responses.
type fakeServer struct {
	mu          sync.Mutex
	received    []receivedRequest
	responses   []map[string]any
	statusCodes []int // optional HTTP status overrides; popped per request
}

func (f *fakeServer) push(resp map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, resp)
}

// pushError scripts the next request to return the given HTTP error status
// with no response body.
func (f *fakeServer) pushError(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCodes = append(f.statusCodes, code)
}

func (f *fakeServer) get(i int) receivedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received[i]
}

func (f *fakeServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	raw, _ := bpickle.Unmarshal(body)
	payload, _ := raw.(map[string]any)

	f.mu.Lock()
	f.received = append(f.received, receivedRequest{
		payload: payload,
		headers: r.Header.Clone(),
	})
	statusCode := http.StatusOK
	if len(f.statusCodes) > 0 {
		statusCode = f.statusCodes[0]
		f.statusCodes = f.statusCodes[1:]
	}
	var resp map[string]any
	if len(f.responses) > 0 {
		resp = f.responses[0]
		f.responses = f.responses[1:]
	} else {
		resp = defaultResponse()
	}
	f.mu.Unlock()

	if statusCode != http.StatusOK {
		w.WriteHeader(statusCode)
		return
	}
	data, _ := bpickle.Marshal(resp)
	w.Write(data) //nolint:errcheck
}

func defaultResponse() map[string]any {
	return map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(0),
	}
}

// testSetup holds everything needed for a test.
type testSetup struct {
	ex    *Exchange
	fs    *fakeServer
	store *persist.Store
	cfg   *config.Config
}

func newTestSetup(t *testing.T) *testSetup {
	t.Helper()

	fs := &fakeServer{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	store := persist.New(t.TempDir() + "/state.json")

	cfg := &config.Config{
		AccountName:            "test-account",
		RegistrationKey:        "test-key",
		ComputerTitle:          "Test Computer",
		URL:                    srv.URL,
		ExchangeInterval:       15 * time.Minute,
		UrgentExchangeInterval: 1 * time.Minute,
	}

	ex := New(cfg, store, tc)
	return &testSetup{ex: ex, fs: fs, store: store, cfg: cfg}
}

// freshState returns a new zero-value state (as if loaded for the first time).
func (ts *testSetup) freshState(t *testing.T) *persist.State {
	t.Helper()
	state, err := ts.store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	return state
}

// -----------------------------------------------------------------------
// Test 1: Registration
// -----------------------------------------------------------------------

func TestRegistration(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{
				"type":        "set-id",
				"secure-id":   "sec123",
				"insecure-id": "ins456",
			},
		},
		"next-expected-sequence": int64(1),
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	if ts.fs.count() != 1 {
		t.Fatalf("expected 1 request, got %d", ts.fs.count())
	}

	// Verify the register message was the first payload message.
	payload := ts.fs.get(0).payload
	msgs, _ := payload["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message in payload")
	}
	first, _ := msgs[0].(map[string]any)
	if first["type"] != "register" {
		t.Errorf("first message type: got %v, want register", first["type"])
	}
	if first["account-name"] != "test-account" {
		t.Errorf("account-name: got %v, want test-account", first["account-name"])
	}
	if first["registration-key"] != "test-key" {
		t.Errorf("registration-key: got %v, want test-key", first["registration-key"])
	}

	// Verify state was persisted with the new SecureID / InsecureID.
	loaded, err := ts.store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if loaded.SecureID != "sec123" {
		t.Errorf("SecureID: got %q, want sec123", loaded.SecureID)
	}
	if loaded.InsecureID != "ins456" {
		t.Errorf("InsecureID: got %q, want ins456", loaded.InsecureID)
	}

	// Second exchange: must NOT include a register message now that set-id was received.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	p2 := ts.fs.get(1).payload
	msgs2, _ := p2["messages"].([]any)
	for _, m := range msgs2 {
		if msg, ok := m.(map[string]any); ok {
			if msg["type"] == "register" {
				t.Error("second exchange should not include a register message after set-id")
			}
		}
	}
}

// -----------------------------------------------------------------------
// Test 2: Normal exchange — queued messages are transmitted, sequence advances
// -----------------------------------------------------------------------

func TestNormalExchange(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	state.InsecureID = "ins456"

	_ = ts.ex.Send(context.Background(), Message{"type": "test-msg", "data": "hello"})
	_ = ts.ex.Send(context.Background(), Message{"type": "test-msg", "data": "world"})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	payload := ts.fs.get(0).payload
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("messages count: got %d, want 2", len(msgs))
	}

	if seq := toInt64(payload["sequence"]); seq != 0 {
		t.Errorf("sequence: got %d, want 0", seq)
	}

	if state.OutboundSequence != 2 {
		t.Errorf("OutboundSequence: got %d, want 2", state.OutboundSequence)
	}
}

// -----------------------------------------------------------------------
// Test 3: Sequence tracking
// -----------------------------------------------------------------------

func TestSequenceTracking(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	// Server says it expects sequence 5 next from us.
	ts.fs.push(map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(5),
	})

	_ = ts.ex.Send(context.Background(), Message{"type": "test-msg"})
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 1: %v", err)
	}

	// OutboundSequence advances by the snapshot size (1 message sent).
	if state.OutboundSequence != 1 {
		t.Errorf("OutboundSequence: got %d, want 1", state.OutboundSequence)
	}
	// NextExpectedFromServer is updated from the response.
	if state.NextExpectedFromServer != 5 {
		t.Errorf("NextExpectedFromServer: got %d, want 5", state.NextExpectedFromServer)
	}

	// Second exchange should carry the updated sequence and next-expected values.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	p2 := ts.fs.get(1).payload
	if seq := toInt64(p2["sequence"]); seq != 1 {
		t.Errorf("second exchange sequence: got %d, want 1", seq)
	}
	if nes := toInt64(p2["next-expected-sequence"]); nes != 5 {
		t.Errorf("second exchange next-expected-sequence: got %d, want 5", nes)
	}
}

// -----------------------------------------------------------------------
// Test 4: accepted-types
// -----------------------------------------------------------------------

func TestAcceptedTypes(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	ts.ex.Subscribe("do-something", func(ctx context.Context, msg Message) {})

	// First exchange: server sends an accepted-types message.
	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{
				"type":  "accepted-types",
				"types": []any{"do-something", "test-msg"},
			},
		},
		"next-expected-sequence": int64(0),
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 1: %v", err)
	}

	// State should have the accepted types and their hash.
	if len(state.AcceptedTypes) != 2 {
		t.Errorf("AcceptedTypes length: got %d, want 2", len(state.AcceptedTypes))
	}
	expectedHash := hashTypes([]string{"do-something", "test-msg"})
	if state.AcceptedTypesHash != expectedHash {
		t.Errorf("AcceptedTypesHash: got %q, want %q", state.AcceptedTypesHash, expectedHash)
	}

	// Second exchange: should send the hash but NOT include client-accepted-types.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	p2 := ts.fs.get(1).payload
	if _, ok := p2["client-accepted-types"]; ok {
		t.Error("second exchange should not include client-accepted-types")
	}
	if hash, _ := p2["accepted-types"].(string); hash != expectedHash {
		t.Errorf("accepted-types hash: got %q, want %q", hash, expectedHash)
	}
}

// -----------------------------------------------------------------------
// Test 5: resynchronize
// -----------------------------------------------------------------------

func TestResynchronize(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	state.OutboundSequence = 10
	state.NextExpectedFromServer = 7

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "resynchronize"},
		},
		"next-expected-sequence": int64(10),
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	if state.OutboundSequence != 0 {
		t.Errorf("OutboundSequence after resync: got %d, want 0", state.OutboundSequence)
	}
	if state.NextExpectedFromServer != 0 {
		t.Errorf("NextExpectedFromServer after resync: got %d, want 0", state.NextExpectedFromServer)
	}
}

// -----------------------------------------------------------------------
// Test 6: Handler dispatch
// -----------------------------------------------------------------------

func TestHandlerDispatch(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	received := make(chan Message, 1)
	ts.ex.Subscribe("do-something", func(ctx context.Context, msg Message) {
		received <- msg
	})

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "do-something", "param": "value"},
		},
		"next-expected-sequence": int64(0),
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	select {
	case msg := <-received:
		if msg["type"] != "do-something" {
			t.Errorf("message type: got %v, want do-something", msg["type"])
		}
		if msg["param"] != "value" {
			t.Errorf("message param: got %v, want value", msg["param"])
		}
	case <-time.After(time.Second):
		t.Fatal("handler was not called within 1 second")
	}
}

// -----------------------------------------------------------------------
// Test 7: Unrecognised message type — logged and discarded, no error
// -----------------------------------------------------------------------

func TestUnrecognisedMessageType(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "unknown-type-xyz", "data": "x"},
		},
		"next-expected-sequence": int64(0),
	})

	// Should not return an error; exchange continues normally.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// -----------------------------------------------------------------------
// Test 8: Exchange token
// -----------------------------------------------------------------------

func TestExchangeToken(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	state.InsecureID = "ins456"

	// Server provides a token in the first response.
	ts.fs.push(map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(0),
		"next-exchange-token":    "token123",
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 1: %v", err)
	}
	if state.ExchangeToken != "token123" {
		t.Errorf("ExchangeToken: got %q, want token123", state.ExchangeToken)
	}

	// Second exchange must carry the token in the X-Exchange-Token header.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	hdr := ts.fs.get(1).headers.Get("X-Exchange-Token")
	if hdr != "token123" {
		t.Errorf("X-Exchange-Token header: got %q, want token123", hdr)
	}
}

// -----------------------------------------------------------------------
// Test 9: Context cancellation — Run returns promptly
// -----------------------------------------------------------------------

func TestContextCancellation(t *testing.T) {
	ts := newTestSetup(t)
	ts.cfg.ExchangeInterval = 500 * time.Millisecond
	ts.cfg.UrgentExchangeInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- ts.ex.Run(ctx)
	}()

	// Let at least one exchange happen.
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("Run took too long to stop: %v (want <100ms)", elapsed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return within 200ms after context cancellation")
	}
}

// -----------------------------------------------------------------------
// Test 10: Urgent interval — non-empty pending after exchange → shorter tick
// -----------------------------------------------------------------------

func TestUrgentInterval(t *testing.T) {
	ts := newTestSetup(t)
	ts.cfg.ExchangeInterval = 500 * time.Millisecond      // long normal interval
	ts.cfg.UrgentExchangeInterval = 10 * time.Millisecond // very short urgent

	// Register a handler that re-queues a message, leaving pending non-empty
	// after performExchange returns (WaitGroup ensures this happens in time).
	ts.ex.Subscribe("trigger-urgent", func(ctx context.Context, msg Message) {
		_ = ts.ex.Send(ctx, Message{"type": "response-msg"})
	})

	// First exchange response includes a trigger message.
	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "trigger-urgent"},
		},
		"next-expected-sequence": int64(0),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ts.ex.Run(ctx) //nolint:errcheck

	// Wait for the second exchange to happen. With UrgentExchangeInterval=10ms
	// it should arrive well within 150ms. With ExchangeInterval=500ms it would
	// not arrive within that window.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ts.fs.count() >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	if ts.fs.count() < 2 {
		t.Errorf("expected ≥2 exchanges within 150ms (urgent interval not working?), got %d", ts.fs.count())
	}
}

// -----------------------------------------------------------------------
// Test 11: Transport failure — messages are re-queued and retried
// -----------------------------------------------------------------------

func TestTransportFailure(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	state.InsecureID = "ins456"

	// Queue one message.
	_ = ts.ex.Send(context.Background(), Message{"type": "test-msg", "data": "hello"})

	// First exchange: server returns 500 → transport.Post returns an HTTPError.
	ts.fs.pushError(http.StatusInternalServerError)
	err := ts.ex.performExchange(context.Background(), state)
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}

	// The message must have been re-queued.
	ts.ex.mu.Lock()
	pending := len(ts.ex.pending)
	ts.ex.mu.Unlock()
	if pending != 1 {
		t.Errorf("pending after transport failure: got %d, want 1", pending)
	}

	// Second exchange: server returns 200 → message is transmitted successfully.
	err = ts.ex.performExchange(context.Background(), state)
	if err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}

	if ts.fs.count() != 2 {
		t.Fatalf("expected 2 requests total, got %d", ts.fs.count())
	}
	payload := ts.fs.get(1).payload
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("second exchange messages: got %d, want 1", len(msgs))
	}
	if len(msgs) > 0 {
		first, _ := msgs[0].(map[string]any)
		if first["type"] != "test-msg" {
			t.Errorf("re-queued message type: got %v, want test-msg", first["type"])
		}
	}
}
