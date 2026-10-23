# Phase 2 — P1 Reliability Implementation Plan

> **For agentic workers:** REQUIRED: Use the `subagent-driven-development` agent (recommended) or `executing-plans` agent to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the daemon hanging silently, stop it corrupting its own persisted identity, and make external-command failures diagnosable by the operator.

**Architecture:** Three timeout commits close every unbounded blocking call (HTTP, snapd, D-Bus). Two liveness commits make plugin collapse visible and add a self-supervising heartbeat watchdog. Three state-integrity commits stop failed reads and failed saves being papered over with empty or stale data. Four external-executable commits introduce a shared `runCmd` helper and route all six `exec` sites through it. One data-fidelity commit ports Python's 32-bit counter rollover correction.

**Tech Stack:** Go 1.25, `context`, `net/http`, `os/exec`, `golang.org/x/sys/unix`, `github.com/godbus/dbus/v5`, `encoding/xml`

**Spec:** [docs/superpowers/specs/2026-08-17-code-review-remediation-design.md](../specs/2026-08-17-code-review-remediation-design.md)

**Branch:** `fix/02-p1-reliability`, cut from `fix/01-p0-defects`

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `internal/transport/transport.go` | Modify | Default `TotalTimeout` to 600s in `New` |
| `internal/transport/transport_test.go` | Modify | Assert a stalled server does not hang forever |
| `internal/snapd/snapd.go` | Modify | Client `Timeout` and `ResponseHeaderTimeout` |
| `internal/monitor/snappackages.go` | Modify | Per-call timeout |
| `internal/monitor/snapservices.go` | Modify | Per-call timeout |
| `internal/monitor/computerinfo.go` | Modify | Per-call timeout |
| `internal/monitor/rebootrequired.go` | Modify | Per-call timeout |
| `cmd/landscape-client-core/main.go` | Modify | Replace detached `context.Background()`; wire the watchdog |
| `internal/manager/system.go` | Modify | D-Bus context; exec error reporting; interpreter validation |
| `internal/monitor/runner.go` | Modify | Report plugin collapse; publish heartbeats |
| `internal/monitor/heartbeat.go` | Create | Heartbeat publisher/observer types |
| `internal/monitor/heartbeat_test.go` | Create | Watchdog staleness tests |
| `internal/monitor/users.go` | Modify | Skip the tick on read failure |
| `internal/persist/persist.go` | Modify | Remove the `Save` fallback; `.old` backup recovery |
| `internal/monitor/mountinfo.go` | Modify | Honour `SetPluginState`/`GetPluginState` errors |
| `internal/monitor/networkdevice.go` | Modify | Honour `SetPluginState`/`GetPluginState` errors |
| `internal/monitor/snapservices.go` | Modify | Honour `SetPluginState` errors |
| `internal/monitor/rebootrequired.go` | Modify | Honour `SetPluginState` errors |
| `internal/monitor/processorinfo.go` | Modify | Honour `GetPluginState` errors |
| `internal/runcmd/runcmd.go` | Create | Shared external-command helper |
| `internal/runcmd/runcmd_test.go` | Create | Stderr surfacing, `ErrNotFound`, timeout |
| `internal/monitor/hardwareinfo.go` | Modify | `lshw` timeout and output validation |
| `internal/monitor/networkactivity.go` | Modify | 32-bit counter rollover correction |
| `go.mod` | Modify | Promote `golang.org/x/sys` to a direct dependency |

---

## Task 0: Create the branch

- [ ] **Step 1: Cut the branch**

```bash
git checkout fix/01-p0-defects
git checkout -b fix/02-p1-reliability
```

If Phase 1 has already merged, cut from `main`.

- [ ] **Step 2: Confirm a clean starting point**

Run: `go build ./... && go test -race ./...`
Expected: all packages `ok` or `no test files`.

---

## Task 1: Apply the documented `TotalTimeout` default

`internal/transport/transport.go` documents the field as `TotalTimeout
time.Duration // default: 600s`, and `doRequest` honours it correctly when
non-zero. But `New` never defaults it the way it defaults `ConnectTimeout`, and
`run()` does not set it. So in production `totalTimeout == 0` and the only bounds
are the dial and TLS handshake timeouts. A server that accepts the connection and
then trickles bytes hangs the exchange forever.

Note `config.Config` has **no** `TotalTimeout` field, so there is nothing to plumb
through — defaulting inside `New` is the whole fix, and it makes the documented
default true for every caller including tests.

**Files:**
- Modify: `internal/transport/transport.go`
- Modify: `internal/transport/transport_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/transport/transport_test.go`:

```go
// TestNew_DefaultsTotalTimeout asserts the documented 600s default is actually
// applied. Without it, production runs with no total request deadline at all.
func TestNew_DefaultsTotalTimeout(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.totalTimeout != 600*time.Second {
		t.Errorf("totalTimeout: want 600s, got %v", c.totalTimeout)
	}
}

func TestNew_HonoursExplicitTotalTimeout(t *testing.T) {
	c, err := New(Config{TotalTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.totalTimeout != 5*time.Second {
		t.Errorf("totalTimeout: want 5s, got %v", c.totalTimeout)
	}
}

// TestDoRequest_StalledServerTimesOut asserts a server that accepts and then
// sends nothing does not hang the caller indefinitely.
func TestDoRequest_StalledServerTimesOut(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	c, err := New(Config{TotalTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	_, err = c.doRequest(context.Background(), http.MethodGet, srv.URL, nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("request took %v; the total timeout was not applied", elapsed)
	}
}
```

`transport_test.go` is `package transport` (internal), so `c.totalTimeout` and
`c.doRequest` are reachable. Confirm with `head -1 internal/transport/transport_test.go`
before writing; if it is external, assert through the exported `Post`/`Get`
methods instead and drop the two field assertions.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/transport/ -run TestNew_DefaultsTotalTimeout -v`
Expected: FAIL with `totalTimeout: want 600s, got 0s`.

- [ ] **Step 3: Apply the default**

In `internal/transport/transport.go`, add a constant next to the existing
`defaultConnectTimeout`:

```go
	defaultTotalTimeout = 600 * time.Second
```

Then in `New`, immediately after the existing connect-timeout defaulting:

```go
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = defaultConnectTimeout
	}

	totalTimeout := cfg.TotalTimeout
	if totalTimeout == 0 {
		totalTimeout = defaultTotalTimeout
	}
```

And in the returned struct literal, replace `totalTimeout: cfg.TotalTimeout` with
`totalTimeout: totalTimeout`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/transport/ -v`
Expected: PASS, including the pre-existing tests.

- [ ] **Step 5: Check no test depended on the absent timeout**

Run: `go test -race ./...`
Expected: clean. A test that previously relied on an unbounded request will now
fail — that is a real finding, not a flake; fix the test to set an explicit
`TotalTimeout`.

- [ ] **Step 6: Commit**

```bash
git add internal/transport/transport.go internal/transport/transport_test.go
git commit -m "fix(transport): apply the documented TotalTimeout default

Config documents 'default: 600s' and doRequest honours the value when set,
but New never defaulted it and nothing set it, so production had no total
request deadline — only the dial and TLS handshake were bounded. A server
that accepts then trickles bytes hung the exchange indefinitely."
```

---

## Task 2: Bound snapd client and per-call deadlines

`internal/snapd/snapd.go`'s `New` builds an `http.Client` with **no `Timeout`** and
no `ResponseHeaderTimeout`. Its four monitor callers pass the daemon-lifetime
context with no per-call timeout, so a stalled `/run/snapd.socket` hangs those
plugins for the life of the process:

| Caller | Call |
|---|---|
| `internal/monitor/snappackages.go:72` | `p.snapdClient.ListSnaps(ctx)` |
| `internal/monitor/snapservices.go:61` | `p.snapdClient.ListServices(ctx)` |
| `internal/monitor/computerinfo.go:238` | `p.snapdClient.GetAssertions(ctx)` |
| `internal/monitor/rebootrequired.go:60` | `p.snapd.GetRebootRequired(ctx)` |

Also in this commit: the `sendSnapUpdate` closure in `run()` calls
`snapPackages.SendNow(context.Background(), exc)` — a detached context, so the
snapd call can outlive shutdown entirely.

**Files:**
- Modify: `internal/snapd/snapd.go`
- Modify: `internal/monitor/snappackages.go`, `snapservices.go`, `computerinfo.go`, `rebootrequired.go`
- Modify: `cmd/landscape-client-core/main.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/snapd/snapd_test.go`:

```go
// TestClient_StalledSocketDoesNotHang asserts a snapd socket that accepts the
// connection and never responds fails within the client timeout rather than
// blocking the calling plugin for the life of the process.
func TestClient_StalledSocketDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "snapd.socket")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn // hold it open, never respond
	}()

	c := New(sockPath)
	start := time.Now()
	_, err = c.ListSnaps(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error from a stalled socket, got nil")
	}
	if elapsed > 60*time.Second {
		t.Errorf("ListSnaps blocked for %v; the client has no timeout", elapsed)
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	default:
	}
}
```

Check the actual method name on `snapd.Client` — `internal/snapd/mock.go` shows
`ListSnaps`, `ListServices`, `GetAssertions`, `GetRebootRequired`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/snapd/ -run TestClient_StalledSocketDoesNotHang -timeout 90s -v`
Expected: FAIL — the test times out, or reports a duration above 60s.

- [ ] **Step 3: Add the client timeout**

In `internal/snapd/snapd.go`'s `New`:

```go
func New(socketPath string) Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &RealClient{
		// snapd change operations are polled separately via WaitForChange, so no
		// single request should take this long.
		http:    &http.Client{Transport: transport, Timeout: 60 * time.Second},
		baseURL: "http://localhost/v2",
	}
}
```

Add `"time"` to the imports if not present.

- [ ] **Step 4: Add per-call timeouts at the four monitor sites**

Add a shared constant in `internal/monitor` (put it in `monitor.go` next to the
other package-level declarations):

```go
// snapdCallTimeout bounds a single snapd request. Plugins hold the
// daemon-lifetime context, so without this a stalled snapd socket wedges the
// plugin permanently.
const snapdCallTimeout = 30 * time.Second
```

Then wrap each call. For `snappackages.go`:

```go
	callCtx, cancel := context.WithTimeout(ctx, snapdCallTimeout)
	snaps, err := p.snapdClient.ListSnaps(callCtx)
	cancel()
```

Use the same shape at `snapservices.go`, `computerinfo.go` and
`rebootrequired.go`, substituting the respective method. Call `cancel()`
immediately after the call rather than deferring it — these are inside loops, and
a deferred cancel would accumulate one per tick for the life of the plugin.

- [ ] **Step 5: Fix the detached context in `run()`**

In `cmd/landscape-client-core/main.go`, inside `run`, the closure currently reads:

```go
	sendSnapUpdate := func() { snapPackages.SendNow(context.Background(), exc) }
```

The daemon context is not yet in scope at that point, so move the closure's
definition below the `ctx, cancel := context.WithCancel(ctx)` line, or capture the
function parameter directly:

```go
	// Uses the daemon context so a snapd call cannot outlive shutdown.
	sendSnapUpdate := func() { snapPackages.SendNow(ctx, exc) }
```

Verify the closure is defined after `ctx` is in scope; if `run`'s parameter is
already named `ctx`, no reordering is needed.

- [ ] **Step 6: Run to verify it passes**

Run: `go test -race ./internal/snapd/ ./internal/monitor/ ./cmd/... -v`
Expected: PASS. The stalled-socket test should now fail fast, in ~60s or less.

- [ ] **Step 7: Confirm no `context.Background()` remains in the daemon path**

Run: `grep -n 'context.Background()' cmd/landscape-client-core/main.go`
Expected: at most one match, in `main()` where the signal context is created.

- [ ] **Step 8: Commit**

```bash
git add internal/snapd/snapd.go internal/monitor/ cmd/landscape-client-core/main.go
git commit -m "fix(snapd): bound client and per-call deadlines

The snapd http.Client had no Timeout and no ResponseHeaderTimeout, and all
four monitor callers passed the daemon-lifetime context with no per-call
timeout, so a stalled /run/snapd.socket wedged those plugins for the life of
the process.

Also replaces the detached context.Background() in the sendSnapUpdate
closure, which let a snapd call outlive shutdown entirely."
```

---

## Task 3: Bound D-Bus connect and call with context

`internal/manager/system.go`'s `dbusShutdown` uses `dbus.ConnectSystemBus()` and
`obj.Call(...)`, neither of which takes a context. godbus offers
`ConnectSystemBusWithContext` and `CallWithContext`. An unresponsive `logind`
currently hangs the shutdown handler forever — and because `manager.Runner` bounds
handlers with a semaphore, a wedged shutdown handler eventually starves all
manager operations.

**Files:**
- Modify: `internal/manager/system.go`

- [ ] **Step 1: Read the current implementation**

Run: `sed -n '28,60p' internal/manager/system.go`
Expected: `dbusShutdown(reboot bool) error` calling `dbus.ConnectSystemBus()`,
then `conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")` and
`obj.Call(...)`.

- [ ] **Step 2: Change the signature to take a context**

```go
// dbusShutdown calls org.freedesktop.login1.Manager Reboot or PowerOff via DBus.
// interactive is passed as false (non-interactive, matches Python client).
// ctx bounds both the bus connection and the method call: an unresponsive logind
// would otherwise hang this handler indefinitely, and the manager semaphore means
// a wedged handler eventually starves all manager operations.
func dbusShutdown(ctx context.Context, reboot bool) error {
	conn, err := dbus.ConnectSystemBusWithContext(ctx)
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")

	method := "org.freedesktop.login1.Manager.PowerOff"
	if reboot {
		method = "org.freedesktop.login1.Manager.Reboot"
	}

	call := obj.CallWithContext(ctx, method, 0, false)
	return call.Err
}
```

Keep whatever method-name selection logic the current code has — read it first and
preserve it exactly; only the connect and call become context-aware.

- [ ] **Step 3: Update the caller with a bounded context**

At the call site in `ShutdownHandler.Handle`, derive a timeout:

```go
	dbusCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := dbusShutdown(dbusCtx, reboot); err != nil {
```

- [ ] **Step 4: Build and run the package tests**

Run: `go build ./... && go test -race ./internal/manager/ -v`
Expected: PASS. If a test calls `dbusShutdown` directly, update it to pass
`context.Background()`.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/system.go
git commit -m "fix(manager): bound D-Bus connect and call with context

ConnectSystemBus and obj.Call take no context, so an unresponsive logind hung
the shutdown handler forever. Because manager.Runner bounds handlers with a
semaphore, a wedged handler eventually starves all manager operations."
```

---

## Task 4: Report plugin collapse from `Runner.Run`

`internal/monitor/runner.go`'s `Run` logs the errgroup error and then returns
`nil` unconditionally — the doc comment even says "It always returns nil". So the
error branch in `run()` is unreachable and a total monitor collapse is invisible
to the supervisor. This must land before the watchdog, which is otherwise watching
a function that can never report a problem.

Note the distinction to preserve: `context.Canceled` during shutdown is **not** a
failure and must still return `nil`.

**Files:**
- Modify: `internal/monitor/runner.go`
- Modify: `internal/monitor/runner_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/monitor/runner_test.go` (package `monitor`):

```go
// failingPlugin returns immediately with an error and never blocks, so the
// runner's restart backoff drives it repeatedly.
type failingPlugin struct{ name string }

func (p *failingPlugin) Name() string { return p.name }

func (p *failingPlugin) Run(ctx context.Context, _ exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	return errors.New("boom")
}

// TestRunner_Run_ReportsCollapse asserts Run surfaces a plugin failure instead
// of swallowing it. The supervisor cannot act on a function documented to
// always return nil.
func TestRunner_Run_ReportsCollapse(t *testing.T) {
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	r := New([]Plugin{&failingPlugin{name: "boom-plugin"}}, &mockSink{}, store)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := r.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil for a plugin that only ever fails")
	}
	if !strings.Contains(err.Error(), "boom-plugin") {
		t.Errorf("error should name the failing plugin, got: %v", err)
	}
}

// TestRunner_Run_CleanShutdownReturnsNil guards the behaviour that must NOT
// change: cancellation is not a failure.
func TestRunner_Run_CleanShutdownReturnsNil(t *testing.T) {
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	r := New([]Plugin{&blockingPlugin{}}, &mockSink{}, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown must return nil, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
```

`blockingPlugin` — a plugin that blocks on `ctx.Done()` — may already exist in
`runner_test.go`. Reuse it if so; otherwise add the obvious two-line type.

The failing-plugin case interacts with the runner's restart backoff
(`runnerInitialBackoff` is 1s), so the 3s context gives it two or three restarts
before cancellation. Design the fix so a plugin that has failed repeatedly is
reported even when the context then expires.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestRunner_Run_ReportsCollapse -v`
Expected: FAIL with `Run returned nil for a plugin that only ever fails`.

- [ ] **Step 3: Record per-plugin failures and report them**

In `internal/monitor/runner.go`, change `runPlugin` to return its last error, and
have `Run` collect failures:

```go
// Run starts one goroutine per plugin and blocks until all goroutines have
// exited. It returns nil on clean shutdown, and an error naming the plugins that
// failed repeatedly — the supervisor needs to distinguish those two cases.
func (r *Runner) Run(ctx context.Context) error {
	eg, egCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	failed := make(map[string]error)

	for _, p := range r.plugins {
		plugin := p
		eg.Go(func() error {
			if err := r.runPlugin(egCtx, plugin); err != nil {
				mu.Lock()
				failed[plugin.Name()] = err
				mu.Unlock()
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("monitor: runner group error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(failed) > 0 {
		names := make([]string, 0, len(failed))
		for name := range failed {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("monitor: plugins failed: %s (last error for %s: %w)",
			strings.Join(names, ", "), names[0], failed[names[0]])
	}
	return nil
}
```

Change `runPlugin`'s signature to `func (r *Runner) runPlugin(ctx context.Context,
plugin Plugin) error`, keep its existing panic recovery and backoff loop exactly
as they are, and return the last non-`context.Canceled` `runErr` at the point
where it currently does a bare `return` on `ctx.Err() != nil`.

Add `"sort"`, `"strings"` and `"sync"` to the imports as needed.

- [ ] **Step 4: Run to verify both tests pass**

Run: `go test -race ./internal/monitor/ -run TestRunner_Run -v`
Expected: PASS for both.

- [ ] **Step 5: Confirm `run()`'s error branch is now reachable**

Run: `grep -n 'monitor runner failed' cmd/landscape-client-core/main.go`
Expected: one match — that branch was dead code before this commit.

- [ ] **Step 6: Commit**

```bash
git add internal/monitor/runner.go internal/monitor/runner_test.go
git commit -m "fix(monitor): report plugin collapse from Runner.Run

Run logged the errgroup error then returned nil unconditionally — the doc
comment said 'It always returns nil' — so run()'s error branch was dead code
and a total monitor collapse was invisible. Clean shutdown still returns nil.

Prerequisite for the watchdog: supervision over a function that cannot report
failure detects nothing."
```

---

## Task 5: Add a heartbeat watchdog

There is no watchdog of any kind — no `sd_notify`, no `WatchdogSec`, no liveness
self-check. The Python client shipped `landscape/client/watchdog.py`, which
AMP-pinged each daemon and restarted unresponsive ones; the rewrite dropped that
supervision without replacing it.

`restart-condition: on-failure` only covers **process exit**. A goroutine blocked
forever in a syscall keeps the process alive and healthy-looking while silently
reporting nothing. Several such blocking calls are only *bounded* by Tasks 1–3;
`syscall.Statfs` in `mountinfo` is genuinely uninterruptible and no context can
rescue it.

**Design decision (from spec §3):** self-supervising, not `daemon: notify`. The
systemd handshake cannot be validated without a device. The heartbeat plumbing is
shaped so `sd_notify(WATCHDOG=1)` becomes an additional observer later.

**Files:**
- Create: `internal/monitor/heartbeat.go`
- Create: `internal/monitor/heartbeat_test.go`
- Modify: `internal/monitor/runner.go`
- Modify: `cmd/landscape-client-core/main.go`

- [ ] **Step 1: Write the failing test**

Create `internal/monitor/heartbeat_test.go` (package `monitor`):

```go
package monitor

import (
	"testing"
	"time"
)

func TestHeartbeat_FreshIsNotStale(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	hb.Beat("cpu-usage")

	stale := hb.Stale(60 * time.Second)
	if len(stale) != 0 {
		t.Errorf("want no stale sources, got %v", stale)
	}
}

func TestHeartbeat_DetectsStaleSource(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	hb.Beat("cpu-usage")
	hb.Beat("mount-info")

	// cpu-usage keeps beating; mount-info wedges.
	now = time.Unix(1000+120, 0)
	hb.Beat("cpu-usage")

	stale := hb.Stale(60 * time.Second)
	if len(stale) != 1 || stale[0] != "mount-info" {
		t.Fatalf("want [mount-info], got %v", stale)
	}
}

func TestHeartbeat_StaleIsSortedAndStable(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	for _, name := range []string{"users", "cpu-usage", "mount-info"} {
		hb.Beat(name)
	}
	now = time.Unix(1000+300, 0)

	stale := hb.Stale(60 * time.Second)
	want := []string{"cpu-usage", "mount-info", "users"}
	if len(stale) != len(want) {
		t.Fatalf("want %v, got %v", want, stale)
	}
	for i := range want {
		if stale[i] != want[i] {
			t.Fatalf("want %v, got %v", want, stale)
		}
	}
}

func TestHeartbeat_UnregisteredSourceIsNotStale(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })

	// A plugin that has never beaten has not yet started; the watchdog must not
	// fire during startup.
	if stale := hb.Stale(time.Second); len(stale) != 0 {
		t.Errorf("want no stale sources before any beat, got %v", stale)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestHeartbeat -v`
Expected: FAIL — compile error, `undefined: NewHeartbeat`.

- [ ] **Step 3: Implement the heartbeat**

Create `internal/monitor/heartbeat.go`:

```go
package monitor

import (
	"sort"
	"sync"
	"time"
)

// Heartbeat records the last time each named source made progress. A source that
// stops beating is wedged: restart-condition only covers process exit, so a
// goroutine blocked in a syscall keeps the process looking healthy while
// reporting nothing.
//
// now is injectable so the watchdog can be tested without sleeping.
type Heartbeat struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func NewHeartbeat(now func() time.Time) *Heartbeat {
	if now == nil {
		now = time.Now
	}
	return &Heartbeat{
		last: make(map[string]time.Time),
		now:  now,
	}
}

// Beat records progress for source.
func (h *Heartbeat) Beat(source string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[source] = h.now()
}

// Stale returns the sorted names of sources that have not beaten within
// threshold. Sources that have never beaten are excluded: they have not started
// yet, and the watchdog must not fire during startup.
func (h *Heartbeat) Stale(threshold time.Duration) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	cutoff := h.now().Add(-threshold)
	var stale []string
	for source, last := range h.last {
		if last.Before(cutoff) {
			stale = append(stale, source)
		}
	}
	sort.Strings(stale)
	return stale
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/monitor/ -run TestHeartbeat -v`
Expected: PASS, all four.

- [ ] **Step 5: Publish heartbeats from the monitor runner**

Add a `heartbeat *Heartbeat` field to `Runner`, set by a new option or by `New`.
In `runPlugin`, beat once per successful plugin loop iteration — the simplest
correct place is immediately before `plugin.Run` is (re)entered and, more usefully,
inside the plugin tick path. Because plugin intervals vary from 15s to 1 hour, the
threshold must be per-source:

```go
// watchdogThreshold returns how long a plugin may go without progress before it
// is considered wedged. Intervals range from 15s to 1h, so a single global
// threshold would either miss wedged fast plugins or false-positive slow ones.
func watchdogThreshold(interval time.Duration) time.Duration {
	const minThreshold = 2 * time.Minute
	t := interval * 3
	if t < minThreshold {
		return minThreshold
	}
	return t
}
```

If `Plugin` has no interval accessor, add one (`Interval() time.Duration`) to the
interface and implement it on all 15 plugins by returning the existing `interval`
field. That is mechanical and makes the threshold honest. Check
`internal/monitor/monitor.go` for the current `Plugin` interface definition first.

- [ ] **Step 6: Wire the supervisor into `run()`**

In `cmd/landscape-client-core/main.go`, add a fourth errgroup goroutine:

```go
	eg.Go(func() error {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-groupCtx.Done():
				return nil
			case <-ticker.C:
				if stale := monRunner.StaleSources(); len(stale) > 0 {
					// Exiting non-zero lets snapd's restart-condition recover a
					// wedged daemon. A blocked goroutine keeps the process alive
					// and healthy-looking, so nothing else would notice.
					slog.Error("watchdog: sources stopped making progress; exiting for restart",
						"sources", stale)
					return fmt.Errorf("watchdog: stale sources: %v", stale)
				}
			}
		}
	})
```

Add the `StaleSources()` method on `monitor.Runner`, returning
`r.heartbeat.Stale(...)` evaluated per plugin against its own threshold.

- [ ] **Step 7: Full suite**

Run: `go test -race ./...`
Expected: clean. Watch particularly for the `run()` shutdown test from Phase 1 —
the watchdog goroutine must exit on context cancellation, not hold shutdown open.

- [ ] **Step 8: Commit**

```bash
git add internal/monitor/heartbeat.go internal/monitor/heartbeat_test.go internal/monitor/runner.go cmd/landscape-client-core/main.go
git commit -m "feat(cmd): add heartbeat watchdog

There was no watchdog of any kind; the Python client had watchdog.py and the
rewrite dropped that supervision without replacing it. restart-condition
covers only process exit, so a goroutine blocked in a syscall — mountinfo's
Statfs is genuinely uninterruptible — keeps the daemon alive and silent.

Self-supervising by design: sd_notify/WatchdogSec cannot be validated without
a device, and the heartbeat plumbing accepts an extra observer later.

Thresholds are per-plugin because intervals range from 15s to 1h."
```

---

## Task 6: Skip the tick when `passwd` or `group` cannot be read

`internal/monitor/users.go` substitutes an **empty map** for a failed parse:

```go
			newUsers, err := p.parsePasswd()
			if err != nil {
				log.Printf("users: parsing passwd: %v", err)
				newUsers = make(map[string]userRecord)
			}
```

`buildUsersDiff` then diffs the saved users against nothing. With three saved
users and two saved groups, one unreadable read produces:

```
  delete-users:  [alice bob root]
  delete-groups: [root sudo]
```

The code then persists the empty map, so the next tick re-emits everything as
`create-users` — a delete-all/recreate-all churn against the server from a single
transient read failure. It hides in testing because in steady-state failure the
saved state is also empty, so no diff appears.

An unreadable source file means "unknown", never "empty".

**Files:**
- Modify: `internal/monitor/users.go`
- Modify: `internal/monitor/sysinfo_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/monitor/sysinfo_test.go` (package `monitor`):

```go
// TestUsers_ReadFailureDoesNotDeleteEveryone asserts a transient passwd read
// error is treated as "unknown", not "no users exist". Substituting an empty
// map makes the client tell the server to delete every user and group.
func TestUsers_ReadFailureDoesNotDeleteEveryone(t *testing.T) {
	dir := t.TempDir()
	passwdPath := filepath.Join(dir, "passwd")
	groupPath := filepath.Join(dir, "group")

	writeFixture(t, passwdPath, "root:x:0:0:root:/root:/bin/bash\nalice:x:1000:1000:Alice:/home/alice:/bin/sh\n")
	writeFixture(t, groupPath, "root:x:0:\nsudo:x:27:alice\n")

	p := &UserMonitor{
		interval:   5 * time.Millisecond,
		passwdPath: passwdPath,
		groupPath:  groupPath,
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// First tick reports the initial users.
	waitForMessages(t, sink, 1, 500*time.Millisecond)

	// Make passwd unreadable, simulating a transient failure.
	if err := os.Remove(passwdPath); err != nil {
		t.Fatalf("remove passwd: %v", err)
	}
	before := len(sink.messages())

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-errCh

	for _, msg := range sink.messages()[before:] {
		if v, ok := msg["delete-users"]; ok {
			t.Errorf("emitted delete-users %v after a transient read failure", v)
		}
		if v, ok := msg["delete-groups"]; ok {
			t.Errorf("emitted delete-groups %v after a transient read failure", v)
		}
	}
}
```

Check `mockSink`'s accessor name in `internal/monitor/sysinfo_test.go` — it may be
`messages()` or a direct field guarded by a mutex. Use whatever exists; do not add
a second mock. Check `UserMonitor`'s field names too (`passwdPath`, `groupPath`,
`interval` per `internal/monitor/users.go`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestUsers_ReadFailureDoesNotDeleteEveryone -v`
Expected: FAIL with `emitted delete-users [alice root] after a transient read failure`.

- [ ] **Step 3: Skip the tick instead of substituting empty data**

In `internal/monitor/users.go`, replace the two error branches:

```go
		case <-ticker.C:
			newUsers, err := p.parsePasswd()
			if err != nil {
				// An unreadable source file means "unknown", never "empty":
				// diffing against an empty map tells the server to delete every
				// user, and persisting it re-creates them all on the next tick.
				log.Printf("users: parsing passwd: %v; skipping tick", err)
				continue
			}
			newGroups, err := p.parseGroup(newUsers)
			if err != nil {
				log.Printf("users: parsing group: %v; skipping tick", err)
				continue
			}
```

Leave the rest of the tick body — the diff, send and save — unchanged.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/monitor/ -run TestUsers -v`
Expected: PASS, including the pre-existing users tests.

- [ ] **Step 5: Audit the other plugins for the same shape**

Run:

```bash
grep -rn -B3 'make(map\[' internal/monitor/*.go | grep -A3 'err != nil'
```

Inspect each hit. The pattern to look for is an error branch that assigns an empty
collection and then falls through to a diff or a save. Fix any found in this same
commit and list them in the commit message. If none are found, say so.

- [ ] **Step 6: Commit**

```bash
git add internal/monitor/users.go internal/monitor/sysinfo_test.go
git commit -m "fix(monitor): skip the tick when passwd or group cannot be read

A failed parse substituted an empty map, so buildUsersDiff emitted
delete-users for every user and delete-groups for every group, then persisted
the empty map so the next tick re-created them all — a delete-all/recreate-all
cycle against the server from one transient read failure.

It hid in testing because in steady-state failure the saved state is also
empty, so no diff is produced. An unreadable source file means unknown, never
empty."
```

---

## Task 7: Remove the `SetPluginState` fallback and honour save failures

`internal/persist/persist.go`'s `SetPluginState` recovers a failed `Update` by
calling `p.store.Save(p.cached)` — a **whole-`State` snapshot** captured when the
plugin started (`monitor/runner.go` loads state, then calls `r.store.Accessor`).
Anything written since is lost. Verified with a corrupt state file, so `Update`
fails at the *decode* step while writes still succeed:

```
SecureID="SECRET-1"     (was SECRET-2-ROTATED)   ← CLOBBERED
OutboundSequence=5      (was 99)                 ← CLOBBERED
plugin-b=               (was {"important":true}) ← LOST
```

Rolling back `SecureID` de-registers the client from the server's point of view;
rolling back `OutboundSequence` breaks message sequencing.

**Coupled, and therefore in this same commit:** four callers discard the result —
`mountinfo.go`, `networkdevice.go`, `snapservices.go`, `rebootrequired.go` all do
`_ = state.SetPluginState(saved)` after advancing the in-memory hash. So a failed
save means the change is **never re-sent** and the plugin believes the old value
was reported. Propagating an error that four callers throw away changes nothing.

**Files:**
- Modify: `internal/persist/persist.go`
- Modify: `internal/persist/persist_test.go`
- Modify: `internal/monitor/mountinfo.go`, `networkdevice.go`, `snapservices.go`, `rebootrequired.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/persist/persist_test.go`:

```go
// TestSetPluginState_DoesNotRollBackOtherFields asserts a failed plugin-state
// save never resurrects a stale whole-State snapshot. The fallback path wrote
// p.cached, captured when the plugin started, clobbering SecureID and
// OutboundSequence written by the exchange since.
func TestSetPluginState_DoesNotRollBackOtherFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := persist.New(path)

	// Plugin starts and captures a snapshot.
	initial, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	initial.SecureID = "SECRET-1"
	initial.OutboundSequence = 5
	if err := store.Save(initial); err != nil {
		t.Fatalf("Save: %v", err)
	}
	accessor := store.Accessor("plugin-a", initial)

	// The exchange rotates the secure ID and advances the sequence.
	if err := store.Update(func(s *persist.State) error {
		s.SecureID = "SECRET-2-ROTATED"
		s.OutboundSequence = 99
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Corrupt the file so the next Update fails at decode.
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	err = accessor.SetPluginState(map[string]string{"hash": "abc"})
	if err == nil {
		t.Fatal("SetPluginState returned nil for a state file that cannot be read")
	}

	// Repair the file by writing known-good content, then confirm the failed
	// save did not overwrite it with the stale snapshot.
	if err := os.WriteFile(path, []byte(`{"secure_id":"SECRET-2-ROTATED","outbound_sequence":99}`), 0600); err != nil {
		t.Fatalf("repair: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load after repair: %v", err)
	}
	if got.SecureID != "SECRET-2-ROTATED" {
		t.Errorf("SecureID rolled back to %q", got.SecureID)
	}
	if got.OutboundSequence != 99 {
		t.Errorf("OutboundSequence rolled back to %d", got.OutboundSequence)
	}
}
```

Check the JSON tags on `persist.State` before writing the repair literal — use the
actual tag names from `internal/persist/persist.go`. `persist_test.go` is an
external test package (`package persist_test`), which is why the identifiers are
qualified.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/persist/ -run TestSetPluginState_DoesNotRollBackOtherFields -v`
Expected: FAIL with `SetPluginState returned nil for a state file that cannot be read`.

- [ ] **Step 3: Delete the fallback**

In `internal/persist/persist.go`, replace the tail of `SetPluginState`:

```go
	if err != nil {
		// Fall back to whatever we have cached rather than failing entirely.
		if err2 := p.ensureLoaded(); err2 != nil {
			return fmt.Errorf("persist: saving plugin state for %q: update failed: %w", p.key, err)
		}
		if p.cached.PluginState == nil {
			p.cached.PluginState = make(map[string]json.RawMessage)
		}
		p.cached.PluginState[p.key] = json.RawMessage(data)
		return p.store.Save(p.cached)
	}
	p.cached = updated
	return nil
```

with:

```go
	if err != nil {
		// Never "recover" a failed save by writing older data: p.cached is a
		// whole-State snapshot from plugin start, so writing it rolls back
		// SecureID and OutboundSequence written by the exchange since.
		return fmt.Errorf("persist: saving plugin state for %q: %w", p.key, err)
	}
	p.cached = updated
	return nil
```

- [ ] **Step 4: Honour the error at the four call sites**

In each of `internal/monitor/mountinfo.go`, `networkdevice.go`, `snapservices.go`
and `rebootrequired.go`, the pattern is:

```go
			saved.Hash = hash
			if state != nil {
				_ = state.SetPluginState(saved)
			}
```

Replace with:

```go
			if state != nil {
				if err := state.SetPluginState(saved); err != nil {
					// Do not advance the in-memory hash: if the save failed, the
					// change must be re-detected and re-sent next tick.
					log.Printf("%s: saving state: %v; will retry next tick", p.Name(), err)
					continue
				}
			}
			saved.Hash = hash
```

Note the reordering — the hash is only advanced **after** a successful save. Adapt
the field name per plugin (`rebootrequired` stores a flag rather than a hash;
apply the same "only advance on success" rule to whatever it stores).

- [ ] **Step 5: Run to verify it passes**

Run: `go test -race ./internal/persist/ ./internal/monitor/ -v`
Expected: PASS. Any existing test that relied on `SetPluginState` silently
succeeding will now fail — fix the test, not the production code.

- [ ] **Step 6: Confirm no discarded `SetPluginState` remains**

Run: `grep -rn '_ = state.SetPluginState\|_ = .*\.SetPluginState' internal/`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/persist/persist.go internal/persist/persist_test.go internal/monitor/
git commit -m "fix(persist): remove the SetPluginState fallback and honour save failures

The fallback recovered a failed Update by writing p.cached — a whole-State
snapshot captured when the plugin started — which rolled back SecureID
(de-registering the client) and OutboundSequence (breaking sequencing), and
dropped other plugins' state.

Coupled: four callers did '_ = state.SetPluginState(...)' after advancing the
in-memory hash, so a failed save meant the change was never re-sent and the
plugin believed the old value had been reported. The hash now advances only
on a successful save."
```

---

## Task 8: Recover a corrupt state file via `.old` backup

Task 7 makes a corrupt state file a hard error. That is correct, but on its own it
leaves the daemon stuck: every save fails until someone deletes the file. Python's
`Persist` keeps an `.old` backup and falls back to it. This commit adds the same.

It also fixes the mirror of Task 7's problem on the read side: four plugins do
`_ = state.GetPluginState(&saved)`, so corrupt state silently becomes zero state.
For `users` that alone re-sends every user as `create-users`.

**Files:**
- Modify: `internal/persist/persist.go`
- Modify: `internal/persist/persist_test.go`
- Modify: `internal/monitor/processorinfo.go`, `users.go`, `mountinfo.go`, `networkdevice.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/persist/persist_test.go`:

```go
// TestLoad_RecoversFromBackupWhenStateIsCorrupt asserts a truncated or corrupt
// state file falls back to the .old backup rather than failing permanently or
// silently reverting to zero state.
func TestLoad_RecoversFromBackupWhenStateIsCorrupt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := persist.New(path)

	first, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	first.SecureID = "GOOD-ID"
	first.OutboundSequence = 7
	if err := store.Save(first); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A second save rotates the previous good file to state.json.old.
	first.OutboundSequence = 8
	if err := store.Save(first); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := os.Stat(path + ".old"); err != nil {
		t.Fatalf("expected a .old backup after the second save: %v", err)
	}

	// Simulate a truncated write.
	if err := os.WriteFile(path, []byte(`{"secure_id":"GOO`), 0600); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	recovered, err := persist.New(path).Load()
	if err != nil {
		t.Fatalf("Load should recover from the backup, got: %v", err)
	}
	if recovered.SecureID != "GOOD-ID" {
		t.Errorf("SecureID: want GOOD-ID from the backup, got %q", recovered.SecureID)
	}
}

// TestLoad_CorruptWithNoBackupIsAnError guards against silently returning zero
// state, which for the users plugin means re-sending every user as a create.
func TestLoad_CorruptWithNoBackupIsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := persist.New(path).Load(); err == nil {
		t.Fatal("want an error for a corrupt state file with no backup, got nil")
	}
}
```

Read `internal/persist/persist.go`'s `loadLocked` first: if a missing file already
returns a fresh empty `State` (it should, for first run), preserve that — the new
error path applies only to a file that exists but cannot be decoded.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/persist/ -run TestLoad_Recovers -v`
Expected: FAIL with `expected a .old backup after the second save`.

- [ ] **Step 3: Rotate a backup on save**

In `saveLocked`, immediately before the final `os.Rename(tmpPath, s.path)`, copy
the current file aside:

```go
	// Keep the previous good state as a backup, mirroring Python's Persist, so a
	// truncated write or disk corruption is recoverable rather than fatal.
	if existing, err := os.ReadFile(s.path); err == nil {
		if err := os.WriteFile(s.path+".old", existing, 0600); err != nil {
			log.Printf("persist: writing backup: %v", err)
		}
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("persist: renaming temp file: %w", err)
	}
```

Reading and rewriting is deliberate rather than renaming the original aside: a
rename would leave no `state.json` at all in the window before the temp file is
moved into place.

- [ ] **Step 4: Fall back to the backup on load**

In `loadLocked`, where the decode error is currently returned, try the backup
first:

```go
	if err := json.Unmarshal(data, state); err != nil {
		backup, berr := os.ReadFile(s.path + ".old")
		if berr == nil {
			var recovered State
			if jerr := json.Unmarshal(backup, &recovered); jerr == nil {
				log.Printf("persist: %s is corrupt (%v); recovered from %s.old", s.path, err, s.path)
				return &recovered, nil
			}
		}
		return nil, fmt.Errorf("persist: decoding %s: %w", s.path, err)
	}
```

Adapt to the actual variable names in `loadLocked` — read it before editing.

- [ ] **Step 5: Honour `GetPluginState` errors at the four call sites**

`processorinfo.go`, `users.go`, `mountinfo.go` and `networkdevice.go` each do:

```go
	if state != nil {
		_ = state.GetPluginState(&saved)
	}
```

Replace with:

```go
	if state != nil {
		if err := state.GetPluginState(&saved); err != nil {
			// Zero state is not equivalent to "no changes yet": for users it
			// re-sends every account as a create.
			log.Printf("%s: loading state: %v; treating as first run", p.Name(), err)
		}
	}
```

Logging rather than failing is deliberate — the plugin can still function, and
`Runner.runPlugin` would otherwise restart it in a loop against a persistently bad
file. The log line is what makes it visible.

- [ ] **Step 6: Run to verify it passes**

Run: `go test -race ./internal/persist/ ./internal/monitor/ -v`
Expected: PASS.

- [ ] **Step 7: Confirm no discarded `GetPluginState` remains**

Run: `grep -rn '_ = state.GetPluginState' internal/`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add internal/persist/persist.go internal/persist/persist_test.go internal/monitor/
git commit -m "fix(persist): recover a corrupt state file via .old backup

Removing the SetPluginState fallback made a corrupt state file a hard error,
which is correct but leaves the daemon stuck until someone deletes the file.
Python's Persist keeps an .old backup; this does the same.

Also fixes the read-side mirror: four plugins discarded GetPluginState
errors, so corrupt state silently became zero state — for users that alone
re-sends every account as create-users."
```

---

## Task 9: Add a `runCmd` helper and route all exec sites through it

`.Output()` captures stderr into `ExitError.Stderr`, but `%v` on the error prints
only `"exit status N"`. Five of the six `exec` sites discard it:

```
hardwareinfo.go style -> log says: "exit status 1"
  ...discarded stderr:    "lshw: DMI probe failed\n"

main.go snapctl style  -> error is: "exit status 2"
  ...discarded stderr:    "error: no such option\n"
```

`cmd/landscape-client-core-config/main.go` already does this correctly — this
commit generalises that pattern. `exec.ErrNotFound` is also never distinguished
anywhere.

The helper is the single place where the per-run timeout of Task 12 is enforced,
and it is written in snapd's `"cannot …"` error form, which starts Phase 4's
convention work early.

**Files:**
- Create: `internal/runcmd/runcmd.go`
- Create: `internal/runcmd/runcmd_test.go`
- Modify: `internal/monitor/hardwareinfo.go`
- Modify: `cmd/landscape-client-core/main.go`

- [ ] **Step 1: Write the failing test**

Create `internal/runcmd/runcmd_test.go`:

```go
package runcmd_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/runcmd"
)

func TestRun_ReturnsStdoutOnSuccess(t *testing.T) {
	t.Parallel()

	out, err := runcmd.Run(context.Background(), 5*time.Second, "/bin/echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("stdout: want %q, got %q", "hello", string(out))
	}
}

func TestRun_SurfacesStderr(t *testing.T) {
	t.Parallel()

	_, err := runcmd.Run(context.Background(), 5*time.Second,
		"/bin/sh", "-c", "echo 'DMI probe failed' >&2; exit 1")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "DMI probe failed") {
		t.Errorf("stderr not surfaced in error: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("exit status not surfaced in error: %v", err)
	}
}

func TestRun_DistinguishesNotFound(t *testing.T) {
	t.Parallel()

	_, err := runcmd.Run(context.Background(), 5*time.Second, "definitely-not-a-real-binary-xyz")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "executable not found") {
		t.Errorf("want an explicit not-found message, got: %v", err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return // also acceptable: the sentinel is preserved
	}
}

func TestRun_EnforcesTimeout(t *testing.T) {
	t.Parallel()

	start := time.Now()
	_, err := runcmd.Run(context.Background(), 200*time.Millisecond, "/bin/sh", "-c", "sleep 30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout not enforced: took %v for a 200ms limit", elapsed)
	}
}

func TestRun_UsesCannotConvention(t *testing.T) {
	t.Parallel()

	_, err := runcmd.Run(context.Background(), 5*time.Second, "/bin/false")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "cannot run ") {
		t.Errorf("error should follow snapd's \"cannot ...\" convention, got: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/runcmd/ -v`
Expected: FAIL — `no Go files in .../internal/runcmd`.

- [ ] **Step 3: Implement the helper**

Create `internal/runcmd/runcmd.go`:

```go
// Package runcmd runs external executables with consistent timeout and error
// handling. Every exec site in the daemon routes through it: .Output() captures
// stderr into ExitError.Stderr, but %v prints only "exit status N", so failures
// were previously logged without the one piece of information that explains them.
package runcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Run executes name with args, bounded by timeout, and returns its stdout.
// A zero timeout means no per-run bound beyond ctx.
//
// The command runs in its own process group so a timeout also kills
// grandchildren, which would otherwise survive holding the stdout pipe and
// block Wait.
func Run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("cannot run %s: executable not found", name)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cannot run %s: %w", name, ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("cannot run %s: %w: %s", name, err, bytes.TrimSpace(ee.Stderr))
	}
	return nil, fmt.Errorf("cannot run %s: %w", name, err)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/runcmd/ -v`
Expected: PASS, all five.

- [ ] **Step 5: Route the exec sites through it**

Six sites exist. Route the four that currently discard stderr:

| Site | Current | Change |
|---|---|---|
| `internal/monitor/hardwareinfo.go` `tick` | `exec.CommandContext(ctx, "lshw", "-xml", "-quiet").Output()` | `runcmd.Run(ctx, lshwTimeout, "lshw", "-xml", "-quiet")` — timeout added in Task 12 |
| `cmd/landscape-client-core/main.go` `snapctl get` | `exec.Command(...)` | `runcmd.Run(ctx, 30*time.Second, "snapctl", ...)` |
| `cmd/landscape-client-core/main.go` `snapctl set` | `CombinedOutput()` | leave as is — folding output into the error is right for a command whose diagnostics matter more than its stdout |
| `cmd/landscape-client-core-config/main.go` (3 sites) | already correct | leave as is |

The `snapctl` sites in `main.go` are called from the `snapctlLoader` and hook
paths, which have no context. Pass `context.Background()` there explicitly with a
comment saying why, or thread a context through `config.Loader` if that is a small
change — check `internal/config/config.go`'s `Loader` interface first and prefer
the smaller diff.

- [ ] **Step 6: Verify**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/runcmd/ internal/monitor/hardwareinfo.go cmd/landscape-client-core/main.go
git commit -m "feat(internal): add runCmd helper and route exec sites through it

Five of six exec sites discarded ExitError.Stderr, so 'lshw: DMI probe
failed' was logged as 'exit status 1'. exec.ErrNotFound was never
distinguished anywhere. cmd/landscape-client-core-config already did this
correctly; this generalises that pattern.

Also the single place where the per-run timeout and process-group kill are
enforced, and written in snapd's \"cannot ...\" form."
```

---

## Task 10: Report exec errors and exit status in `result-text`

In `internal/manager/system.go`, `runErr` is used only as a boolean. Its text is
never sent. When the failure happens at `fork/exec` time there is no script output
either, so the operator sees a failure with **no explanation at all**:

| Scenario | Local log | `result-text` sent |
|---|---|---|
| interpreter not executable | `fork/exec ...: permission denied` | `""` |
| interpreter is a directory | `fork/exec ...: permission denied` | `""` |
| interpreter not a valid binary | `fork/exec ...: exec format error` | `""` |
| script exits 42 | `exit status 42` | output only, exit code absent |

On Ubuntu Core the log may not be readable to whoever issued the operation, so the
Landscape UI is the only feedback channel and it shows a blank failure. Python is
comparable here, but nothing prevents Go doing better — this is purely additive to
the payload.

**Files:**
- Modify: `internal/manager/system.go`
- Modify: `internal/manager/system_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/manager/system_test.go` (package `manager_test`):

```go
// TestScriptExec_ExecFailureReportsReason asserts a fork/exec failure sends the
// reason rather than an empty result-text. On Ubuntu Core the Landscape UI may
// be the operator's only feedback channel.
func TestScriptExec_ExecFailureReportsReason(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(1),
		"code":         "echo hi\n",
		"interpreter":  notExecutable,
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.output == "" {
		t.Fatal("result-text was empty: the operator sees a blank failure")
	}
	if !strings.Contains(call.output, "permission denied") {
		t.Errorf("result-text should explain the failure, got %q", call.output)
	}
}

// TestScriptExec_NonZeroExitReportsCode asserts the exit status reaches the
// server alongside the script's own output.
func TestScriptExec_NonZeroExitReportsCode(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(2),
		"code":         "echo to-stdout; exit 42\n",
		"interpreter":  "/bin/sh",
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if !strings.Contains(call.output, "to-stdout") {
		t.Errorf("script output lost: %q", call.output)
	}
	if !strings.Contains(call.output, "42") {
		t.Errorf("exit status 42 not reported: %q", call.output)
	}
}
```

`mockResultSink` in `package manager_test` lives in
`internal/manager/snap_test.go` with a `lastCall()` accessor returning a
`resultCall`. Check that struct's field names and use them; there is a second,
different `mockResultSink` in `internal/manager/runner_test.go` (package
`manager`) — do not confuse them.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/manager/ -run TestScriptExec_ExecFailureReportsReason -v`
Expected: FAIL with `result-text was empty: the operator sees a blank failure`.

- [ ] **Step 3: Report the reason**

In `internal/manager/system.go`, replace the failure branch:

```go
	if runErr != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103, output)
		return nil
	}
```

with:

```go
	if runErr != nil {
		text := output
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			// The script ran and failed: keep its output, append the exit status
			// so the operator can tell 42 from 1.
			text = fmt.Sprintf("%s\nexit status %d", output, exitErr.ExitCode())
		default:
			// The interpreter could not be executed at all, so there is no script
			// output to report — without this the Landscape UI shows a blank
			// failure with no explanation.
			text = fmt.Sprintf("execute-script: cannot run interpreter %s: %v", interpreterBin, runErr)
		}
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103, text)
		return nil
	}
```

`errors` and `os/exec` are already imported in this file.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/manager/ -v`
Expected: PASS, including the Phase 1 time-limit tests, which assert result-code
102 and partial output and must be unaffected.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/system.go internal/manager/system_test.go
git commit -m "fix(manager): report exec errors and exit status in result-text

runErr was used only as a boolean, so a fork/exec failure sent result-code
103 with an empty result-text — and on Ubuntu Core the Landscape UI may be
the operator's only feedback channel, so the failure was simply blank.

Distinguishes 'interpreter could not be executed' from 'script ran and
failed', and reports the exit status. Purely additive to the payload."
```

---

## Task 11: Validate the server-supplied interpreter

`internal/manager/system.go` does:

```go
	interpreterFields := strings.Fields(interpreter)
	interpreterBin := interpreterFields[0]
```

The `== ""` default above it does not catch `" "`, `"\t"` or `"\n"` —
`strings.Fields` returns an empty slice for all of them, so the index panics.
Reproduced through the real handler:

```
PANIC escaped Handle: runtime error: index out of range [0] with length 0
```

`manager.Runner`'s `recover()` catches it, so the daemon survives and the server
receives `panic: runtime error: index out of range [0] with length 0` as the
operator's result text. That containment is why this is P1 and not P0 — but a
server-supplied field reaching an index-out-of-range is a validation gap.

Same function: the `os.Stat` executability check is insufficient — it passes for
directories and non-executable files, which then fail later at `fork/exec`.

**Files:**
- Modify: `internal/manager/system.go`
- Modify: `internal/manager/system_test.go`
- Modify: `go.mod`

- [ ] **Step 1: Write the failing test**

Add to `internal/manager/system_test.go`:

```go
// TestScriptExec_WhitespaceInterpreter asserts a whitespace-only interpreter is
// treated as absent rather than panicking. strings.Fields returns an empty
// slice for all of these, and the code indexed [0] unguarded.
func TestScriptExec_WhitespaceInterpreter(t *testing.T) {
	tests := []struct {
		name        string
		interpreter string
	}{
		{"empty", ""},
		{"space", " "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"spaces", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := manager.NewScriptExecHandler(t.TempDir(), nil)
			sink := &mockResultSink{}

			msg := exchange.Message{
				"type":         "execute-script",
				"operation-id": int64(1),
				"code":         "echo ran-ok\n",
				"interpreter":  tt.interpreter,
			}

			if err := h.Handle(context.Background(), msg, sink); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			call, ok := sink.lastCall()
			if !ok {
				t.Fatal("no result sent")
			}
			if strings.Contains(call.output, "panic") {
				t.Fatalf("handler panicked; operator sees a Go runtime error: %q", call.output)
			}
			if !strings.Contains(call.output, "ran-ok") {
				t.Errorf("script should have run under the default interpreter, got %q", call.output)
			}
		})
	}
}

// TestScriptExec_DirectoryInterpreter asserts a directory is rejected before
// exec with a specific reason, rather than passing os.Stat and failing later.
func TestScriptExec_DirectoryInterpreter(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(2),
		"code":         "echo hi\n",
		"interpreter":  t.TempDir(),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if !strings.Contains(call.output, "not executable") {
		t.Errorf("want an explicit not-executable message, got %q", call.output)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/manager/ -run TestScriptExec_WhitespaceInterpreter -v`
Expected: FAIL — the `space`, `tab`, `newline` and `spaces` subtests report a
panic in the result text.

- [ ] **Step 3: Promote `golang.org/x/sys` to a direct dependency**

`unix.Access` needs `golang.org/x/sys/unix`, currently an **indirect** dependency
(`v0.27.0`). It is already in the module graph and is Go-team owned, so promoting
it adds no new supply-chain surface.

Run:

```bash
go get golang.org/x/sys@v0.27.0
go mod tidy
grep -n 'golang.org/x/sys' go.mod
```

Expected: `golang.org/x/sys v0.27.0` now appears in the direct `require` block
without the `// indirect` marker.

- [ ] **Step 4: Validate the interpreter**

In `internal/manager/system.go`, replace:

```go
	interpreter, _ := getString(msg, "interpreter")
	if interpreter == "" {
		interpreter = "/bin/sh"
	}

	interpreterFields := strings.Fields(interpreter)
	interpreterBin := interpreterFields[0]
	interpreterArgs := interpreterFields[1:]
```

with:

```go
	// Whitespace-only is absent: strings.Fields returns an empty slice for " ",
	// "\t" and "\n", which the == "" check does not catch.
	interpreter, _ := getString(msg, "interpreter")
	if strings.TrimSpace(interpreter) == "" {
		interpreter = "/bin/sh"
	}

	interpreterFields := strings.Fields(interpreter)
	if len(interpreterFields) == 0 {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			"execute-script: cannot determine interpreter")
		return nil
	}
	interpreterBin := interpreterFields[0]
	interpreterArgs := interpreterFields[1:]
```

The explicit length guard is belt-and-braces after the `TrimSpace` default, and it
is what makes the code obviously safe to a reader.

- [ ] **Step 5: Replace the `os.Stat` executability check**

Replace:

```go
	if _, err := os.Stat(interpreterBin); err != nil {
		_ = result.SendResult(ctx, opID, exchange.StatusFailed,
			fmt.Sprintf("execute-script: interpreter not found: %s", interpreterBin))
		return nil
	}
```

with:

```go
	// os.Stat passes for directories and non-executable files, which then fail
	// later at fork/exec with a less specific message.
	if fi, err := os.Stat(interpreterBin); err != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			fmt.Sprintf("execute-script: interpreter not found: %s", interpreterBin))
		return nil
	} else if fi.IsDir() {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			fmt.Sprintf("execute-script: interpreter %s is not executable: is a directory", interpreterBin))
		return nil
	}
	if err := unix.Access(interpreterBin, unix.X_OK); err != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			fmt.Sprintf("execute-script: interpreter %s is not executable: %v", interpreterBin, err))
		return nil
	}
```

Add `"golang.org/x/sys/unix"` to the imports.

Note this changes the not-found case from `SendResult` to `SendResultCode` with
103, making it consistent with every other failure path in this handler. Check
whether an existing test asserts the old shape and update it if so.

- [ ] **Step 6: Run to verify it passes**

Run: `go test -race ./internal/manager/ -v`
Expected: PASS, all subtests.

- [ ] **Step 7: Commit**

```bash
git add internal/manager/system.go internal/manager/system_test.go go.mod go.sum
git commit -m "fix(manager): validate the server-supplied interpreter

strings.Fields returns an empty slice for \" \", \"\\t\" and \"\\n\", which
the == \"\" default did not catch, so interpreterFields[0] panicked on a
server-supplied field. Runner's recover() contained it, but the operator's
result text became a Go runtime error.

Also replaces the os.Stat executability check — which passes for directories
and non-executable files — with unix.Access(X_OK), reporting the specific
reason instead of failing later at fork/exec.

Promotes golang.org/x/sys from indirect to direct: already in the module
graph, Go-team owned, no new supply-chain surface."
```

---

## Task 12: Bound and validate `lshw`

Two defects in `internal/monitor/hardwareinfo.go`.

**No per-run timeout.** `tick` passes the daemon-lifetime `ctx`, so the context
only cancels at shutdown. `lshw` probes PCI, USB, DMI and SCSI and can wedge on a
misbehaving device, blocking this goroutine for the life of the process. It also
runs immediately at startup, competing for CPU and IO during Core boot.

**No output validation.** Exit code 0 is treated as sufficient, so zero-length or
truncated stdout is forwarded verbatim:

```
empty-but-success:     err=<nil> len(out)=0
  => sends {"type":"hardware-info","data":<empty>} to the server
truncated XML, exit 0: len=11 content="<list><node"
  => sent verbatim; no XML validation
```

`lshw` under strict confinement can be partially denied by AppArmor and still exit
0, so this is reachable — and sending empty `hardware-info` may cause the server to
overwrite good inventory with nothing.

**Files:**
- Modify: `internal/monitor/hardwareinfo.go`
- Modify: `internal/monitor/sysinfo_test.go`

- [ ] **Step 1: Make the command injectable**

`hardwareinfo.go` calls `exec.CommandContext` directly, so the failure modes above
cannot be tested. Add a field:

```go
type HardwareInfo struct {
	interval time.Duration
	// run is injectable so the empty-output and truncated-XML cases — which
	// require an AppArmor denial to reproduce naturally — are testable.
	run func(ctx context.Context) ([]byte, error)
}

func NewHardwareInfo() *HardwareInfo {
	return &HardwareInfo{
		interval: 24 * time.Hour,
		run: func(ctx context.Context) ([]byte, error) {
			return runcmd.Run(ctx, lshwTimeout, "lshw", "-xml", "-quiet")
		},
	}
}

// lshwTimeout bounds a single lshw run. It probes PCI, USB, DMI and SCSI and can
// wedge on a misbehaving device; the plugin holds the daemon-lifetime context,
// so without this the goroutine is blocked for the life of the process.
const lshwTimeout = 2 * time.Minute
```

Preserve the existing `interval` value — read it from the current constructor
rather than assuming 24h.

- [ ] **Step 2: Write the failing test**

Add to `internal/monitor/sysinfo_test.go`:

```go
func TestHardwareInfo_RejectsEmptyOutput(t *testing.T) {
	p := &HardwareInfo{
		interval: time.Hour,
		run: func(_ context.Context) ([]byte, error) {
			return []byte{}, nil // exit 0, no output: a partial AppArmor denial
		},
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-errCh

	if n := len(sink.messages()); n != 0 {
		t.Errorf("sent %d hardware-info messages for empty lshw output; the server may overwrite good inventory with nothing", n)
	}
}

func TestHardwareInfo_RejectsTruncatedXML(t *testing.T) {
	p := &HardwareInfo{
		interval: time.Hour,
		run: func(_ context.Context) ([]byte, error) {
			return []byte("<list><node"), nil
		},
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-errCh

	if n := len(sink.messages()); n != 0 {
		t.Errorf("sent %d hardware-info messages for truncated XML", n)
	}
}

func TestHardwareInfo_SendsValidXML(t *testing.T) {
	const valid = `<list><node id="test"><description>Computer</description></node></list>`
	p := &HardwareInfo{
		interval: time.Hour,
		run: func(_ context.Context) ([]byte, error) {
			return []byte(valid), nil
		},
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	<-errCh

	if msgs[0]["type"] != "hardware-info" {
		t.Errorf("type: want hardware-info, got %v", msgs[0]["type"])
	}
	if string(msgs[0]["data"].([]byte)) != valid {
		t.Errorf("data was altered in transit")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestHardwareInfo_Rejects -v`
Expected: FAIL — `sent 1 hardware-info messages for empty lshw output...`

- [ ] **Step 4: Validate before sending**

Rewrite `tick`:

```go
func (p *HardwareInfo) tick(ctx context.Context, sink exchange.MessageSink) {
	out, err := p.run(ctx)
	if err != nil {
		log.Printf("hardware-info: %v", err)
		return
	}
	if err := validateLshwXML(out); err != nil {
		// lshw under strict confinement can be partially AppArmor-denied and
		// still exit 0. Sending empty or truncated inventory can make the server
		// overwrite good data with nothing.
		log.Printf("hardware-info: discarding lshw output: %v", err)
		return
	}
	msg := exchange.Message{
		"type": "hardware-info",
		"data": out,
	}
	if err := sink.Send(ctx, msg); err != nil {
		log.Printf("hardware-info: send: %v", err)
	}
}

func validateLshwXML(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("lshw produced no output")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lshw output is not valid XML: %w", err)
		}
	}
}
```

Add `"bytes"`, `"encoding/xml"`, `"errors"` and `"io"` to the imports.

Token-scanning rather than `xml.Unmarshal` into a struct is deliberate: it
validates well-formedness without constraining the schema, so a future `lshw`
version adding elements does not start failing validation.

- [ ] **Step 5: Run to verify it passes**

Run: `go test -race ./internal/monitor/ -run TestHardwareInfo -v`
Expected: PASS, all three.

- [ ] **Step 6: Verify against real lshw**

Run:

```bash
lshw -xml -quiet | head -c 200
```

Expected: well-formed XML beginning `<?xml ...` or `<list>`. If `lshw` is not
installed locally, note that the snap stages it and rely on the injected tests.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/hardwareinfo.go internal/monitor/sysinfo_test.go
git commit -m "fix(monitor): bound and validate lshw

tick passed the daemon-lifetime context, so there was no per-run timeout;
lshw probes PCI/USB/DMI/SCSI and can wedge on a misbehaving device, blocking
the goroutine for the life of the process.

Exit 0 was also treated as sufficient, so empty or truncated stdout was
forwarded verbatim. lshw under strict confinement can be partially
AppArmor-denied and still exit 0, and sending empty hardware-info may make
the server overwrite good inventory with nothing.

Command is now injectable so both cases are testable on a dev host."
```

---

## Task 13: Correct 32-bit network counter rollover

`internal/monitor/networkactivity.go`'s `delta` clamps a negative delta to `0`:

```go
		// Clamp rollover or counter resets to zero.
		if rxDelta < 0 {
			rxDelta = 0
		}
```

Python adds back `2**32` on 32-bit systems
(`landscape/client/monitor/networkactivity.py:21,37-39,97-99`, with an explicit
comment that 64-bit does not roll over). On 32-bit armhf Core devices, every 4 GiB
of traffic per interface silently vanishes from reporting.

Per the dev-host-only validation constraint, the correction is tested as pure
arithmetic rather than on real hardware.

**Files:**
- Modify: `internal/monitor/networkactivity.go`
- Modify: `internal/monitor/metrics_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/monitor/metrics_test.go` (package `monitor`):

```go
func TestCounterDelta_NormalIncrement(t *testing.T) {
	if got := counterDelta(2000, 1000); got != 1000 {
		t.Errorf("want 1000, got %d", got)
	}
}

// TestCounterDelta_32BitRollover: on armhf the kernel counter wraps at 2^32, so
// a negative raw delta with a previous value near MaxUint32 is a wrap, not a
// reset. Clamping it to zero loses 4 GiB of traffic per interface per wrap.
func TestCounterDelta_32BitRollover(t *testing.T) {
	const maxUint32 = int64(1) << 32

	prev := maxUint32 - 1000 // 4294966296
	now := int64(500)        // wrapped

	want := int64(1500) // 1000 to the wrap point, then 500 more
	if got := counterDelta(now, prev); got != want {
		t.Errorf("want %d, got %d", want, got)
	}
}

// TestCounterDelta_GenuineReset: an interface re-created from scratch has a low
// previous value, so a negative delta there is a reset and must still clamp.
func TestCounterDelta_GenuineReset(t *testing.T) {
	prev := int64(5000)
	now := int64(10)

	if got := counterDelta(now, prev); got != 0 {
		t.Errorf("a genuine counter reset should clamp to 0, got %d", got)
	}
}

func TestCounterDelta_NoChange(t *testing.T) {
	if got := counterDelta(1000, 1000); got != 0 {
		t.Errorf("want 0, got %d", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestCounterDelta -v`
Expected: FAIL — compile error, `undefined: counterDelta`.

- [ ] **Step 3: Implement the correction**

Add to `internal/monitor/networkactivity.go`:

```go
// counterWrapThreshold is how close to 2^32 the previous value must be for a
// negative delta to be treated as a wrap rather than a counter reset. A
// re-created interface starts near zero, so its previous value is nowhere near
// the threshold.
const (
	counterWrap          = int64(1) << 32
	counterWrapThreshold = counterWrap - (1 << 24) // within 16 MiB of the wrap point
)

// counterDelta returns the traffic between two samples of a kernel byte counter.
//
// On 32-bit systems (armhf Core devices) the counter wraps at 2^32, so a
// negative raw delta following a near-maximum previous value is a wrap and the
// wrapped amount must be added back — clamping it loses 4 GiB per interface per
// wrap. A negative delta from a low previous value is a genuine reset (interface
// re-created) and still clamps to zero. 64-bit counters do not wrap.
func counterDelta(now, prev int64) int64 {
	d := now - prev
	if d >= 0 {
		return d
	}
	if prev >= counterWrapThreshold {
		return d + counterWrap
	}
	return 0
}
```

Then replace the clamping in `delta`:

```go
		rxDelta := counterDelta(rxNow, lastRx)
		txDelta := counterDelta(txNow, lastTx)
```

removing the two `if ... < 0 { ... = 0 }` blocks.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/monitor/ -run TestCounterDelta -v`
Expected: PASS, all four.

- [ ] **Step 5: Verify the plugin's own tests still pass**

Run: `go test -race ./internal/monitor/ -v`
Expected: PASS, including the existing `NetworkActivity` tests.

- [ ] **Step 6: Commit**

```bash
git add internal/monitor/networkactivity.go internal/monitor/metrics_test.go
git commit -m "fix(monitor): correct 32-bit network counter rollover

A negative delta was clamped to zero, so on 32-bit armhf Core devices every
4 GiB of traffic per interface silently vanished from reporting. Python adds
back 2**32 in the same situation.

The clamp is kept only for genuine counter resets, distinguished by whether
the previous value was near the wrap point — a re-created interface starts
near zero."
```

---

## Task 14: Verify the phase

- [ ] **Step 1: Full verification**

Run:

```bash
gofmt -l .
go vet ./...
go test -race ./...
golangci-lint run
```

Expected: all clean.

- [ ] **Step 2: Confirm no unbounded blocking call remains**

Run:

```bash
grep -rn 'ConnectSystemBus()\|obj.Call(' internal/ || echo "D-Bus bounded"
grep -rn 'http.Client{' internal/ | grep -v Timeout || echo "HTTP clients bounded"
grep -rn 'exec.Command(' internal/ cmd/ || echo "no context-free exec in internal/"
```

Expected: the three "bounded" messages, or only hits you can justify in the PR
description.

- [ ] **Step 3: Confirm the error-discard fixes hold**

Run:

```bash
grep -rn '_ = state.SetPluginState\|_ = state.GetPluginState' internal/ || echo "clean"
```

Expected: `clean`.

- [ ] **Step 4: Check the dependency tree did not grow**

Run: `go mod graph | cut -d' ' -f2 | cut -d'@' -f1 | sort -u`
Expected: the same four modules as before — `github.com/godbus/dbus/v5`,
`golang.org/x/sync`, `golang.org/x/sys`, `golang.org/x/term`. `golang.org/x/sys`
moved from indirect to direct; nothing new was added.

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin fix/02-p1-reliability
```

PR title: `Phase 2: P1 reliability — timeouts, watchdog, state integrity, exec error handling`

---

## Done when

- Every HTTP, snapd and D-Bus call is bounded by a timeout, and no `context.Background()` remains in the daemon path.
- `Runner.Run` reports plugin collapse; clean shutdown still returns `nil`.
- A wedged plugin is detected by the heartbeat watchdog and the daemon exits for restart.
- A transient `passwd` read failure emits no `delete-users`.
- A failed plugin-state save cannot roll back `SecureID` or `OutboundSequence`, and does not advance the in-memory hash.
- A corrupt state file recovers from `.old`.
- Every `exec` failure surfaces stderr, the exit status, or an explicit not-found message.
- A whitespace-only interpreter runs under `/bin/sh` instead of panicking.
- Empty or truncated `lshw` output is discarded rather than sent.
- A 32-bit counter wrap reports the real traffic; a genuine reset still clamps.
