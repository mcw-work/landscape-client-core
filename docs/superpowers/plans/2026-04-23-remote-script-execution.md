# Remote Script Execution Implementation Plan

> **For agentic workers:** REQUIRED: Use the `subagent-driven-development` agent (recommended) or `executing-plans` agent to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enhance the existing `ScriptExecHandler` to support interpreter selection, file attachments, correct result codes, and a 5 MiB output cap with truncation marker — achieving wire compatibility with the Python landscape-client.

**Architecture:** Extend `ResultSink` with `SendResultCode` for failure result codes (102/103/104). Add a `Get` method to `transport.Client` and a `TransportAttachmentFetcher` implementing a new `AttachmentFetcher` interface. Update `ScriptExecHandler` to use the interpreter field, download attachments, and emit correct result codes.

**Tech Stack:** Go 1.22, `os/exec`, `net/http`, `internal/exchange`, `internal/transport`, `internal/persist`

**Spec:** [docs/superpowers/specs/2026-04-23-remote-script-execution-design.md](../specs/2026-04-23-remote-script-execution-design.md)

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `internal/exchange/exchange.go` | Modify | Add `SendResultCode` to `ResultSink` interface + implement on `Exchange` |
| `internal/exchange/exchange_test.go` | Modify | Add test for `SendResultCode` |
| `internal/manager/runner_test.go` | Modify | Add `SendResultCode` to `mockResultSink` (package `manager`) |
| `internal/manager/snap_test.go` | Modify | Add `SendResultCode` to `mockResultSink` (package `manager_test`) |
| `internal/manager/system.go` | Modify | Interpreter support, 5 MiB cap + truncation marker, result codes, attachment handling |
| `internal/manager/system_test.go` | Modify | Tests for all new behaviour |
| `internal/transport/transport.go` | Modify | Add `Get` method to `Client` |
| `internal/transport/transport_test.go` | Modify | Add test for `Get` method |
| `internal/transport/attachments.go` | Create | `TransportAttachmentFetcher` |
| `internal/transport/attachments_test.go` | Create | Tests for `TransportAttachmentFetcher` |
| `cmd/landscape-client-core/main.go` | Modify | Pass `TransportAttachmentFetcher` to `NewScriptExecHandler` |

---

## Task 1: Extend `ResultSink` with `SendResultCode`

**Files:**
- Modify: `internal/exchange/exchange.go`
- Modify: `internal/exchange/exchange_test.go`

- [ ] **Step 1: Write the failing test in `exchange_test.go`**

Add this test to `internal/exchange/exchange_test.go` (after the existing tests):

```go
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
	go exc.Run(runCtx) //nolint:errcheck

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
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /home/michael.croft-white@canonical.com/source/landscape-client-core
go test ./internal/exchange/... -run TestExchange_SendResultCode -v
```

Expected: compile error — `exchange.ResultSink` has no method `SendResultCode` and `Exchange` has no method `SendResultCode`.

- [ ] **Step 3: Add `SendResultCode` to the `ResultSink` interface**

In `internal/exchange/exchange.go`, update the `ResultSink` interface:

```go
// ResultSink allows manager handlers to send operation results back to the server.
type ResultSink interface {
	SendResult(ctx context.Context, operationID int64, status int, output string) error
	// SendResultCode is like SendResult but includes a result-code field in the
	// operation-result message. Used by ScriptExecHandler for codes 102/103/104.
	SendResultCode(ctx context.Context, operationID int64, status int, resultCode int64, output string) error
}
```

- [ ] **Step 4: Implement `SendResultCode` on `Exchange`**

In `internal/exchange/exchange.go`, add after `SendResult`:

```go
// SendResultCode enqueues an operation-result message with a result-code field.
func (e *Exchange) SendResultCode(ctx context.Context, operationID int64, status int, resultCode int64, output string) error {
	return e.Send(ctx, Message{
		"type":         "operation-result",
		"operation-id": operationID,
		"status":       int64(status),
		"result-code":  resultCode,
		"result-text":  output,
	})
}
```

- [ ] **Step 5: Add `SendResultCode` stub to `mockResultSink` in `runner_test.go`**

`runner_test.go` is `package manager` (internal test package). Update `resultCall` struct and add the new method:

```go
type resultCall struct {
	opID       int64
	status     int
	resultCode int64 // 0 means not set
	output     string
}

// Add after the existing SendResult method:
func (m *mockResultSink) SendResultCode(_ context.Context, operationID int64, status int, resultCode int64, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, resultCall{opID: operationID, status: status, resultCode: resultCode, output: output})
	return nil
}
```

- [ ] **Step 6: Add `SendResultCode` stub to `mockResultSink` in `snap_test.go`**

`snap_test.go` is `package manager_test` (external test package, shared with `system_test.go`). Update `resultCall` struct and add the new method:

```go
type resultCall struct {
	opID       int64
	status     int
	resultCode int64 // 0 means not set
	output     string
}

// Add after the existing SendResult method:
func (m *mockResultSink) SendResultCode(_ context.Context, opID int64, status int, resultCode int64, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, resultCall{opID, status, resultCode, output})
	return nil
}
```

Also update the existing `SendResult` in `snap_test.go` to match the updated `resultCall` struct (add `0` for `resultCode`):

```go
func (m *mockResultSink) SendResult(_ context.Context, opID int64, status int, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, resultCall{opID, status, 0, output})
	return nil
}
```

- [ ] **Step 7: Run all manager and exchange tests to verify they pass**

```bash
go test ./internal/exchange/... ./internal/manager/... -v 2>&1 | tail -30
```

Expected: all tests PASS (including the new `TestExchange_SendResultCode`).

- [ ] **Step 8: Commit**

```bash
git add internal/exchange/exchange.go internal/exchange/exchange_test.go \
        internal/manager/runner_test.go internal/manager/snap_test.go
git commit -m "feat: extend ResultSink with SendResultCode for script result codes"
```

---

## Task 2: Add Interpreter Support to `ScriptExecHandler`

**Files:**
- Modify: `internal/manager/system.go`
- Modify: `internal/manager/system_test.go`

- [ ] **Step 1: Write failing tests**

Add these tests to `internal/manager/system_test.go`:

```go
func TestScriptExecHandler_Interpreter(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(20),
		"interpreter":  "/usr/bin/python3",
		"code":         "print('hello from python')",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded; output: %s", call.status, call.output)
	}
	if !strings.Contains(call.output, "hello from python") {
		t.Errorf("output = %q, want to contain %q", call.output, "hello from python")
	}
}

func TestScriptExecHandler_DefaultsToSh(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	// No interpreter field — should default to /bin/sh.
	msg := exchange.Message{
		"operation-id": int64(21),
		"code":         "echo shell_default",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded", call.status)
	}
	if !strings.Contains(call.output, "shell_default") {
		t.Errorf("output = %q, want to contain %q", call.output, "shell_default")
	}
}

func TestScriptExecHandler_BadInterpreter(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(22),
		"interpreter":  "/nonexistent/interpreter",
		"code":         "echo hi",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
}
```

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/manager/... -run "TestScriptExecHandler_Interpreter|TestScriptExecHandler_DefaultsToSh|TestScriptExecHandler_BadInterpreter" -v
```

Expected: compile errors because `NewScriptExecHandler` takes only 1 argument and `nil` (AttachmentFetcher) is not yet defined.

- [ ] **Step 3: Update `ScriptExecHandler` to support interpreter and accept `AttachmentFetcher`**

In `internal/manager/system.go`, update the struct, constructor, and `Handle` method. First, add the `AttachmentFetcher` interface and update the struct:

```go
// AttachmentFetcher downloads attachment content from the Landscape server by ID.
type AttachmentFetcher interface {
	FetchAttachment(ctx context.Context, id int64) ([]byte, error)
}

// ScriptExecHandler handles "execute-script" commands.
type ScriptExecHandler struct {
	snapCommon string
	fetcher    AttachmentFetcher // nil = attachments not supported (log warning)
}

// NewScriptExecHandler creates a ScriptExecHandler.
// snapCommon is the $SNAP_COMMON directory; use t.TempDir() in tests.
// fetcher may be nil if attachment support is not needed.
func NewScriptExecHandler(snapCommon string, fetcher AttachmentFetcher) *ScriptExecHandler {
	return &ScriptExecHandler{snapCommon: snapCommon, fetcher: fetcher}
}
```

Now update the `Handle` method to:
1. Extract `interpreter` (default `/bin/sh` if absent)
2. Verify interpreter binary exists with `os.Stat`
3. Write script with shebang: `"#!" + interpreter + "\n" + code`
4. Run using `exec.CommandContext(execCtx, interpreter, scriptPath)` instead of `sh scriptPath`

Replace the entire `Handle` method body:

```go
func (h *ScriptExecHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}
	code, err := getString(msg, "code")
	if err != nil {
		return err
	}

	// interpreter defaults to /bin/sh if not provided.
	interpreter, _ := getString(msg, "interpreter")
	if interpreter == "" {
		interpreter = "/bin/sh"
	}

	// username switching is unsupported under strict confinement — log and ignore.
	if username, _ := getString(msg, "username"); username != "" {
		log.Printf("execute-script: username switching not supported under strict confinement, ignoring username %q", username)
	}

	// Verify interpreter binary exists.
	if _, err := os.Stat(interpreter); err != nil {
		_ = result.SendResult(ctx, opID, exchange.StatusFailed,
			fmt.Sprintf("execute-script: interpreter not found: %s", interpreter))
		return nil
	}

	// time-limit of 0 means no limit.
	timeLimit, _ := getInt64(msg, "time-limit")

	// Create per-operation script directory.
	scriptDir := filepath.Join(h.snapCommon, "scripts", fmt.Sprintf("%d", opID))
	if err := os.MkdirAll(scriptDir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(scriptDir)

	// Write script with shebang.
	scriptPath := filepath.Join(scriptDir, "script")
	scriptContent := "#!" + interpreter + "\n" + code
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0700); err != nil {
		return err
	}

	// Build execution context.
	execCtx := ctx
	if timeLimit > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeLimit)*time.Second)
		defer cancel()
	}

	// Run the script.
	cmd := exec.CommandContext(execCtx, interpreter, scriptPath)
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: 5 * 1024 * 1024}
	cmd.Stdout = lw
	cmd.Stderr = lw

	runErr := cmd.Run()
	output := buf.String()

	if execCtx.Err() == context.DeadlineExceeded {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 102, output)
		return nil
	}
	if runErr != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103, output)
		return nil
	}

	_ = result.SendResult(ctx, opID, exchange.StatusSucceeded, output)
	return nil
}
```

- [ ] **Step 4: Fix existing tests that call `NewScriptExecHandler` with one argument**

All existing `TestScriptExecHandler_*` tests in `system_test.go` call `manager.NewScriptExecHandler(t.TempDir())`. Update them all to pass `nil` as the second argument:

```go
h := manager.NewScriptExecHandler(t.TempDir(), nil)
```

There are 6 existing calls at lines 128, 152, 176, 198, 228, 254. Update each one.

Also update `TestScriptExecHandler_OutputTruncated`: the limit is now 5 MiB (not 1 MiB), so produce 10 MiB of output and check the cap:

```go
func TestScriptExecHandler_OutputTruncated(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	// Produce 10 MiB of output; limit is 5 MiB.
	msg := exchange.Message{
		"operation-id": int64(13),
		"code":         "yes x | head -c 10485760",
		"time-limit":   int64(30),
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	const limit = 5 * 1024 * 1024
	if len(call.output) == 0 {
		t.Error("output should not be empty")
	}
	// Allow slightly over limit for the truncation marker.
	if len(call.output) > limit+100 {
		t.Errorf("output not truncated: len = %d, want <= %d (5 MiB + marker)", len(call.output), limit+100)
	}
	if !strings.Contains(call.output, "**OUTPUT TRUNCATED**") {
		t.Errorf("output missing truncation marker; got: %q", call.output[:min(100, len(call.output))])
	}
}
```

Also update `TestScriptExecHandler_Cleanup` at line 254 to fix `NewScriptExecHandler` call:
```go
h := manager.NewScriptExecHandler(snapCommon, nil)
```

- [ ] **Step 5: Fix the `main.go` compile error**

`main.go` calls `manager.NewScriptExecHandler(snapCommon)`. Update it temporarily to pass `nil` (will be corrected in Task 8):

```go
manager.NewScriptExecHandler(snapCommon, nil),
```

- [ ] **Step 6: Run tests**

```bash
go test ./internal/manager/... -run "TestScriptExecHandler" -v 2>&1 | tail -40
```

Expected: all `TestScriptExecHandler_*` tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/manager/system.go internal/manager/system_test.go \
        cmd/landscape-client-core/main.go
git commit -m "feat: add interpreter support to ScriptExecHandler"
```

---

## Task 3: Fix `limitWriter` — 5 MiB Cap and Truncation Marker

**Files:**
- Modify: `internal/manager/system.go`
- Modify: `internal/manager/system_test.go`

Note: Task 2 already updated the cap constant to `5 * 1024 * 1024`. This task focuses on the truncation marker, which was not yet added.

- [ ] **Step 1: Verify the truncation marker test currently fails**

```bash
go test ./internal/manager/... -run TestScriptExecHandler_OutputTruncated -v
```

Expected: FAIL — output does not contain `"**OUTPUT TRUNCATED**"` (the marker hasn't been implemented yet).

- [ ] **Step 2: Update `limitWriter` to append the truncation marker**

In `internal/manager/system.go`, replace the `limitWriter` struct and `Write` method:

```go
const truncationMarker = "\n**OUTPUT TRUNCATED**"

// limitWriter caps combined writes to n bytes total, then appends a truncation
// marker once, and silently discards all subsequent writes.
// It is safe for concurrent use (stdout and stderr copy goroutines).
type limitWriter struct {
	mu        sync.Mutex
	w         io.Writer
	n         int
	truncated bool
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.truncated {
		return len(p), nil
	}
	if len(p) <= lw.n {
		n, err := lw.w.Write(p)
		lw.n -= n
		if err != nil {
			return n, err
		}
		return len(p), nil
	}
	// Write remaining capacity, then append the truncation marker.
	if lw.n > 0 {
		if _, err := lw.w.Write(p[:lw.n]); err != nil {
			return 0, err
		}
		lw.n = 0
	}
	_, _ = lw.w.Write([]byte(truncationMarker))
	lw.truncated = true
	return len(p), nil
}
```

- [ ] **Step 3: Run the truncation test**

```bash
go test ./internal/manager/... -run TestScriptExecHandler_OutputTruncated -v
```

Expected: PASS.

- [ ] **Step 4: Run all manager tests**

```bash
go test ./internal/manager/... -v 2>&1 | tail -30
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/system.go
git commit -m "fix: limitWriter appends truncation marker at 5 MiB cap"
```

---

## Task 4: Verify Result Codes 102/103 Are Sent

**Files:**
- Modify: `internal/manager/system_test.go`

The implementation already emits `SendResultCode` with 102/103 from Task 2. This task adds explicit tests to verify the codes reach the result sink.

- [ ] **Step 1: Write failing tests**

Add these tests to `internal/manager/system_test.go`:

```go
func TestScriptExecHandler_TimeoutResultCode(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(30),
		"code":         "sleep 10",
		"time-limit":   int64(1),
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 102 {
		t.Errorf("resultCode = %d, want 102 (timeout)", call.resultCode)
	}
}

func TestScriptExecHandler_NonZeroExitResultCode(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(31),
		"code":         "exit 1",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 103 {
		t.Errorf("resultCode = %d, want 103 (process failed)", call.resultCode)
	}
}

func TestScriptExecHandler_SuccessNoResultCode(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(32),
		"code":         "echo ok",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded", call.status)
	}
	if call.resultCode != 0 {
		t.Errorf("resultCode = %d, want 0 (no code on success)", call.resultCode)
	}
}
```

- [ ] **Step 2: Run the tests**

```bash
go test ./internal/manager/... -run "TestScriptExecHandler_TimeoutResultCode|TestScriptExecHandler_NonZeroExitResultCode|TestScriptExecHandler_SuccessNoResultCode" -v
```

Expected: all PASS (implementation was done in Task 2).

- [ ] **Step 3: Commit**

```bash
git add internal/manager/system_test.go
git commit -m "test: add result code assertions for script exec timeout and failure"
```

---

## Task 5: Add `Get` Method to `transport.Client`

**Files:**
- Modify: `internal/transport/transport.go`
- Modify: `internal/transport/transport_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/transport/transport_test.go`:

```go
func TestClient_Get(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.Header.Get("X-Custom-Header") != "test-value" {
			t.Errorf("X-Custom-Header = %q, want %q", r.Header.Get("X-Custom-Header"), "test-value")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("attachment-bytes"))
	}))
	defer srv.Close()

	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body, err := c.Get(context.Background(), srv.URL+"/attachment/123", map[string]string{
		"X-Custom-Header": "test-value",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "attachment-bytes" {
		t.Errorf("body = %q, want %q", body, "attachment-bytes")
	}
}

func TestClient_Get_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(Config{})
	_, err := c.Get(context.Background(), srv.URL+"/attachment/999", nil)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Errorf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", httpErr.StatusCode)
	}
}
```

Note: add `"errors"` to the import block if not present.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/transport/... -run "TestClient_Get" -v
```

Expected: compile error — `Client` has no method `Get`.

- [ ] **Step 3: Implement `Get` on `transport.Client`**

Add to `internal/transport/transport.go` after the `Post` method:

```go
// Get sends a GET request to rawURL with the given headers.
// Returns the response body on success (2xx), or HTTPError for non-2xx responses.
// The caller owns the returned bytes.
func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	if c.totalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.totalTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("transport: creating request: %w", err)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		const maxErrBody = 4096
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: errBody, URL: rawURL}
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("transport: reading response from %s: %w", rawURL, err)
	}
	return respBody, nil
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/transport/... -v 2>&1 | tail -20
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transport/transport.go internal/transport/transport_test.go
git commit -m "feat: add Get method to transport.Client for attachment fetching"
```

---

## Task 6: Add `TransportAttachmentFetcher`

**Files:**
- Create: `internal/transport/attachments.go`
- Create: `internal/transport/attachments_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/transport/attachments_test.go`:

```go
package transport_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

func TestTransportAttachmentFetcher_Success(t *testing.T) {
	var gotComputerID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotComputerID = r.Header.Get("X-Computer-ID")
		if r.URL.Path != "/attachment/42" {
			t.Errorf("path = %q, want /attachment/42", r.URL.Path)
		}
		w.Write([]byte("file-contents"))
	}))
	defer srv.Close()

	// The message-system URL ends with "/message-system"; base URL strips that segment.
	msgURL := srv.URL + "/message-system"

	store := persist.New(t.TempDir() + "/state.json")
	st, _ := store.Load()
	st.SecureID = "my-secure-id"
	if err := store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	tc, _ := transport.New(transport.Config{})
	fetcher := transport.NewAttachmentFetcher(tc, msgURL, store)

	data, err := fetcher.FetchAttachment(context.Background(), 42)
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if string(data) != "file-contents" {
		t.Errorf("data = %q, want %q", data, "file-contents")
	}
	if gotComputerID != "my-secure-id" {
		t.Errorf("X-Computer-ID = %q, want %q", gotComputerID, "my-secure-id")
	}
}

func TestTransportAttachmentFetcher_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := persist.New(t.TempDir() + "/state.json")
	st, _ := store.Load()
	st.SecureID = "id"
	_ = store.Save(st)

	tc, _ := transport.New(transport.Config{})
	fetcher := transport.NewAttachmentFetcher(tc, srv.URL+"/message-system", store)

	_, err := fetcher.FetchAttachment(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestTransportAttachmentFetcher_URLConstruction(t *testing.T) {
	// Verify that the attachment URL strips the last path segment of the
	// message-system URL and appends "attachment/<id>".
	tests := []struct {
		msgURL  string
		id      int64
		wantPath string
	}{
		{"https://host/message-system", 5, "/attachment/5"},
		{"https://host/landscape/message-system", 99, "/landscape/attachment/99"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("id=%d", tt.id), func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Write([]byte("ok"))
			}))
			defer srv.Close()

			// Replace the host in the test URL with the test server's host.
			import_url := srv.URL + tt.msgURL[len("https://host"):]
			store := persist.New(t.TempDir() + "/state.json")
			st, _ := store.Load()
			st.SecureID = "id"
			_ = store.Save(st)

			tc, _ := transport.New(transport.Config{})
			fetcher := transport.NewAttachmentFetcher(tc, import_url, store)
			_, _ = fetcher.FetchAttachment(context.Background(), tt.id)

			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}
```

Note: the URL construction test substitutes the host portion to point at the test server. If this feels awkward, simplify by just testing the two simple cases with their own servers.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/transport/... -run "TestTransportAttachmentFetcher" -v
```

Expected: compile error — `transport.NewAttachmentFetcher` does not exist.

- [ ] **Step 3: Create `internal/transport/attachments.go`**

```go
package transport

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/canonical/landscape-client-core/internal/persist"
)

// TransportAttachmentFetcher downloads script attachments from the Landscape server.
// It derives the attachment base URL from the message-system URL by stripping its
// last path segment: e.g. "https://host/message-system" → "https://host/attachment/".
type TransportAttachmentFetcher struct {
	client    *Client
	baseURL   string // e.g. "https://host/attachment/"
	store     *persist.Store
}

// NewAttachmentFetcher creates a TransportAttachmentFetcher.
// msgURL is the Landscape message-system URL (cfg.URL).
func NewAttachmentFetcher(client *Client, msgURL string, store *persist.Store) *TransportAttachmentFetcher {
	idx := strings.LastIndex(msgURL, "/")
	baseURL := msgURL[:idx+1] // includes trailing slash, e.g. "https://host/"
	return &TransportAttachmentFetcher{
		client:  client,
		baseURL: baseURL + "attachment/",
		store:   store,
	}
}

// FetchAttachment fetches a single attachment by ID from the Landscape server.
func (f *TransportAttachmentFetcher) FetchAttachment(ctx context.Context, id int64) ([]byte, error) {
	state, err := f.store.Load()
	if err != nil {
		return nil, fmt.Errorf("attachments: loading state: %w", err)
	}

	attachURL := f.baseURL + strconv.FormatInt(id, 10)
	headers := map[string]string{
		"X-Computer-ID": state.SecureID,
	}
	return f.client.Get(ctx, attachURL, headers)
}
```

- [ ] **Step 4: Fix the URL construction test**

The URL construction test above has a Go syntax error (`import_url` is not valid Go). Replace the URL construction test body with a simpler approach:

```go
func TestTransportAttachmentFetcher_URLConstruction(t *testing.T) {
	tests := []struct {
		msgSuffix string
		id        int64
		wantSuffix string
	}{
		{"/message-system", 5, "/attachment/5"},
		{"/landscape/message-system", 99, "/landscape/attachment/99"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("id=%d", tt.id), func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Write([]byte("ok"))
			}))
			defer srv.Close()

			store := persist.New(t.TempDir() + "/state.json")
			st, _ := store.Load()
			st.SecureID = "id"
			_ = store.Save(st)

			tc, _ := transport.New(transport.Config{})
			fetcher := transport.NewAttachmentFetcher(tc, srv.URL+tt.msgSuffix, store)
			_, _ = fetcher.FetchAttachment(context.Background(), tt.id)

			if gotPath != tt.wantSuffix {
				t.Errorf("path = %q, want %q", gotPath, tt.wantSuffix)
			}
		})
	}
}
```

Update the `attachments_test.go` file to use the corrected version of this test.

- [ ] **Step 5: Run the tests**

```bash
go test ./internal/transport/... -run "TestTransportAttachmentFetcher" -v
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/transport/attachments.go internal/transport/attachments_test.go
git commit -m "feat: add TransportAttachmentFetcher for script attachment downloads"
```

---

## Task 7: Add Attachment Handling to `ScriptExecHandler`

**Files:**
- Modify: `internal/manager/system.go`
- Modify: `internal/manager/system_test.go`

- [ ] **Step 1: Write failing tests**

Add these tests to `internal/manager/system_test.go`. First, add a `fakeAttachmentFetcher` helper at the top of the file (after the imports):

```go
// fakeAttachmentFetcher returns canned bytes for each attachment ID.
type fakeAttachmentFetcher struct {
	data map[int64][]byte
	err  error
}

func (f *fakeAttachmentFetcher) FetchAttachment(_ context.Context, id int64) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.data[id]
	if !ok {
		return nil, fmt.Errorf("fakeAttachmentFetcher: unknown id %d", id)
	}
	return b, nil
}
```

Then add the tests:

```go
func TestScriptExecHandler_Attachments(t *testing.T) {
	fetcher := &fakeAttachmentFetcher{
		data: map[int64][]byte{
			int64(7): []byte("attachment content"),
		},
	}
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), fetcher)

	// The script reads $LANDSCAPE_ATTACHMENTS/myfile.txt and echoes its content.
	msg := exchange.Message{
		"operation-id": int64(40),
		"code":         "cat $LANDSCAPE_ATTACHMENTS/myfile.txt",
		"attachments":  map[string]any{"myfile.txt": int64(7)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded; output: %s", call.status, call.output)
	}
	if !strings.Contains(call.output, "attachment content") {
		t.Errorf("output = %q, want to contain %q", call.output, "attachment content")
	}
}

func TestScriptExecHandler_AttachmentFetchFailed(t *testing.T) {
	fetcher := &fakeAttachmentFetcher{
		err: fmt.Errorf("server returned 403"),
	}
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), fetcher)

	msg := exchange.Message{
		"operation-id": int64(41),
		"code":         "echo hi",
		"attachments":  map[string]any{"file.sh": int64(1)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 104 {
		t.Errorf("resultCode = %d, want 104 (attachment fetch failed)", call.resultCode)
	}
}

func TestScriptExecHandler_NoFetcherWithAttachments(t *testing.T) {
	// When fetcher is nil and attachments are present, report failure with code 104.
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(42),
		"code":         "echo hi",
		"attachments":  map[string]any{"file.sh": int64(1)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 104 {
		t.Errorf("resultCode = %d, want 104", call.resultCode)
	}
}
```

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/manager/... -run "TestScriptExecHandler_Attachment" -v
```

Expected: FAIL — `attachments` field is not processed; the `fakeAttachmentFetcher` type doesn't compile yet either.

- [ ] **Step 3: Add attachment handling to `Handle` in `system.go`**

Add a helper to extract the attachments map from a message and add attachment download logic. In `internal/manager/system.go`, add this helper function after `getBool`:

```go
// getAttachments extracts the optional "attachments" field.
// Returns nil if absent or empty.
func getAttachments(msg exchange.Message) map[string]int64 {
	v, ok := msg["attachments"]
	if !ok {
		return nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]int64, len(raw))
	for name, idAny := range raw {
		if id, ok := idAny.(int64); ok {
			result[name] = id
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
```

Then update the `Handle` method to download attachments after creating the script dir and before running the script. Insert the following block after the `os.WriteFile(scriptPath, ...)` call and before building the execution context:

```go
	// Handle attachments.
	attachments := getAttachments(msg)
	if len(attachments) > 0 {
		if h.fetcher == nil {
			_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 104,
				"execute-script: attachment fetching not configured")
			return nil
		}
		for filename, attachID := range attachments {
			data, err := h.fetcher.FetchAttachment(ctx, attachID)
			if err != nil {
				_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 104,
					fmt.Sprintf("execute-script: fetching attachment %q: %v", filename, err))
				return nil
			}
			destPath := filepath.Join(scriptDir, filename)
			if err := os.WriteFile(destPath, data, 0600); err != nil {
				return err
			}
		}
	}

	// Set LANDSCAPE_ATTACHMENTS env var when attachments are present.
	var cmdEnv []string
	if len(attachments) > 0 {
		cmdEnv = append(os.Environ(), "LANDSCAPE_ATTACHMENTS="+scriptDir)
	}
```

Then update the `exec.CommandContext` call to use `cmdEnv`:

```go
	cmd := exec.CommandContext(execCtx, interpreter, scriptPath)
	if len(cmdEnv) > 0 {
		cmd.Env = cmdEnv
	}
```

- [ ] **Step 4: Run attachment tests**

```bash
go test ./internal/manager/... -run "TestScriptExecHandler_Attachment" -v
```

Expected: all PASS.

- [ ] **Step 5: Run the full manager test suite**

```bash
go test ./internal/manager/... -v 2>&1 | tail -30
```

Expected: all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/manager/system.go internal/manager/system_test.go
git commit -m "feat: add attachment support to ScriptExecHandler"
```

---

## Task 8: Wire `TransportAttachmentFetcher` into `main.go`

**Files:**
- Modify: `cmd/landscape-client-core/main.go`

- [ ] **Step 1: Update the handler registration**

In `cmd/landscape-client-core/main.go`, replace the temporary `nil` fetcher with a real `TransportAttachmentFetcher`. The `tc` (transport client), `cfg.URL`, and `store` are all already available in scope.

Replace:
```go
manager.NewScriptExecHandler(snapCommon, nil),
```

With:
```go
manager.NewScriptExecHandler(snapCommon, transport.NewAttachmentFetcher(tc, cfg.URL, store)),
```

- [ ] **Step 2: Build to verify no compile errors**

```bash
go build ./cmd/landscape-client-core/...
```

Expected: exits 0, no output.

- [ ] **Step 3: Run all tests**

```bash
go test ./... 2>&1 | tail -20
```

Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/landscape-client-core/main.go
git commit -m "feat: wire TransportAttachmentFetcher into ScriptExecHandler in main"
```

---

## Self-Review

### Spec coverage check

| Spec requirement | Task |
|---|---|
| Handle `execute-script` message | Pre-existing + Task 2 |
| Interpreter field (build shebang, run via interpreter) | Task 2 |
| Run as current process user (snap/root mode) | Pre-existing |
| Buffered stdout+stderr output | Pre-existing |
| 5 MiB output cap + truncation marker | Task 3 |
| `result-code: 102` on timeout | Task 2+4 |
| `result-code: 103` on non-zero exit | Task 2+4 |
| `result-code: 104` on attachment fetch failure | Task 7 |
| Attachment download via `X-Computer-ID` auth | Task 6 |
| Attachments written to script dir, `LANDSCAPE_ATTACHMENTS` env var set | Task 7 |
| Interpreter binary existence check | Task 2 |
| Cleanup temp dir on exit | Pre-existing |
| Wire up in `main.go` | Task 8 |

All requirements covered.

### Type/method consistency

- `AttachmentFetcher` interface defined in `internal/manager/system.go`, used in same file ✓
- `TransportAttachmentFetcher.FetchAttachment` matches `AttachmentFetcher.FetchAttachment` signature ✓
- `ResultSink.SendResultCode` signature matches all call sites in `system.go` ✓
- `NewScriptExecHandler(snapCommon, fetcher)` signature consistent across `system.go`, `system_test.go`, `main.go` ✓
