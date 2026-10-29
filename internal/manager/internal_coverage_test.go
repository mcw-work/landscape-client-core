package manager

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// errWriter accepts up to limit bytes, then fails every write.
type errWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *errWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		return 0, errors.New("write failed")
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		return room, errors.New("short write")
	}
	return w.buf.Write(p)
}

func TestLimitWriter_ExactFitDoesNotTruncate(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: 5}

	n, err := lw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if lw.truncated {
		t.Error("writing exactly the cap must not mark output truncated")
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
}

func TestLimitWriter_TruncatesAtBoundary(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: 5}

	if n, err := lw.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first Write = (%d, %v), want (3, nil)", n, err)
	}
	if n, err := lw.Write([]byte("defgh")); err != nil || n != 5 {
		t.Fatalf("second Write = (%d, %v), want (5, nil)", n, err)
	}
	if n, err := lw.Write([]byte("ignored")); err != nil || n != 7 {
		t.Fatalf("post-truncation Write = (%d, %v), want (7, nil)", n, err)
	}

	want := "abcde" + truncationMarker
	if buf.String() != want {
		t.Errorf("buf = %q, want %q", buf.String(), want)
	}
}

func TestLimitWriter_ZeroCapacityWritesOnlyMarker(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: 0}

	if _, err := lw.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != truncationMarker {
		t.Errorf("buf = %q, want only the marker", buf.String())
	}
}

func TestLimitWriter_UnderlyingErrorWithinCap(t *testing.T) {
	ew := &errWriter{limit: 2}
	lw := &limitWriter{w: ew, n: 10}

	n, err := lw.Write([]byte("abcd"))
	if err == nil {
		t.Fatal("expected underlying write error")
	}
	if n != 2 {
		t.Errorf("n = %d, want 2 (bytes actually written)", n)
	}
	if lw.n != 8 {
		t.Errorf("remaining budget = %d, want 8", lw.n)
	}
}

func TestLimitWriter_UnderlyingErrorAtTruncation(t *testing.T) {
	ew := &errWriter{limit: 0}
	lw := &limitWriter{w: ew, n: 2}

	n, err := lw.Write([]byte("abcd"))
	if err == nil {
		t.Fatal("expected underlying write error")
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if lw.truncated {
		t.Error("failed partial write must not mark output truncated")
	}
}

func TestGetBool(t *testing.T) {
	tests := []struct {
		name string
		msg  exchange.Message
		want bool
	}{
		{"absent", exchange.Message{}, false},
		{"true", exchange.Message{"k": true}, true},
		{"false", exchange.Message{"k": false}, false},
		{"wrong type", exchange.Message{"k": "true"}, false},
		{"int", exchange.Message{"k": int64(1)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getBool(tt.msg, "k"); got != tt.want {
				t.Errorf("getBool = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetAttachments(t *testing.T) {
	tests := []struct {
		name string
		msg  exchange.Message
		want map[string]int64
	}{
		{"absent", exchange.Message{}, nil},
		{"not a map", exchange.Message{"attachments": []any{"a"}}, nil},
		{"empty map", exchange.Message{"attachments": map[string]any{}}, nil},
		{"no int64 ids", exchange.Message{"attachments": map[string]any{"a": "1", "b": 2}}, nil},
		{"mixed", exchange.Message{"attachments": map[string]any{"a": int64(1), "b": "x"}}, map[string]int64{"a": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getAttachments(tt.msg)
			if tt.want == nil {
				if got != nil {
					t.Errorf("getAttachments = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("getAttachments = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("getAttachments[%q] = %d, want %d", k, got[k], v)
				}
			}
		})
	}
}

func TestGetSnaps(t *testing.T) {
	tests := []struct {
		name    string
		msg     exchange.Message
		wantLen int
		wantErr string
	}{
		{"missing", exchange.Message{}, 0, "missing required field"},
		{"not a list", exchange.Message{"snaps": "hello"}, 0, "expected list"},
		{"entry not dict", exchange.Message{"snaps": []any{"hello"}}, 0, "expected dict"},
		{"empty list", exchange.Message{"snaps": []any{}}, 0, ""},
		{"two entries", exchange.Message{"snaps": []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}}}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getSnaps(tt.msg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestGetStringAndInt64_WrongType(t *testing.T) {
	if _, err := getString(exchange.Message{"k": int64(1)}, "k"); err == nil || !strings.Contains(err.Error(), "expected string") {
		t.Errorf("getString err = %v, want type error", err)
	}
	if _, err := getInt64(exchange.Message{"k": "1"}, "k"); err == nil || !strings.Contains(err.Error(), "expected int64") {
		t.Errorf("getInt64 err = %v, want type error", err)
	}
}

func TestOperationContextManager_RegisterNilCancelIgnored(t *testing.T) {
	m := NewOperationContextManager()
	m.Register(1, nil)

	m.mu.Lock()
	_, ok := m.operations[1]
	m.mu.Unlock()
	if ok {
		t.Error("nil cancel func must not be registered")
	}
	m.Cancel(1) // must not panic on an unregistered id
}

func TestOperationContextManager_ReRegisterReplacesCancel(t *testing.T) {
	m := NewOperationContextManager()
	var first, second int
	m.Register(1, func() { first++ })
	m.Register(1, func() { second++ })

	m.Cancel(1)
	if first != 0 || second != 1 {
		t.Errorf("first=%d second=%d, want 0 and 1", first, second)
	}

	m.Cleanup(1)
	m.Cancel(1)
	if second != 1 {
		t.Errorf("Cancel after Cleanup invoked cancel func again (second=%d)", second)
	}
}

func TestCancelHandler_MissingOperationID(t *testing.T) {
	h := NewCancelHandler(NewOperationContextManager())
	if got := h.MessageType(); got != "cancel-operation" {
		t.Errorf("MessageType = %q", got)
	}
	if err := h.Handle(context.Background(), exchange.Message{}, nil); err == nil {
		t.Error("expected error for missing operation-id")
	}
}

func TestCancelHandler_NilManagerIsNoop(t *testing.T) {
	h := NewCancelHandler(nil)
	if err := h.Handle(context.Background(), exchange.Message{"operation-id": int64(1)}, nil); err != nil {
		t.Errorf("Handle: %v", err)
	}
}

// TestRunner_SemaphoreAcquireFailureReportsFailed saturates a 1-slot runner,
// then dispatches with a cancelled context so Acquire fails.
func TestRunner_SemaphoreAcquireFailureReportsFailed(t *testing.T) {
	src := newMockCommandSource()
	sink := &mockResultSink{}
	block := make(chan struct{})
	h := newFakeHandler("slow")
	h.started = make(chan struct{}, 1)
	h.block = block

	r := NewRunner([]Handler{h}, src, sink, 1)
	r.Register()

	src.Dispatch(context.Background(), "slow", exchange.Message{"operation-id": int64(1)})
	if _, ok := waitChan(h.started, 2*time.Second); !ok {
		t.Fatal("first handler did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src.Dispatch(ctx, "slow", exchange.Message{"operation-id": int64(2)})

	call, ok := sink.first()
	if !ok {
		t.Fatal("no result sent for rejected dispatch")
	}
	if call.opID != 2 || call.status != exchange.StatusFailed {
		t.Errorf("result = %+v, want op 2 StatusFailed", call)
	}
	if !strings.Contains(call.output, "semaphore acquire failed") {
		t.Errorf("output = %q, want semaphore failure", call.output)
	}

	close(block)
	if err := r.WaitWithTimeout(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	n := len(sink.results)
	sink.mu.Unlock()
	if n != 1 {
		t.Errorf("results = %d, want 1 (second handler must not run)", n)
	}
}
