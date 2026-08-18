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
		"messages": []any{},
		// Omit next-expected-sequence — the client will default to ACK'ing all sent messages.
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
				"id":          "sec123",
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
	if first["account_name"] != "test-account" {
		t.Errorf("account-name: got %v, want test-account", first["account_name"])
	}
	if first["registration_password"] != "test-key" {
		t.Errorf("registration-key: got %v, want test-key", first["registration_password"])
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

	// Server ACKs with next-expected-sequence=5, meaning it wants client msg 5 next.
	// This advances our OutboundSequence to 5 (the server's ACK).
	ts.fs.push(map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(5),
	})

	_ = ts.ex.Send(context.Background(), Message{"type": "test-msg"})
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 1: %v", err)
	}

	// OutboundSequence must be set to what the server ACK'd (5), not self-incremented.
	if state.OutboundSequence != 5 {
		t.Errorf("OutboundSequence: got %d, want 5", state.OutboundSequence)
	}
	// NextExpectedFromServer advances only for server→client messages (none here).
	if state.NextExpectedFromServer != 0 {
		t.Errorf("NextExpectedFromServer: got %d, want 0", state.NextExpectedFromServer)
	}

	// Second exchange should carry the server-ACK'd sequence (5).
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	p2 := ts.fs.get(1).payload
	if seq := toInt64(p2["sequence"]); seq != 5 {
		t.Errorf("second exchange sequence: got %d, want 5", seq)
	}
	// next-expected-sequence in outgoing payload = our server→client tracking (0, no server msgs).
	if nes := toInt64(p2["next-expected-sequence"]); nes != 0 {
		t.Errorf("second exchange next-expected-sequence: got %d, want 0", nes)
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
	if string(state.AcceptedTypesHash) != string(expectedHash) {
		t.Errorf("AcceptedTypesHash: got %x, want %x", state.AcceptedTypesHash, expectedHash)
	}

	// Second exchange: should send the hash but NOT include client-accepted-types.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	p2 := ts.fs.get(1).payload
	if _, ok := p2["client-accepted-types"]; ok {
		t.Error("second exchange should not include client-accepted-types")
	}
	if hash, _ := p2["accepted-types"].([]byte); string(hash) != string(expectedHash) {
		t.Errorf("accepted-types hash: got %x, want %x", hash, expectedHash)
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
			map[string]any{"type": "resynchronize", "operation-id": int64(42)},
		},
		"next-expected-sequence": int64(10),
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	// OutboundSequence must NOT be reset — it stays at the server's ACK value.
	// Resetting it to 0 causes an infinite resynchronize loop because the server
	// ignores all messages with seq < its next_expected_sequence.
	if state.OutboundSequence != 10 {
		t.Errorf("OutboundSequence after resync: got %d, want 10 (must not reset)", state.OutboundSequence)
	}
	// NextExpectedFromServer advances by 1 (the resynchronize message received).
	if state.NextExpectedFromServer != 8 {
		t.Errorf("NextExpectedFromServer after resync: got %d, want 8", state.NextExpectedFromServer)
	}
	// A resynchronize ack must be queued so the server resets its next_expected_sequence.
	ts.ex.mu.Lock()
	pending := make([]Message, len(ts.ex.pending))
	copy(pending, ts.ex.pending)
	ts.ex.mu.Unlock()
	if len(pending) == 0 {
		t.Fatal("resynchronize ack not queued")
	}
	if pending[0]["type"] != "resynchronize" {
		t.Errorf("pending[0] type: got %v, want resynchronize", pending[0]["type"])
	}
	if pending[0]["operation-id"] != int64(42) {
		t.Errorf("pending[0] operation-id: got %v, want 42", pending[0]["operation-id"])
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
// Test 10: Urgent interval — urgent pending after exchange → shorter tick
// -----------------------------------------------------------------------

func TestUrgentInterval(t *testing.T) {
	ts := newTestSetup(t)
	ts.cfg.ExchangeInterval = 500 * time.Millisecond      // long normal interval
	ts.cfg.UrgentExchangeInterval = 10 * time.Millisecond // very short urgent

	// Pre-register so Run does not inject a register message (which would force
	// the urgent interval regardless of pending) — this isolates the
	// urgent-pending path.
	st, _ := ts.store.Load()
	st.SecureID = "sec123"
	if err := ts.store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Seed an urgent (operation-result) message. The server ACKs nothing
	// (next-expected-sequence stays 0), so performExchange re-queues it and it
	// remains pending after each exchange, keeping hasUrgentPendingLocked true.
	_ = ts.ex.Send(context.Background(), Message{"type": "operation-result", "operation-id": int64(1)})
	for i := 0; i < 5; i++ {
		ts.fs.push(map[string]any{
			"messages":               []any{},
			"next-expected-sequence": int64(0),
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ts.ex.Run(ctx) //nolint:errcheck

	// With an urgent message pending and UrgentExchangeInterval=10ms the second
	// exchange arrives well within 150ms. With ExchangeInterval=500ms (used when
	// only non-urgent messages are pending) it would not.
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

func TestExchange_SendResultCode(t *testing.T) {
	srv := &fakeServer{}
	srv.push(map[string]any{
		"next-expected-sequence": int64(1),
		"next-exchange-token":    "tok",
		"messages":               []any{},
	})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, _ := transport.New(transport.Config{})
	store := persist.New(t.TempDir() + "/state.json")
	cfg := &config.Config{URL: ts.URL, AccountName: "acc"}
	exc := New(cfg, store, tc)

	// Pre-register so exchange doesn't inject a register message.
	st, _ := store.Load()
	st.SecureID = "test-secure-id"
	_ = store.Save(st)

	ctx := context.Background()
	if err := exc.SendResultCode(ctx, int64(42), StatusFailed, int64(102), "timed out"); err != nil {
		t.Fatalf("SendResultCode: %v", err)
	}

	// Force an exchange so the message gets sent to the fake server.
	exc.TriggerExchange()
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = exc.Run(runCtx) }()

	// Wait for the server to receive the exchange.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.count() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.count() == 0 {
		t.Fatal("no exchange received")
	}

	req := srv.get(0)
	msgs, _ := req.payload["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in exchange payload")
	}
	msg, _ := msgs[0].(map[string]any)
	if got := msg["type"]; got != "operation-result" {
		t.Errorf("type = %v, want operation-result", got)
	}
	if got := msg["result-code"]; got != int64(102) {
		t.Errorf("result-code = %v, want 102", got)
	}
	if got := msg["result-text"]; got != "timed out" {
		t.Errorf("result-text = %v, want %q", got, "timed out")
	}

	cancel() // stop the exchange loop
	<-done   // wait for Run to return
}

func TestPerformExchange_HandlerPanicIsContained(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	panicked := make(chan struct{})
	ts.ex.Subscribe("boom", func(ctx context.Context, msg Message) {
		defer close(panicked)
		panic("boom")
	})

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "boom"},
		},
		"next-expected-sequence": int64(0),
	})

	// Handlers are dispatched fire-and-forget: a panicking handler is recovered
	// inside its own goroutine and must not fail the exchange.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange must not fail because a handler panicked: %v", err)
	}
	select {
	case <-panicked:
	case <-time.After(time.Second):
		t.Fatal("handler was never dispatched")
	}
}

func TestPerformExchange_DoesNotWaitForHandlers(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	done := make(chan struct{})
	ts.ex.Subscribe("fanout", func(ctx context.Context, msg Message) {
		time.Sleep(300 * time.Millisecond)
		close(done)
	})

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "fanout"},
		},
		"next-expected-sequence": int64(0),
	})

	// performExchange must return promptly without blocking on the handler;
	// tying handler lifetime to the exchange cycle was the P0 defect.
	start := time.Now()
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("performExchange waited for the handler (%v); dispatch must be fire-and-forget", elapsed)
	}
	// The handler still runs to completion in the background.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never completed")
	}
}

func TestPerformExchange_HandlerDispatchDoesNotFailExchange(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	ran := make(chan struct{})
	ts.ex.Subscribe("cancel-me", func(handlerCtx context.Context, msg Message) {
		close(ran)
	})

	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "cancel-me"},
		},
		"next-expected-sequence": int64(0),
	})

	// Handlers are fire-and-forget, so nothing a handler does — including its
	// context ending — propagates back as an exchange error.
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange must not fail due to handler dispatch: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("handler was never dispatched")
	}
}

// TestSend_DoesNotForceAnExchange asserts plugin telemetry queues for the next
// scheduled exchange instead of triggering one per message. Measured before this
// fix: 24 sends produced 24 exchanges with a 1-hour interval configured.
func TestSend_DoesNotForceAnExchange(t *testing.T) {
	srv := &fakeServer{}
	for i := 0; i < 10; i++ {
		srv.push(map[string]any{
			"next-expected-sequence": int64(0),
			"next-exchange-token":    "tok",
			"messages":               []any{},
		})
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(t.TempDir() + "/state.json")
	st, _ := store.Load()
	st.SecureID = "already-registered"
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := &config.Config{
		URL:                    ts.URL,
		AccountName:            "acc",
		ExchangeInterval:       time.Hour,
		UrgentExchangeInterval: time.Hour,
	}
	exc := New(cfg, store, tc)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- exc.Run(ctx) }()

	// Let the initial exchange happen.
	time.Sleep(200 * time.Millisecond)
	baseline := srv.count()

	for i := 0; i < 20; i++ {
		if err := exc.Send(ctx, Message{"type": "cpu-usage"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	extra := srv.count() - baseline
	cancel()
	<-runDone

	if extra > 0 {
		t.Errorf("20 non-urgent sends triggered %d extra exchanges; exchange-interval was 1h", extra)
	}
}

// TestSendUrgent_TriggersAnExchange asserts operation results are not delayed
// until the next scheduled exchange — the server is waiting on those.
func TestSendUrgent_TriggersAnExchange(t *testing.T) {
	srv := &fakeServer{}
	for i := 0; i < 5; i++ {
		srv.push(map[string]any{
			"next-expected-sequence": int64(0),
			"next-exchange-token":    "tok",
			"messages":               []any{},
		})
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(t.TempDir() + "/state.json")
	st, _ := store.Load()
	st.SecureID = "already-registered"
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := &config.Config{
		URL:                    ts.URL,
		AccountName:            "acc",
		ExchangeInterval:       time.Hour,
		UrgentExchangeInterval: time.Hour,
	}
	exc := New(cfg, store, tc)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- exc.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	baseline := srv.count()

	if err := exc.SendResult(ctx, 42, StatusSucceeded, "done"); err != nil {
		t.Fatalf("SendResult: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	extra := srv.count() - baseline
	cancel()
	<-runDone

	if extra == 0 {
		t.Error("an operation-result did not trigger an exchange")
	}
}

// TestBackoff_EscalatesOn5xx asserts repeated server errors increase the delay
// rather than retrying at a fixed interval, which lets a fleet hammer an
// already-struggling server.
func TestBackoff_EscalatesOn5xx(t *testing.T) {
	b := newBackoff()

	if d := b.current(); d != 0 {
		t.Errorf("initial backoff should be zero, got %v", d)
	}

	b.failure(500)
	first := b.current()
	if first < 300*time.Second || first > 360*time.Second {
		t.Errorf("first backoff should be ~300s with jitter, got %v", first)
	}

	b.failure(500)
	second := b.current()
	if second <= first {
		t.Errorf("backoff should escalate: first=%v second=%v", first, second)
	}

	for i := 0; i < 20; i++ {
		b.failure(503)
	}
	// current() adds up to 20% jitter on top of the capped base delay, so the
	// bound is backoffMax plus one jitter step. The point is that the delay stays
	// bounded near 7200s instead of growing unboundedly with every 5xx.
	if capped := b.current(); capped > backoffMax+backoffMax/5 {
		t.Errorf("backoff should cap near 7200s, got %v", capped)
	}
}

func TestBackoff_DecaysOnSuccess(t *testing.T) {
	b := newBackoff()
	b.failure(500)
	b.failure(500)
	if b.current() == 0 {
		t.Fatal("expected a non-zero backoff after failures")
	}

	b.success()
	if d := b.current(); d != 0 {
		t.Errorf("backoff should reset on success, got %v", d)
	}
}

func TestBackoff_IgnoresClientErrors(t *testing.T) {
	b := newBackoff()

	// A 400 is our bug, not server overload; backing off does not help.
	b.failure(400)
	if d := b.current(); d != 0 {
		t.Errorf("4xx other than 429 should not back off, got %v", d)
	}

	b.failure(429)
	if d := b.current(); d == 0 {
		t.Error("429 should back off")
	}
}

func TestBackoff_JitterVaries(t *testing.T) {
	seen := make(map[time.Duration]bool)
	for i := 0; i < 20; i++ {
		b := newBackoff()
		b.failure(500)
		seen[b.current()] = true
	}
	if len(seen) < 2 {
		t.Error("backoff has no jitter; a fleet would retry in lockstep")
	}
}
