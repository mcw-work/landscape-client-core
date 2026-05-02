package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type mockCommandSource struct {
	mu       sync.Mutex
	handlers map[string]func(context.Context, exchange.Message)
}

func newMockCommandSource() *mockCommandSource {
	return &mockCommandSource{handlers: make(map[string]func(context.Context, exchange.Message))}
}

func (m *mockCommandSource) Subscribe(msgType string, h func(context.Context, exchange.Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[msgType] = h
}

func (m *mockCommandSource) Dispatch(ctx context.Context, msgType string, msg exchange.Message) {
	m.mu.Lock()
	h := m.handlers[msgType]
	m.mu.Unlock()
	if h != nil {
		h(ctx, msg)
	}
}

func (m *mockCommandSource) subscribedTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	types := make([]string, 0, len(m.handlers))
	for t := range m.handlers {
		types = append(types, t)
	}
	return types
}

// mockResultSink records SendResult calls.
type mockResultSink struct {
	mu      sync.Mutex
	results []resultCall
}

type resultCall struct {
	opID       int64
	status     int
	resultCode int64 // 0 means not set
	output     string
}

func (m *mockResultSink) SendResult(_ context.Context, operationID int64, status int, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, resultCall{opID: operationID, status: status, output: output})
	return nil
}

func (m *mockResultSink) SendResultCode(_ context.Context, operationID int64, status int, resultCode int64, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, resultCall{opID: operationID, status: status, resultCode: resultCode, output: output})
	return nil
}

func (m *mockResultSink) first() (resultCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.results) == 0 {
		return resultCall{}, false
	}
	return m.results[0], true
}

// fakeHandler is a configurable Handler stub.
type fakeHandler struct {
	msgType string
	called  chan exchange.Message
	err     error
	panic_  bool
}

func newFakeHandler(msgType string) *fakeHandler {
	return &fakeHandler{msgType: msgType, called: make(chan exchange.Message, 1)}
}

func (f *fakeHandler) MessageType() string { return f.msgType }

func (f *fakeHandler) Handle(_ context.Context, msg exchange.Message, _ exchange.ResultSink) error {
	if f.panic_ {
		panic("test panic")
	}
	f.called <- msg
	return f.err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitChan waits for a value on ch with a deadline; returns zero value + false on timeout.
func waitChan[T any](ch <-chan T, d time.Duration) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-time.After(d):
		var zero T
		return zero, false
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRunner_AllHandlersRegistered verifies that Register subscribes every
// handler's message type exactly once.
func TestRunner_AllHandlersRegistered(t *testing.T) {
	source := newMockCommandSource()
	sink := &mockResultSink{}

	h1 := newFakeHandler("install-snaps")
	h2 := newFakeHandler("remove-snaps")
	h3 := newFakeHandler("reboot")

	runner := NewRunner([]Handler{h1, h2, h3}, source, sink)
	runner.Register()

	subscribed := source.subscribedTypes()
	if len(subscribed) != 4 {
		t.Fatalf("expected 4 subscriptions, got %d: %v", len(subscribed), subscribed)
	}

	want := map[string]bool{"install-snaps": true, "remove-snaps": true, "reboot": true, "cancel-operation": true}
	for _, typ := range subscribed {
		if !want[typ] {
			t.Errorf("unexpected subscription: %q", typ)
		}
	}
}

// TestRunner_InboundMessageDispatched verifies that dispatching a message
// causes the correct handler to be invoked with that message.
func TestRunner_InboundMessageDispatched(t *testing.T) {
	source := newMockCommandSource()
	sink := &mockResultSink{}

	h := newFakeHandler("install-snaps")
	runner := NewRunner([]Handler{h}, source, sink)
	runner.Register()

	msg := exchange.Message{"operation-id": int64(42), "name": "my-snap"}
	source.Dispatch(context.Background(), "install-snaps", msg)

	got, ok := waitChan(h.called, 2*time.Second)
	if !ok {
		t.Fatal("handler was not called within timeout")
	}
	if got["name"] != "my-snap" {
		t.Errorf("unexpected message: %v", got)
	}
}

// TestRunner_PanicSendsFailed verifies that a panicking handler causes a
// StatusFailed result to be sent with the panic details in the output.
func TestRunner_PanicSendsFailed(t *testing.T) {
	source := newMockCommandSource()
	sink := &mockResultSink{}

	h := newFakeHandler("reboot")
	h.panic_ = true

	runner := NewRunner([]Handler{h}, source, sink)
	runner.Register()

	msg := exchange.Message{"operation-id": int64(99)}
	source.Dispatch(context.Background(), "reboot", msg)

	// Poll for the result to arrive (goroutine may not have run yet).
	var res resultCall
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := sink.first(); ok {
			res = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res.status == 0 {
		t.Fatal("no result received after panic")
	}

	if res.status != exchange.StatusFailed {
		t.Errorf("expected StatusFailed (%d), got %d", exchange.StatusFailed, res.status)
	}
	if res.opID != 99 {
		t.Errorf("expected opID 99, got %d", res.opID)
	}
	if !strings.Contains(res.output, "panic") {
		t.Errorf("expected output to contain 'panic', got %q", res.output)
	}
}

// TestRunner_HandlerErrorLogged verifies that a handler returning an error
// causes the error to be logged and does not crash the runner.
func TestRunner_HandlerErrorLogged(t *testing.T) {
	// Redirect log output so we can inspect it.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	source := newMockCommandSource()
	sink := &mockResultSink{}

	h := newFakeHandler("remove-snaps")
	h.err = errors.New("something went wrong")

	runner := NewRunner([]Handler{h}, source, sink)
	runner.Register()

	msg := exchange.Message{"operation-id": int64(7)}
	source.Dispatch(context.Background(), "remove-snaps", msg)

	// Wait for the handler goroutine to complete.
	_, ok := waitChan(h.called, 2*time.Second)
	if !ok {
		t.Fatal("handler was not called within timeout")
	}

	// Give the goroutine a moment to log after returning from Handle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "something went wrong") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "something went wrong") {
		t.Errorf("expected error logged, got: %q", logOutput)
	}
	// Runner should still be functioning (no crash), verify via fmt to avoid unused import.
	_ = fmt.Sprintf("runner ok")
}
