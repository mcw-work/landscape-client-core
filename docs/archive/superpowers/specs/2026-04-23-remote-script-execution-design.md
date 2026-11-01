# Remote Script Execution — Design Spec

**Date:** 2026-04-23  
**Status:** Approved  
**Codebase:** landscape-client-core (Go)

---

## Background

The Python landscape-client supports remote script execution via the `execute-script` message type. This spec defines how to add the same capability to the Go client-core, maintaining wire-compatibility with the existing Landscape server protocol.

---

## Scope

- Handle inbound `execute-script` messages from the Landscape server
- Execute arbitrary scripts as the current process user (snap-mode: always root)
- Return buffered stdout+stderr output via `operation-result`
- Support file attachments (downloaded from server, placed in temp dir alongside script)
- **Out of scope:** user-switching / privilege drop, streaming output, custom graph scripts

---

## File Layout

Three new files are added to `internal/manager/`:

```
internal/manager/
    executor.go       # Executor interface + real os/exec implementation
    script.go         # ExecuteScriptHandler + attachment fetcher interface
    script_test.go    # Unit tests
```

`cmd/landscape-client-core/main.go` gains one new handler registration. No other files change.

---

## Message Schema

### Inbound: `execute-script` (server → client)

| Field | Type | Required | Notes |
|---|---|---|---|
| `operation-id` | `int64` | yes | Correlates result back to server request |
| `interpreter` | `string` | yes | e.g. `/bin/bash`, `/usr/bin/python3` |
| `code` | `string` | yes | Script body — no shebang line |
| `username` | `string` | no | Ignored; always runs as current user |
| `time-limit` | `int64` | no | Seconds; 0 or absent = no timeout |
| `attachments` | `map[string]int64` | no | filename → attachment ID |

### Outbound: `operation-result` (client → server)

| Field | Value |
|---|---|
| `type` | `"operation-result"` |
| `operation-id` | Echoed from inbound message |
| `status` | `6` (succeeded) or `5` (failed) |
| `result-text` | Buffered stdout+stderr, UTF-8 with replacement chars |
| `result-code` | `102` (timeout), `103` (non-zero exit), `104` (attachment fetch failed); absent on success |

---

## Key Types & Interfaces

### `executor.go`

```go
// Executor abstracts process execution for testability.
type Executor interface {
    Run(ctx context.Context, scriptPath string, env []string, attachmentDir string) (output []byte, exitCode int, err error)
}

// OSExecutor is the real implementation using os/exec.
type OSExecutor struct{}
```

The real implementation sets `cmd.Stdout` and `cmd.Stderr` to a shared capping writer (see Output Handling). The `attachmentDir` is passed as the working directory (`cmd.Dir`).

### `script.go`

```go
// AttachmentFetcher downloads attachment content from the Landscape server.
type AttachmentFetcher interface {
    FetchAttachment(ctx context.Context, id int64) ([]byte, error)
}

// ExecuteScriptHandler handles "execute-script" messages from the server.
type ExecuteScriptHandler struct {
    Executor        Executor
    Fetcher         AttachmentFetcher
    OutputLimit     int    // default: 5 * 1024 * 1024 (5 MB)
}

func (h *ExecuteScriptHandler) MessageType() string { return "execute-script" }
func (h *ExecuteScriptHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error
```

---

## Execution Flow

Inside `Handle`:

1. Extract `operation-id`, `interpreter`, `code` — return error if missing/wrong type (no result sent; runner logs it).
2. Extract optional `time-limit` and `attachments`.
3. Verify interpreter binary exists via `os.Stat(interpreter)` — on failure: `reportResult` with `StatusFailed`, descriptive text, no result-code.
4. Write script to `os.CreateTemp("", "landscape-script-*")`, chmod 0700, content = `"#!" + interpreter + "\n" + code` as UTF-8.
5. If `attachments` non-empty:
   - Create temp dir via `os.MkdirTemp`, chmod 0700.
   - For each attachment: call `Fetcher.FetchAttachment(ctx, id)`, write to `dir/filename`, chmod 0600.
   - On any fetch error: `reportResult` with `StatusFailed`, `result-code: 104`; jump to cleanup.
6. Build execution context:
   - If `time-limit > 0`: `context.WithTimeout(ctx, time.Duration(timeLimit) * time.Second)`.
   - Otherwise use `ctx` directly.
7. Call `Executor.Run(execCtx, scriptPath, env, attachmentDir)`.  
   The `env` slice passed to the executor is built from: a minimal safe `PATH`, and any server-supplied `env` map entries from the message merged on top. The `LANDSCAPE_*` variables are injected by the server into that `env` map.
8. Interpret result:
   - Timeout (`execCtx.Err() == context.DeadlineExceeded`): `StatusFailed`, `result-code: 102`
   - Non-zero exit: `StatusFailed`, `result-code: 103`
   - Exit 0: `StatusSucceeded`, no result-code
9. `defer` cleanup: `os.Remove(scriptPath)`, `os.RemoveAll(attachmentDir)`.

---

## Output Handling

`OSExecutor.Run` sets `cmd.Stdout` and `cmd.Stderr` to a shared `cappedWriter`:

```go
type cappedWriter struct {
    buf       []byte
    limit     int
    truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
    if w.truncated {
        return len(p), nil
    }
    remaining := w.limit - len(w.buf)
    if len(p) >= remaining {
        w.buf = append(w.buf, p[:remaining]...)
        w.buf = append(w.buf, []byte("\n**OUTPUT TRUNCATED**")...)
        w.truncated = true
    } else {
        w.buf = append(w.buf, p...)
    }
    return len(p), nil
}
```

- Cap: **5 MB** (5 × 1024 × 1024 bytes).
- The script process continues running after truncation — only buffering stops.
- Final output decoded with `strings.ToValidUTF8(string(buf), "\ufffd")` to replace invalid UTF-8 sequences.

---

## Error Handling Summary

| Scenario | result-code | status |
|---|---|---|
| Missing/wrong-type required field | — (no result sent) | — |
| Interpreter binary not found | absent | Failed |
| Attachment fetch error | `104` | Failed |
| Script timeout | `102` | Failed |
| Script non-zero exit | `103` | Failed |
| Script exit 0 | absent | Succeeded |
| OS/temp file error | absent | Failed |

---

## Testing Strategy

Unit tests use injected fakes — no real processes or HTTP calls.

```go
type fakeExecutor struct {
    output   []byte
    exitCode int
    err      error
}

type fakeAttachmentFetcher struct {
    data map[int64][]byte
    err  error
}
```

### Test cases

1. **Happy path** — script succeeds, output returned in `result-text`, `status: 6`
2. **Non-zero exit** — `result-code: 103`, `status: 5`
3. **Timeout** — `result-code: 102`, `status: 5`
4. **Output truncated** — output > 5 MB capped with truncation marker
5. **Attachment fetched** — attachment dir path passed to executor, correct file written
6. **Attachment fetch failure** — `result-code: 104`, `status: 5`, cleanup runs
7. **Missing `interpreter` field** — `Handle` returns error, no result sent
8. **Interpreter binary not found** — `StatusFailed` result sent, descriptive text

---

## Registration in `main.go`

```go
handlers := []manager.Handler{
    // ... existing snap handlers ...
    &manager.ExecuteScriptHandler{
        Executor:    &manager.OSExecutor{},
        Fetcher:     transport.NewAttachmentFetcher(cfg, state),
        OutputLimit: 5 * 1024 * 1024,
    },
}
```

`transport.NewAttachmentFetcher` is a new constructor wrapping the existing transport client, using the secure-id for authenticated GETs to the attachment endpoint.

---

## Compatibility Notes

- Wire protocol is identical to the Python client; the server cannot distinguish client implementations.
- `username` field is received and ignored — the server always sends it; running as root (snap mode) is correct behaviour.
- Attachment IDs (not inline bytes) are used — this is the ≥11.11 client protocol; the Go client registers with `client-api: 3.3` which is well above that threshold.
