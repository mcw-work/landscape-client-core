# Phase 5 - Runtime Boundaries Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound inbound dispatch concurrency, make snapd change polling match the real API, reuse process-lifetime timers, and close the remaining `clkTck` documentation gap.

**Architecture:** Exchange invokes subscriber callbacks synchronously with panic containment; manager callbacks remain bounded asynchronous dispatchers and the direct interval callback returns promptly. snapd retains its supported 500 ms status polling but reuses one timer and treats `ready`/`err` according to the actual API. Ping reuses one resettable timer without changing interval-update semantics.

**Tech Stack:** Go 1.25, `context`, channels, `sync`, `time`, Unix-socket HTTP

**Spec:** [docs/superpowers/specs/2026-08-28-code-review-follow-up-design.md](../specs/2026-08-28-code-review-follow-up-design.md)

**Branch:** `fix/05-runtime-boundaries`, cut from `fix/04-p2-hygiene`

---

## Constraints

- Do not tie handler lifetime to an individual exchange request.
- Do not silently drop an inbound operation when dispatch is saturated.
- Do not expose dispatcher sizing as user configuration in this phase.
- Do not wait for all handlers before completing an exchange.
- Preserve panic containment and daemon-lifetime cancellation.
- Keep the 500 ms delay after a non-terminal snapd response to bound local polling load.
- Keep `Pinger.SetInterval` semantics: a new value affects the next arm, not the timer already waiting.

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/exchange/exchange.go` | Modify | Invoke subscriber callbacks without unbounded goroutines |
| `internal/exchange/exchange_test.go` | Modify | Prove sequential handoff and panic containment |
| `internal/snapd/snapd.go` | Modify | Decode real change completion/error fields and reuse retry timer |
| `internal/snapd/snapd_test.go` | Modify | Assert retries, terminal states and cancellation |
| `internal/ping/ping.go` | Modify | Reuse one timer in process-lifetime loop |
| `internal/ping/ping_test.go` | Modify | Guard interval and cancellation behavior |
| `internal/monitor/activeprocessinfo.go` | Modify | Correct `USER_HZ` comment without changing value |
| `docs/concurrency.md` | Modify | Record dispatcher ownership and overload semantics |

## Task 0: Create the branch and record the baseline

- [ ] **Step 1: Verify the parent branch is clean**

Run:

```bash
git switch fix/04-p2-hygiene
git status --short
git rev-parse --short HEAD
```

Expected: no tracked changes and HEAD `b3be298`. The untracked August review and
follow-up planning documents may be present; do not delete or stage the review.

- [ ] **Step 2: Create the branch**

```bash
git switch -c fix/05-runtime-boundaries
```

- [ ] **Step 3: Run focused baselines**

```bash
go test -race ./internal/exchange ./internal/snapd ./internal/ping
rg -n 'go func\(\)' internal/exchange/exchange.go
rg -n 'time\.After\(' internal/snapd/snapd.go internal/ping/ping.go
```

Expected: tests pass; one unbounded exchange handler spawn is visible; one looped
`time.After` is visible in each of snapd and ping.

## Task 1: Remove unbounded exchange callback goroutines

The manager subscriptions already acquire their own semaphore, add the operation
to a wait group, spawn bounded work, and return. The only direct callback updates
an atomic ping interval. Invoke these dispatch callbacks synchronously so the
exchange creates no goroutine per server message and records sequence progress
only after each callback has accepted the work.

**Files:**
- Modify: `internal/exchange/exchange.go`
- Modify: `internal/exchange/exchange_test.go`
- Modify: `docs/concurrency.md`

- [ ] **Step 1: Write a fail-first sequential-handoff test**

Replace `TestPerformExchange_DoesNotWaitForHandlers` with:

```go
func TestPerformExchange_InvokesSubscribersSequentially(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	ts.fs.push(map[string]any{"messages": []any{
		map[string]any{"type": "fanout", "id": int64(1)},
		map[string]any{"type": "fanout", "id": int64(2)},
	}})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	ts.ex.Subscribe("fanout", func(_ context.Context, msg Message) {
		if toInt64(msg["id"]) == 1 {
			close(firstStarted)
			<-releaseFirst
			return
		}
		close(secondStarted)
	})

	done := make(chan error, 1)
	go func() { done <- ts.ex.performExchange(context.Background(), state) }()
	<-firstStarted
	select {
	case <-secondStarted:
		t.Fatal("second subscriber ran before first returned")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second subscriber was not invoked")
	}
	if err := <-done; err != nil {
		t.Fatalf("performExchange: %v", err)
	}
}
```

- [ ] **Step 2: Verify the test fails**

```bash
go test ./internal/exchange -run TestPerformExchange_InvokesSubscribersSequentially -count=10
```

Expected before implementation: FAIL because both goroutines may start while the
first callback is blocked.

- [ ] **Step 3: Extract panic-contained synchronous invocation**

Add this method beside `Subscribe`:

```go
func (e *Exchange) invokeHandler(ctx context.Context, messageType string, handler func(context.Context, Message), msg Message) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("exchange: handler panicked", "type", messageType, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	handler(ctx, msg)
}
```

Replace the per-handler `go func()` in `performExchange` with:

```go
			for _, handler := range handlers {
				e.invokeHandler(handlerCtx, msgType, handler, msg)
			}
```

Keep `dispatchCtx` daemon-scoped. Synchronous invocation is only the callback
handoff; manager operations still execute concurrently under `manager.Runner`'s
semaphore and wait group.

- [ ] **Step 4: Update existing dispatch tests**

Keep `TestPerformExchange_HandlerPanicIsContained`, but make its assertion
synchronous: call `performExchange`, then assert it returned nil and the panic log
was emitted. Replace `TestPerformExchange_HandlerDispatchDoesNotFailExchange`
with an explicit daemon-context test:

```go
func TestPerformExchange_HandlerContextUsesDaemonLifetime(t *testing.T) {
	ts := newTestSetup(t)
	state := ts.freshState(t)
	type contextKey string
	const (
		daemonKey  contextKey = "daemon"
		requestKey contextKey = "request"
	)
	daemonCtx := context.WithValue(context.Background(), daemonKey, "alive")
	ts.ex.dispatchCtx = daemonCtx

	received := make(chan context.Context, 1)
	ts.ex.Subscribe("context", func(ctx context.Context, _ Message) { received <- ctx })
	ts.fs.push(map[string]any{"messages": []any{map[string]any{"type": "context"}}})

	requestCtx, cancelRequest := context.WithCancel(
		context.WithValue(context.Background(), requestKey, "request-only"),
	)
	if err := ts.ex.performExchange(requestCtx, state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}
	handlerCtx := <-received
	cancelRequest()
	if got := handlerCtx.Value(daemonKey); got != "alive" {
		t.Fatalf("daemon context value = %v, want alive", got)
	}
	if got := handlerCtx.Value(requestKey); got != nil {
		t.Fatalf("handler inherited request context value %v", got)
	}
	if err := handlerCtx.Err(); err != nil {
		t.Fatalf("handler context ended with request: %v", err)
	}
}
```

- [ ] **Step 5: Run focused validation**

```bash
gofmt -w internal/exchange/exchange.go internal/exchange/exchange_test.go
go test -race ./internal/exchange -count=3
go test ./internal/exchange -run 'TestPerformExchange_(InvokesSubscribersSequentially|HandlerPanicIsContained|HandlerContextUsesDaemonLifetime)' -count=20
```

Expected: all pass and `performExchange` contains no per-message goroutine spawn.

- [ ] **Step 6: Document the callback contract**

Update `docs/concurrency.md`:

```markdown
## Inbound dispatch ownership

Exchange invokes subscriber callbacks synchronously and in server-message order.
Callbacks must either complete promptly or hand work to a bounded owner. Manager
callbacks hand operations to `Runner`, whose semaphore bounds concurrency and
whose wait group owns shutdown. This keeps sequence persistence after callback
handoff and avoids a second queue with ambiguous shutdown semantics.
```

- [ ] **Step 7: Commit**

```bash
git add internal/exchange/exchange.go internal/exchange/exchange_test.go docs/concurrency.md
git commit -m "fix(exchange): bound inbound callback dispatch"
```

## Task 2: Correct snapd change polling

The review recommends `/v2/changes/<id>?timeout=`, but the official snapd endpoint
has no query parameters. `/v2/notices` supports long polling, but adopting it
would require version negotiation and notice/change correlation for an infrequent
management path. Keep status polling, remove timer churn, and fix the response
model to match snapd's `ready` boolean and string `err`.

**Files:**
- Modify: `internal/snapd/snapd.go`
- Modify: `internal/snapd/snapd_test.go`

- [ ] **Step 1: Correct the fail-first error fixture**

In `internal/snapd/snapd_test.go`, add:

```go
func TestWaitForChange_ErrorString(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(syncResponse(map[string]any{
			"status": "Error", "ready": true, "err": "something went wrong",
		}))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	err := client.WaitForChange(context.Background(), "change-bad")
	if err == nil || !strings.Contains(err.Error(), "something went wrong") {
		t.Fatalf("error = %v, want snapd error text", err)
	}
}
```

- [ ] **Step 2: Verify the test fails**

```bash
go test ./internal/snapd -run TestWaitForChange_ErrorString -count=1
```

Expected: FAIL because the current `err` field expects an object, not a string.

- [ ] **Step 3: Match the documented snapd response**

Replace `changeResult` with:

```go
type changeResult struct {
	Status string `json:"status"`
	Ready  bool   `json:"ready"`
	Err    string `json:"err"`
}

const changeRetryDelay = 500 * time.Millisecond
```

At the start of `WaitForChange`, create one stopped timer:

```go
	retryTimer := time.NewTimer(changeRetryDelay)
	if !retryTimer.Stop() {
		<-retryTimer.C
	}
	defer retryTimer.Stop()
```

After decoding each response, replace the status switch with:

```go
		if cr.Ready {
			if cr.Status == "Done" && cr.Err == "" {
				return nil
			}
			if cr.Err != "" {
				return fmt.Errorf("snapd: change %s failed: %s", changeID, cr.Err)
			}
			return fmt.Errorf("snapd: change %s completed with status %s", changeID, cr.Status)
		}
```

Then replace `time.After` with:

```go
		retryTimer.Reset(changeRetryDelay)
		select {
		case <-ctx.Done():
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			return ctx.Err()
		case <-retryTimer.C:
		}
```

Do not append an unsupported timeout query. Keep the retry delay to avoid loading
the local daemon while a change is in progress.

- [ ] **Step 4: Strengthen existing state tests**

Update `TestWaitForChange_Done` fixtures to include `"ready": false` while Doing
and `"ready": true` when Done. Replace the old map-shaped `err` fixture with the
string fixture from Step 1.

Add a ready non-success test:

```go
func TestWaitForChange_ReadyNonSuccessIsError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(syncResponse(map[string]any{"status": "Hold", "ready": true}))
	})
	_, socketPath := startUnixServer(t, handler)
	err := snapd.New(socketPath).WaitForChange(context.Background(), "held")
	if err == nil || !strings.Contains(err.Error(), "status Hold") {
		t.Fatalf("error = %v, want ready Hold failure", err)
	}
}
```

Add this cancellation test for the retry timer:

```go
func TestWaitForChange_CancelsRetryTimer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(syncResponse(map[string]any{"status": "Doing", "ready": false}))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.WaitForChange(ctx, "held") }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForChange did not cancel retry timer")
	}
}
```

- [ ] **Step 5: Run focused validation**

```bash
gofmt -w internal/snapd/snapd.go internal/snapd/snapd_test.go
go test -race ./internal/snapd -run WaitForChange -count=3
rg -n 'time\.After\(' internal/snapd/snapd.go
```

Expected: all WaitForChange tests pass; real string errors are surfaced; any ready
non-success status terminates; grep returns no production match.

- [ ] **Step 6: Commit**

```bash
git add internal/snapd/snapd.go internal/snapd/snapd_test.go
git commit -m "fix(snapd): handle completed changes correctly"
```

## Task 3: Reuse the ping timer

**Files:**
- Modify: `internal/ping/ping.go`
- Modify: `internal/ping/ping_test.go`

- [ ] **Step 1: Add a cancellation behavior test**

Add to `internal/ping/ping_test.go`:

```go
func TestPinger_RunCancelsWhileWaiting(t *testing.T) {
	p := New("", func() string { return "" }, func() {}, time.Hour, newTransport(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop while timer was armed")
	}
}
```

This behavior test passes before the refactor. The fail-first optimization check is:

```bash
rg -n 'time\.After\(' internal/ping/ping.go
```

Expected before implementation: one match in `Run`.

Extract timer waiting behind a small private interface so the interval contract
can be tested without wall-clock races:

```go
type fakeTimer struct {
	c      chan time.Time
	resets chan time.Duration
}

func (t *fakeTimer) Chan() <-chan time.Time { return t.c }
func (t *fakeTimer) Reset(d time.Duration) bool { t.resets <- d; return true }
func (t *fakeTimer) Stop() bool { return true }

func receiveWithin[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel value")
		var zero T
		return zero
	}
}

func TestPinger_SetIntervalAffectsNextArm(t *testing.T) {
	p := New("", func() string { return "" }, func() {}, time.Minute, newTransport(t))
	timer := &fakeTimer{c: make(chan time.Time, 1), resets: make(chan time.Duration, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 2)
	go func() { done <- p.wait(ctx, timer) }()

	if got := receiveWithin(t, timer.resets); got != time.Minute {
		t.Fatalf("first arm = %v, want 1m", got)
	}
	p.SetInterval(time.Second)
	select {
	case <-done:
		t.Fatal("SetInterval interrupted the armed timer")
	case <-time.After(20 * time.Millisecond):
	}
	timer.c <- time.Now()
	if !receiveWithin(t, done) {
		t.Fatal("timer firing should continue the ping loop")
	}

	go func() { done <- p.wait(ctx, timer) }()
	if got := receiveWithin(t, timer.resets); got != time.Second {
		t.Fatalf("next arm = %v, want 1s", got)
	}
	timer.c <- time.Now()
	receiveWithin(t, done)
}
```

The first observed `Reset` is the deterministic handshake proving the original
interval is armed before `SetInterval` runs. The timeout helper ensures every
receive fails rather than hanging on a regression.

- [ ] **Step 2: Replace `time.After` with one timer**

Add the private timer interface and real wrapper beside `Pinger`:

```go
type pingTimer interface {
	Chan() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

type systemTimer struct {
	timer *time.Timer
}

func (t *systemTimer) Chan() <-chan time.Time { return t.timer.C }
func (t *systemTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }
func (t *systemTimer) Stop() bool { return t.timer.Stop() }
```

Then add:

```go
func (p *Pinger) wait(ctx context.Context, timer pingTimer) bool {
	timer.Reset(p.GetInterval())
	select {
	case <-ctx.Done():
		return false
	case <-timer.Chan():
		return true
	}
}
```

At the start of `Pinger.Run`, create and stop one wrapped timer:

```go
	timer := &systemTimer{timer: time.NewTimer(time.Hour)}
	if !timer.Stop() {
		<-timer.Chan()
	}
	defer timer.Stop()
```

At the top of each loop, arm it with the latest interval:

```go
		if !p.wait(ctx, timer) {
			return nil
		}
```

Do not add a wake channel to `SetInterval`; that would change its documented
"next scheduled ping" behavior.

- [ ] **Step 3: Validate behavior and allocation-site removal**

```bash
gofmt -w internal/ping/ping.go internal/ping/ping_test.go
go test -race ./internal/ping -count=10
rg -n 'time\.After\(' internal/ping/ping.go
```

Expected: tests pass and grep returns no production match.

- [ ] **Step 4: Commit**

```bash
git add internal/ping/ping.go internal/ping/ping_test.go
git commit -m "perf(ping): reuse the interval timer"
```

## Task 4: Correct the `USER_HZ` comment

**File:**
- Modify: `internal/monitor/activeprocessinfo.go`

- [ ] **Step 1: Change only the misleading comment**

Replace:

```go
// clkTck is the kernel timer frequency; 100 Hz is universal on modern Linux.
const clkTck = 100
```

with:

```go
// clkTck is Linux USER_HZ for /proc process accounting, independent of CONFIG_HZ.
const clkTck = 100
```

Do not derive this from kernel `CONFIG_HZ`; `/proc/<pid>/stat` uses `USER_HZ`.

- [ ] **Step 2: Verify the value and process tests**

```bash
gofmt -w internal/monitor/activeprocessinfo.go
rg -n 'clkTck = 100|USER_HZ|CONFIG_HZ' internal/monitor/activeprocessinfo.go
go test -race ./internal/monitor -run 'ActiveProcess|Process' -count=3
```

Expected: `clkTck` remains 100 and focused tests pass.

- [ ] **Step 3: Commit**

```bash
git add internal/monitor/activeprocessinfo.go
git commit -m "docs(monitor): clarify process clock ticks"
```

## Task 5: Verify Phase 5

- [ ] **Step 1: Run formatting and static checks**

```bash
test -z "$(gofmt -l .)"
go vet ./...
export PATH="$HOME/go/bin:$PATH" GOTOOLCHAIN=go1.26.6
golangci-lint run
```

Expected: no output from gofmt, vet succeeds, lint reports zero issues.

- [ ] **Step 2: Run compatibility and race tests**

```bash
go test -tags compat ./internal/bpickle/...
go test -race -count=3 ./...
```

Expected: all packages pass. The known 30-second stalled-socket snapd test will
dominate each full-suite run.

- [ ] **Step 3: Audit the requested properties**

```bash
rg -n 'go func\(\)' internal/exchange/exchange.go
rg -n 'invokeHandler|time\.After\(' internal/exchange/exchange.go internal/snapd/snapd.go
rg -n 'time\.After\(' internal/ping/ping.go
rg -n 'clkTck = 100|USER_HZ|CONFIG_HZ' internal/monitor/activeprocessinfo.go
```

Expected: exchange has no per-message goroutine and uses panic-contained
`invokeHandler`; snapd and ping have no looped `time.After`; `clkTck` remains 100
and its comment names `USER_HZ`, not kernel timer frequency.

- [ ] **Step 4: Review the phase diff**

```bash
git diff --stat fix/04-p2-hygiene..HEAD
git log --oneline fix/04-p2-hygiene..HEAD
git diff --check fix/04-p2-hygiene..HEAD
```

Expected: four implementation commits, no whitespace errors, and changes limited
to the eight files in the file map.
