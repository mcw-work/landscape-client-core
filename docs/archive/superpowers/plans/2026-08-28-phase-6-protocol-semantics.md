# Phase 6 - Protocol Semantics Decision Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to execute this evidence-gathering plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reach server-approved decisions on HTTPS ping and three Python-compatible telemetry calculations without changing production wire behavior prematurely.

**Architecture:** This is a decision phase. It captures current behavior, measures the alternative on representative Ubuntu Core systems, checks Landscape server ingestion/alert assumptions, and records one of `KEEP`, `CHANGE`, or `DEFER` for each item. Any `CHANGE` outcome creates a separate implementation plan with rollout and rollback criteria.

**Tech Stack:** Go 1.25, Linux `/proc`, `statfs`, HTTP/TLS, Landscape message API, Ubuntu Core

**Spec:** [docs/superpowers/specs/2026-08-28-code-review-follow-up-design.md](../specs/2026-08-28-code-review-follow-up-design.md)

**Branch:** `research/06-protocol-semantics`, cut from `fix/05-runtime-boundaries` after Phase 5 verification

---

## Inputs and Ownership

Before starting, identify named owners for:

- Landscape client behavior;
- Landscape server message ingestion;
- server-side alerts/graphs derived from memory, disk and CPU values;
- the `/ping` endpoint and its TLS/proxy routing;
- at least one representative Ubuntu Core test device.

Do not put credentials, device IDs, customer hostnames, registration keys, or raw
server logs in the repository. Record aggregate values and redact endpoint hosts
unless the host is already public documentation.

## Decision Record

This phase creates one artifact:

`docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md`

Use this exact top-level structure:

```markdown
# Protocol Semantics Decision

**Evidence date:** YYYY-MM-DD
**Client baseline:** <commit>
**Server build/environment:** <non-secret identifier>
**Ubuntu Core devices:** <model, architecture, Core series, kernel>
**Decision owners:** <names or teams>

## Summary

| Item | Current | Candidate | Decision | Owner approval |
|---|---|---|---|---|
| Ping transport | HTTP default | HTTPS | KEEP/CHANGE/DEFER | name/date |
| Free memory | MemFree+Buffers+Cached | MemAvailable | KEEP/CHANGE/DEFER | name/date |
| Free disk | Bfree | Bavail | KEEP/CHANGE/DEFER | name/date |
| CPU total | Includes guest fields twice | Exclude guest fields from total | KEEP/CHANGE/DEFER | name/date |
| Process CPU | Lifetime average | Interval utilization | KEEP/CHANGE/DEFER | name/date |

## Ping Evidence
## Memory Evidence
## Disk Evidence
## CPU Evidence
## Process CPU Evidence
## Compatibility And Rollout
## Follow-up Work
```

A row may be marked `CHANGE` only when the corresponding task's acceptance gate
is satisfied. Unknown ownership or unavailable representative data means `DEFER`,
not an inferred approval.

## Task 0: Create the research branch

- [ ] **Step 1: Verify the Phase 5 baseline**

```bash
git switch fix/05-runtime-boundaries
git status --short
go test -race ./...
```

Expected: tests pass and no tracked changes. Preserve the untracked August review
if it has not yet been committed separately.

- [ ] **Step 2: Create the branch**

```bash
git switch -c research/06-protocol-semantics
```

- [ ] **Step 3: Record exact current code anchors**

```bash
rg -n 'MemFree|Buffers|Cached|MemAvailable' internal/monitor/memoryinfo.go internal/monitor/metrics_test.go
rg -n 'Bfree|Bavail' internal/monitor/mountinfo.go internal/monitor/*_test.go
rg -n 'guest|guest_nice|total' internal/monitor/cpuusage.go internal/monitor/metrics_test.go
rg -n 'GetPingURL|Scheme: "http"|insecure_id' internal/config internal/ping
```

Expected: current formulas match Python compatibility: memory uses
`MemFree+Buffers+Cached`, disk uses `Bfree`, CPU sums guest fields already included
in user/nice, and the derived ping URL uses HTTP.

## Task 1: Verify HTTPS ping compatibility

**Files inspected:**
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/ping/ping.go`
- Landscape server/proxy configuration outside this repository

- [ ] **Step 1: Establish the current client contract**

Run:

```bash
go test ./internal/config ./internal/ping -count=1
rg -n 'GetPingURL|PingURL|PostForm' internal/config internal/ping
```

Record in the decision document:

- explicit `PingURL` overrides derivation;
- default derivation forces `http://<main-host>/ping`;
- pings POST form field `insecure_id` every configured interval;
- the response is bpickle and does not depend on transport scheme;
- the bpickle depth/size defenses from Phase 1 remain required even with HTTPS.

- [ ] **Step 2: Obtain endpoint-owner permission for a synthetic probe**

Before sending a request, record approval to use a non-device value such as
`phase-6-probe`. If the endpoint owner does not approve, mark the decision `DEFER`
and skip network probing.

- [ ] **Step 3: Probe HTTP and HTTPS routing without real device identity**

Set only a public/non-secret server origin:

```bash
export LANDSCAPE_SERVER_ORIGIN='https://landscape.example.invalid'
```

Replace the example value in the shell, not in a committed file. Then run:

```bash
host="$(printf '%s' "$LANDSCAPE_SERVER_ORIGIN" | sed -E 's#^[a-z]+://([^/]+).*$#\1#')"
for scheme in http https; do
  curl --silent --show-error --output "/tmp/phase6-ping-${scheme}.body" \
    --dump-header "/tmp/phase6-ping-${scheme}.headers" \
    --write-out "${scheme} status=%{http_code} redirect=%{redirect_url} tls=%{ssl_verify_result}\n" \
    --data-urlencode 'insecure_id=phase-6-probe' \
    "${scheme}://${host}/ping"
done
```

Do not use `--insecure`. Record status, redirects, certificate verification,
proxy path, response content type/length, and whether both routes reach the same
ping service. Delete the temporary bodies after recording non-sensitive facts:

```bash
rm -f /tmp/phase6-ping-http.body /tmp/phase6-ping-http.headers \
      /tmp/phase6-ping-https.body /tmp/phase6-ping-https.headers
```

- [ ] **Step 4: Check deployment compatibility**

The ping owner must answer and the record must capture:

1. Is HTTPS `/ping` supported on every Landscape deployment targeted by this snap?
2. Do any supported proxies route HTTP ping separately from the main HTTPS API?
3. How many clients set an explicit `PingURL`?
4. Should derivation copy the main URL scheme, force HTTPS, or remain unchanged?
5. What is the rollback if upgraded clients cannot reach HTTPS ping?

- [ ] **Step 5: Apply the decision gate**

Mark:

- `CHANGE` only if all supported deployments serve HTTPS ping and the owner
  approves a rollout/rollback path;
- `KEEP` if HTTP is an accepted product/security trade-off;
- `DEFER` if deployment coverage or ownership is unknown.

If `CHANGE`, the follow-up plan must test explicit override, HTTPS main URL, HTTP
development URL, proxy behavior, and fallback policy. Do not implement fallback
from HTTPS to cleartext automatically unless the security owner explicitly
approves downgrade behavior.

## Task 2: Measure free-memory semantics

**Files inspected:**
- `internal/monitor/memoryinfo.go`
- `internal/monitor/metrics_test.go`

- [ ] **Step 1: Capture representative device metadata**

For each approved Ubuntu Core test device, record architecture, Core series,
kernel version, total RAM, installed snap count, and whether workloads use tmpfs.
Do not record serial numbers or machine IDs.

- [ ] **Step 2: Measure both formulas under the same snapshots**

Run this on each test device at idle, during a snap refresh, and under a
representative workload:

```bash
awk '
  /^(MemFree|Buffers|Cached|Shmem|MemAvailable):/ { value[$1] = $2 }
  END {
    legacy = value["MemFree:"] + value["Buffers:"] + value["Cached:"]
    available = value["MemAvailable:"]
    printf "legacy_kib=%d available_kib=%d shmem_kib=%d delta_kib=%d delta_pct=%.2f\n",
      legacy, available, value["Shmem:"], legacy-available,
      legacy == 0 ? 0 : 100*(legacy-available)/legacy
  }
' /proc/meminfo
```

Take at least 30 snapshots per state using the device team's normal collection
tooling; do not add a permanent polling utility to this repository. Record median,
p95 and maximum absolute/percentage delta.

- [ ] **Step 3: Verify kernel and server assumptions**

Record:

- whether every supported Core kernel supplies `MemAvailable`;
- how missing values would be handled;
- which server alerts, graphs and APIs consume the existing free-memory value;
- whether thresholds encode the historical formula;
- whether mixed old/new clients can be distinguished without changing the tuple.

Do not append a formula marker to the existing bpickle tuple without proving the
server decoder accepts extra fields.

- [ ] **Step 4: Apply the decision gate**

`CHANGE` requires representative measurements, a missing-field policy, server
threshold review, mixed-fleet handling, and rollback criteria. Otherwise choose
`KEEP` or `DEFER` and explain why.

## Task 3: Measure free-disk semantics

**Files inspected:**
- `internal/monitor/mountinfo.go`
- `internal/monitor/sysinfo_test.go`

- [ ] **Step 1: Measure `Bfree` and `Bavail` for reported filesystems**

On each representative device, enumerate the same stable local filesystems the
plugin reports, then run for each mount point:

```bash
stat -f -c 'mount=%m client_bsize=%s fundamental_block_size=%S blocks=%b bfree=%f bavail=%a' /
```

Repeat with each reported mount substituted for `/`. Compute:

$$
\Delta_{bytes} = (Bfree - Bavail) \times clientBsize
$$

and record the absolute delta, percentage of filesystem size, filesystem type,
and configured reserve. Use GNU stat `%s`, corresponding to the client's
`Statfs_t.Bsize`, for client-compatible byte values; retain `%S` to expose
systems where the fundamental block size differs. Include one nearly-full test
filesystem if the device owner can provide it safely.

- [ ] **Step 2: Trace server consumers**

The server owner must identify alerts, UI labels, APIs and capacity calculations
that interpret `free-space`. Confirm whether they mean root-available space,
total unallocated blocks, or strict Python compatibility.

- [ ] **Step 3: Apply the decision gate**

A `CHANGE` to `Bavail` requires server approval for the fleet-wide downward step,
threshold review, mixed-client behavior, and rollback criteria. Reporting both
values is not automatically compatible: it needs an agreed new message field or
message type. Without that protocol, choose `KEEP` or `DEFER`.

## Task 4: Measure guest CPU accounting

**Files inspected:**
- `internal/monitor/cpuusage.go`
- `internal/monitor/metrics_test.go`

- [ ] **Step 1: Confirm the parser's field mapping**

Record the `/proc/stat` positions used by the client:

```text
cpu user nice system idle iowait irq softirq steal guest guest_nice
```

The kernel reports guest inside user and guest_nice inside nice. The compatible
formula sums all fields, counting those two twice; the candidate total sums only
through steal.

- [ ] **Step 2: Quantify affected hosts**

On representative Core devices and at least one approved host running guests,
run:

```bash
awk '/^cpu / {
  legacy=0; corrected=0
  for (i=2; i<=NF; i++) legacy += $i
  for (i=2; i<=9 && i<=NF; i++) corrected += $i
  guest=(NF>=10 ? $10 : 0) + (NF>=11 ? $11 : 0)
  printf "legacy_total=%d corrected_total=%d guest=%d denominator_delta_pct=%.4f\n",
    legacy, corrected, guest, legacy == 0 ? 0 : 100*(legacy-corrected)/legacy
}' /proc/stat
```

Use two snapshots from the existing monitoring/benchmark tooling to compare
utilization deltas under an actual guest workload. Do not infer utilization from
a single cumulative snapshot.

- [ ] **Step 3: Trace server consumers and apply the gate**

Identify CPU alerts and historical comparisons. `CHANGE` requires evidence that
the affected population and value shift are understood, plus server-owner
approval and rollback criteria. Systems with zero guest time should be explicitly
shown unchanged. Otherwise choose `KEEP` or `DEFER`.

## Task 5: Decide process CPU semantics

**Files inspected:**
- `internal/monitor/activeprocessinfo.go`
- `internal/monitor/sysinfo_test.go`
- Landscape server process-monitoring consumers outside this repository

- [ ] **Step 1: Record the current formula and wire behavior**

The current plugin reports process CPU as cumulative process CPU time divided by
process lifetime. Confirm the exact fields and rounding:

```bash
rg -n 'percentCPU|startTime|uptime|clkTck|processChanged' internal/monitor/activeprocessinfo.go internal/monitor/sysinfo_test.go
```

Record that Phase 3 deliberately excluded `percentCPU` from process-change
detection to avoid resending most of the table every 30 seconds. A change to
interval utilization therefore needs a separate decision about when CPU-only
changes should be emitted.

- [ ] **Step 2: Compare lifetime and interval values on representative devices**

Use approved process-monitoring tooling on at least one idle Core device and one
device with a bursty workload. For each sampled PID, capture two `/proc/<pid>/stat`
snapshots and system uptime 30 seconds apart. Compute:

$$
CPU_{lifetime} = round_{0.1}\left(\min\left(99.0,
100 \times \frac{utime + stime}{USER\_HZ \times process\_age}\right)\right)
$$

and:

$$
CPU_{interval} = 100 \times
\frac{\Delta(utime + stime)}{USER\_HZ \times \Delta time}
$$

The lifetime expression reproduces the current wire scale, one-decimal rounding,
and 99.0 cap. For the interval candidate, calculate both single-core percentage
and host-normalized percentage; record values above 100 before applying any cap.
The server owner must select scale, multicore normalization, rounding and cap
rather than inheriting the current 99.0 clamp accidentally. Aggregate median,
p95 and maximum absolute delta by workload class. Do not commit process names,
command lines, UIDs or raw `/proc` captures.

- [ ] **Step 3: Trace server interpretation and apply the gate**

The server owner must state whether the field is displayed as lifetime average,
current utilization, or an opaque compatibility value; whether CPU-only updates
are expected; and whether alerting consumes it. `CHANGE` requires a sampling/state
design, message-frequency budget, mixed-client behavior and rollback criteria.
Otherwise choose `KEEP` or `DEFER`.

## Task 6: Finalize and review the decision record

- [ ] **Step 1: Write the record using the required structure**

Create `docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md`. For
each item include raw aggregate evidence, interpretation, compatibility analysis,
owner/date, decision, and follow-up.

- [ ] **Step 2: Validate completeness**

Run:

```bash
rg -n '^## (Ping|Memory|Disk|CPU|Process CPU|Compatibility|Follow-up)' docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
git add -N docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
awk -F'|' '
  /^\| (Ping transport|Free memory|Free disk|CPU total|Process CPU) / {
  item=$2
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", item)
    decision=$5
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", decision)
    if (decision !~ /^(KEEP|CHANGE|DEFER)$/) exit 1
  seen[item]++
  }
  END {
    split("Ping transport,Free memory,Free disk,CPU total,Process CPU", required, ",")
    for (i in required) if (seen[required[i]] != 1) exit 1
  }
' docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
git diff --check -- docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
```

Expected: every required section exists, all five summary rows have one decision
and named approval or a documented reason for deferral, and no whitespace errors.

- [ ] **Step 3: Review with client and server owners**

Do not treat silence as approval. Incorporate factual corrections, rerun the
completeness checks, and record final owner/date values.

- [ ] **Step 4: Create conditional follow-up plans**

For every `CHANGE` row, create a separate implementation plan containing:

- exact client and server protocol changes;
- fail-first compatibility tests;
- mixed-version rollout order;
- observability during rollout;
- rollback trigger and mechanism;
- whether historical data/thresholds require migration.

If all rows are `KEEP` or `DEFER`, Phase 6 is complete with no production code
commit. Do not manufacture implementation work to make the phase appear active.

Name each local client plan
`docs/superpowers/plans/2026-08-28-change-<item>.md`, link it from `Follow-up
Work`, and create linked server work in the server repository.

- [ ] **Step 5: Validate and commit every deliverable**

```bash
git add -N docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
for plan in docs/superpowers/plans/2026-08-28-change-*.md; do
  test -e "$plan" || continue
  git add -N "$plan"
done
git diff --check -- docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md docs/superpowers/plans
git add docs/superpowers/specs/2026-08-28-protocol-semantics-decision.md
for plan in docs/superpowers/plans/2026-08-28-change-*.md; do
  test -e "$plan" || continue
  git add "$plan"
done
git commit -m "docs: decide wire-visible reporting semantics"
```

Expected: the decision record and every required local follow-up plan are tracked
in the same commit; external server issue links are present in the record.
