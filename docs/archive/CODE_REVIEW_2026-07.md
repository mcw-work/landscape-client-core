# landscape-client-core — Code Review (2026-07-31)

Review of the Go rewrite of the Python `landscape-client`, targeting Ubuntu Core
only (deb/apt management dropped), shipped as a strictly-confined snap.

**Reference baselines used**
- Python original: `landscape-client-python`
- Go conventions: `snapd` (`go/src/github.com/snapcore/snapd`), incl. its `CODING.md` and `.golangci.yml`

**State at review time:** commit `c8c76ce` ("Migrate to confdb based configuration"), 62 Go files, ~14.9k lines.
`go test ./...` passes; `go test -race ./...` passes; `go vet` clean.
Coverage: persist 64%, snapd 76%, monitor 77%, bpickle 81%, transport 83%, config 84%, exchange 85%, manager 80%, ping 91%.

**Verification note:** every finding marked **[verified]** was reproduced with a
throwaway test during this review. Findings marked **[observed]** come from
reading code/config without a dedicated repro.

---

## 0. Review query — what was asked, and what was answered

Recorded so a future agent can tell what this document covers, what it deliberately
does not, and which conclusions rest on reproduction rather than reading.

### 0.1 Context given

- The project is a refactor/rewrite of the Python `landscape-client`.
- It targets the **Ubuntu Core runtime only** — all deb/apt package-management functionality is intentionally dropped.
- It ships as a **snap**, which is to be taken into account (strict confinement, `snapctl`/confdb configuration, snapd REST API, hooks, resource limits on small devices).

### 0.2 Original review request — 8 focus areas

| # | Question asked | Answered in | Short answer |
|---|---|---|---|
| 1 | Do Go syntax, function and variable naming align with `snapd`? | §4 | Mostly yes; main divergence is error strings (snapd uses `"cannot …"`) and stdlib `log` vs an own logger. |
| 2 | Are there any tests? | §5.1–5.2 | Yes, and genuinely decent — 64–91% per package, race-clean. But structural gaps let real bugs through. |
| 3 | Are there Python tests worth leveraging? | §5.3 | Yes — a mapped table of high-value ports. One of them (`test_networkdevice.py`) directly asserts the bug found in §1.4. |
| 4 | Does it follow the Python original too closely; could Go idioms / memory / CPU be better? | §3, §7 | Architecture is well-adapted, not transliterated. Real wins available in allocation, batching, and exchange frequency. §7.1 flags where fidelity must be *kept*. |
| 5 | Is there a watchdog to stop the client hanging? | §2 | **No watchdog of any kind**, and multiple unbounded blocking calls. |
| 6 | Are general good Go practices followed? | §4, §7.3 | Broadly yes; `gofmt`/`vet` clean apart from one file. Cleanups listed, mostly mechanical. |
| 7 | Is the project too trigger-happy with dependencies? | §6 | **No — a clear strength.** 3 direct dependencies, all justified. |
| 8 | Are any dependencies questionable for long-term support? | §6 | No. Three are Go-team owned; `godbus` is community-maintained but is also what snapd uses. |

### 0.3 Follow-up request — external-executable error handling

> *"Ensure the Go implementation does not miss on correct error handling, especially
> when invoking external executables, regardless if the Python version does it
> correctly."*

Answered in **§8**, as a standalone audit of all 6 `exec.*` sites. The
"regardless of Python" instruction changed one verdict materially: §8.2 (exec
failures reporting empty `result-text`) is a defect Python shares, so a
parity-only review would have passed it. It is reported as a defect here.

§8 also produced the review's fourth P0 (§8.1, `time-limit` not enforced) — where
Python is in fact *more* correct, with a source comment describing the exact
failure mode Go now has.

### 0.4 Scope boundaries

- **Not a security audit.** Security observations appear where they intersect correctness (§1.2 remote crash, §8.3 unvalidated server input, §6.2 prebuilt artifacts), but no threat model or dependency CVE sweep was performed. `govulncheck` is recommended in §6.1, not run.
- **No source changes were made.** This document is the only deliverable; the working tree is otherwise untouched. All reproduction used throwaway tests, since deleted.
- **Not verified on target hardware.** Findings specific to 32-bit armhf (§1.3) and to tmpfs-heavy Core images (§7.1) are reasoned from source and the Python baseline, not measured on a device. §1.4's flag mismatch *was* measured, but on an x86-64 dev host.
- **Runtime behaviour against a real Landscape server was not exercised.** Wire-format claims come from comparing Go against the Python implementation and its test assertions.

### 0.5 How to action this document

Findings are tagged **[verified]** (reproduced with a throwaway test) or
**[observed]** (read-only). Prefer the verified ones — this review found at least
one plausible-sounding claim that did not survive checking, and several that needed
narrowing once reproduced (§1.6 requires a corrupt *read*, not any write failure).

Before "fixing" anything wire-visible, read **§7.1** — four items look like bugs but
faithfully match Python, so changing them alters reported values fleet-wide. §7.2 is
the Go-only regression list and is safe to act on. The consolidated, prioritised
work queue is at the end of this document.

---

## Executive summary

The project is in good shape architecturally: small, well-justified dependency
tree (3 direct deps), clean package boundaries, real test coverage, thorough
`docs/`, race-clean under `-race`, and correct path-traversal defence in the
script-execution path. The dominant risks are not structural — they are a
handful of concrete lifecycle/correctness defects, and the absence of the
self-protection mechanisms the Python client had (backoff, durable queue, watchdog).

**Top 7, in priority order:**

1. **P0** — Inbound handler contexts are cancelled microseconds after dispatch, killing every long-running operation (`execute-script`, `install-snaps`). **[verified]**
2. **P0** — Untrusted server response can crash the daemon via unbounded recursion in `bpickle` (stack overflow, unrecoverable). **[verified]**
3. **P0** — `network-device` sends Go's `net.Flags` where the server expects Linux `IFF_*`: every flag except UP is wrong on the wire. Silent data corruption, nothing errors. **[verified]**
4. **P0** — `execute-script`'s `time-limit` is **not enforced**: a script that spawns a subprocess holds the stdout pipe and blocks `cmd.Run` past the deadline. Measured 20s for a 1s limit. **[verified]**
5. **P1** — No HTTP total timeout in production, and no timeout at all on snapd/D-Bus calls: the documented default is never applied. **[verified]**
6. **P1** — `exchange-interval` is entirely bypassed; the client exchanges once per plugin message (~60×/15min instead of 1×). **[verified]**
7. **P1** — No watchdog and no exponential backoff; the Python client had both. A hung goroutine is invisible and unrecovered. **[observed]**

Two further **[verified]** P1s worth calling out because they cause *server-visible
data damage* rather than local failure: a transient `passwd` read error makes the
client emit `delete-users` for every user (1.5), and a corrupt state file makes a
plugin's state save roll back the client's `SecureID` and sequence number (1.6).

**A caution for anyone actioning this document:** §7.1 lists four items that look
like bugs but faithfully match the Python client, so the server has consumed those
semantics for years. Fixing them changes reported values fleet-wide — treat them as
product decisions, not cleanups. §7.2 is the Go-only regression list; that one is
safe to act on.

**§8 is a standalone audit of external-executable invocation and error handling**,
covering all 6 `exec.*` sites. It is held to a correct standard rather than to
Python parity: where Python is also weak (exit codes discarded, §8.2) that is noted
but not treated as justification. Its headline finding is the unenforced
`time-limit` above; note that Python *did* handle it, with a source comment
describing this precise failure mode.

---

## 1. Correctness & lifecycle defects

### 1.1 [P0] Handler contexts cancelled immediately after dispatch — **[verified]**

`internal/exchange/exchange.go:401` creates `errgroup.WithContext(ctx)` and
`:511` calls `handlerEG.Wait()`. `errgroup.WithContext` cancels its derived
context **as soon as `Wait()` returns**. But `manager.Runner.Register`
(`internal/manager/runner.go:67-91`) is a *dispatcher*: its `Subscribe` callback
spawns a goroutine and returns immediately. So `handlerCtx` is cancelled while
the real work is just starting.

Reproduced end-to-end with the real `Runner` + `ScriptExecHandler`: a
`sleep 2` script is killed instantly and the server is told
`result-code 103` (failure) with empty output:

```
execute-script: op=42 run complete: err=context canceled output-bytes=0
BUG CONFIRMED: script did not complete (marker missing)
```

Blast radius: **every** `execute-script`, `install-snaps`, `remove-snaps`,
`refresh-snaps` operation. The 10-minute `changeTimeout` and the per-operation
`time-limit` are both dead code in practice.

Why tests miss it: `internal/manager/*_test.go` call `handler.Handle(ctx, ...)`
directly with a live context, bypassing the errgroup dispatch entirely.

**Fix:** do not tie handler lifetime to the exchange cycle. Dispatch inbound
messages under a long-lived context owned by the daemon (e.g. the `main`
errgroup ctx), and drop `handlerEG` — `performExchange` should not wait for
manager handlers at all. Add a regression test that drives dispatch through
`manager.Runner.Register` rather than calling `Handle` directly.

### 1.2 [P0] Unbounded recursion in bpickle on untrusted input — **[verified]**

`unmarshalList`/`unmarshalTuple`/`unmarshalDict`
(`internal/bpickle/bpickle.go:332,352,371`) recurse per nesting level with no
depth cap. `transport.maxResponseBytes` allows 32 MiB, so a response of ~20 MiB
of `'l'` bytes exhausts the 1 GB goroutine stack limit:

```
runtime: goroutine stack exceeds 1000000000-byte limit
fatal error: stack overflow
```

A stack overflow is a **fatal runtime error** — `recover()` cannot catch it, so
the panic-recovery in the exchange and monitor runners is no help. The daemon
dies; `restart-condition: on-failure` restarts it, and a persistent hostile
response becomes a crash loop.

Reachability: the exchange endpoint is HTTPS-verified, but the **ping URL is
derived as plain `http://`** (`internal/config/config.go:36-45`) and its
response is fed to `bpickle.Unmarshal` (`internal/ping/ping.go:111`) — so a
network-position attacker needs no TLS compromise. Also reachable from a
compromised/buggy server.

**Fix:** thread a depth counter through `unmarshalValue` and reject beyond a
sane limit (Python's own C-level recursion cap is ~1000; 100 is ample for this
protocol). Add fuzz coverage: `FuzzUnmarshal` would have found this. Consider
lowering `maxResponseBytes` — 32 MiB is far above any legitimate payload.

### 1.3 [P1] Network counter rollover clamped to zero instead of corrected — **[observed]**

`internal/monitor/networkactivity.go:136-142` clamps a negative delta to `0`.
Python adds back `2**32` on 32-bit systems
(`landscape/client/monitor/networkactivity.py:21,37-39,97-99`, with the explicit
comment that 64-bit does not roll over). On 32-bit armhf Core devices, every
4 GiB of traffic per interface silently vanishes from reporting.

**Fix:** port the Python logic — detect pointer/counter width and add
`1<<32` when the delta is negative and the previous value was near `MaxUint32`;
keep clamping only for genuine counter resets (interface re-created).

### 1.4 [P0] `network-device` sends Go's `net.Flags`, not the kernel's `IFF_*` — **[verified]**

`internal/monitor/networkdevice.go:113` does `flags := int(iface.Flags)` and sends
that value on the wire. Go's `net.Flags` is its own sequential bitmask
(`FlagUp=1<<0`, `FlagBroadcast=1<<1`, `FlagLoopback=1<<2`, `FlagPointToPoint=1<<3`,
`FlagMulticast=1<<4`, `FlagRunning=1<<5`), whereas the Python client sends the raw
`SIOCGIFFLAGS` value from the kernel (`landscape/lib/network.py:107,157-161`,
`@see /usr/include/linux/if.h`), where `IFF_LOOPBACK=8`, `IFF_RUNNING=64`,
`IFF_MULTICAST=4096`. The server interprets these as Linux `IFF_*`
(`landscape/client/monitor/tests/test_networkdevice.py:36-38` asserts
`flags & 1`/`flags & 8`/`flags & 64` for UP/LOOPBACK/RUNNING).

Measured on a live host — Go value vs the kernel value Python sends:

| Interface | Go `net.Flags` | Kernel `IFF_*` | LOOPBACK (`&8`) | RUNNING (`&64`) | MULTICAST (`&4096`) |
|---|---|---|---|---|---|
| `lo` | 37 | 9 | Go 4 → **false** | Go 32 → **false** | — |
| `wlp0s20f3` | 51 | 4099 | — | Go 32 → **false** | Go 16 → **false** |
| `wwan0` | 41 | 145 | — | Go 32 → **false** | — |

Only bit 0 (UP) coincides. Every other flag the server tests for is wrong:
loopback interfaces are not detected as loopback, and no interface is ever
reported as RUNNING. This is a silent data-fidelity regression — nothing errors,
the server just receives false information.

**Fix:** do not send `net.Flags`. Read
`/sys/class/net/<iface>/flags` (hex, already the kernel `IFF_*` value) or issue
`SIOCGIFFLAGS` via `golang.org/x/sys/unix`, and add a test asserting the
Python-documented bit values.

### 1.5 [P1] Transient `passwd`/`group` read failure tells the server to delete every user — **[verified]**

`internal/monitor/users.go:75-84` substitutes an **empty map** for a failed parse:

```go
newUsers, err := p.parsePasswd()
if err != nil {
	log.Printf("users: parsing passwd: %v", err)
	newUsers = make(map[string]userRecord)   // ← treated as "no users exist"
}
```

`buildUsersDiff` then diffs the saved users against nothing. Reproduced with three
saved users and two saved groups:

```
message emitted on transient read failure:
  delete-users:  [alice bob root]
  delete-groups: [root sudo]
```

Worse, `:92-97` then persists the empty map, so the next tick re-emits every user
as `create-users` — a delete-all/recreate-all churn against the server from one
unreadable read of `/var/lib/extrausers/passwd`. This hides in testing because in
steady-state failure the saved state is also empty, so no diff is produced.

**Fix:** on error, `continue` — skip the tick and leave saved state untouched.
An unreadable source file means "unknown", never "empty".

### 1.6 [P1] `SetPluginState` fallback rolls back `SecureID` and the sequence number — **[verified]**

`internal/persist/persist.go:199-209`: when `Update` fails, the fallback path calls
`p.store.Save(p.cached)` — writing a **whole-State snapshot** that
`monitor/runner.go:75-81` captured when the plugin started. Any state written since
is lost. Verified with a corrupt state file (so `Update` fails at the *decode*
step while writes still succeed):

```
SecureID="SECRET-1"          (was SECRET-2-ROTATED)   ← CLOBBERED
OutboundSequence=5           (was 99)                  ← CLOBBERED
plugin-b=                    (was {"important":true})  ← LOST
```

Rolling back `SecureID` de-registers the client from the server's point of view;
rolling back `OutboundSequence` breaks message sequencing. Note this needs
`Update` to fail at *read*, not write — an unwritable directory fails the
fallback `Save` too, so the blast radius is corrupt-file/decode-error cases
(truncated write, disk corruption, a schema change on upgrade).

**Fix:** delete the fallback entirely and propagate the error — a failed save must
not be "recovered" by writing older data. Give `PluginStateAccessor` access to only
its own key rather than a full `*State`, so it structurally cannot write another
component's fields. Also recover from a corrupt state file explicitly (keep an
`.old` backup, as Python's `Persist` does) rather than silently reverting.

### 1.7 [P2] `version.Version` and snap version disagree — **[observed]**

`internal/version/version.go:4` is `26.08~beta.2`; `snap/snapcraft.yaml:5` is
`26.04`. The former is sent as `User-Agent` and the server gates snap-monitoring
support on it, so the two must not drift.

**Fix:** inject via `-ldflags "-X .../internal/version.Version=$VERSION"` in the
snapcraft `override-build` and make `snapcraft.yaml` the single source of truth.

---

## 2. Hang risk / watchdog

**There is no watchdog of any kind.** No `sd_notify`, no `WatchdogSec`, no
liveness self-check (searched: `watchdog|sd_notify|NOTIFY_SOCKET|WatchdogSec` —
zero hits). The Python client shipped a dedicated
`landscape/client/watchdog.py` that AMP-pinged each daemon and killed/restarted
unresponsive ones. The Go rewrite dropped that supervision without replacing it.

`restart-condition: on-failure` only covers *process exit*. A goroutine blocked
forever in a syscall keeps the process alive and healthy-looking while silently
reporting nothing.

### Unbounded blocking calls found

| Location | Issue |
|---|---|
| `internal/transport/transport.go:33,115` | `TotalTimeout` documented "default: 600s" but **never defaulted**; `main.go:84-91` doesn't set it → `totalTimeout == 0` → no total deadline in production. **[verified]** Only `TLSHandshakeTimeout` and dial timeout apply; a server that accepts then trickles bytes hangs the exchange forever. |
| `internal/snapd/snapd.go:87-97` | `http.Client` has **no `Timeout`**; no `ResponseHeaderTimeout`. |
| `internal/monitor/{snappackages.go:72, snapservices.go:61, computerinfo.go:234, rebootrequired.go:60}` | Pass the long-lived root ctx to snapd with **no per-call timeout** → a stalled `/run/snapd.socket` hangs these plugins indefinitely. **[verified: no `WithTimeout` anywhere in `internal/monitor`]** |
| `internal/manager/system.go:33,46` | `dbus.ConnectSystemBus()` and `obj.Call(...)` take **no context**; godbus offers `CallWithContext`. An unresponsive `logind` hangs the shutdown handler forever. |
| `internal/manager/system.go:254-263` | `exec.CommandContext` without `cmd.WaitDelay`: if the script spawns a child holding stdout, `Wait` blocks past cancellation. No `Setpgid`, so grandchildren survive and leak. |
| `cmd/.../main.go:238,255`, `cmd/...-config/main.go:32,48,59` | `exec.Command` (no ctx) for `snapctl`; a wedged `snapctl` hangs the configure hook. |
| `internal/monitor/hardwareinfo.go:43` | `exec.CommandContext(ctx, "lshw", "-xml", "-quiet")` with **no per-run timeout** — `ctx` is daemon-lifetime, so it only cancels at shutdown. `lshw` probes PCI/USB/DMI/SCSI and can wedge on a misbehaving device, blocking this goroutine for the process's life. It also runs immediately at startup (`:29`), competing for CPU/IO during Core boot. **[verified]** |
| `internal/monitor/mountinfo.go:166` | `syscall.Statfs` is **uninterruptible** — on a hung autofs/removable mount no `ctx` check can rescue the goroutine. Partly mitigated by the `/dev/`-prefix + `stableFilesystems` filter (`:156-164`), which excludes network filesystems. |
| `internal/monitor/networkdevice.go:171-190` | `readSpeed`/`readDuplex` read `/sys/class/net/<if>/speed`, which triggers a driver `get_link_ksettings`/MDIO query on some NICs — done synchronously for every interface every 30s. |
| `cmd/landscape-client-core/main.go:125` | `snapPackages.SendNow(context.Background(), exc)` — a **detached context** from a manager callback, so the snapd call can outlive shutdown entirely. **[verified]** |

Per-item loops also have no cancellation point: `activeprocessinfo.go:192-208`
performs 600–1000 syscalls without checking `ctx`, as do `temperature.go:51-60`
and `mountinfo.go:78-104`. Shutdown waits for the whole tick to finish.

**Fix (layered):**
1. Set `TotalTimeout` in `main.go` and apply the documented 600s default in `transport.New`.
2. Give the snapd client a `Timeout` and wrap each monitor call in `context.WithTimeout`.
3. Use `dbus.ConnectSystemBusWithContext` / `CallWithContext`.
4. Set `cmd.WaitDelay` and `SysProcAttr{Setpgid: true}`, and kill the process group.
5. Add a watchdog: have each runner publish a heartbeat timestamp; a supervisor goroutine `os.Exit(1)`s (letting systemd restart) if any heartbeat goes stale. If the snap gains `daemon: notify`, wire `WatchdogSec` + `sd_notify(WATCHDOG=1)` so systemd enforces liveness.

---

## 3. Runtime efficiency (CPU / memory / network)

### 3.1 [P1] `exchange-interval` is bypassed — ~60× more exchanges than configured — **[verified]**

`Exchange.Send` (`internal/exchange/exchange.go:167-173`) unconditionally calls
`TriggerExchange()`. Every plugin message therefore forces an immediate HTTP
exchange. With `memory-info` at 15s (`internal/monitor/memoryinfo.go:29`), the
client exchanges every ~15s regardless of `exchange-interval=900`.

Measured with a 1-hour interval configured and a 50ms send cadence:

```
plugin sends=24, HTTP exchanges=24 (exchange-interval was 1h)
```

1:1 — the interval has no effect. On a metered/cellular Core fleet this is a
~60× bandwidth and wakeup regression, and it defeats TCP connection reuse
benefits, battery idle, and server-side load assumptions.

Python's semantics: `send_message(..., urgent=False)` is the **default**
(`landscape/client/broker/server.py:200-210`) — messages queue for the next
*scheduled* exchange. Only genuinely urgent events set `urgent=True`.

**Fix:** add an `urgent bool` to the sink interface (or a separate
`SendUrgent`). Plugin telemetry uses the non-urgent path (queue only);
`operation-result` and ping-triggered wakeups use the urgent path.

### 3.2 [P1] No exponential backoff on server errors — **[observed]**

Python backs off 300→7200s with jitter on HTTP 429/5xx
(`landscape/client/broker/exchange.py:418,629-639,700-705`), explicitly to shed
load from an overloaded server. The Go client retries at a fixed interval and
treats all failures identically (`exchange.go:119-120` just logs). A fleet of
Core devices hitting a struggling server will hammer it — and, combined with
3.1, each device retries on every plugin tick.

**Fix:** port `ExponentialBackoff` with randomised delay; increase on 429/5xx,
decrease on success. Handle the 404 server-API-downgrade path too
(`exchange.py:617-625`), which is currently absent.

### 3.3 [P1] No message batching — every sample is its own message — **[observed]**

Python accumulates data points and sends one message per *hour*
(`monitor_interval=60*60`) containing many points, via `Accumulator`/step-size
(`landscape/client/accumulate.py:75-103`,
`landscape/client/monitor/cpuusage.py:24-25,50-55`). Go sends one message per
sample with a single point (`cpuusage.go:64-67`, `memoryinfo.go:50-53`).

The Go plugins conflate *sampling interval* with *send interval* — Python
deliberately separates them. Result: far more messages, each with full bpickle
envelope overhead, plus the amplified exchanges from 3.1.

**Fix:** separate `sampleInterval` from `sendInterval` per plugin; buffer points
and emit one message per send window. This composes with 3.1 and is where the
real bandwidth win is.

### 3.4 [P1] Message queue is memory-only and unbounded — **[verified]**

`Exchange.pending` is an in-memory slice; `persist.State` has no queue field
(zero `pending` references in `internal/persist/persist.go`).

Two consequences:
- **Data loss:** every daemon restart — including every `snap refresh` — silently drops all unsent messages, including `operation-result`s the server is waiting on. Python persists messages to a spool directory (`landscape/client/broker/store.py`).
- **Unbounded growth:** with no cap and no durability, a long server outage grows `pending` without limit; on a 512 MB Core device that is an OOM path. Python caps at `max_messages=100` per exchange (`exchange.py:386,752`) with a bounded on-disk spool (`directory_size=1000`).

Also note `exchange.go:230,338,382,463` use `append([]Message{x}, e.pending...)`
— a full reallocation + copy of the queue on every re-queue, O(n) per operation.

**Fix:** persist the queue to `$SNAP_COMMON` (a simple JSON spool suffices),
cap messages per exchange at 100, and bound total queue size with a documented
drop policy (oldest telemetry first, never drop `operation-result`). Use a ring
buffer or `slices.Insert` to avoid the repeated O(n) prepends.

### 3.4b [P2] Per-tick allocation in the monitor hot path — **[observed]**

`activeprocessinfo` runs every 30s (`activeprocessinfo.go:47`) and rebuilds the
full process table each tick: `os.ReadDir(/proc)` plus one `os.ReadFile` per PID
(`:187,:220`), then allocates a fresh `map[int64]processInfo` (`:191,:318-319`)
and `map[string]any` per process via `processToMap`. On a device with a few
hundred processes that is a few hundred small maps and slices every 30s — the
main steady-state GC pressure in the daemon, and the reason `GOGC=50` was
probably reached for.

Note the slice capacity hints (`:100,118,125,132`) are already correct — this is
about map/interface allocation, not sizing.

**Fix (in impact order):** reuse the previous tick's map (clear with `clear()`
rather than reallocating); avoid `map[string]any` per process in favour of a
struct converted only at the wire boundary; reuse a `[]byte` read buffer across
per-PID `stat` reads. Combined with 3.3's batching this materially lowers both
allocation rate and message volume.

### 3.4c [P1] Plugin scheduling: no initial tick, and no launch stagger — **[verified]**

Two scheduling divergences from Python, one of which is a genuine regression.

**(a) Missing initial tick.** Only 4 of 15 plugins do work before entering their
ticker loop (`computerinfo.go:60`, `hardwareinfo.go:29`, `processorinfo`,
`snappackages`; `cpuusage.go:44` primes a baseline but emits nothing). The other 11
wait a full interval before their first report — `users` waits **1 hour**,
`mountinfo`/`temperature`/`loadaverage`/`rebootrequired` 5 minutes,
`snapservices` 1 minute.

Be precise about the Python comparison: `BrokerClientPlugin.run_immediately`
defaults to `False` (`landscape/client/broker/client.py:42`), so most plugins
waiting is **faithful**, not a bug. The real regression is narrower:
`landscape/client/monitor/rebootrequired.py` sets `run_immediately = True` and
Go's `rebootrequired.go:53` does not. On a device that reboots frequently, the
server can wait 5 minutes to learn a reboot is still required.

**(b) No launch stagger — Go-only regression.** Python delays each plugin's loop
start by `random.random() * run_interval * config.stagger_launch`
(`client.py:117-121`). Go has no equivalent — `grep` for `stagger|jitter|rand\.`
across `internal/` and `cmd/` returns nothing. All 15 plugins start at once and
share intervals (five at 30s, five at 5m), so they re-converge on the same tick
forever: a periodic CPU spike plus a burst of simultaneous `Send` calls which,
because of 3.1, each triggers its own HTTP exchange.

**Fix:** add `runImmediately bool` to the plugin config and set it for
`rebootrequired` to match Python; add a stagger equivalent to Python's
`stagger_launch` (a random initial delay bounded by the interval) in the shared
ticker helper. Both belong in the `runTicker` helper suggested below, so they are
fixed once rather than 15 times.

### 3.4d [P2] Ticker scaffolding duplicated 15× — **[observed]**

Every plugin hand-rolls the same `ticker := time.NewTicker(p.interval);
defer ticker.Stop(); for { select { case <-ctx.Done(): ...; case <-ticker.C: ... } }`.
Credit where due: **all 15 have `ctx.Done()` as the first select arm and all
`defer` the ticker stop** — the loop shape is correct everywhere. But it means
every cross-cutting fix (initial tick, stagger, per-tick timeout) has 15 edit
sites, which is how 3.4c and §2's missing timeouts arose in the first place.

Four plugins additionally duplicate a "hash → compare → persist → send" watcher
pattern with four independent `Hash string` state structs
(`processorinfo.go:58-90`, `mountinfo.go:106-122`, `networkdevice.go:60-81`,
`snapservices.go:70-96`), each calling `json.Marshal` on the whole payload purely
to feed `sha256` — then usually discarding the result because the hash matched.

**Fix:** extract `runTicker(ctx, interval, runImmediately bool, fn func(context.Context, time.Time))`
and a `watch(hashOf func() any, onChange func())` helper. Stream to the hasher via
`json.NewEncoder(h)` instead of allocating an intermediate `[]byte`, and use
`hex.EncodeToString` rather than `fmt.Sprintf("%x", ...)`.

### 3.5 [P2] Runtime tuning is coarse — **[observed]**

`snap/snapcraft.yaml:25` sets `GOGC: "50"`, which trades CPU for RSS by GCing
twice as often — a reasonable instinct on a constrained device, but a blunt one.
`GOMEMLIMIT` is the modern lever: it lets the GC run lazily when there's
headroom and aggressively only near the ceiling.

**Fix:** prefer `GOMEMLIMIT` (e.g. `64MiB`) and leave `GOGC` at default, or set
`GOGC=off` with a hard `GOMEMLIMIT`. Measure RSS before/after. Also consider
`GOMAXPROCS` — 15 plugin goroutines on a 4-core device don't need the default.

### 3.6 [P2] `curl` staged into the snap but never used — **[verified]**

`snap/snapcraft.yaml:72` stages `curl`; no Go code, hook, or script references
it (`lshw` at `internal/monitor/hardwareinfo.go:43` *is* genuinely used). Dead
payload weight plus a needless CVE surface in a strictly-confined snap.

**Fix:** drop `curl` from `stage-packages`.

---

## 4. Alignment with snapd Go conventions

Checked against `snapd/CODING.md` and measured usage in the snapd tree.

| Convention | snapd | this project | Verdict |
|---|---|---|---|
| Error messages `"cannot …"`, not `"failed to …"` | 939 `cannot` vs 69 `failed to` | **0 of either** — bare gerunds: `"exchange: posting to server: %w"`, `"decoding response: %w"` | **Diverges.** Reword to `"cannot post to server: %w"`. Existing style is at least consistent and lowercase/no-period ✓ |
| Error wrapping | mixed, `%v` common | 77 uses of `%w` | **Better than snapd** — keep |
| Logging | own `logger` pkg (`Noticef`/`Debugf`) | **21 non-test files still use stdlib `log`**; only `main.go` uses `slog` | **Incomplete.** Commit `e9af620` claims "consolidate logging to use slog throughout" but did not — `log.Printf` bypasses the configured level, so `log-level` silently doesn't work for any package output |
| `Get`-prefixed getters | 5 in whole tree (rare) | 7 (`GetPingURL`, `GetInterval`, `GetPluginState`, `GetAssertions`, `GetRebootRequired`) | Minor divergence; Go style omits `Get`. `GetAssertions`/`GetRebootRequired` are defensible as remote calls |
| Package/type naming | `client.Client`, `New()` | `exchange.Exchange`, `snapd.Client`, `persist.Store` + `New()` | **Aligned** ✓ |
| Interface naming | behavioural | `MessageSink`, `CommandSource`, `ResultSink`, `Plugin`, `Handler` | **Good** ✓ |
| `gofmt` clean | enforced | **`internal/transport/transport.go` is not gofmt-clean** (struct field misalignment, lines 96-103) | **Fix**; CI lint should have caught this — see 6.1 |
| Doc comments on exported names | expected | consistently present, plus `doc.go` per package | **Good** ✓ |
| External test packages (`testpackage` linter) | enforced | mixed: `config_test`/`persist_test` external; `exchange`/`monitor` internal | Minor; internal is fine where testing unexported logic, but be deliberate |

**Note on test framework:** snapd uses `gopkg.in/check.v1` (1168 uses). This
project uses stdlib `testing` with table-driven tests and zero assertion
dependencies. **Do not change this** — stdlib `testing` is current Go best
practice; snapd's gocheck usage is legacy. This is a case where diverging from
snapd is correct.

---

## 5. Tests

### 5.1 What exists — genuinely good

- All 12 packages have tests; all pass, including under `-race`.
- Coverage 64–91% on real logic packages.
- `integration/integration_test.go` spins a fake Landscape server and covers registration, sequence tracking, resynchronize, accepted-types, and a CPU-usage round trip.
- `internal/bpickle/compat_test.go` (build tag `compat`) cross-validates the wire encoding against the **actual Python implementation** as a subprocess, wired into `.github/workflows/compat.yml`. This is the single best test asset in the repo — exactly the right way to guarantee wire compatibility.
- `internal/snapd/mock.go` gives a clean seam for snapd.
- Path-traversal defence in the attachment path is correct — **[verified]**: absolute paths are safely joined into the script dir, `../` escapes are rejected with result-code 104.

### 5.2 Structural gaps

- **`cmd/landscape-client-core` is 3.7% covered.** All wiring in `main()` — including the handler-lifetime bug (1.1) — is untested. Extract `main()`'s body into a testable `run(ctx, cfg, deps) error` and cover the wiring.
- **Handler dispatch is never tested through the real path.** Tests call `Handle` directly, which is precisely why 1.1 survived. Add tests that drive `manager.Runner.Register` + exchange dispatch.
- **No fuzzing.** `bpickle` parses untrusted network input and is the ideal `FuzzUnmarshal` target; it would have found 1.2 in seconds.
- **Almost no `t.Parallel()`** (only `persist_test.go`). `internal/manager` takes 20s of the 26s suite — mostly real `sleep`s. Inject a clock instead of sleeping.
- **No negative/hostile-input tests** for transport (truncated body, slow-loris, oversized response, non-2xx with garbage) — the areas where 1.2 and the missing timeouts live.

### 5.3 Python tests worth porting

The Python suite has **133 test files**, many covering behaviour the Go rewrite
kept. High-value targets, mapped to their Go homes:

| Python source | Scenario | Go target | Priority |
|---|---|---|---|
| `broker/tests/test_exchange.py` | 429/5xx backoff escalation & decay; 404 API downgrade | `internal/exchange` | **P0** (3.2 — currently absent) |
| `broker/tests/test_exchange.py` | `max_messages` batching; partial-ACK re-queue ordering | `internal/exchange` | **P0** (3.4) |
| `broker/tests/test_store.py` | queue durability across restart; spool cap | `internal/persist` | **P0** (3.4) |
| `lib/tests/test_bpickle.py` | malformed/adversarial payload edge cases | `internal/bpickle` | **P0** (1.2) |
| `monitor/tests/test_networkactivity.py` | **32-bit counter rollover** | `internal/monitor` | **P1** (1.3) |
| `manager/tests/test_scriptexecution.py` | time-limit kill; output truncation; attachment handling | `internal/manager` | **P1** (1.1) |
| `monitor/tests/test_cpuusage.py` | accumulator/step-size batching semantics | `internal/monitor` | **P1** (3.3) |
| `broker/tests/test_ping.py` | ping response handling; insecure-id gating | `internal/ping` | P2 |
| `monitor/tests/test_computerinfo.py`, `test_mountinfo.py`, `test_activeprocessinfo.py` | field-level message-shape assertions | `internal/monitor` | P2 |
| `monitor/tests/test_networkdevice.py:36-38` | **asserts `flags & 1`/`& 8`/`& 64` for UP/LOOPBACK/RUNNING** — porting this test alone would have caught 1.4 | `internal/monitor` | **P0** (1.4) |
| `monitor/tests/test_users.py` | `passwd`/`group` read failure must **not** produce a delete-all diff | `internal/monitor` | **P1** (1.5) |
| `lib/tests/test_persist.py` | corrupt state file recovery via `.old` backup; save must not roll back other keys | `internal/persist` | **P1** (1.6) |
| `broker/tests/test_client.py` | `run_immediately` semantics and `stagger_launch` delay | `internal/monitor` | **P1** (3.4c) |

The `test_networkdevice.py` row is the strongest single argument for this porting
work: the Go package has **77% coverage and a passing test suite**, yet ships a
wire-format bug that the existing Python test asserts against directly. Coverage
percentage is measuring the wrong thing — these are the assertions that encode the
protocol contract.

Correctly **not** portable (features intentionally dropped): apt/deb
(`packagemonitor`, `packagemanager`, `aptsources`, `aptpreferences`,
`updatemanager`), Ubuntu Pro/livepatch/USG, FDE recovery key, keystone,
ceph/swift, cloud-init, customgraph, processkiller.

---

## 6. Dependencies & supply chain

**This is a clear strength — the project is *not* trigger-happy.**

Full module graph is 4 modules:

```
github.com/godbus/dbus/v5 v5.2.2
golang.org/x/sync        v0.20.0
golang.org/x/term        v0.13.0
golang.org/x/sys         v0.27.0 (indirect)
```

| Package | Used for | Long-term support assessment |
|---|---|---|
| `golang.org/x/sync` | `errgroup`, `semaphore` | **Safe.** Go team owned. `errgroup` is ubiquitous; snapd uses it too |
| `golang.org/x/term` | password prompt in config wizard | **Safe.** Go team owned, stable, tiny |
| `golang.org/x/sys` | indirect | **Safe.** Go team owned |
| `github.com/godbus/dbus/v5` | logind Reboot/PowerOff | **Acceptable, watch it.** Community-maintained, low activity, and v5 has had long quiet stretches. **But snapd depends on the same package** (`v5.1.0`) — so this is the ecosystem-standard choice, and Canonical already carries the maintenance risk. Note this project is *ahead* of snapd (v5.2.2 vs v5.1.0) |

No test-only deps (no testify/gocheck) — stdlib `testing` throughout. No
transitive bloat. Everything else (HTTP, TLS, JSON, bpickle) is stdlib or
hand-rolled in-tree. `bpickle` being in-tree is the right call: it's a
niche wire format with no maintained Go library.

**Only real consideration:** for a *single* D-Bus call (reboot/poweroff),
shelling out to `dbus-send`, or using snapd's own `dbusutil`, would remove the
last third-party dependency. Given snapd shares the dependency, keeping godbus
is defensible — but pin it and watch upstream activity.

### 6.1 Build & CI inconsistencies — **[verified]**

These are cheap to fix and currently mask real problems:

- **Go version mismatch (three-way):** `go.mod` requires `go 1.25.0`; `.github/workflows/ci.yml:16` and `compat.yml:20` pin `go-version: "1.22"`; `snapcraft.yaml:70` builds with `go/1.22/stable`. A 1.22 toolchain cannot build a `go 1.25.0` module — so **CI and the snap build are either failing or silently resolving a different toolchain**. Align all three on 1.25.
- **Lint is not actually gating:** `internal/transport/transport.go` is not gofmt-clean yet CI passes. `golangci-lint v1.64.0` is pinned in CI while `.golangci.yml` uses no version key and enables only 4 linters (`errcheck`, `staticcheck`, `govet`) — v2 config format differs from v1, so the config may be silently ignored. Compare with snapd's `.golangci.yml` (`version: "2"`, `depguard`, `misspell`, `modernize`, `nakedret`, `testpackage`, `unused`).
- **No `-race` in CI.** The suite is currently race-clean — lock that in.
- **No `govulncheck`.** Cheap to add for a network-facing daemon.

### 6.2 Repository hygiene — **[verified]**

`.git` is **226 MiB** (`size-pack: 224.56 MiB`) because build artifacts are
committed, and **there is no `.gitignore` at all** — so every artifact is offered
to `git add` on every commit.

**Priority item: 8 committed `.snap` packages totalling 54 MB.** These are release
outputs; a `.snap` is a squashfs image, so it is incompressible to git and each
rebuild stores a brand-new full blob:

| Tracked `.snap` file | Size |
|---|---|
| `landscape-client-core_0+git.75aec9b-dirty_amd64.snap` | 7.4M |
| `landscape-client-core_0+git.75aec9b_amd64.snap` | 7.4M |
| `landscape-client-core_0+git.7f5508a_amd64.snap` | 7.4M |
| `landscape-client-core_0+git.e2a0966-dirty_amd64.snap` | 7.4M |
| `landscape-client-core_0+git.e2a0966_amd64.snap` | 6.1M |
| `landscape-client-core_0+git.ea82cf8_amd64.snap` | 7.4M |
| `landscape-client-core_26.04_amd64.snap` | 4.6M |
| `landscape-client-core_good-working-version-dirty_amd64.snap` | 6.1M |
| **Total** | **54M** |

Note the `-dirty` suffixes and `good-working-version` — these are uncommitted-tree
snapshots and an ad-hoc "known good" copy, i.e. exactly the local scratch state that
version control is meant to replace. They also carry a supply-chain smell: a
prebuilt binary artifact in-tree can be consumed by someone assuming it corresponds
to the source beside it, and a `-dirty` snap by definition does not.

Also tracked: both unstripped Go binaries (`landscape-client-core` 10 MB with
`debug_info`, `landscape-client-core-config` 3.5 MB), `snapcraft_output.log`,
`test_output.log`, `test_output.txt`, `test_results.txt`, `fix_mountinfo.patch`,
`fix_transport.py`, `internal/transport/transport.patch`. The stray `fix_*.py`/
`*.patch` files suggest ad-hoc patching that belongs in git history, not the tree.

**Fix — step 1, add a `.gitignore`** (none exists today):

```gitignore
# Snap build output — release artifacts belong in the Snap Store, not git
*.snap
parts/
prime/
stage/
snapcraft_output.log

# Compiled binaries (built by `go build ./cmd/...`)
/landscape-client-core
/landscape-client-core-config

# Test and scratch output
*.log
test_output.txt
test_results.txt
*.patch
```

Anchor the binary entries with a leading `/` so they match only the repo-root build
output and never a same-named directory or package path. Keep `*.snap` first and
commented — it is the single highest-value line, and it also stops the
`snapcraft`-in-repo workflow from silently re-adding a 7 MB blob.

**Step 2, untrack the artifacts** (keeps the local files):

```
git rm --cached '*.snap' landscape-client-core landscape-client-core-config \
  snapcraft_output.log test_output.log test_output.txt test_results.txt \
  fix_mountinfo.patch fix_transport.py internal/transport/transport.patch
```

**Step 3, decide about history.** Be aware that steps 1–2 do **not** reclaim the
224 MiB: the blobs are reachable from existing commits (verified — e.g.
`landscape-client-core_26.04_amd64.snap` is touched by 2 commits), so every clone
still pays for them. Only a history rewrite (`git filter-repo --path-glob '*.snap'
--invert-paths`) actually shrinks the repo, and it rewrites every hash — coordinate
with anyone holding a clone or open branch first. This is the one item in this
document that gets *harder* to fix the longer it waits, since each new build adds
another full blob.

Ship releases via the Snap Store channels or GitHub Releases instead.

---

## 7. Python-fidelity vs Go idiom

The rewrite is **mostly well-adapted**, not a blind transliteration: it uses
goroutines + channels instead of a Twisted reactor, `errgroup` for lifecycle,
`bufio.Scanner` for `/proc` parsing, typed structs for snapd JSON, and
`context` throughout. Files like `cpuusage.go` are clean idiomatic Go.

Where it followed Python too literally:

- **Message representation.** `Message = map[string]any` (`exchange.go:27`) mirrors Python dicts and forces runtime type-switching everywhere (`exchange.go:406-428`, `toInt64`, `msgBytes`, `manager/snap.go:15-71`). Inbound/outbound message *shapes* are known and finite — typed structs with a custom bpickle codec would move these errors to compile time and cut allocations. This is a large refactor; the pragmatic middle is to keep `map[string]any` at the wire boundary and decode into typed structs per handler.
- **Where fidelity was rightly kept:** field names, the `md5(";".join(types))` accepted-types hash (`exchange.go:604-611`), status constants, `strings.ToValidUTF8` mirroring `decode("utf-8","replace")` (`system.go:269`). These are wire-visible — do not "improve" them, and the compat tests should keep guarding them.
- **Where fidelity was wrongly dropped:** backoff (3.2), batching/accumulator (3.3), queue durability + caps (3.4), rollover correction (1.3), urgent-vs-normal exchange (3.1), watchdog (§2). These weren't Python quirks — they were load-shedding and reliability features earned in production.

### 7.1 Faithful-but-arguably-wrong — **do not "fix" without server-side agreement**

These look like bugs and will be flagged by any future audit, but each **matches
the Python client exactly**, so the server has been receiving these semantics for
years. Changing them unilaterally changes reported values across the fleet. Listed
here so they get triaged as product decisions, not silently "corrected".

| Item | Go | Python | Note |
|---|---|---|---|
| Free memory over-reports | `memoryinfo.go:105` `(MemFree+Buffers+Cached)/1024` | `landscape/lib/sysstats.py:34` — identical | `Cached` includes `Shmem`/tmpfs, which is **not** reclaimable. Ubuntu Core mounts every snap plus `/run` on tmpfs, so this inflates free memory on exactly the target platform. `MemAvailable` is the correct field — but it is a wire-visible behaviour change. |
| Free disk includes reserved blocks | `mountinfo.go:175` `stat.Bfree` | `landscape/lib/disk.py:71` `f_bfree` — identical | `Bavail` is what `df` shows an unprivileged user; `Bfree` includes the ~5% root reserve. Faithful; would change reported free space everywhere. |
| CPU total double-counts guest time | `cpuusage.go:94-101` sums all `/proc/stat` fields | Deliberate parity, noted at `cpuusage.go:123` | Linux already counts `guest`/`guest_nice` inside `user`/`nice`. Inflates the total ⇒ under-reports utilisation. Negligible on a Core appliance, material on a hypervisor. |
| Lifetime-average CPU per process | `activeprocessinfo.go:247-251` | Same formula | Not instantaneous usage. Accurate to Python but misleading — and it drives the churn in 7.2 below. |

`activeprocessinfo.go:213`'s `const clkTck = 100` deserves a specific warning: the
**value is correct** (the kernel fixes `USER_HZ` at 100 for `/proc` regardless of
`CONFIG_HZ`, which is 250 on Raspberry Pi kernels) but the comment calls it the
"kernel timer frequency", which invites a future contributor to "fix" it to
`CONFIG_HZ`. Correct the comment, not the constant.

### 7.2 Go-only regressions worth fixing (no Python excuse)

- **`diffProcesses` degenerates** (`activeprocessinfo.go:324`): `oldInfo != newInfo` compares the whole struct, including `percentCPU` rounded to 0.1. Any process doing work changes that field, so `update-processes` carries a large share of the process table every 30s and the diff stops saving bandwidth. Exclude or bucket `percentCPU` in the change test.
- **`temperature.go:51` iterates a map**, so multi-zone devices emit messages in nondeterministic order (one `exchange.Message` per zone per tick). Sort the zones.
- **`processorinfo.go:176-179,218-222`**: an ARM `/proc/cpuinfo` block with no `processor` line defaults to `processor-id: 0`, so multiple such blocks collide and the `sort.Slice` at `:63` is unstable across them. Use `slices.SortStableFunc` and key on a real identifier.
- **`mountinfo.go:124-132`**: `free-space` is sent unconditionally every 5 minutes with no change detection and no cap — 288 messages/day from this plugin alone into the unbounded queue of 3.4.
- **`Runner.Run` always returns nil** (`monitor/runner.go:43-58`), so `main.go:180`'s error branch is unreachable and a total monitor collapse is invisible to the supervisor. Directly undercuts the watchdog work in §2.
- **Silently discarded errors that fabricate data:** `activeprocessinfo.go:240-243` has four `_ =` `ParseInt`s on `utime`/`stime`/`starttime`/`vsize`, so a malformed field reports the process as 0% CPU with a boot-time start; `:295` ignores `scanner.Err()`. `computerinfo.go:75` `hostname, _ := os.Hostname()` sends an empty hostname. `mountinfo.go:106` `layoutData, _ := json.Marshal(...)` hashes `null` on failure.
- **`_ = state.SetPluginState(...)`** (`mountinfo.go:111`, `networkdevice.go:72`, `snapservices.go:76`, `rebootrequired.go:72`): the in-memory hash advances regardless, so a failed save means the change is **never re-sent** and the plugin believes the old value was reported. Pairs badly with 1.6.
- **`_ = state.GetPluginState(...)`** (`processorinfo.go:44`, `users.go:59`, `mountinfo.go:57`, `networkdevice.go:45`): corrupt state silently becomes zero state — for `users`, every user is re-sent as `create-users`.

### 7.3 Idiom cleanups (mechanical, low risk)

Confirmed `go vet ./internal/monitor/` and `gofmt -l internal/monitor` are clean;
these are style/allocation items rather than defects:

- **Zero use of `slices`/`maps` despite `go 1.25.0`** — a repo-wide grep returns nothing. Candidates: `sort.Slice`→`slices.SortFunc` (`networkdevice.go:92`, `processorinfo.go:63`, `snapservices.go:67`), `sort.Strings`→`slices.Sort` (6 sites in `users.go`), and the hand-rolled set intersection/difference at `users.go:255-281` (four `map[string]bool` builds per group per tick).
- **`strings.Cut` over `SplitN`/`Index`** — `computerinfo.go:209`, `processorinfo.go:126,185`, and the manual colon hunt at `networkactivity.go:102-107`.
- **`fmt.Sprintf` doing non-formatting work** — `networkdevice.go:172,185` build sysfs paths (use `filepath.Join`); `networkdevice.go:134` formats a netmask byte-by-byte (use `net.IP(mask).String()`); `fmt.Sprintf("%x", sha256.Sum256(...))` at `processorinfo.go:73`, `mountinfo.go:107`, `networkdevice.go:66`, `snapservices.go:104` (use `hex.EncodeToString`).
- **`map[string]any` where a struct belongs**, then type-asserted back: `processorinfo.go:104,113,162` returns `[]map[string]any` and `:64-66` asserts `["processor-id"].(int)` to sort; `mountinfo.go:137` likewise, with `:79-93` re-asserting every field and logging "unexpected type" for structurally impossible cases. A typed struct plus a `toMap()` at the wire boundary removes ~40 lines and every assert.
- **`computerinfo.go:81-138`** — twelve copy-pasted `if !prev.Initialized || x != prev.X {...}` blocks plus a mirror struct rebuilt field-by-field at `:140-153`; naked returns with named results at `:162,199`.
- **Duplicated `/proc` parsing** — `/proc/meminfo` is opened and parsed twice with different key sets (`computerinfo.go:162-188` at 5m, `memoryinfo.go:63-107` at 15s); the same key:value scanner boilerplate recurs in 6 files. One `scanKV(r, sep, fn)` helper covers all. `cpuusage.go:84` builds a 64 KB `bufio.Scanner` to read a single line.

The `main()` wiring (`cmd/landscape-client-core/main.go:29-232`) is also doing
too much: config, logging, 15 plugins, 8 handlers, signals, and a hand-rolled
double-`select` shutdown with two 5s timeouts. The shutdown logic reads
`groupDone` in two places (`:211`, `:225`), so the second read can block the
full 5s even when the group already exited. Extract `run(ctx, deps) error` and
simplify.

---

## 8. External executable invocation & error handling

Audited every `exec.*` site in the tree (6 non-test sites across 4 files) plus the
error handling around each. **This section deliberately holds Go to a correct
standard rather than to Python parity** — where Python is also wrong, that is noted
but not used as justification.

| # | Site | Binary | Verdict |
|---|---|---|---|
| 1 | `internal/manager/system.go:254-263` | interpreter (server-supplied) | **3 defects — 8.1, 8.2, 8.3** |
| 2 | `internal/monitor/hardwareinfo.go:43` | `lshw` | **2 defects — 8.4, 8.5** |
| 3 | `cmd/landscape-client-core/main.go:238` | `snapctl get` | **1 defect — 8.4** |
| 4 | `cmd/landscape-client-core/main.go:255` | `snapctl set` | OK — uses `CombinedOutput`, wraps output into the error |
| 5 | `cmd/.../-config/main.go:32` | `snapctl get` | **Correct — the reference implementation (8.4)** |
| 6 | `cmd/.../-config/main.go:48,59` | `snapctl set`/`restart` | OK — `CombinedOutput`, output in error |

### 8.1 [P0] `time-limit` is not enforced — a script that spawns a subprocess runs unbounded — **[verified]**

`exec.CommandContext` kills only the direct child. Because `cmd.Stdout`/`cmd.Stderr`
are an `io.Writer` (`limitWriter`) rather than an `*os.File`, `os/exec` creates a
pipe and copies it in a goroutine — and `cmd.Run()` cannot return until **every**
process holding the write end has exited. A killed shell's `sleep` inherits that
pipe and keeps `Wait()` blocked. Measured:

| Script | `time-limit` | Actual duration | Overrun |
|---|---|---|---|
| `sleep 10` | 1s | **10.0s** | 9.0s |
| `sleep 20 & echo done` | 1s | **20.0s** | 19.0s |

`cmd.WaitDelay` is never set and `SysProcAttr.Setpgid` is never set, so there is
also no process-group kill: grandchildren survive indefinitely as orphans holding
the snap's file descriptors.

Python explicitly handles this, and its comment describes this exact failure
(`landscape/client/manager/scriptexecution.py:412-426`):

```python
# Sometimes children of the shell we're killing won't die unless their
# file descriptors are closed! For example, if /bin/sh -c "cat" is the
# process, "cat" won't die when we kill its shell.
for i in (0, 1, 2):
    self.transport.closeChildFD(i)
self.transport.signalProcess("KILL")
```

So this is **lost production hardening**, not a Python-parity question. A
server-issued `execute-script` with `time-limit` set is the client's only bound on
untrusted work; unbounded, it is a resource-exhaustion path — and note the
semaphore in `manager/runner.go:68` means stuck handlers eventually block *all*
manager operations.

**Fix:** set `cmd.WaitDelay` (e.g. 5s) so `Wait` returns after the deadline even
with the pipe held open, and set `SysProcAttr: &syscall.SysProcAttr{Setpgid: true}`,
then signal the whole group (`syscall.Kill(-cmd.Process.Pid, SIGKILL)`) on timeout.
Add a regression test asserting `Handle` returns within `time-limit + WaitDelay`
for `sleep 30 & echo done`.

### 8.2 [P1] Exec failures report an empty `result-text` to the server — **[verified]**

`runErr` is used only as a boolean (`system.go:274-277`); its text is never sent.
When the failure happens at `fork/exec` time there is no script output either, so
the operator sees a failure with **no explanation at all**:

| Scenario | Local log | `result-text` sent to server |
|---|---|---|
| interpreter not executable | `fork/exec ...: permission denied` | `""` |
| interpreter is a directory | `fork/exec ...: permission denied` | `""` |
| interpreter not a valid binary | `fork/exec ...: exec format error` | `""` |
| script exits 42 | `exit status 42` | `"to-stdout\nto-stderr\n"` (exit code absent) |

On Ubuntu Core the log may not be readable to whoever issued the operation — the
Landscape UI is the only feedback channel, and it shows a blank failure.

Python is comparable here (it also maps to a bare `PROCESS_FAILED_RESULT` and
discards the captured `exit_code`, `scriptexecution.py:219-236`), but nothing
prevents Go from doing better: this is purely additive to the payload.

**Fix:** when output is empty, put the error text in `result-text`
(`fmt.Sprintf("execute-script: %v", runErr)`). Include the exit status when
available via `errors.As(runErr, &exitErr)` / `exitErr.ExitCode()`. Distinguish
"interpreter could not be executed" (exec error) from "script ran and failed"
(non-zero exit) — currently both are result-code 103.

### 8.3 [P1] Whitespace-only `interpreter` panics the handler — **[verified]**

`system.go:175-177`:

```go
interpreterFields := strings.Fields(interpreter)
interpreterBin := interpreterFields[0]   // panics when interpreter is all whitespace
```

The `== ""` default at `:171` does not catch `" "`, `"\t"`, or `"\n"` —
`strings.Fields` returns an empty slice for all of them. Reproduced through the
real handler:

```
PANIC escaped Handle: runtime error: index out of range [0] with length 0
messages sent to server before panic: 0
```

The `recover()` in `manager/runner.go:80-86` catches it, so the daemon survives and
the server receives `panic: runtime error: index out of range [0] with length 0`.
That containment is why this is P1 and not P0 — but a server-supplied field
reaching an index-out-of-range is a validation gap, and the operator gets a Go
runtime error as their result text.

**Fix:** treat whitespace-only as absent — `if strings.TrimSpace(interpreter) == ""
{ interpreter = "/bin/sh" }` — and guard `len(interpreterFields) == 0` before
indexing. Table-test `""`, `" "`, `"\t"`, `"\n"`.

Related, same function: `os.Stat` at `system.go:184` is an insufficient
executability check — it passes for directories and non-executable files, which
then fail later at `fork/exec` with the empty-`result-text` problem of 8.2. Use
`unix.Access(path, unix.X_OK)` plus a mode check, and report the specific reason.

### 8.4 [P1] `ExitError.Stderr` discarded at 5 of 6 exec sites — **[verified]**

`.Output()` captures stderr into `ExitError.Stderr`, but `%v` on the error prints
only `"exit status N"`. Demonstrated:

```
hardwareinfo.go:43 style -> log says: "exit status 1"
  ...discarded stderr:    "lshw: DMI probe failed\n"
  ...discarded exit code: 1

main.go:238 style        -> error is: "exit status 2"
  ...discarded stderr:    "error: no such option\n"
```

`cmd/landscape-client-core-config/main.go:34-37` **already does this correctly** —
that pattern should simply be applied at the other sites, most importantly
`main.go:238` (`snapctl get`, which the daemon depends on for all configuration)
and `hardwareinfo.go:43`.

Also missing everywhere: `exec.ErrNotFound` is never distinguished. `lshw` is
staged in the snap (`snapcraft.yaml:71`), but if that ever changes the daemon logs
a generic failure daily rather than "lshw not installed". `errors.Is(err,
exec.ErrNotFound)` gives an unambiguous, actionable message.

**Fix:** add one shared helper and route every exec site through it:

```go
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("cannot run %s: executable not found", name)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("cannot run %s: %w: %s", name, err, bytes.TrimSpace(ee.Stderr))
	}
	return nil, fmt.Errorf("cannot run %s: %w", name, err)
}
```

This also satisfies the snapd `"cannot …"` convention (§4) and is the natural place
to enforce the per-run timeout that §2 wants for `lshw`.

### 8.5 [P2] `lshw` success is not validated — empty/truncated XML is sent as data — **[verified]**

`hardwareinfo.go:43-51` treats exit code 0 as sufficient. A zero-length or
truncated stdout is forwarded verbatim:

```
empty-but-success:     err=<nil> len(out)=0
  => sends {"type":"hardware-info","data":<empty>} to the server
truncated XML, exit 0: len=11 content="<list><node"
  => sent verbatim; no XML validation
```

`lshw` under strict confinement can be partially denied by AppArmor and still exit
0, so this is reachable. Sending empty `hardware-info` may cause the server to
overwrite good inventory with nothing.

**Fix:** reject empty output, and validate parseability
(`xml.Unmarshal` into a minimal struct, or `xml.NewDecoder` token-scan) before
sending. Log and skip the tick on failure rather than reporting bad inventory.

### 8.6 What is already correct

Worth recording so a future pass does not "fix" it:

- `limitWriter` (`system.go:90-110`) correctly shares one 5 MiB budget across stdout and stderr under a mutex, with an explicit note about concurrent copying.
- `strings.ToValidUTF8` (`system.go:269`) mirrors Python's `decode("utf-8","replace")` — wire-visible, keep it.
- The attachment path-traversal guard (`system.go:225-231`) is correct; tested and passing.
- The script is written `0700` into a per-operation dir under `$SNAP_COMMON` and `defer os.RemoveAll`'d (`system.go:196-206`) — no world-writable temp file, no leaked scripts.
- `context.WithTimeout` is correctly derived per operation (`system.go:245-250`) — the mechanism is right, it is only the *enforcement* that 8.1 breaks.
- Timeout vs failure are correctly distinguished into result-codes 102 vs 103 (`system.go:274-277`), matching Python's `TIMEOUT_RESULT`/`PROCESS_FAILED_RESULT`.
- Partial output *is* preserved on timeout (verified: `result-text="before-timeout\n"` with code 102).
- `snapctl set` sites use `CombinedOutput` and fold the output into the returned error — the right call for a command whose diagnostics matter more than its stdout.

---

## Prioritised action list

**P0 — correctness/availability, do first**
1. Fix handler context lifetime (1.1) — decouple handler execution from the exchange cycle; add a dispatch-path regression test.
2. Add bpickle recursion depth limit (1.2) + `FuzzUnmarshal`; consider lowering `maxResponseBytes`.
2b. Enforce `time-limit`: set `cmd.WaitDelay` + `Setpgid` and kill the process group (8.1). Verified unbounded today — a `sleep 20 &` script ran 20× its 1s limit. Supersedes the narrower item 11 below.

**P1 — reliability & efficiency**
3. Apply the documented `TotalTimeout` default and set it in `main.go`; add timeouts to snapd client, monitor snapd calls, and D-Bus (§2).
4. Introduce urgent-vs-normal send semantics so `exchange-interval` is honoured (3.1).
5. Port exponential backoff with jitter for 429/5xx + the 404 API-downgrade path (3.2).
6. Persist the message queue; cap per-exchange at 100 and bound total size (3.4).
7. Add a watchdog (heartbeat + self-restart; `daemon: notify` + `WatchdogSec` if feasible) (§2).
8. Separate sample interval from send interval; batch data points (3.3), and cut per-tick allocation in `activeprocessinfo` (3.4b).
9. Align Go version across `go.mod`/CI/snapcraft; make lint actually gate; add `-race` and `govulncheck` (6.1).
10. Fix 32-bit counter rollover (1.3).
11. *(folded into 2b — `WaitDelay`/`Setpgid` is P0, not P1: the timeout is currently unenforced, not merely imprecise.)*
11b. Send the exec error in `result-text` when output is empty, and include the exit status; split "interpreter not executable" from "script failed" (8.2). Today the operator sees a blank failure in the Landscape UI.
11c. Treat whitespace-only `interpreter` as absent and guard the `Fields[0]` index (8.3); replace the `os.Stat` executability check with `unix.Access(..., X_OK)`.
11d. Add the `runCmd` helper from 8.4 and route all 6 exec sites through it — surfaces `ExitError.Stderr` and `exec.ErrNotFound`, and is the single place to enforce the `lshw` timeout. Copy the pattern already correct at `cmd/.../-config/main.go:34-37`.
12. **Never substitute empty data for a failed read** — fix `users.go` to skip the tick (1.5), and audit the other plugins for the same shape.
13. **Delete the `SetPluginState` fallback** and scope `PluginStateAccessor` to its own key so it cannot write `SecureID`/sequence numbers; add `.old`-backup recovery for a corrupt state file (1.6).
14. Add a per-run timeout to `lshw` and per-tick timeouts to all snapd calls; replace `context.Background()` at `main.go:125` with the daemon context (§2).
15. Make `Runner.Run` report plugin collapse instead of always returning nil (7.2) — required for the watchdog in item 7 to mean anything.
16. Extract a shared `runTicker` helper, then fix initial-tick (`rebootrequired`, to match Python) and add launch stagger in that one place (3.4c, 3.4d).
17. Stop `diffProcesses` degenerating by excluding/bucketing `percentCPU` (7.2).

**P2 — hygiene & consistency**
18. Complete the `slog` migration so `log-level` works — **verified 87 `log.Printf` sites vs 22 `slog` calls repo-wide, incl. 56 sites across all 16 files in `internal/monitor` alone**, which is why per-tick chatter still appears at `log-level=error`.
19. **Add a `.gitignore` (there is none) and untrack the 8 committed `.snap` files — 54 MB of release artifacts in git.** Then `git rm --cached` the unstripped binaries and log/patch scratch files. Note `git rm --cached` alone will *not* shrink the 224 MiB pack; that needs `git filter-repo`, which rewrites hashes and must be coordinated. Do this early — every rebuild adds another incompressible blob (6.2).
20. `gofmt` `internal/transport/transport.go` (still the only unformatted file).
21. Reword errors to snapd's `"cannot …"` form (§4).
22. Drop unused `curl` from `stage-packages` (3.6).
23. Single-source the version string (1.7).
24. Extract `run()` from `main()`; raise `cmd/` coverage above 3.7%; simplify shutdown (§7).
25. Port the P0/P1 Python test scenarios in 5.3; add `t.Parallel()` and inject a clock to cut the 20s manager suite.
26. Re-evaluate `GOGC=50` vs `GOMEMLIMIT`; consider `GOMAXPROCS` (3.5).
27. Sort `temperature` zones; stabilise `processorinfo` IDs; add change-detection to `mountinfo` free-space; stop discarding `ParseInt`/`scanner.Err()` errors that fabricate data (7.2).
27b. Validate `lshw` output before sending — reject empty, check it parses as XML (8.5).
28. Idiom pass: adopt `slices`/`maps`, `strings.Cut`, `filepath.Join`, `hex.EncodeToString`; replace `map[string]any` internals with typed structs; dedupe `/proc` scanner boilerplate (7.3).
29. Fix the misleading `clkTck` comment **without changing the value** (7.1).

**Triage separately — faithful to Python, wire-visible (§7.1)**
30. Decide, with server-side agreement, whether to switch free-memory to `MemAvailable`, free-disk to `Bavail`, and to stop double-counting guest CPU time. Do not treat these as cleanups.
