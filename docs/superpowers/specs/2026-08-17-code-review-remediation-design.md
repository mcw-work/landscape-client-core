# Code Review Remediation — Design Spec

**Source:** [docs/CODE_REVIEW_2026-07.md](../../CODE_REVIEW_2026-07.md) (review dated 2026-07-31, against commit `c8c76ce`)

**Date:** 2026-08-17
**Baseline commit:** `a1cfeae` (merge of PR #2, "fix golangci-lint version mismatch")

---

## Goal

Resolve the 30-item action list produced by the 2026-07 code review of
`landscape-client-core`, as 41 commits across 5 sequenced phases, each phase
delivering working, tested software on its own.

## Scope

**In scope:** all P0, P1 and P2 items from the review's prioritised action list.

**Out of scope:** the four §7.1 "faithful-but-arguably-wrong" items. These match
the Python client exactly and are wire-visible, so changing them alters reported
values across every enrolled device. They are recorded as a decision request in
[§7 of this spec](#7-decision-request-71-wire-visible-behaviours) and get no
commits.

**Deliberately excluded:** a `git filter-repo` history rewrite. See
[§6](#6-repository-history).

---

## 1. Baseline verification

The review was written against `c8c76ce`. Before planning, every finding was
re-checked against `a1cfeae`. Phase 0 of the review's action list was partially
completed by PR #2:

| Review item | Status at `a1cfeae` | Evidence |
|---|---|---|
| Lint actually gating (6.1) | **Done** | `.golangci.yml` is now `version: "2"`; CI uses `golangci-lint-action@v8` at `v2.12.2`, so the config is no longer silently ignored |
| gofmt clean (item 20) | **Done** | `gofmt -l .` returns nothing; `internal/transport/transport.go` struct alignment fixed in `c99de21` |
| Go version — CI build/test (6.1) | **Done** | `.github/workflows/ci.yml` now pins `"1.25"` |
| Go version — compat workflow + snap build (6.1) | **Not done** | `.github/workflows/compat.yml` still `"1.22"`; `snap/snapcraft.yaml` still `go/1.22/stable` |
| `-race` in CI (6.1) | **Not done** | CI test step is bare `go test ./...` |
| `govulncheck` (6.1) | **Not done** | No occurrence in either workflow |
| `.gitignore` + untrack artifacts (item 19) | **Not done** | No `.gitignore` exists; 15 artifacts still tracked |

All other findings were confirmed still present. Two specific re-checks worth
recording:

- **The errcheck fixes in `c99de21` are benign.** Every change is a cleanup-path
  suppression (`defer f.Close()` → `defer func() { _ = f.Close() }()`, and
  `os.Remove(tmpPath)` on the persist rollback path). None of the review's
  data-fabricating discards (§7.2's `ParseInt`s in `activeprocessinfo`,
  `scanner.Err()`, `_ = state.SetPluginState(...)`) were touched, so those
  findings stand unchanged and were not entrenched behind a satisfied linter.
- **`go/1.25/stable` exists** in the Go snap (1.25.12, revision 11218), so the
  snapcraft alignment in Phase 0 is a one-line change.

## 2. Approach

Three approaches were considered:

- **A — Defect-first, in place.** Fix each finding where it lives, no new
  abstractions. Fastest to P0, lowest review risk. Rejected because the root
  causes persist: 15 hand-rolled ticker loops means the next cross-cutting fix is
  15 edits again, and 6 `exec` sites each keep their own error handling.
- **B — Thin seams, then defects.** *(chosen)* Land only the enabling changes each
  phase needs — `runCmd`, `runTicker`, injectable filesystem roots, a testable
  `run(ctx, deps) error` — then fix defects through them. Costs a handful of extra
  commits, converts several findings into one-line changes, and makes the
  untestable ones testable.
- **C — Design-led rework.** Typed message structs replacing `map[string]any`, a
  proper scheduler, a spool subsystem, a supervisor. Highest payoff, but it is a
  rewrite inside a rewrite, and the P0 crash and corruption defects would wait
  behind it.

**B is chosen** with the seams kept deliberately thin, and with the P0 defects not
blocked behind seam work beyond the single `run()` extraction that P0's own
regression test requires.

The review's central lesson drives this: `internal/monitor` has 77% coverage and a
passing, race-clean suite, yet ships a wire-format bug (`network-device` flags)
that the *existing Python test* asserts against directly. Coverage was measuring
the wrong thing. Tests that bypass the real dispatch path are why the P0 handler
lifetime bug survived. Seams exist to make the real path testable, not to be
elegant.

## 3. Decisions taken

| Decision | Choice | Rationale |
|---|---|---|
| Scope | P0 + P1 + P2; §7.1 documented only | §7.1 needs server-side agreement, not a code change |
| Delivery | Phased branches, one PR per priority tier | Reviewable chunks; P0 lands first and fast |
| Commit granularity | One commit per review item | As requested; coupled items noted in [§4](#4-ordering-constraints) |
| Verification | TDD per commit — failing test first | Directly targets the class of bug the review found |
| Validation environment | Dev host only | Hardware-specific behaviour covered by injected filesystem roots, fakes and fixtures |
| Git history | Untrack only, no rewrite | Rewriting invalidates 6 local branches, 2 open remote branches and any clone |
| Watchdog design | Self-supervising heartbeat | `daemon: notify` cannot be validated without a device; plumbing shaped so it drops in later |
| Queue spool format | Single JSON file, atomic rename | Reuses the proven temp+fsync+rename path; queue is capped at 100 so per-exchange rewrite is cheap |
| Queue drop policy | Never drop `operation-result`; drop oldest telemetry first | The server blocks on operation results; telemetry is resampled anyway |
| Linter expansion | Phase 4, before the idiom pass | `modernize` mechanically verifies most of the idiom work |

### 3.1 Validation strategy under dev-host-only constraint

Four findings concern behaviour that cannot be reproduced natively on an x86-64
dev host. Each is made testable rather than deferred:

| Finding | Dev-host strategy |
|---|---|
| 1.4 kernel `IFF_*` flags | `NetworkDevice.sysNetPath` is already an injectable field — assert against a fixture tree containing known hex flag values |
| 1.3 32-bit counter rollover | Rollover correction is pure arithmetic over parsed counters; table-test the delta function directly with `MaxUint32`-adjacent values |
| §2 watchdog | Injected clock plus a deliberately stalled heartbeat source; assert the supervisor's exit decision, not the process exit |
| 8.5 `lshw` under confinement | Fake the command via the `runCmd` seam; assert empty and truncated stdout with exit 0 are both rejected |

## 4. Ordering constraints

These are hard dependencies. Violating them produces either an unreviewable diff
or a commit whose test cannot be written.

1. **`run()` extraction before the handler-lifetime fix.** The 1.1 regression test
   must drive the real wiring; `cmd/` is 3.7% covered because everything lives in
   `main()`.
2. **`Runner.Run` error reporting before the watchdog.** A watchdog over a function
   documented "It always returns nil" detects nothing.
3. **`SetPluginState` fallback removal together with the four `_ =` discard sites.**
   Propagating an error that four callers discard changes nothing.
4. **Urgent/normal classification before the queue cap and drop policy.** The drop
   policy is defined in terms of that classification.
5. **`runTicker` extraction before the initial-tick and stagger fixes.** Otherwise
   those are 15 edits instead of one.
6. **`runCmd` helper before the `lshw` timeout and output validation.** The helper
   is where the per-run timeout is enforced.
7. **Allocation reduction before the `GOMEMLIMIT` change.** Cutting the allocation
   rate changes what the correct ceiling is.
8. **slog migration first within Phase 4.** It touches nearly every file; running it
   after the idiom pass guarantees conflicts.

## 5. Phase structure

| Phase | Branch | Commits | Contents |
|---|---|---|---|
| 0 — Foundation | `fix/00-foundation` | 3 | Remaining Go version alignment, `-race`, `govulncheck`, `.gitignore` + untrack |
| 1 — P0 defects | `fix/01-p0-defects` | 6 | `run()` seam, handler context lifetime, bpickle recursion + fuzz, `IFF_*` flags, `time-limit` enforcement |
| 2 — P1 reliability | `fix/02-p1-reliability` | 13 | Timeouts, watchdog, state integrity, external-executable error handling, counter rollover |
| 3 — P1 efficiency | `fix/03-p1-efficiency` | 10 | Exchange rework (urgent/backoff/cap/durability), monitor scheduling, allocation, §7.2 regressions |
| 4 — P2 hygiene | `fix/04-p2-hygiene` | 9 | slog migration, linter expansion, error convention, version single-sourcing, test debt, runtime tuning, idiom pass |

Phase 0 goes first because CI is not currently a meaningful gate for the phases
that follow: the compat workflow and the snap build still pin Go 1.22 against a
`go 1.25.0` module. It is also where `.gitignore` lands, before more `.snap` blobs
accrete.

Phases 2 and 3 are split rather than merged because Phase 3 rewrites
`internal/exchange`'s queue and send semantics as one coherent change, while
Phase 2 is many small independent fixes. Mixing them would make the exchange
rework unreviewable.

### 5.1 Item-to-commit map

| Review item | Phase | Commit |
|---|---|---|
| 6.1 Go version alignment (compat + snapcraft) | 0 | 1 |
| 6.1 `-race`, `govulncheck` | 0 | 2 |
| 19 / 6.2 `.gitignore` + untrack | 0 | 3 |
| 24 (partial) `run()` extraction | 1 | 1 |
| 1.1 handler context lifetime | 1 | 2 |
| 1.2 bpickle recursion depth | 1 | 3 |
| 1.2 `FuzzUnmarshal` | 1 | 4 |
| 1.4 `network-device` kernel flags | 1 | 5 |
| 8.1 `time-limit` enforcement | 1 | 6 |
| §2 `TotalTimeout` default | 2 | 1 |
| §2 snapd client + per-call timeouts, `main.go:125` detached ctx | 2 | 2 |
| §2 D-Bus context bounding | 2 | 3 |
| 15 / 7.2 `Runner.Run` reports collapse | 2 | 4 |
| §2 watchdog | 2 | 5 |
| 1.5 empty-data substitution | 2 | 6 |
| 1.6 `SetPluginState` fallback + `_ =` Set discards | 2 | 7 |
| 1.6 corrupt-state `.old` recovery + `_ =` Get discards | 2 | 8 |
| 8.4 `runCmd` helper | 2 | 9 |
| 8.2 exec error in `result-text` | 2 | 10 |
| 8.3 interpreter validation | 2 | 11 |
| 8.5 + §2 `lshw` timeout and validation | 2 | 12 |
| 1.3 counter rollover | 2 | 13 |
| 3.1 urgent vs scheduled send | 3 | 1 |
| 3.2 exponential backoff + 404 downgrade | 3 | 2 |
| 3.4 per-exchange cap | 3 | 3 |
| 3.4 queue durability + bound | 3 | 4 |
| 3.4 O(n) prepends | 3 | 5 |
| 3.4d `runTicker` helper | 3 | 6 |
| 3.4c initial tick + stagger | 3 | 7 |
| 3.3 sample vs send interval | 3 | 8 |
| 3.4b allocation + 7.2 `diffProcesses` | 3 | 9 |
| 27 / 7.2 ordering and error-discard regressions | 3 | 10 |
| 18 slog migration | 4 | 1 |
| 6.1 linter expansion | 4 | 2 |
| 21 `"cannot …"` convention | 4 | 3 |
| 22 drop `curl` | 4 | 4 |
| 23 version single-sourcing | 4 | 5 |
| 24 `cmd` coverage + shutdown | 4 | 6 |
| 25 Python test ports, `t.Parallel`, clock injection | 4 | 7 |
| 26 `GOMEMLIMIT` / `GOMAXPROCS` | 4 | 8 |
| 28 + 29 idiom pass, `clkTck` comment | 4 | 9 |

## 6. Repository history

`.git` is 226 MiB (`size-pack` 224.56 MiB) because build artifacts are committed
and no `.gitignore` exists. Phase 0 commit 3 adds the ignore file and untracks 15
artifacts, including 8 `.snap` packages totalling 54 MB, both unstripped binaries,
and assorted log and patch scratch files.

**This does not reclaim the 224 MiB.** The blobs remain reachable from existing
commits, so every clone still pays for them. Only `git filter-repo --path-glob
'*.snap' --invert-paths` shrinks the pack, and it rewrites every commit hash,
invalidating the 6 local branches, the 2 open `origin/copilot/*` branches, and any
clone another engineer holds.

That rewrite is **deliberately not in this plan**. It is a coordinated operation
requiring explicit sign-off and a window where no one holds unmerged work. It
gets harder to defer indefinitely — each snap build adds another incompressible
7 MB blob — so it should be scheduled, not forgotten.

## 7. Decision request: §7.1 wire-visible behaviours

Four behaviours that look like defects, match the Python client exactly, and have
therefore been feeding the Landscape server for years. Each needs server-side
agreement before any code change, because altering them changes reported values
across every enrolled device.

### 7.1 Free memory over-reports

`internal/monitor/memoryinfo.go:105` computes `(MemFree+Buffers+Cached)/1024`,
identical to `landscape/lib/sysstats.py:34`.

`Cached` includes `Shmem`/tmpfs, which is **not** reclaimable. Ubuntu Core mounts
every snap plus `/run` on tmpfs, so this over-reports free memory on precisely the
target platform. `MemAvailable` is the kernel's own answer to this question.

**Impact if changed:** reported free memory drops on every device, by an amount
proportional to tmpfs usage. Alert thresholds tuned against current values would
fire.

### 7.2 Free disk includes reserved blocks

`internal/monitor/mountinfo.go:175` uses `stat.Bfree`, identical to
`landscape/lib/disk.py:71`'s `f_bfree`. `Bavail` is what `df` reports to an
unprivileged user; `Bfree` includes the ~5% root reserve.

**Impact if changed:** reported free space drops ~5% on every ext4 mount.

### 7.3 CPU total double-counts guest time

`internal/monitor/cpuusage.go:94-101` sums all `/proc/stat` fields, deliberate
parity noted in a comment at line 123. Linux already counts `guest` and
`guest_nice` inside `user` and `nice`, so the total is inflated and utilisation
under-reported.

**Impact if changed:** negligible on a Core appliance (no guests), material on a
hypervisor. Reported CPU utilisation would rise where guests run.

### 7.4 Lifetime-average CPU per process

`internal/monitor/activeprocessinfo.go:247-251` reports a lifetime average, not
instantaneous usage — accurate to Python, but misleading to an operator reading
it as "current CPU".

**Impact if changed:** every process's reported CPU changes meaning. Note this is
also what drives the `update-processes` diff churn addressed in Phase 3 commit 9;
that commit fixes the *churn* without changing the *value*.

### 7.5 Related — do not "fix" this one

`internal/monitor/activeprocessinfo.go:213`'s `const clkTck = 100` is **correct**.
The kernel fixes `USER_HZ` at 100 for `/proc` regardless of `CONFIG_HZ`, which is
250 on Raspberry Pi kernels. Only its comment — which calls it the "kernel timer
frequency" and so invites exactly the wrong change — is corrected, in Phase 4
commit 9.

## 8. Success criteria

- All 30 review action items are either implemented or, for §7.1, recorded here
  as a decision request.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` and `golangci-lint run` are
  clean at every commit.
- CI gates on `-race` and `govulncheck`.
- The four P0 defects have regression tests that fail against `a1cfeae`.
- No commit changes wire-visible reported values without an entry in §7.

## 9. Plans

- [Phase 0 — Foundation](../plans/2026-08-17-phase-0-foundation.md)
- [Phase 1 — P0 defects](../plans/2026-08-17-phase-1-p0-defects.md)
- [Phase 2 — P1 reliability](../plans/2026-08-17-phase-2-p1-reliability.md)
- [Phase 3 — P1 efficiency](../plans/2026-08-17-phase-3-p1-efficiency.md)
- [Phase 4 — P2 hygiene](../plans/2026-08-17-phase-4-p2-hygiene.md)
