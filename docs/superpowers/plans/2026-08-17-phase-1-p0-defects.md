# Phase 1 — P0 Defects Implementation Plan

> **For agentic workers:** REQUIRED: Use the `subagent-driven-development` agent (recommended) or `executing-plans` agent to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the four defects that cause silent data corruption, unrecoverable crashes, unbounded script execution, and the death of every long-running remote operation.

**Architecture:** One seam commit (`run(ctx, deps) error` extracted from `main()`) so the dispatch path becomes testable, then four defect fixes and a fuzz target. Handler execution is decoupled from the exchange cycle; `bpickle` gains a depth cap; `network-device` reads kernel `IFF_*` flags from sysfs instead of sending Go's `net.Flags`; script execution gains a process-group kill and `WaitDelay`.

**Tech Stack:** Go 1.25, `os/exec`, `syscall`, `net`, stdlib `testing` + `testing.F`

**Spec:** [docs/superpowers/specs/2026-08-17-code-review-remediation-design.md](../specs/2026-08-17-code-review-remediation-design.md)

**Branch:** `fix/01-p0-defects`, cut from `fix/00-foundation` (or `main` once Phase 0 has merged)

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `cmd/landscape-client-core/main.go` | Modify | Extract `run(ctx, deps) error`; `main()` becomes wiring + exit code |
| `cmd/landscape-client-core/run_test.go` | Create | Cover the extracted `run()` |
| `internal/exchange/exchange.go` | Modify | Dispatch handlers on a daemon-lifetime context; delete `handlerEG` |
| `internal/exchange/dispatch_test.go` | Create | Regression test driving `manager.Runner` through the real dispatch path |
| `internal/bpickle/bpickle.go` | Modify | Thread a depth counter through `unmarshalValue` |
| `internal/bpickle/depth_test.go` | Create | Depth-limit tests |
| `internal/bpickle/fuzz_test.go` | Create | `FuzzUnmarshal` |
| `internal/transport/transport.go` | Modify | Lower `maxResponseBytes` |
| `internal/monitor/networkdevice.go` | Modify | Read kernel `IFF_*` from `<sysNetPath>/<iface>/flags` |
| `internal/monitor/sysinfo_test.go` | Modify | Flag fixture + assertions ported from `test_networkdevice.py` |
| `internal/manager/system.go` | Modify | `Setpgid`, `cmd.Cancel` group kill, `cmd.WaitDelay` |
| `internal/manager/system_test.go` | Modify | Time-limit enforcement tests |

---

## Task 0: Create the branch

- [ ] **Step 1: Cut the branch**

```bash
git checkout fix/00-foundation
git checkout -b fix/01-p0-defects
```

If Phase 0 has already merged, cut from `main` instead.

- [ ] **Step 2: Confirm a clean starting point**

Run: `go build ./... && go test -race ./...`
Expected: all packages `ok` or `no test files`.

---

## Task 1: Extract `run(ctx, deps) error` from `main()`

`main()` currently does config loading, logger setup, transport and snapd
construction, 15 plugins, 8 handlers, signal handling, the errgroup and a
hand-rolled shutdown — roughly 200 lines. `cmd/` is 3.7% covered as a result, and
the P0 defect in Task 2 lives in exactly this wiring.

This commit changes no behaviour.

**Files:**
- Modify: `cmd/landscape-client-core/main.go`
- Create: `cmd/landscape-client-core/run_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/landscape-client-core/run_test.go`:

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// TestRun_ShutsDownCleanly asserts run() returns without error when its context
// is cancelled, and that it does so promptly rather than burning the full
// shutdown grace period.
func TestRun_ShutsDownCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	snapCommon := t.TempDir()
	tc, err := transport.New(transport.Config{UserAgent: "test"})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	d := deps{
		cfg: &config.Config{
			URL:                    srv.URL,
			AccountName:            "acc",
			ComputerTitle:          "test",
			ExchangeInterval:       time.Hour,
			UrgentExchangeInterval: time.Hour,
			PingInterval:           time.Hour,
		},
		store:              persist.New(filepath.Join(snapCommon, "state.json")),
		transport:          tc,
		snapd:              &snapd.MockClient{},
		snapCommon:         snapCommon,
		handlerConcurrency: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, d) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of context cancellation")
	}
}
```

`snapd.MockClient` is the existing seam in `internal/snapd/mock.go` — a struct
with no constructor, so `&snapd.MockClient{}` is correct. Check its fields if the
test needs it to return specific data.

Likewise check the real field names on `config.Config` before running; use
whatever `internal/config/config.go` actually declares.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/landscape-client-core/ -run TestRun_ShutsDownCleanly -v`
Expected: FAIL — compile error, `undefined: deps` and `undefined: run`.

- [ ] **Step 3: Add the `deps` struct and `run` function**

In `cmd/landscape-client-core/main.go`, add above `main()`:

```go
// deps holds the constructed collaborators run() needs, so the daemon wiring
// can be exercised in tests without touching snapctl or the real snapd socket.
type deps struct {
	cfg                *config.Config
	store              *persist.Store
	transport          *transport.Client
	snapd              snapd.Client
	snapCommon         string
	handlerConcurrency int
}
```

Move the body of `main()` from the `// Create exchange.` comment through the end
of the function into:

```go
func run(ctx context.Context, d deps) error {
	...
}
```

Replace every use of the old local variables with the `d.` fields: `cfg` →
`d.cfg`, `store` → `d.store`, `tc` → `d.transport`, `snapdClient` → `d.snapd`,
`snapCommon` → `d.snapCommon`, `*handlerConcurrency` → `d.handlerConcurrency`.

Three changes to the moved code:

1. The signal context is now created by the caller, so delete the
   `signal.NotifyContext` lines from the moved body and derive the errgroup from
   the passed `ctx`:

```go
	eg, groupCtx := errgroup.WithContext(ctx)
```

   The shutdown `select` needs a cancel function for the group, so obtain one
   locally:

```go
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	eg, groupCtx := errgroup.WithContext(ctx)
```

2. Replace the `os.Exit(1)` paths in the moved code with returned errors.

3. Return `nil` at the end.

- [ ] **Step 4: Rewrite `main()` to build deps and call `run`**

`main()` keeps flag parsing, the `--validate-config` and `--sync-confdb` paths,
`SNAP_COMMON` resolution, config loading, logger setup, store, transport and
snapd construction, then:

```go
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, deps{
		cfg:                cfg,
		store:              store,
		transport:          tc,
		snapd:              snapdClient,
		snapCommon:         snapCommon,
		handlerConcurrency: *handlerConcurrency,
	}); err != nil {
		slog.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./cmd/landscape-client-core/ -run TestRun_ShutsDownCleanly -v`
Expected: PASS

- [ ] **Step 6: Verify nothing else broke**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add cmd/landscape-client-core/main.go cmd/landscape-client-core/run_test.go
git commit -m "refactor(cmd): extract run(ctx, deps) error from main()

main() did config, logging, 15 plugins, 8 handlers, signals and shutdown
inline, leaving cmd/ at 3.7% coverage and the daemon wiring untestable.
No behaviour change."
```

---

## Task 2: Dispatch inbound handlers on a daemon-lifetime context

**The defect.** `internal/exchange/exchange.go:401` creates
`errgroup.WithContext(ctx)` and line 511 calls `handlerEG.Wait()`.
`errgroup.WithContext` cancels its derived context **as soon as `Wait()`
returns**. But `manager.Runner.Register` (`internal/manager/runner.go:67-91`) is a
dispatcher — its `Subscribe` callback spawns a goroutine and returns immediately.
So `handlerCtx` is cancelled microseconds after dispatch, while the real work is
just starting.

Blast radius: every `execute-script`, `install-snaps`, `remove-snaps` and
`refresh-snaps` operation. The 10-minute `changeTimeout` and the per-operation
`time-limit` are both dead code in practice.

The existing tests miss it because `internal/manager/*_test.go` call
`handler.Handle(ctx, ...)` directly with a live context, bypassing dispatch.

**Files:**
- Modify: `internal/exchange/exchange.go`
- Create: `internal/exchange/dispatch_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/exchange/dispatch_test.go` (package `exchange`, so it can reuse
the existing `fakeServer` helper in `exchange_test.go`):

```go
package exchange

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/manager"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// TestDispatch_LongRunningHandlerSurvivesExchangeCycle drives a real
// manager.Runner through the real Subscribe/dispatch path — not by calling
// Handle directly, which is what let this bug ship. A script that takes longer
// than one exchange cycle must still complete.
func TestDispatch_LongRunningHandlerSurvivesExchangeCycle(t *testing.T) {
	snapCommon := t.TempDir()
	marker := filepath.Join(snapCommon, "marker")

	srv := &fakeServer{}
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
	exc := New(cfg, store, tc)

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
```

Check `manager.NewScriptExecHandler`'s actual signature before running — if it
requires a non-nil attachment fetcher, pass the same value
`internal/manager/system_test.go` uses.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/exchange/ -run TestDispatch_LongRunningHandlerSurvivesExchangeCycle -v`
Expected: FAIL with `script did not complete: marker file was never created...`

Note the server-visible symptom this corresponds to — the client reports
`result-code 103` with empty output for a script that never got to run.

- [ ] **Step 3: Add a dispatch context to `Exchange`**

In the `Exchange` struct, alongside the existing mutex-guarded fields:

```go
	// dispatchCtx is the daemon-lifetime context used to run inbound message
	// handlers. Handler lifetime must not be tied to the exchange cycle:
	// manager.Runner dispatches into a goroutine and returns immediately, so a
	// per-exchange context would be cancelled while the operation is still running.
	dispatchCtx context.Context
```

- [ ] **Step 4: Set it at the top of `Run`**

In `Run`, immediately after the `e.insecureID = state.InsecureID` block:

```go
	e.mu.Lock()
	e.insecureID = state.InsecureID
	e.dispatchCtx = ctx
	e.mu.Unlock()
```

- [ ] **Step 5: Replace the errgroup dispatch**

Replace:

```go
	handlerEG, handlerCtx := errgroup.WithContext(ctx)
```

with:

```go
	e.mu.Lock()
	handlerCtx := e.dispatchCtx
	e.mu.Unlock()
	if handlerCtx == nil {
		// performExchange called outside Run (tests, final drain).
		handlerCtx = context.Background()
	}
```

Replace the dispatch block:

```go
			for _, h := range handlers {
				h := h
				msg := msg
				handlerEG.Go(func() (handlerErr error) {
					defer func() {
						if rec := recover(); rec != nil {
							log.Printf("exchange: handler panic type=%q: %v\n%s", msgType, rec, debug.Stack())
							handlerErr = fmt.Errorf("handler panic for %q: %v", msgType, rec)
						}
					}()
					h(handlerCtx, msg)
					if err := handlerCtx.Err(); err != nil {
						handlerErr = fmt.Errorf("handler for %q stopped: %w", msgType, err)
					}
					return handlerErr
				})
			}
```

with:

```go
			for _, h := range handlers {
				h := h
				msg := msg
				go func() {
					defer func() {
						if rec := recover(); rec != nil {
							log.Printf("exchange: handler panic type=%q: %v\n%s", msgType, rec, debug.Stack())
						}
					}()
					h(handlerCtx, msg)
				}()
			}
```

The `recover` stays: subscribers registered directly on `Exchange` (the
`set-intervals` closure in `run()`) have no other recovery. `manager.Runner` has
its own.

- [ ] **Step 6: Delete the wait block**

Remove entirely:

```go
	if err := handlerEG.Wait(); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("exchange: handler group canceled: %v", err)
		} else {
			log.Printf("exchange: handler group error: %v", err)
		}
		return fmt.Errorf("exchange: waiting for handlers: %w", err)
	}
```

An exchange must not fail because a handler is still running.

- [ ] **Step 7: Drop the now-unused import**

`golang.org/x/sync/errgroup` was used only at the deleted line 401. Remove it from
the import block. Leave `errors` if it is still referenced elsewhere in the file.

Run: `go build ./internal/exchange/`
Expected: no output. If it reports an unused import, remove that one too.

- [ ] **Step 8: Run the test to verify it passes**

Run: `go test ./internal/exchange/ -run TestDispatch_LongRunningHandlerSurvivesExchangeCycle -v`
Expected: PASS

- [ ] **Step 9: Verify shutdown still drains handlers**

`run()` already waits via `mgRunner.WaitWithTimeout(5 * time.Second)`, which is
now the only thing bounding in-flight handlers at shutdown. Confirm it is still
called.

Run: `grep -n 'WaitWithTimeout' cmd/landscape-client-core/main.go`
Expected: one match inside `run`.

- [ ] **Step 10: Full suite**

Run: `go test -race ./...`
Expected: clean.

- [ ] **Step 11: Commit**

```bash
git add internal/exchange/exchange.go internal/exchange/dispatch_test.go
git commit -m "fix(exchange): dispatch inbound handlers on a daemon-lifetime context

errgroup.WithContext cancels its context as soon as Wait() returns, and
manager.Runner returns immediately after spawning its goroutine, so every
handler context died microseconds after dispatch. Every execute-script and
snap operation was killed instantly and reported to the server as
result-code 103 with empty output.

Handlers now run on the context owned by Run, and performExchange no longer
waits for them. Shutdown still drains via mgRunner.WaitWithTimeout.

The regression test drives manager.Runner through the real Subscribe path
rather than calling Handle directly, which is what let this ship."
```

---

## Task 3: Cap bpickle decode nesting depth

`unmarshalList`, `unmarshalTuple` and `unmarshalDict` recurse per nesting level
with no depth cap, and `transport.maxResponseBytes` allows 32 MiB. A response of
roughly 20 MiB of `'l'` bytes exhausts the 1 GB goroutine stack limit:

```
runtime: goroutine stack exceeds 1000000000-byte limit
fatal error: stack overflow
```

A stack overflow is a **fatal runtime error** — `recover()` cannot catch it, so the
panic recovery in the exchange and monitor runners is no help. The daemon dies, and
`restart-condition: on-failure` turns a persistent hostile response into a crash
loop.

Reachability does not require a TLS compromise: the ping URL is derived as plain
`http://` in `internal/config/config.go:36-45` and its response is fed straight to
`bpickle.Unmarshal` at `internal/ping/ping.go:111`.

**Files:**
- Modify: `internal/bpickle/bpickle.go`
- Modify: `internal/transport/transport.go`
- Create: `internal/bpickle/depth_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/bpickle/depth_test.go`:

```go
package bpickle

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnmarshal_RejectsExcessiveNesting(t *testing.T) {
	// 10,000 nested lists: 'l' repeated, then the matching ';' terminators.
	depth := 10000
	data := append(bytes.Repeat([]byte{'l'}, depth), bytes.Repeat([]byte{';'}, depth)...)

	_, err := Unmarshal(data)
	if err == nil {
		t.Fatal("expected an error for deeply nested input, got nil")
	}
	if !strings.Contains(err.Error(), "nesting") {
		t.Errorf("error should mention nesting depth, got: %v", err)
	}
}

func TestUnmarshal_AcceptsReasonableNesting(t *testing.T) {
	// The protocol never nests deeply; 10 levels must still decode.
	depth := 10
	data := append(bytes.Repeat([]byte{'l'}, depth), bytes.Repeat([]byte{';'}, depth)...)

	if _, err := Unmarshal(data); err != nil {
		t.Fatalf("depth %d should decode, got: %v", depth, err)
	}
}

func TestUnmarshal_DepthAppliesToDictsAndTuples(t *testing.T) {
	tests := []struct {
		name   string
		marker byte
	}{
		{"dict", 'd'},
		{"tuple", 't'},
		{"list", 'l'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			depth := 10000
			data := append(bytes.Repeat([]byte{tt.marker}, depth), bytes.Repeat([]byte{';'}, depth)...)
			if _, err := Unmarshal(data); err == nil {
				t.Fatalf("%s: expected an error for %d levels of nesting", tt.name, depth)
			}
		})
	}
}
```

The dict case terminates on the depth check rather than on a key-type error,
because `unmarshalDict` recurses into `unmarshalValue` for the key before it
inspects the type.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/bpickle/ -run TestUnmarshal_Rejects -v`
Expected: FAIL — either no error returned, or the process dies with
`fatal error: stack overflow`. Both count as a failing test here.

If the runner crashes rather than failing cleanly, that *is* the defect; proceed.

- [ ] **Step 3: Thread a depth counter through the decoder**

In `internal/bpickle/bpickle.go`, add near the other constants:

```go
// maxNestingDepth bounds decoder recursion. The protocol nests only a few
// levels; the cap exists so a hostile response cannot exhaust the goroutine
// stack, which is a fatal runtime error that recover() cannot catch.
const maxNestingDepth = 100
```

Change the signature and add the check:

```go
func unmarshalValue(data []byte, pos, depth int) (any, int, error) {
	if pos >= len(data) {
		return nil, pos, fmt.Errorf("bpickle: unexpected end of data at position %d", pos)
	}
	if depth > maxNestingDepth {
		return nil, pos, fmt.Errorf("bpickle: maximum nesting depth %d exceeded at position %d", maxNestingDepth, pos)
	}
	switch data[pos] {
	case 'n':
		return nil, pos + 1, nil
	case 'b':
		return unmarshalBool(data, pos)
	case 'i':
		return unmarshalInt(data, pos)
	case 'f':
		return unmarshalFloat(data, pos)
	case 's':
		return unmarshalBytes(data, pos)
	case 'u':
		return unmarshalUnicode(data, pos)
	case 'l':
		return unmarshalList(data, pos, depth)
	case 't':
		return unmarshalTuple(data, pos, depth)
	case 'd':
		return unmarshalDict(data, pos, depth)
	default:
		return nil, pos, fmt.Errorf("bpickle: unknown type marker %q at position %d", data[pos], pos)
	}
}
```

Update the three container functions to take `depth` and pass `depth+1`:

```go
func unmarshalList(data []byte, pos, depth int) (any, int, error) {
	pos++ // consume 'l'
	result := make([]any, 0)
	for {
		if pos >= len(data) {
			return nil, pos, fmt.Errorf("bpickle: unterminated list")
		}
		if data[pos] == ';' {
			return result, pos + 1, nil
		}
		val, newPos, err := unmarshalValue(data, pos, depth+1)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos
		result = append(result, val)
	}
}
```

Apply the same change to `unmarshalTuple`. In `unmarshalDict`, both the key and
the value calls take `depth+1`.

- [ ] **Step 4: Update the entry point**

Find the caller of `unmarshalValue` in `Unmarshal` and pass an initial depth of 0.

Run: `go build ./internal/bpickle/`
Expected: no output. Any remaining compile error names a call site that still
passes two arguments — fix it.

- [ ] **Step 5: Lower the response size cap**

In `internal/transport/transport.go`:

```go
	// maxResponseBytes caps successful response bodies to prevent a
	// misbehaving or compromised server from exhausting process memory.
	// 32 MiB was far above any legitimate payload; 4 MiB also bounds the
	// input the bpickle decoder can be asked to walk.
	maxResponseBytes = 4 * 1024 * 1024 // 4 MiB
```

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/bpickle/ ./internal/transport/ -v`
Expected: PASS, including the existing round-trip tests.

- [ ] **Step 7: Verify wire compatibility is unaffected**

Run: `go test -tags compat ./internal/bpickle/...`
Expected: PASS, or a skip if `LANDSCAPE_CLIENT_PATH` is unset. If the Python
reference is available locally, set it:

```bash
LANDSCAPE_CLIENT_PATH=/path/to/landscape-client go test -tags compat -v ./internal/bpickle/...
```

- [ ] **Step 8: Commit**

```bash
git add internal/bpickle/bpickle.go internal/bpickle/depth_test.go internal/transport/transport.go
git commit -m "fix(bpickle): cap decode nesting depth and lower max response size

The list/tuple/dict decoders recursed with no depth cap, so ~20 MiB of 'l'
bytes exhausted the 1 GB goroutine stack. A stack overflow is fatal and
recover() cannot catch it, so the runners' panic recovery was no defence and
restart-condition turned a hostile response into a crash loop.

Reachable without a TLS compromise: the ping URL is plain http:// and its
response is fed straight to Unmarshal."
```

---

## Task 4: Add `FuzzUnmarshal`

`bpickle` parses untrusted network input and had no fuzzing. The Task 3 defect
would have surfaced in seconds.

**Files:**
- Create: `internal/bpickle/fuzz_test.go`

- [ ] **Step 1: Write the fuzz target**

Create `internal/bpickle/fuzz_test.go`:

```go
package bpickle

import (
	"bytes"
	"testing"
)

// FuzzUnmarshal asserts the decoder never panics and never loops forever on
// arbitrary input. It parses untrusted server responses, including from the
// plain-http ping endpoint.
func FuzzUnmarshal(f *testing.F) {
	f.Add([]byte("i42;"))
	f.Add([]byte("u5:hello"))
	f.Add([]byte("s5:hello"))
	f.Add([]byte("b1"))
	f.Add([]byte("f3.14;"))
	f.Add([]byte("n"))
	f.Add([]byte("li1;i2;i3;;"))
	f.Add([]byte("ti1;i2;;"))
	f.Add([]byte("du4:types li1;;;"))
	f.Add([]byte("du4:types lu3:foou3:bar;;"))
	f.Add(append(bytes.Repeat([]byte{'l'}, 200), bytes.Repeat([]byte{';'}, 200)...))
	f.Add([]byte("l"))
	f.Add([]byte("s99:short"))
	f.Add([]byte("i;"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Only requirement: return a value or an error, never panic.
		_, _ = Unmarshal(data)
	})
}
```

- [ ] **Step 2: Run the seed corpus**

Run: `go test ./internal/bpickle/ -run FuzzUnmarshal -v`
Expected: PASS (seed corpus only).

- [ ] **Step 3: Fuzz for 60 seconds**

Run: `go test ./internal/bpickle/ -fuzz FuzzUnmarshal -fuzztime 60s`
Expected: no failures. If a crasher is found, Go writes it to
`internal/bpickle/testdata/fuzz/FuzzUnmarshal/`. **Commit the crasher, fix the
decoder, and note both in the commit message** — a reproducer is more valuable
than a clean run.

- [ ] **Step 4: Commit**

```bash
git add internal/bpickle/fuzz_test.go internal/bpickle/testdata 2>/dev/null || git add internal/bpickle/fuzz_test.go
git commit -m "test(bpickle): add FuzzUnmarshal

bpickle parses untrusted network input and had no fuzz coverage; the
unbounded-recursion defect fixed in the previous commit would have been
found in seconds."
```

---

## Task 5: Send kernel `IFF_*` flags in `network-device`

`internal/monitor/networkdevice.go:113` does `flags := int(iface.Flags)` and puts
that on the wire. Go's `net.Flags` is its own sequential bitmask
(`FlagUp=1<<0`, `FlagBroadcast=1<<1`, `FlagLoopback=1<<2`, `FlagPointToPoint=1<<3`,
`FlagMulticast=1<<4`, `FlagRunning=1<<5`). The Python client sends the raw
`SIOCGIFFLAGS` value, where `IFF_LOOPBACK=8`, `IFF_RUNNING=64`,
`IFF_MULTICAST=4096`, and the server interprets the field as Linux `IFF_*`.

Measured on a live host:

| Interface | Go `net.Flags` | Kernel `IFF_*` | LOOPBACK (`&8`) | RUNNING (`&64`) |
|---|---|---|---|---|
| `lo` | 37 | 9 | Go 4 → false | Go 32 → false |
| `wlp0s20f3` | 51 | 4099 | — | Go 32 → false |

Only bit 0 (UP) coincides. Nothing errors; the server is simply told the wrong
thing.

**Files:**
- Modify: `internal/monitor/networkdevice.go`
- Modify: `internal/monitor/sysinfo_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/monitor/sysinfo_test.go`, after the existing `NetworkDevice`
tests. This ports the assertions in `landscape/client/monitor/tests/test_networkdevice.py:36-38`:

```go
// TestNetworkDevice_SendsKernelIFFFlags asserts the wire value is the kernel's
// IFF_* bitmask, not Go's net.Flags. Ported from the Python client's
// test_networkdevice.py, which asserts flags&1 (UP), flags&8 (LOOPBACK) and
// flags&64 (RUNNING).
func TestNetworkDevice_SendsKernelIFFFlags(t *testing.T) {
	dir := t.TempDir()
	p, _ := makeNetMock(t, dir)

	// 0x1043 = IFF_UP|IFF_BROADCAST|IFF_RUNNING|IFF_MULTICAST = 1|2|64|4096.
	writeFixture(t, filepath.Join(dir, "eth0", "flags"), "0x1043\n")

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	devices := msgs[0]["devices"].([]map[string]any)
	flags, ok := devices[0]["flags"].(int)
	if !ok {
		t.Fatalf("flags: want int, got %T", devices[0]["flags"])
	}

	if flags != 0x1043 {
		t.Errorf("flags: want 4163 (0x1043), got %d", flags)
	}
	if flags&1 == 0 {
		t.Error("IFF_UP (&1) not set")
	}
	if flags&64 == 0 {
		t.Error("IFF_RUNNING (&64) not set — this is the bit Go's net.Flags never sets correctly")
	}
	if flags&4096 == 0 {
		t.Error("IFF_MULTICAST (&4096) not set")
	}
	if flags&8 != 0 {
		t.Error("IFF_LOOPBACK (&8) set for a non-loopback interface")
	}
}

// TestNetworkDevice_FlagsFallback asserts that when sysfs has no flags file the
// interface is still reported, with net.Flags translated into IFF_* positions,
// rather than dropped from inventory.
func TestNetworkDevice_FlagsFallback(t *testing.T) {
	dir := t.TempDir()
	p, _ := makeNetMock(t, dir) // makeNetMock writes no flags file

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	<-errCh

	devices := msgs[0]["devices"].([]map[string]any)
	if len(devices) != 1 {
		t.Fatalf("device dropped when flags file is missing: got %d devices", len(devices))
	}
	// makeNetMock's interface is net.FlagUp|net.FlagBroadcast.
	flags := devices[0]["flags"].(int)
	if flags&1 == 0 {
		t.Error("IFF_UP (&1) not set in fallback translation")
	}
	if flags&2 == 0 {
		t.Error("IFF_BROADCAST (&2) not set in fallback translation")
	}
}
```

Check the actual key name for the flags field in the message built at
`internal/monitor/networkdevice.go:145` onward, and use it — the assertions above
assume `"flags"`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestNetworkDevice_SendsKernelIFFFlags -v`
Expected: FAIL with `flags: want 4163 (0x1043), got 3` — Go reports
`FlagUp|FlagBroadcast` as 3.

- [ ] **Step 3: Add the sysfs flag reader**

In `internal/monitor/networkdevice.go`, add:

```go
// Linux IFF_* values from <linux/if.h>. Go's net.Flags uses a different,
// sequential bitmask; the Landscape server interprets this field as IFF_*.
const (
	iffUp           = 0x1
	iffBroadcast    = 0x2
	iffLoopback     = 0x8
	iffPointToPoint = 0x10
	iffRunning      = 0x40
	iffMulticast    = 0x1000
)

// readFlags returns the kernel IFF_* bitmask for iface, read from
// /sys/class/net/<name>/flags. When sysfs is unavailable — no such entry, or an
// AppArmor denial under strict confinement — it falls back to translating Go's
// net.Flags into IFF_* positions, so the interface is still reported rather than
// dropped from the server's inventory.
func (p *NetworkDevice) readFlags(iface *net.Interface) int {
	data, err := os.ReadFile(filepath.Join(p.sysNetPath, iface.Name, "flags"))
	if err == nil {
		s := strings.TrimSpace(string(data))
		s = strings.TrimPrefix(s, "0x")
		if v, err := strconv.ParseInt(s, 16, 64); err == nil {
			return int(v)
		}
	}
	return translateGoFlags(iface.Flags)
}

func translateGoFlags(f net.Flags) int {
	var out int
	if f&net.FlagUp != 0 {
		out |= iffUp
	}
	if f&net.FlagBroadcast != 0 {
		out |= iffBroadcast
	}
	if f&net.FlagLoopback != 0 {
		out |= iffLoopback
	}
	if f&net.FlagPointToPoint != 0 {
		out |= iffPointToPoint
	}
	if f&net.FlagRunning != 0 {
		out |= iffRunning
	}
	if f&net.FlagMulticast != 0 {
		out |= iffMulticast
	}
	return out
}
```

Add `"path/filepath"` to the imports if it is not already present.

- [ ] **Step 4: Use it**

Replace:

```go
		flags := int(iface.Flags)
```

with:

```go
		flags := p.readFlags(iface)
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/monitor/ -run TestNetworkDevice -v`
Expected: PASS for all `NetworkDevice` tests, including the pre-existing
`TestNetworkDevice_HappyPath`.

- [ ] **Step 6: Sanity-check against the live host**

Run:

```bash
for i in /sys/class/net/*; do echo "$(basename "$i"): $(cat "$i"/flags 2>/dev/null)"; done
```

Expected: hex values such as `0x1043` for an active NIC and `0x9` for `lo`.
Confirm `lo` has bit 8 set — that is the LOOPBACK bit Go never reported.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/networkdevice.go internal/monitor/sysinfo_test.go
git commit -m "fix(monitor): report kernel IFF_* flags in network-device

The plugin sent int(iface.Flags) — Go's own sequential bitmask — where the
server expects the Linux IFF_* value the Python client sends. Only bit 0 (UP)
coincided: loopback interfaces were never reported as loopback and no
interface was ever reported as RUNNING. Silent data corruption; nothing
errored.

Reads /sys/class/net/<iface>/flags, falling back to a translation of
net.Flags so an interface is never dropped from inventory.

Test ports the assertions in the Python client's test_networkdevice.py,
which asserts flags&1, flags&8 and flags&64 directly."
```

---

## Task 6: Enforce `time-limit` with a process-group kill

`exec.CommandContext` kills only the direct child. Because `cmd.Stdout` and
`cmd.Stderr` are an `io.Writer` (`limitWriter`) rather than an `*os.File`,
`os/exec` creates a pipe and copies it in a goroutine — and `cmd.Run()` cannot
return until **every** process holding the write end has exited. A killed shell's
`sleep` inherits that pipe and keeps `Wait()` blocked:

| Script | `time-limit` | Actual duration |
|---|---|---|
| `sleep 10` | 1s | 10.0s |
| `sleep 20 & echo done` | 1s | 20.0s |

`cmd.WaitDelay` is never set and `SysProcAttr.Setpgid` is never set, so
grandchildren also survive indefinitely as orphans holding the snap's descriptors.

Python handles this explicitly, with a comment describing this exact failure
(`landscape/client/manager/scriptexecution.py:412-426`) — so this is lost
production hardening, not a parity question. It matters beyond one script:
`internal/manager/runner.go:68` bounds handlers with a semaphore, so stuck
operations eventually block all manager work.

**Files:**
- Modify: `internal/manager/system.go`
- Modify: `internal/manager/system_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/manager/system_test.go`:

```go
// TestScriptExec_TimeLimitKillsProcessGroup asserts a script that spawns a
// background child cannot outlive its time-limit. exec.CommandContext alone
// kills only the direct child, and the surviving child holds the stdout pipe
// open, which blocks cmd.Run well past the deadline.
func TestScriptExec_TimeLimitKillsProcessGroup(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(1),
		"code":         "sleep 30 & echo started\n",
		"interpreter":  "/bin/sh",
		"time-limit":   int64(1),
	}

	start := time.Now()
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("time-limit not enforced: Handle took %v for a 1s limit", elapsed)
	}

	results := sink.results()
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].resultCode != 102 {
		t.Errorf("result-code: want 102 (timeout), got %d", results[0].resultCode)
	}
}

// TestScriptExec_TimeLimitPreservesPartialOutput guards behaviour that is
// already correct and must not regress.
func TestScriptExec_TimeLimitPreservesPartialOutput(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(2),
		"code":         "echo before-timeout; sleep 30\n",
		"interpreter":  "/bin/sh",
		"time-limit":   int64(1),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	results := sink.results()
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].resultCode != 102 {
		t.Errorf("result-code: want 102, got %d", results[0].resultCode)
	}
	if !strings.Contains(results[0].output, "before-timeout") {
		t.Errorf("partial output lost: got %q", results[0].output)
	}
}
```

Match `mockResultSink`'s actual accessor and field names — check the existing
definition in `internal/manager/system_test.go` and adapt the assertions rather
than adding a second mock.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/manager/ -run TestScriptExec_TimeLimitKillsProcessGroup -v`
Expected: FAIL with `time-limit not enforced: Handle took 30.0...s for a 1s limit`.

This test takes ~30s while failing. That is the point.

- [ ] **Step 3: Set up the process group and cancellation**

In `internal/manager/system.go`, replace:

```go
	cmd := exec.CommandContext(execCtx, interpreterBin, append(interpreterArgs, scriptPath)...)
	if len(cmdEnv) > 0 {
		cmd.Env = cmdEnv
	}
```

with:

```go
	cmd := exec.CommandContext(execCtx, interpreterBin, append(interpreterArgs, scriptPath)...)
	// Run the script in its own process group so a timeout kills grandchildren
	// too. Without this they survive as orphans holding the stdout pipe, which
	// blocks cmd.Wait indefinitely — the pipe exists because Stdout is an
	// io.Writer rather than an *os.File.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Bound Wait even if something still holds the pipe open.
	cmd.WaitDelay = 5 * time.Second
	if len(cmdEnv) > 0 {
		cmd.Env = cmdEnv
	}
```

Add `"syscall"` to the import block.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/manager/ -run TestScriptExec_TimeLimit -v`
Expected: PASS, both tests, in roughly 6–12s total rather than 60s.

- [ ] **Step 5: Confirm no orphan survives**

Run:

```bash
go test ./internal/manager/ -run TestScriptExec_TimeLimitKillsProcessGroup
pgrep -af 'sleep 30' || echo "no orphans"
```

Expected: `no orphans`.

- [ ] **Step 6: Verify the success path is unaffected**

Run: `go test -race ./internal/manager/ -v`
Expected: PASS for the whole package, including the existing result-code 102/103/104
and path-traversal tests.

- [ ] **Step 7: Commit**

```bash
git add internal/manager/system.go internal/manager/system_test.go
git commit -m "fix(manager): enforce script time-limit with a process-group kill

exec.CommandContext kills only the direct child, and because Stdout is an
io.Writer the child's pipe keeps cmd.Run blocked until every holder exits.
Measured: 'sleep 20 & echo done' ran 20s against a 1s limit, and orphaned
grandchildren survived holding the snap's descriptors.

Sets Setpgid, signals the whole group on cancellation, and sets WaitDelay so
Wait returns even if the pipe is still held. Python handles this explicitly,
with a comment describing this exact failure mode.

Matters beyond one script: manager.Runner bounds handlers with a semaphore,
so stuck operations eventually block all manager work."
```

---

## Task 7: Verify the phase

- [ ] **Step 1: Full verification**

Run:

```bash
gofmt -l .
go vet ./...
go test -race ./...
golangci-lint run
```

Expected: all clean.

- [ ] **Step 2: Confirm each P0 test fails against the baseline**

For each of the four regression tests, check out the pre-phase commit into a
worktree, copy the test file in, and confirm it fails:

```bash
git worktree add /tmp/lcc-baseline a1cfeae
```

Copy `internal/exchange/dispatch_test.go`, `internal/bpickle/depth_test.go`, the
two `NetworkDevice` flag tests and the two `TimeLimit` tests into the worktree and
run them. Each must fail. A regression test that passes against the baseline is
not testing the defect.

```bash
git worktree remove /tmp/lcc-baseline
```

- [ ] **Step 3: Push and open the PR**

```bash
git push -u origin fix/01-p0-defects
```

PR title: `Phase 1: P0 defects — handler lifetime, bpickle recursion, network flags, time-limit`

---

## Done when

- A long-running `execute-script` dispatched through `manager.Runner` completes instead of being killed on dispatch.
- Deeply nested bpickle input returns an error instead of crashing the process.
- `network-device` reports kernel `IFF_*` values, verified against `flags&1`, `&8`, `&64` and `&4096`.
- A script spawning a background child terminates within its `time-limit` plus `WaitDelay`, leaving no orphans.
- `FuzzUnmarshal` exists and survives 60s of fuzzing.
- All four regression tests fail against `a1cfeae` and pass on this branch.
