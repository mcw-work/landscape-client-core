package exchange

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// lockedLog is a goroutine-safe slog sink; Run logs from its own goroutine.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// hasLine reports whether a single log line contains every substring.
func (l *lockedLog) hasLine(subs ...string) bool {
	for line := range strings.SplitSeq(l.String(), "\n") {
		all := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func captureExchangeLogs(t *testing.T) *lockedLog {
	t.Helper()
	l := &lockedLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// pollUntil fails the test if cond does not become true before the deadline.
func pollUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// statusServer answers every request with a fixed HTTP status and counts them.
type statusServer struct {
	code int
	hits atomic.Int64
}

func (s *statusServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.hits.Add(1)
	w.WriteHeader(s.code)
}

// newExchangeAt builds an Exchange against url with state in a fresh temp dir.
// The urgent interval is short so an unregistered client polls rapidly.
func newExchangeAt(t *testing.T, url string) (*Exchange, *persist.Store, string) {
	t.Helper()
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))
	cfg := &config.Config{
		URL:                    url,
		AccountName:            "acc",
		ExchangeInterval:       time.Hour,
		UrgentExchangeInterval: 5 * time.Millisecond,
	}
	return New(cfg, store, tc), store, dir
}

func markRegistered(t *testing.T, store *persist.Store) {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st.SecureID = "already-registered"
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// startRun runs ex.Run in a goroutine and returns a stop func that cancels it
// and returns Run's result.
func startRun(t *testing.T, ex *Exchange) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ex.Run(ctx) }()
	var once sync.Once
	var result error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after cancellation")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func messageTypes(t *testing.T, payload map[string]any) []string {
	t.Helper()
	msgs, ok := payload["messages"].([]any)
	if !ok {
		t.Fatalf("messages: unexpected type %T", payload["messages"])
	}
	var types []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		s, _ := mm["type"].(string)
		types = append(types, s)
	}
	return types
}

func TestRun_StateLoadErrorIsReturned(t *testing.T) {
	ex, _, dir := newExchangeAt(t, "http://127.0.0.1:1")
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := ex.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot load state") {
		t.Errorf("Run err = %v, want a state load error", err)
	}
}

func TestRun_RestoredSpoolIsSentBeforeNewMessages(t *testing.T) {
	logs := captureExchangeLogs(t)
	fs := &fakeServer{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	ex, store, dir := newExchangeAt(t, srv.URL)
	markRegistered(t, store)

	spoolPath := filepath.Join(dir, "queue.json")
	if err := newSpool(spoolPath).save([]Message{{"type": "spooled"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	ex.SetSpool(spoolPath)
	_ = ex.Send(context.Background(), Message{"type": "fresh"})

	stop := startRun(t, ex)
	pollUntil(t, 5*time.Second, "first exchange", func() bool { return fs.count() >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := strings.Join(messageTypes(t, fs.get(0).payload), ","); got != "spooled,fresh" {
		t.Errorf("first exchange types = %q, want spooled,fresh", got)
	}
	if !logs.hasLine("restored queued messages from the spool", "count=1") {
		t.Errorf("restore was not logged: %q", logs.String())
	}
}

// Spool I/O errors must degrade to warnings: a broken spool must not stop the
// exchange loop.
func TestRun_SpoolIOErrorsAreLoggedNotFatal(t *testing.T) {
	logs := captureExchangeLogs(t)
	fs := &fakeServer{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	ex, store, dir := newExchangeAt(t, srv.URL)
	markRegistered(t, store)

	spoolPath := filepath.Join(dir, "queue.json")
	if err := os.Mkdir(spoolPath, 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	ex.SetSpool(spoolPath)

	stop := startRun(t, ex)
	pollUntil(t, 5*time.Second, "first exchange", func() bool { return fs.count() >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !logs.hasLine("exchange: cannot load spool") {
		t.Errorf("spool load failure was not logged: %q", logs.String())
	}
	if !logs.hasLine("exchange: cannot save spool") {
		t.Errorf("spool save failure was not logged: %q", logs.String())
	}
}

// A 404 is not server overload: the client logs that it has no API version to
// fall back to and keeps polling at its normal cadence instead of backing off.
func TestRun_404IsLoggedAndDoesNotBackOff(t *testing.T) {
	logs := captureExchangeLogs(t)
	ss := &statusServer{code: http.StatusNotFound}
	srv := httptest.NewServer(ss)
	t.Cleanup(srv.Close)
	ex, _, _ := newExchangeAt(t, srv.URL)

	stop := startRun(t, ex)
	// Unregistered, so the urgent interval (5ms) applies; a backoff would hold
	// the second attempt off for 300s.
	pollUntil(t, 5*time.Second, "three exchange attempts", func() bool { return ss.hits.Load() >= 3 })
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !logs.hasLine("no earlier version to downgrade to", "api_version="+apiVersion) {
		t.Errorf("404 downgrade message not logged: %q", logs.String())
	}
	if logs.hasLine("backing off after server errors") {
		t.Errorf("404 must not trigger backoff: %q", logs.String())
	}
	if !logs.hasLine("final drain exchange failed", "HTTP 404") {
		t.Errorf("final drain failure not logged: %q", logs.String())
	}
}

// After a 5xx the backoff must override the urgent interval, even while
// unregistered.
func TestRun_5xxBacksOffOverUrgentInterval(t *testing.T) {
	logs := captureExchangeLogs(t)
	ss := &statusServer{code: http.StatusServiceUnavailable}
	srv := httptest.NewServer(ss)
	t.Cleanup(srv.Close)
	ex, _, _ := newExchangeAt(t, srv.URL)

	stop := startRun(t, ex)
	pollUntil(t, 5*time.Second, "backoff log", func() bool {
		return logs.hasLine("backing off after server errors")
	})
	// Absence check: 20 urgent intervals elapse here, none may produce a request.
	time.Sleep(100 * time.Millisecond)
	if got := ss.hits.Load(); got != 1 {
		t.Errorf("requests during backoff = %d, want 1", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Shutdown still attempts one final drain.
	if got := ss.hits.Load(); got != 2 {
		t.Errorf("requests after shutdown = %d, want 2 (initial + final drain)", got)
	}
}

func TestPersistQueue_FullQueueDropsTelemetryAndWarns(t *testing.T) {
	logs := captureExchangeLogs(t)
	ex, _, dir := newExchangeAt(t, "http://127.0.0.1:1")
	spoolPath := filepath.Join(dir, "queue.json")
	ex.SetSpool(spoolPath)

	for range maxQueuedMessages + 5 {
		_ = ex.Send(context.Background(), Message{"type": "cpu-usage"})
	}
	_ = ex.Send(context.Background(), Message{"type": "operation-result", "operation-id": int64(1)})
	ex.persistQueue()

	ex.mu.Lock()
	pending := len(ex.pending)
	ex.mu.Unlock()
	if pending != maxQueuedMessages {
		t.Errorf("pending = %d, want %d", pending, maxQueuedMessages)
	}
	if !logs.hasLine("queue full", "dropped=6") {
		t.Errorf("drop warning not logged: %q", logs.String())
	}

	saved, err := newSpool(spoolPath).load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(saved) != maxQueuedMessages {
		t.Fatalf("spooled %d messages, want %d", len(saved), maxQueuedMessages)
	}
	if saved[len(saved)-1]["type"] != "operation-result" {
		t.Errorf("operation-result not retained in the spool: %v", saved[len(saved)-1])
	}
}

func TestPerformExchange_DropsTypesServerDoesNotAccept(t *testing.T) {
	logs := captureExchangeLogs(t)
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	state.AcceptedTypes = []string{"cpu-usage"}

	for _, typ := range []string{"cpu-usage", "disk-usage", "operation-result"} {
		_ = ts.ex.Send(context.Background(), Message{"type": typ})
	}
	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	if got := strings.Join(messageTypes(t, ts.fs.get(0).payload), ","); got != "cpu-usage,operation-result" {
		t.Errorf("sent types = %q, want cpu-usage,operation-result", got)
	}
	if !logs.hasLine("not in server accepted list", "dropped=1") {
		t.Errorf("drop not logged: %q", logs.String())
	}
}

func TestPerformExchange_UnmarshalablePayloadIsNotPosted(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"

	_ = ts.ex.Send(context.Background(), Message{"type": "bad", "value": struct{}{}})
	err := ts.ex.performExchange(context.Background(), state)
	if err == nil || !strings.Contains(err.Error(), "cannot marshal payload") {
		t.Fatalf("performExchange err = %v, want a marshal error", err)
	}
	if n := ts.fs.count(); n != 0 {
		t.Errorf("server received %d requests, want 0", n)
	}
}

func TestPerformExchange_MalformedResponseIsError(t *testing.T) {
	notDict, err := bpickle.Marshal([]any{"x"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	tests := []struct {
		name    string
		body    []byte
		wantErr string
	}{
		{"undecodable", []byte("\xff not bpickle"), "cannot unmarshal response"},
		{"not a dict", notDict, "response is not a dict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write(tc.body) //nolint:errcheck
			}))
			t.Cleanup(srv.Close)
			ts := newTestSetup(t)
			ts.cfg.URL = srv.URL
			state := ts.freshState(t)
			state.SecureID = "sec123"
			state.OutboundSequence = 7

			err := ts.ex.performExchange(context.Background(), state)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("performExchange err = %v, want %q", err, tc.wantErr)
			}
			if state.OutboundSequence != 7 {
				t.Errorf("OutboundSequence = %d, want unchanged 7", state.OutboundSequence)
			}
		})
	}
}

// The server may encode identifiers as bytes or integers rather than strings.
func TestPerformExchange_SetIDAndAcceptedTypesAcceptNonStringValues(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "set-id", "id": []byte("sec-bytes"), "insecure-id": int64(77)},
			map[string]any{"type": "accepted-types", "types": []any{[]byte("a"), "b", int64(3)}},
		},
	})
	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "set-id", "id": int64(5), "insecure-id": []byte("ins-bytes")},
		},
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 1: %v", err)
	}
	if state.SecureID != "sec-bytes" || state.InsecureID != "77" {
		t.Errorf("ids = (%q, %q), want (sec-bytes, 77)", state.SecureID, state.InsecureID)
	}
	if got := ts.ex.InsecureID(); got != "77" {
		t.Errorf("InsecureID() = %q, want 77", got)
	}
	if got := strings.Join(state.AcceptedTypes, ","); got != "a,b" {
		t.Errorf("AcceptedTypes = %q, want a,b (non-string entries ignored)", got)
	}

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange 2: %v", err)
	}
	if state.SecureID != "5" || state.InsecureID != "ins-bytes" {
		t.Errorf("ids = (%q, %q), want (5, ins-bytes)", state.SecureID, state.InsecureID)
	}
}

func TestPerformExchange_UnknownIDClearsIdentity(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	state.InsecureID = "ins456"
	ts.fs.push(map[string]any{"messages": []any{map[string]any{"type": "unknown-id"}}})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}
	if state.SecureID != "" || state.InsecureID != "" {
		t.Errorf("ids = (%q, %q), want both cleared", state.SecureID, state.InsecureID)
	}
	saved, err := ts.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.SecureID != "" {
		t.Errorf("persisted SecureID = %q, want cleared", saved.SecureID)
	}
}

func TestPerformExchange_RegistrationOutcomesAreLogged(t *testing.T) {
	logs := captureExchangeLogs(t)
	ts := newTestSetup(t)
	state := ts.freshState(t)
	state.SecureID = "sec123"
	ts.fs.push(map[string]any{
		"messages": []any{
			map[string]any{"type": "registration", "info": "unknown-account"},
			map[string]any{"type": "registration", "info": []byte("awaiting-approval")},
			map[string]any{"type": "registration-complete"},
		},
	})

	if err := ts.ex.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}
	if !logs.hasLine("level=WARN", "registration failed", "info=unknown-account") {
		t.Errorf("registration failure not logged as a warning: %q", logs.String())
	}
	if !logs.hasLine("level=INFO", "registration pending", "info=awaiting-approval") {
		t.Errorf("pending registration not logged: %q", logs.String())
	}
	if !logs.hasLine("registration complete") {
		t.Errorf("registration-complete not logged: %q", logs.String())
	}
	if logs.hasLine("no handler for message type") {
		t.Errorf("registration messages must not go to subscribers: %q", logs.String())
	}
	if state.NextExpectedFromServer != 3 {
		t.Errorf("NextExpectedFromServer = %d, want 3", state.NextExpectedFromServer)
	}
}

func TestPerformExchange_StateSaveErrorIsReturned(t *testing.T) {
	fs := &fakeServer{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	ex, store, dir := newExchangeAt(t, srv.URL)
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	state.SecureID = "sec123"
	// Update's read-modify-write fails on a corrupt file.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err = ex.performExchange(context.Background(), state)
	if err == nil || !strings.Contains(err.Error(), "cannot save state") {
		t.Fatalf("performExchange err = %v, want a save error", err)
	}
	if fs.count() != 1 {
		t.Errorf("server requests = %d, want 1", fs.count())
	}
}

func TestExtractMessages_SkipsMalformedEntries(t *testing.T) {
	if got := extractMessages(map[string]any{}); got != nil {
		t.Errorf("no messages key: got %v, want nil", got)
	}
	if got := extractMessages(map[string]any{"messages": "not a list"}); got != nil {
		t.Errorf("non-list messages: got %v, want nil", got)
	}
	got := extractMessages(map[string]any{"messages": []any{
		int64(1),
		map[string]any{"type": "ok"},
		[]byte("raw"),
	}})
	if len(got) != 1 || got[0]["type"] != "ok" {
		t.Errorf("got %v, want only the map entry", got)
	}
}

func TestSpool_SaveUnencodableMessageIsError(t *testing.T) {
	dir := t.TempDir()
	s := newSpool(filepath.Join(dir, "queue.json"))
	err := s.save([]Message{{"type": "x", "c": make(chan int)}})
	if err == nil || !strings.Contains(err.Error(), "cannot encode message spool") {
		t.Fatalf("save err = %v, want an encode error", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("save left %d files behind", len(entries))
	}
}

func TestSpool_SaveRenameFailureRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")
	// A non-empty directory at the spool path cannot be renamed over.
	if err := os.MkdirAll(filepath.Join(path, "child"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	err := newSpool(path).save([]Message{{"type": "x"}})
	if err == nil || !strings.Contains(err.Error(), "cannot rename spool temp file") {
		t.Fatalf("save err = %v, want a rename error", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "queue.json" {
		t.Errorf("temp file leaked: %v", entries)
	}
}
