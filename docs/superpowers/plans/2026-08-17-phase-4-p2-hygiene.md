# Phase 4 — P2 Hygiene Implementation Plan

> **For agentic workers:** REQUIRED: Use the `subagent-driven-development` agent (recommended) or `executing-plans` agent to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the configured log level actually work, make the linter enforce the conventions the codebase claims, remove drift and dead weight from the snap, and pay down the test and idiom debt the review identified.

**Architecture:** The slog migration goes first because it touches nearly every file. The linter expansion goes second so `modernize` mechanically verifies the idiom pass that goes last. Between them sit four small, independent commits (error convention, `curl`, version single-sourcing, `cmd` coverage) and two measurement-driven ones (test debt, runtime tuning).

**Tech Stack:** Go 1.25, `log/slog`, `slices`, `maps`, `strings.Cut`, golangci-lint v2, snapcraft

**Spec:** [docs/superpowers/specs/2026-08-17-code-review-remediation-design.md](../specs/2026-08-17-code-review-remediation-design.md)

**Branch:** `fix/04-p2-hygiene`, cut from `fix/03-p1-efficiency`

---

## Line-reference caution

Every line number in this plan comes from the review dated 2026-07-31 against
commit `c8c76ce`. The baseline is now `a1cfeae`, and Phases 1–3 land first —
several of these files are substantially rewritten by then, particularly
`internal/monitor/activeprocessinfo.go`, every plugin's `Run` method (now using
`runTicker`), and `internal/exchange/exchange.go`.

**Locate code by function and content, not by line number.** Where a step cites a
line, treat it as a hint and verify with `grep` before editing. If a reference has
moved or the code no longer exists because an earlier phase changed it, say so in
the commit message rather than forcing the edit.

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `internal/**/*.go`, `cmd/**/*.go` | Modify | `log.Printf` → `slog` |
| `.golangci.yml` | Modify | Expand the linter set |
| `snap/snapcraft.yaml` | Modify | Drop `curl`; `-ldflags -X` version; `GOMEMLIMIT`/`GOMAXPROCS` |
| `internal/version/version.go` | Modify | `const` → `var` so `-ldflags -X` works |
| `cmd/landscape-client-core/main.go` | Modify | Simplify shutdown |
| `cmd/landscape-client-core/run_test.go` | Modify | Raise `cmd` coverage |
| `internal/ping/ping_test.go` | Modify | Port `test_ping.py` scenarios |
| `internal/monitor/sysinfo_test.go` | Modify | Port message-shape assertions |
| `internal/manager/*.go` | Modify | Inject a clock to cut 20s of real sleeps |
| `internal/monitor/*.go` | Modify | Idiom pass |

---

## Task 0: Create the branch

- [ ] **Step 1: Cut the branch**

```bash
git checkout fix/03-p1-efficiency
git checkout -b fix/04-p2-hygiene
```

- [ ] **Step 2: Record the starting counts**

Run:

```bash
grep -rn 'log\.Printf' --include=*.go internal cmd | wc -l
grep -rn 'slog\.' --include=*.go internal cmd | wc -l
grep -rn 'slices\.\|maps\.' --include=*.go internal cmd | wc -l
```

Expected at the `a1cfeae` baseline: 87, ~22, 0. Phases 1–3 will have shifted
these; record the actual numbers, because Tasks 1 and 9 are measured against them.

---

## Task 1: Complete the slog migration

Commit `e9af620` claims "consolidate logging to use slog throughout" but did not.
`log.Printf` bypasses the configured slog handler entirely, so the `log-level`
setting **silently does nothing** for any package output — which is why per-tick
plugin chatter still appears at `log-level=error`.

This goes first because it touches nearly every file; running it after the idiom
pass would guarantee conflicts.

**Files:**
- Modify: every file containing `log.Printf` under `internal/` and `cmd/`
- Modify: `cmd/landscape-client-core/run_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/landscape-client-core/run_test.go`:

```go
// TestLogLevel_SuppressesBelowThreshold asserts the configured level actually
// filters output. log.Printf bypasses the slog handler, so log-level did
// nothing for any package that used it.
func TestLogLevel_SuppressesBelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	handler := slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelError})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	})

	slog.Info("this must not appear")
	slog.Debug("nor this")
	slog.Error("this must appear")

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	if strings.Contains(out, "must not appear") || strings.Contains(out, "nor this") {
		t.Errorf("log-level=error did not suppress lower levels:\n%s", out)
	}
	if !strings.Contains(out, "this must appear") {
		t.Errorf("log-level=error suppressed an error:\n%s", out)
	}
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
```

This test passes today — it only exercises `slog`. Its value is as a guard: pair
it with the grep assertion in Step 5, which is the real check.

- [ ] **Step 2: Confirm the scale of the problem**

Run:

```bash
grep -rln 'log\.Printf' --include=*.go internal cmd
grep -rn 'log\.Printf' --include=*.go internal/monitor | wc -l
```

Expected: a list of files, and 56 hits in `internal/monitor` alone at the
`a1cfeae` baseline (all 16 files).

- [ ] **Step 3: Migrate, choosing levels deliberately**

This is not a mechanical `log.Printf` → `slog.Info` substitution. Map by meaning:

| Current call site | Level | Reason |
|---|---|---|
| Per-tick plugin chatter (`cpu-usage: %v`, `network-device: send: %v`) | `slog.Debug` | It is the reason `log-level=error` is noisy today |
| Send and collect failures the plugin recovers from | `slog.Warn` | Actionable but not fatal |
| Plugin panics, runner failures, state save failures | `slog.Error` | Requires attention |
| Registration and exchange lifecycle (`registered successfully`, `accepted-types: N types`) | `slog.Info` | Operator-meaningful, low volume |

Convert printf formatting to attributes:

```go
	log.Printf("exchange: server ACK'd %d/%d messages (our seq=%d, server wants=%d); re-queuing %d",
		nAcked, len(snapshot), state.OutboundSequence, serverACK, len(snapshot)-nAcked)
```

becomes:

```go
	slog.Info("exchange: partial ACK, re-queuing",
		"acked", nAcked,
		"sent", len(snapshot),
		"our_sequence", state.OutboundSequence,
		"server_expects", serverACK,
		"requeued", len(snapshot)-nAcked)
```

Keep the message prefix (`exchange:`, `cpu-usage:`) — it identifies the component
and the existing convention is consistent.

- [ ] **Step 4: Remove the `log` import from every migrated file**

Run: `go build ./...`
Expected: no output. Any `imported and not used: "log"` error names a file where
the import was left behind.

- [ ] **Step 5: Verify no `log.Printf` remains**

Run:

```bash
grep -rn 'log\.Printf\|log\.Println\|log\.Fatal' --include=*.go internal cmd || echo "migration complete"
```

Expected: `migration complete`.

- [ ] **Step 6: Verify the suite still passes**

Run: `go test -race ./...`
Expected: PASS. Tests that capture stdlib `log` output will fail — update them to
capture the `slog` handler instead. Note the repo already fixed race-prone log
capture once (commit `8c195f7`), so check how those tests do it and follow the
same approach.

- [ ] **Step 7: Commit**

```bash
git add internal/ cmd/
git commit -m "refactor: complete the slog migration

e9af620 claimed to 'consolidate logging to use slog throughout' but left 87
log.Printf sites, including 56 across all 16 files in internal/monitor.
log.Printf bypasses the configured handler, so the log-level setting silently
did nothing — which is why per-tick chatter still appeared at
log-level=error.

Levels are chosen by meaning rather than substituted mechanically: per-tick
plugin chatter is Debug, recoverable failures Warn, panics and state-save
failures Error, lifecycle events Info."
```

---

## Task 2: Expand the golangci-lint linter set

`.golangci.yml` enables only `errcheck`, `staticcheck` and `govet`. snapd's
configuration is considerably broader. `modernize` in particular will flag most of
what Task 9 does by hand, which is why this sequencing was chosen — the idiom pass
becomes verified rather than eyeballed.

**Files:**
- Modify: `.golangci.yml`

- [ ] **Step 1: Read the current config**

Run: `cat .golangci.yml`
Expected:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - staticcheck
    - govet

run:
  go: "1.25"
```

- [ ] **Step 2: Expand it**

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - misspell
    - modernize
    - nakedret
    - staticcheck
    - testpackage
    - unused
    - depguard

  settings:
    depguard:
      rules:
        main:
          # The small dependency tree is a deliberate property of this project;
          # keep it closed rather than discovering additions in review.
          allow:
            - $gostd
            - github.com/canonical/landscape-client-core
            - github.com/godbus/dbus/v5
            - golang.org/x/sync
            - golang.org/x/sys
            - golang.org/x/term

  exclusions:
    rules:
      # Several packages test unexported logic and are deliberately internal test
      # packages; the review explicitly endorses that where it is intentional.
      - path: internal/(monitor|exchange|transport|bpickle|persist)/.*_test\.go
        linters:
          - testpackage

run:
  go: "1.25"
```

Verify the `exclusions` schema against the installed golangci-lint v2 version —
the v2 config format differs from v1, and getting this wrong silently disables the
rule rather than erroring. Check with `golangci-lint config verify` if that
subcommand exists in the pinned version.

- [ ] **Step 3: Run the linter and count the findings**

Run: `golangci-lint run 2>&1 | tail -5`
Expected: a non-zero number of findings — `modernize` alone will flag `sort.Slice`,
`interface{}`, and pre-`slices` loop patterns across `internal/monitor`.

Record the count. If it is large (say above 100), fix them in this commit anyway —
they are mechanical, and deferring them means the linter is not gating for the
rest of the phase, which is the exact failure Phase 0 was fixing.

- [ ] **Step 4: Apply the automatic fixes**

Run: `golangci-lint run --fix`
Then re-run without `--fix` and fix the remainder by hand.

- [ ] **Step 5: Verify**

Run:

```bash
golangci-lint run
gofmt -l .
go test -race ./...
```

Expected: all clean.

- [ ] **Step 6: Commit**

```bash
git add .golangci.yml internal/ cmd/
git commit -m "ci: expand the golangci-lint linter set

Only errcheck, staticcheck and govet were enabled. Adds misspell, modernize,
nakedret, testpackage, unused and depguard, the last configured to keep the
four-module dependency tree closed rather than discovering additions in
review.

testpackage is excluded for the packages that deliberately test unexported
logic. modernize does mechanically most of what the idiom pass would do by
hand, which is why it lands before it."
```

---

## Task 3: Adopt snapd's "cannot …" error convention

The tree has **zero** instances of either `cannot` or `failed to`; errors are bare
gerunds such as `"exchange: posting to server: %w"` and `"decoding response: %w"`.
snapd runs 939 `cannot` to 69 `failed to`.

The existing style is already consistent, lowercase and unpunctuated, so this is a
rewording pass rather than a restructure. The `runCmd` helper added in Phase 2
already uses the target form.

**Files:**
- Modify: files containing `fmt.Errorf` under `internal/` and `cmd/`

- [ ] **Step 1: Check no test asserts on the current strings**

Run:

```bash
grep -rn 'Contains(err.Error()\|err.Error() ==\|ErrorContains' --include=*_test.go internal cmd
```

Inspect each hit. Where a test asserts on wording this task changes, update the
test in the same commit — but prefer asserting on a sentinel or type where one
exists, since that is what made the assertion brittle.

- [ ] **Step 2: Survey the current errors**

Run: `grep -rn 'fmt.Errorf(' --include=*.go internal cmd | wc -l`
Record the count.

- [ ] **Step 3: Reword**

Examples of the transformation:

```go
	return fmt.Errorf("exchange: posting to server: %w", err)
	return fmt.Errorf("transport: creating request: %w", err)
	return fmt.Errorf("persist: writing temp file: %w", err)
	return fmt.Errorf("opening %s: %w", p.mountsPath, err)
```

become:

```go
	return fmt.Errorf("exchange: cannot post to server: %w", err)
	return fmt.Errorf("transport: cannot create request: %w", err)
	return fmt.Errorf("persist: cannot write temp file: %w", err)
	return fmt.Errorf("cannot open %s: %w", p.mountsPath, err)
```

Keep the component prefix where one exists. Keep errors lowercase and
unpunctuated. Do not change error *types* or wrapping — 77 uses of `%w` are
already better than snapd's own practice and must be preserved.

- [ ] **Step 4: Verify**

Run:

```bash
grep -rn 'fmt.Errorf("[a-z]*: [a-z]*ing ' --include=*.go internal cmd || echo "no gerund-style errors remain"
go test -race ./...
golangci-lint run
```

Expected: `no gerund-style errors remain`, and a clean suite.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "refactor: adopt snapd's \"cannot ...\" error convention

The tree had zero instances of either 'cannot' or 'failed to'; errors were
bare gerunds. snapd runs 939 'cannot' to 69 'failed to'.

Rewording only: error types, %w wrapping, component prefixes and the
lowercase/unpunctuated style are all unchanged."
```

---

## Task 4: Drop unused `curl` from `stage-packages`

`snap/snapcraft.yaml` stages both `lshw` and `curl`. `lshw` is genuinely used by
`internal/monitor/hardwareinfo.go`; `curl` is referenced nowhere. Dead payload
weight plus needless CVE surface in a strictly-confined snap.

**Files:**
- Modify: `snap/snapcraft.yaml`

- [ ] **Step 1: Prove `curl` is unused**

Run:

```bash
grep -rn 'curl' --include=*.go internal cmd
grep -rn 'curl' snap/hooks/ Makefile 2>/dev/null
grep -rn 'curl' snap/snapcraft.yaml
```

Expected: no hits from the first two; one hit in `snapcraft.yaml`. If any other
hit appears — a hook or a Makefile target — stop and keep `curl`.

- [ ] **Step 2: Prove `lshw` is used**

Run: `grep -rn 'lshw' --include=*.go internal cmd`
Expected: at least one hit in `internal/monitor/hardwareinfo.go`.

- [ ] **Step 3: Remove it**

```yaml
    stage-packages:
      - lshw
```

- [ ] **Step 4: Verify the snap still builds**

Run: `snapcraft --destructive-mode 2>&1 | tail -20`
Expected: a successful build. If `snapcraft` is unavailable locally, run
`grep -c 'stage-packages' -A3 snap/snapcraft.yaml` to confirm the edit and note in
the PR that the build was not exercised locally.

- [ ] **Step 5: Commit**

```bash
git add snap/snapcraft.yaml
git commit -m "chore(snap): drop unused curl from stage-packages

Nothing in the Go source, hooks or Makefile references curl. lshw on the
adjacent line is genuinely used by hardwareinfo and stays. Dead payload plus
needless CVE surface in a strictly-confined snap."
```

---

## Task 5: Single-source the version string

`internal/version/version.go` declares `26.08~beta.2`; `snap/snapcraft.yaml`
declares `version: 26.04`. `version.Version` goes out as `User-Agent`, and the
**server** gates snap-monitoring support on it, so drift is server-visible.

**There is a blocker the review does not mention:** both identifiers are `const`.

```go
const Version = "26.08~beta.2"
const UserAgent = "landscape-client/" + Version
```

`-ldflags -X` can only set a **`var` of type string**, and it cannot set a value
computed by constant folding. Both declarations must change.

**Files:**
- Modify: `internal/version/version.go`
- Modify: `snap/snapcraft.yaml`
- Create: `internal/version/version_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/version/version_test.go`:

```go
package version

import (
	"strings"
	"testing"
)

func TestUserAgent_TracksVersion(t *testing.T) {
	if !strings.HasSuffix(UserAgent, Version) {
		t.Errorf("UserAgent %q does not carry Version %q", UserAgent, Version)
	}
	if !strings.HasPrefix(UserAgent, "landscape-client/") {
		t.Errorf("UserAgent %q lost the expected prefix; the server parses this", UserAgent)
	}
}

func TestVersion_IsOverridable(t *testing.T) {
	// -ldflags -X requires a var, not a const. A const would make the snap build
	// silently ship the hard-coded default while snapcraft.yaml says otherwise.
	orig := Version
	defer func() { Version = orig }()

	Version = "99.99"
	if Version != "99.99" {
		t.Fatal("Version is not assignable; -ldflags -X cannot override a const")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/version/ -v`
Expected: FAIL — compile error, `cannot assign to Version (neither addressable nor a map index expression)`.

- [ ] **Step 3: Convert to overridable vars**

```go
package version

// Version is the landscape-client version string expected by the server.
// Overridden at build time via -ldflags -X, with snapcraft.yaml as the single
// source of truth. Must be a var, not a const: -ldflags cannot set a const.
var Version = "0.0.0-dev"

// UserAgent is the HTTP header value sent to the Landscape server for
// compatibility checks. Derived at init rather than by constant folding, so an
// overridden Version is reflected here too.
var UserAgent = "landscape-client/" + Version
```

The `0.0.0-dev` default is deliberate: a plain `go build` should be obviously
unversioned rather than claiming a release number. Note the server gates
snap-monitoring on `>= 23.02+git6282`, so a dev build will not be treated as
snap-capable — that is correct for a dev build, but say it in the PR so nobody is
surprised.

**Ordering caveat:** package-level `var` initialisation means `UserAgent` is
computed from the *default* `Version` unless `-ldflags -X` also sets `UserAgent`.
Two options:

- (a) set both via `-ldflags`, which duplicates the prefix in the build command;
- (b) make `UserAgent` a function: `func UserAgent() string { return "landscape-client/" + Version }`.

**Prefer (b)** — it cannot go stale. It requires updating the call site in
`cmd/landscape-client-core/main.go` (`UserAgent: version.UserAgent` becomes
`version.UserAgent()`) and adjusting the test above. Confirm there is only one
call site:

Run: `grep -rn 'version.UserAgent' --include=*.go .`

- [ ] **Step 4: Inject the version in the snap build**

In `snap/snapcraft.yaml`'s `override-build`:

```yaml
    override-build: |
      go build -ldflags="-s -w -X github.com/canonical/landscape-client-core/internal/version.Version=$CRAFT_PROJECT_VERSION" -trimpath -o "$CRAFT_PART_INSTALL/bin/" ./cmd/landscape-client-core/ ./cmd/landscape-client-core-config/
```

`$CRAFT_PROJECT_VERSION` is set by snapcraft from the top-level `version:` key,
making `snapcraft.yaml` authoritative. Verify that variable name against the
`core24` snapcraft documentation before relying on it; if it differs, use the
correct one rather than hard-coding the number again.

- [ ] **Step 5: Decide the version number**

`snapcraft.yaml` currently says `26.04` and `version.go` says `26.08~beta.2`. These
must be reconciled deliberately, not silently. Raise which is correct; if
`26.08~beta.2` is the true current version, update `snapcraft.yaml`'s `version:`
key to match, since it is now the single source.

- [ ] **Step 6: Verify the injection works**

Run:

```bash
go build -ldflags="-X github.com/canonical/landscape-client-core/internal/version.Version=99.99-test" -o /tmp/lcc-version-test ./cmd/landscape-client-core/
strings /tmp/lcc-version-test | grep -c '99.99-test'
rm /tmp/lcc-version-test
```

Expected: a non-zero count. Zero means the symbol path is wrong — check it with
`go tool nm` against the built binary.

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/version/ ./cmd/... -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/version/ snap/snapcraft.yaml cmd/
git commit -m "build: single-source the version string

version.go said 26.08~beta.2 while snapcraft.yaml said 26.04. Version goes
out as User-Agent and the server gates snap-monitoring support on it, so the
drift was server-visible.

Both identifiers were const, which -ldflags -X cannot set — the snap build
would have silently shipped the hard-coded value. Version is now a var and
UserAgent a function, so an injected version cannot go stale.

snapcraft.yaml is the single source via CRAFT_PROJECT_VERSION; a plain go
build reports 0.0.0-dev rather than claiming a release number."
```

---

## Task 6: Cover `run()` wiring and simplify shutdown

Phase 1 extracted `run(ctx, deps) error`; this raises the coverage it enabled and
fixes the shutdown wart the review identified.

The shutdown logic reads `groupDone` in two places, so the second read can block
the full 5 seconds even when the group has already exited.

**Files:**
- Modify: `cmd/landscape-client-core/main.go`
- Modify: `cmd/landscape-client-core/run_test.go`

- [ ] **Step 1: Measure the baseline**

Run: `go test ./cmd/... -cover`
Expected: a coverage figure — 3.7% before Phase 1, higher now. Record it.

- [ ] **Step 2: Write the failing test**

```go
// TestRun_ShutsDownPromptly asserts shutdown does not burn the grace period when
// the runners have already exited. The old code read groupDone twice, so the
// second read could block the full 5s regardless.
func TestRun_ShutsDownPromptly(t *testing.T) {
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

	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Errorf("shutdown took %v; the grace periods are being burned unnecessarily", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return")
	}
}
```

- [ ] **Step 3: Read the current shutdown block**

Run: `grep -n 'groupDone' cmd/landscape-client-core/main.go`
Expected: three or four hits — the channel creation, the goroutine write, and two
reads.

- [ ] **Step 4: Read `groupDone` once**

Replace the double-read structure with a single read:

```go
	groupDone := make(chan error, 1)
	go func() {
		groupDone <- eg.Wait()
	}()

	// Wait for shutdown signal or the first runner error.
	var groupErr error
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case groupErr = <-groupDone:
		if groupErr != nil {
			slog.Error("first runner error", "error", groupErr)
		}
		cancel()
		slog.Info("shutting down")
	}

	if err := mgRunner.WaitWithTimeout(5 * time.Second); err != nil {
		slog.Error("error waiting for manager runner", "error", err)
	}

	// If the group has not already reported, wait for it now.
	if groupErr == nil {
		select {
		case err := <-groupDone:
			if err != nil {
				slog.Error("runner group exited with error", "error", err)
			}
		case <-time.After(5 * time.Second):
			slog.Warn("shutdown timeout, exiting")
		}
	}
```

The guard is what fixes it: when the group already reported in the first `select`,
the channel is drained and the second read would otherwise wait the full 5 seconds
for a value that will never arrive.

- [ ] **Step 5: Add coverage for the wiring**

Add tests for the paths a subagent can reach without snapctl: config-driven log
level selection, `SNAP_COMMON` defaulting, and the `set-intervals` subscriber that
updates the ping interval. `ping.Pinger` already exposes both `SetInterval` and
`GetInterval`, so the last one is directly testable:

```go
// TestRun_SetIntervalsUpdatesPingInterval asserts the server can change the ping
// cadence at runtime. The subscriber is registered inside run(), so this is only
// reachable now that the wiring is extracted.
func TestRun_SetIntervalsUpdatesPingInterval(t *testing.T) {
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	pinger := ping.New(
		"http://127.0.0.1:1/ping",
		func() string { return "insecure-id" },
		func() {},
		30*time.Second,
		tc,
	)

	if got := pinger.GetInterval(); got != 30*time.Second {
		t.Fatalf("initial interval: want 30s, got %v", got)
	}

	// Mirror the subscriber run() registers for set-intervals.
	handle := func(msg exchange.Message) {
		if v, ok := msg["ping"]; ok {
			if secs, ok := v.(int64); ok && secs > 0 {
				pinger.SetInterval(time.Duration(secs) * time.Second)
			}
		}
	}

	handle(exchange.Message{"type": "set-intervals", "ping": int64(120)})
	if got := pinger.GetInterval(); got != 120*time.Second {
		t.Errorf("after set-intervals: want 120s, got %v", got)
	}

	// A zero or negative value must be ignored rather than disabling pings.
	handle(exchange.Message{"type": "set-intervals", "ping": int64(0)})
	if got := pinger.GetInterval(); got != 120*time.Second {
		t.Errorf("a zero ping interval should be ignored, got %v", got)
	}
}
```

Check `ping.New`'s parameter order against `internal/ping/ping.go` — it takes the
URL, an insecure-ID getter, a trigger callback, the interval and the transport.

This test duplicates the subscriber's logic rather than driving it through
`run()`, which is a deliberate trade-off: driving it end-to-end needs a live
exchange loop and a scripted server. If you would rather assert the real
subscriber, extract it from `run()` into a named function and call that from both
places — that is a better test and a small change.

- [ ] **Step 6: Verify coverage improved**

Run: `go test ./cmd/... -cover`
Expected: materially above the Step 1 figure. Record both numbers for the commit
message.

- [ ] **Step 7: Commit**

```bash
git add cmd/landscape-client-core/
git commit -m "test(cmd): cover run() wiring and simplify shutdown

Shutdown read groupDone in two places, so when the group had already reported
in the first select the second read waited the full 5s grace period for a
value that would never arrive.

cmd/ coverage: <before>% -> <after>%"
```

---

## Task 7: Port remaining Python scenarios and cut suite runtime

The P0 and P1 Python test ports land in their own phases, where they act as
regression tests. This picks up the P2 remainder from the review's §5.3 table.

`internal/manager` also spends 20s of the suite's 26s in real `sleep`s, and only
`internal/persist/persist_test.go` uses `t.Parallel()`.

**Files:**
- Modify: `internal/ping/ping_test.go`
- Modify: `internal/monitor/sysinfo_test.go`
- Modify: `internal/manager/*.go` and their tests

- [ ] **Step 1: Port the ping scenarios**

From `landscape/client/broker/tests/test_ping.py`: ping response handling and
insecure-id gating — the client must not ping before it has an insecure ID, and
must trigger an exchange only when the server reports messages waiting.

```go
func TestPinger_DoesNotPingWithoutInsecureID(t *testing.T) {
	var pings int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&pings, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	p := ping.New(srv.URL, func() string { return "" }, func() {}, 10*time.Millisecond, tc)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	if n := atomic.LoadInt32(&pings); n != 0 {
		t.Errorf("pinged %d times before registration; the server has no way to answer", n)
	}
}
```

Check `ping.New`'s real signature — Phase 1's `run()` calls it with
`(url, exc.InsecureID, exc.TriggerExchange, cfg.PingInterval, tc)`.

- [ ] **Step 2: Port the message-shape assertions**

From `test_computerinfo.py`, `test_mountinfo.py` and `test_activeprocessinfo.py`:
field-level assertions on the emitted messages. These are the assertions that
encode the protocol contract — the review's strongest single point is that
`internal/monitor` had 77% coverage and a passing suite while shipping a
wire-format bug that the Python suite asserts against directly.

For each of the three plugins, assert every field name and Go type in the emitted
message against what the Python test expects. Where a field is `[]byte` in Go and
`bytes` in Python, assert the Go type explicitly — that distinction is what
bpickle encodes.

- [ ] **Step 3: Inject a clock into `internal/manager`**

Run: `go test ./internal/manager/ -v 2>&1 | tail -3`
Record the elapsed time — roughly 20s at baseline.

Find the real sleeps:

```bash
grep -rn 'time.Sleep' internal/manager/
```

Where they are in *production* code (for example, change-polling loops), inject a
clock:

```go
// clock is injectable so tests do not have to sleep in real time; the manager
// suite spent 20s of the 26s total in real sleeps.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
```

Where they are in *test* code, replace with synchronisation — a channel the fake
snapd closes when it has received the call — rather than a sleep.

- [ ] **Step 4: Add `t.Parallel()`**

Add it to every test that does not mutate global state. The candidates are tests
using only `t.TempDir()`, `httptest` and local fixtures. Do **not** add it to tests
that call `slog.SetDefault` or mutate package-level variables such as
`runnerInitialBackoff`.

- [ ] **Step 5: Measure the improvement**

Run: `go test ./... 2>&1 | tail -15`
Expected: total wall time materially below the 26s baseline. Record before and
after.

- [ ] **Step 6: Verify no flakiness was introduced**

Run: `go test -race -count=5 ./...`
Expected: PASS all five runs. Parallelism plus the Phase 3 stagger is the most
likely source of new flakiness; investigate any failure rather than re-running.

- [ ] **Step 7: Commit**

```bash
git add internal/
git commit -m "test: port remaining Python scenarios and cut suite runtime

Ports the P2 remainder of the review's test table: ping response handling and
insecure-id gating, plus field-level message-shape assertions for
computerinfo, mountinfo and activeprocessinfo. Those assertions encode the
protocol contract — the review's central point is that internal/monitor had
77% coverage and a green suite while shipping a wire-format bug the Python
suite asserts against directly.

Also injects a clock into internal/manager, which spent 20s of the suite's
26s in real sleeps, and adds t.Parallel() where there is no shared state.

Suite wall time: <before> -> <after>"
```

---

## Task 8: Replace `GOGC=50` with `GOMEMLIMIT`

`snap/snapcraft.yaml` sets `GOGC: "50"` on the daemon app, which trades CPU for
RSS by collecting twice as often — a reasonable instinct on a constrained device,
but a blunt one. `GOMEMLIMIT` lets the GC stay lazy when there is headroom and
work hard only near the ceiling.

This lands after Phase 3's allocation work because cutting the allocation rate
changes what the right ceiling is.

**Files:**
- Modify: `snap/snapcraft.yaml`

- [ ] **Step 1: Measure the current behaviour**

Build and run the daemon locally against the fake server used in the integration
tests, and sample RSS:

```bash
go build -o /tmp/lcc ./cmd/landscape-client-core/
GOGC=50 SNAP_COMMON=/tmp/lcc-common /tmp/lcc &
LCC_PID=$!
for i in $(seq 1 12); do
  grep VmRSS /proc/$LCC_PID/status
  sleep 10
done
kill $LCC_PID
```

The daemon will fail to load config outside a snap; if it exits immediately, drive
`run()` from a long-running test instead and sample from inside the process with
`runtime.ReadMemStats`. Either way, record a steady-state figure — this task is
measurement-driven and a guess is worse than leaving `GOGC` alone.

- [ ] **Step 2: Measure with `GOMEMLIMIT`**

Repeat with `GOGC=off GOMEMLIMIT=64MiB` and with `GOMEMLIMIT=64MiB` alone
(default `GOGC`). Record RSS and, if the harness allows, GC CPU fraction from
`runtime/metrics`.

- [ ] **Step 3: Choose based on the measurement**

Set the app environment to whichever configuration gave acceptable RSS at lower GC
CPU:

```yaml
    environment:
      GOMEMLIMIT: "64MiB"
      GOMAXPROCS: "2"
```

`GOMAXPROCS: "2"` reflects that the workload is 15 mostly-idle plugin goroutines
doing IO, not parallel computation; the default equals the core count and buys
nothing here. If the measurement does not support the change, keep `GOGC=50` and
record why — that is a legitimate outcome.

- [ ] **Step 4: Verify the daemon still behaves**

Run: `go test -race ./...`
Expected: PASS. Environment variables do not affect tests, so this only guards
against an accidental YAML error.

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('snap/snapcraft.yaml')); print('yaml ok')"`
Expected: `yaml ok`.

- [ ] **Step 5: Commit**

```bash
git add snap/snapcraft.yaml
git commit -m "perf(snap): replace GOGC=50 with GOMEMLIMIT

GOGC=50 trades CPU for RSS by collecting twice as often — a reasonable
instinct on a constrained device, but blunt. GOMEMLIMIT lets the GC stay lazy
when there is headroom and work only near the ceiling. GOMAXPROCS is set
because the workload is 15 mostly-idle IO-bound goroutines, not parallel
computation.

Lands after the allocation work in Phase 3, which changed what the right
ceiling is.

Measured RSS: GOGC=50 <before> -> GOMEMLIMIT=64MiB <after>"
```

---

## Task 9: Idiom pass

Zero uses of `slices` or `maps` anywhere despite `go 1.25.0`. Task 2's `modernize`
linter will have flagged much of this already; this commit finishes it and covers
what `modernize` cannot see.

**Behaviour-preserving: every existing test must pass unchanged.**

**Files:**
- Modify: `internal/monitor/*.go` predominantly; `internal/exchange/`, `internal/manager/` where flagged

- [ ] **Step 1: Confirm the baseline**

Run:

```bash
grep -rn 'slices\.\|maps\.' --include=*.go internal cmd | wc -l
grep -rn 'sort.Slice\|sort.Strings' --include=*.go internal cmd
grep -rn 'fmt.Sprintf("%x"' --include=*.go internal
```

Record the counts. Phase 3 already converted some `sort.Slice` calls; only the
remainder is in scope here.

- [ ] **Step 2: Adopt `slices` and `maps`**

| Site | Change |
|---|---|
| `networkdevice.go`, `processorinfo.go`, `snapservices.go` | `sort.Slice` → `slices.SortFunc` (or `SortStableFunc` where Phase 3 required stability) |
| `users.go` (6 sites) | `sort.Strings` → `slices.Sort` |
| `users.go` set intersection/difference | replace the four `map[string]bool` builds per group per tick with `slices.Contains` on sorted slices, or a single reused set |

- [ ] **Step 3: Adopt `strings.Cut`**

```go
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		continue
	}
	key, value := parts[0], parts[1]
```

becomes:

```go
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		continue
	}
```

Apply in `computerinfo.go`, `processorinfo.go` (two sites) and the manual colon
hunt in `networkactivity.go`.

- [ ] **Step 4: Replace `fmt.Sprintf` doing non-formatting work**

| Site | Change |
|---|---|
| `networkdevice.go` sysfs paths | `filepath.Join` |
| `networkdevice.go` netmask | `net.IP(mask).String()` |
| `processorinfo.go`, `mountinfo.go`, `networkdevice.go`, `snapservices.go` hashes | `hex.EncodeToString(h[:])` instead of `fmt.Sprintf("%x", sha256.Sum256(...))` |

Also stream to the hasher rather than allocating an intermediate `[]byte`:

```go
	h := sha256.New()
	if err := json.NewEncoder(h).Encode(payload); err != nil {
		slog.Warn("mount-info: cannot hash layout", "error", err)
		return
	}
	hash := hex.EncodeToString(h.Sum(nil))
```

Note `json.Encoder.Encode` appends a newline, which changes the hash value. That
is harmless — the hash is only compared against the previously *stored* hash — but
it will cause one spurious re-send after upgrade, on every device. State that in
the commit message.

- [ ] **Step 5: Replace `map[string]any` internals with typed structs**

`processorinfo.go` returns `[]map[string]any` and then type-asserts
`["processor-id"].(int)` to sort it; `mountinfo.go` does the same and re-asserts
every field, logging "unexpected type" for structurally impossible cases. Define a
struct and a `toMap()` used only at the wire boundary. The wire shape must be
byte-identical — assert that with the message-shape tests added in Task 7.

- [ ] **Step 6: Collapse the `computerinfo` repetition**

Twelve copy-pasted `if !prev.Initialized || x != prev.X {...}` blocks plus a mirror
struct rebuilt field-by-field, and two naked returns with named results. Replace
with a small helper over a field list, and give the naked returns explicit values.

- [ ] **Step 7: Deduplicate the `/proc` parsing**

`/proc/meminfo` is opened and parsed twice with different key sets — once in
`computerinfo.go` at a 5-minute interval, once in `memoryinfo.go` at 15s — and the
same key:value scanner boilerplate recurs in six files. Add one helper:

```go
// scanKV parses "key<sep>value" lines, calling fn for each. Six files
// reimplemented this loop, and /proc/meminfo was parsed twice with different key
// sets.
func scanKV(r io.Reader, sep string, fn func(key, value string)) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), sep)
		if !ok {
			continue
		}
		fn(strings.TrimSpace(key), strings.TrimSpace(value))
	}
	return scanner.Err()
}
```

Also fix `cpuusage.go` building a 64 KB `bufio.Scanner` to read a single line.

- [ ] **Step 8: Correct the `clkTck` comment — and nothing else about it**

`internal/monitor/activeprocessinfo.go` has:

```go
	const clkTck = 100 // kernel timer frequency
```

**The value is correct and must not change.** The kernel fixes `USER_HZ` at 100
for `/proc` regardless of `CONFIG_HZ`, which is 250 on Raspberry Pi kernels. Only
the comment is wrong, and its wording invites exactly the change that would break
`/proc` parsing on ARM:

```go
	// USER_HZ, the unit /proc/<pid>/stat uses for CPU times. The kernel fixes
	// this at 100 regardless of CONFIG_HZ (250 on Raspberry Pi kernels), so do
	// not "correct" it to the timer frequency.
	const clkTck = 100
```

- [ ] **Step 9: Verify nothing changed behaviourally**

Run:

```bash
go test -race -count=3 ./...
golangci-lint run
gofmt -l .
grep -rn 'slices\.\|maps\.' --include=*.go internal cmd | wc -l
```

Expected: clean, and a `slices`/`maps` count well above the Step 1 baseline.

- [ ] **Step 10: Verify the wire shape is unchanged**

Run: `go test -tags compat ./internal/bpickle/...`
Expected: PASS. Also run the Task 7 message-shape tests specifically — they are
what guards the `map[string]any` → struct conversion.

- [ ] **Step 11: Commit**

```bash
git add internal/
git commit -m "refactor: idiom pass

Zero uses of slices or maps despite go 1.25.0. Adopts slices.Sort/SortFunc,
strings.Cut, filepath.Join, net.IP.String and hex.EncodeToString; replaces
map[string]any internals with typed structs converted only at the wire
boundary; and adds one scanKV helper for the /proc scanner boilerplate
duplicated across six files, where /proc/meminfo was being parsed twice with
different key sets.

Hashes now stream through json.Encoder, which appends a newline and therefore
changes the hash value — harmless, since it is only compared against the
previously stored hash, but it causes one spurious re-send per device after
upgrade.

clkTck stays 100. Only its comment changes: the kernel fixes USER_HZ at 100
for /proc regardless of CONFIG_HZ, and the old wording invited exactly the
change that would break ARM parsing."
```

---

## Task 10: Verify the phase

- [ ] **Step 1: Full verification**

Run:

```bash
gofmt -l .
go vet ./...
go test -race -count=3 ./...
golangci-lint run
go test -tags compat ./internal/bpickle/...
```

Expected: all clean.

- [ ] **Step 2: Confirm the hygiene targets**

Run:

```bash
grep -rn 'log\.Printf' --include=*.go internal cmd || echo "slog migration complete"
grep -rn 'curl' snap/snapcraft.yaml || echo "curl removed"
grep -c 'GOGC' snap/snapcraft.yaml || echo "GOGC replaced"
grep -rn 'slices\.\|maps\.' --include=*.go internal cmd | wc -l
```

Expected: the three confirmations, and a non-zero `slices`/`maps` count.

- [ ] **Step 3: Confirm the version is single-sourced**

Run: `grep -n 'version' snap/snapcraft.yaml | head -3; grep -n 'Version =' internal/version/version.go`
Expected: `snapcraft.yaml` carries the number; `version.go` carries `0.0.0-dev`.

- [ ] **Step 4: Record the coverage and runtime deltas**

Run: `go test ./... -cover 2>&1 | grep -v 'no test files'`
Put the per-package figures in the PR description alongside the pre-phase
baseline (persist 64%, snapd 76%, monitor 77%, bpickle 81%, transport 83%,
config 84%, exchange 85%, manager 80%, ping 91%, cmd 3.7%).

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin fix/04-p2-hygiene
```

PR title: `Phase 4: P2 hygiene — logging, linting, conventions, test debt, idiom`

The description should note the one user-visible consequence: the hash format
change in Task 9 causes a single spurious re-send per device on first run after
upgrade.

---

## Done when

- No `log.Printf` remains; `log-level` demonstrably filters output.
- `golangci-lint` enables the expanded set and passes, with `depguard` keeping the dependency tree closed.
- Errors follow the `"cannot …"` convention with `%w` wrapping preserved.
- `curl` is gone from the snap; `lshw` remains.
- `snapcraft.yaml` is the single source of the version, injected via `-ldflags -X` into a `var`.
- Shutdown returns promptly when the runners have already exited, and `cmd` coverage is materially above baseline.
- The suite runs materially faster and passes `-race -count=5`.
- Runtime tuning is backed by a recorded measurement, or `GOGC=50` is kept with a recorded reason.
- `slices`/`maps` are in use, the `/proc` boilerplate is deduplicated, and `clkTck` still equals 100.
