# Code Review Follow-up - Design Spec

**Source:** [docs/CODE_REVIEW_2026-08.md](../../CODE_REVIEW_2026-08.md)

**Date:** 2026-08-28
**Baseline commit:** `b3be298` (`fix/04-p2-hygiene`)

## Goal

Address the material additions in the August review without reopening findings
already resolved by Phases 0-4 or changing Python-compatible wire semantics
without server-side agreement.

## Review Delta

The August file still says it reviews commit `c8c76ce` and repeats Sections 1-8
from the July review. Most defect findings in those sections are stale against
`b3be298`; Phases 0-4 resolved them and verified the resulting branch with
`go test -race -count=3 ./...`, `go vet ./...`, the compatibility tests, and
golangci-lint. The exceptions are listed explicitly below rather than hidden by
a blanket completion claim.

The material addition is Section 9, the architecture review. It adds these
follow-up items:

| Review item | Classification at `b3be298` | Disposition |
|---|---|---|
| 17b: unbounded exchange dispatch fan-out | Verified, implementation-ready | Phase 5 |
| 17c: snapd change polling every 500 ms | Verified, but proposed `?timeout=` API does not exist | Reject long-poll recommendation; Phase 5 fixes polling correctness/timer allocation |
| 29b: `time.After` inside loops | Verified in ping and snapd | Phase 5 |
| 30: memory, disk and guest-CPU semantics | Faithful to Python; wire-visible | Phase 6 decision |
| 31: cleartext default ping URL | Faithful to Python; security/product decision | Phase 6 decision |
| 32: do not collapse monitor goroutines | Sound architectural constraint | Preserve; no phase |
| 33: serial-assertion-based onboarding | Feasible direction, protocol dependencies unknown | Phase 7 research/design |

Still-open items carried from Sections 7.1 and 7.3 are:

| Earlier item | Status | Disposition |
|---|---|---|
| Lifetime-average process CPU | Faithful to Python and wire-visible | Add to Phase 6 decision evidence |
| Misleading `clkTck` comment | Comment still incorrectly calls `USER_HZ` the kernel timer frequency | Correct in Phase 5 without changing 100 |
| Typed `processorinfo`/`mountinfo` internals | Explicitly deferred from Phase 4; no observed defect | Separate maintainability work only when justified |
| Shared `/proc` scanner helper | Explicitly deferred from Phase 4; no observed defect | Separate maintainability work only when justified |

Section 9 also observes that converged plugin ticks amplify full state-file
rewrites. Phase 3 already staggered plugin launch, removing the stated trigger.
Further write coalescing is excluded until target-device measurement shows that
state persistence remains material; weakening immediate durability based only on
a read-only observation would be speculative.

## Technical Decisions

### Bounded dispatch

A second queue or semaphore in `Exchange` would duplicate ownership and introduce
an acknowledgment boundary the protocol does not expose. Queued callbacks could
be persisted as delivered and then lost at shutdown; a blocking semaphore could
produce the same ambiguity.

The actual subscribers make a smaller solution possible. Manager subscriptions
are already bounded dispatchers: they acquire the manager semaphore, register
their lifecycle in a wait group, spawn the operation, and return. The only direct
subscriber is the short `set-intervals` callback. Phase 5 therefore invokes
subscriber callbacks synchronously, one at a time, with panic containment. This
removes the unbounded exchange goroutines while preserving manager operation
concurrency and ensuring callback handoff occurs before inbound sequence state is
persisted. The `Subscribe` contract will require callbacks to dispatch bounded
work or return promptly.

### snapd polling

The August review's proposed `/v2/changes/<id>?timeout=` long poll is not a snapd
API. The official endpoint has no query parameters; long polling belongs to
`/v2/notices`. Migrating to notices would add version and event-correlation
complexity for infrequent management operations, so it is not justified here.

Phase 5 retains 500 ms change polling, reuses one timer, and fixes the current
response model to match snapd: `ready` determines completion and `err` is a
string. A ready non-`Done` status becomes an error rather than polling forever.

### Ping timer

The ping loop will reuse one `time.Timer`, using the same stop/drain/reset pattern
as the exchange loop. `SetInterval` keeps its current contract: a new interval
takes effect after the current wait, not by interrupting an already armed timer.

### Wire-visible telemetry and ping

No production behavior changes in Phase 6. It first records current payloads,
measures value deltas on representative Ubuntu Core systems, checks server alert
and ingestion assumptions, and verifies HTTPS `/ping` support and proxy routing.
Each item ends in an explicit keep/change decision owned jointly with the
Landscape server side.

A chosen change requires a follow-on implementation plan with compatibility tests
and rollout/rollback criteria. Adding fields speculatively to existing tuples is
not considered backward-compatible unless server decoding proves it.

### Asymmetric onboarding

Phase 7 is a feasibility and protocol-design phase, not client implementation.
The client can preserve and forward complete serial/model assertions, but that
only proves that an authorized assertion authority issued the serial assertion.
The brand-authorized model may delegate serial issuance through `serial-authority`;
verification must follow that authorization and the account/account-key chain.
Proof of possession requires a fresh server challenge and a device-key signing
operation that a strictly
confined client can invoke through a supported snapd API.

The server, not this client, should verify the snap assertion trust chain. Snap
assertions use snapd's assertion format; the plan will not introduce a generic
OpenPGP dependency based on an unverified format assumption.

No registration field, fallback path, or credential-storage behavior changes
until the Landscape server and snapd/security owners agree on:

- assertion and account-key trust-chain verification;
- nonce construction, expiry and replay protection;
- a supported signing operation and confinement policy;
- mixed-fleet negotiation and downgrade behavior;
- key rotation and revocation;
- whether `secure-id` remains a bearer credential after onboarding.

## Phase Structure

| Phase | Branch | Outcome |
|---|---|---|
| 5 - Runtime boundaries | `fix/05-runtime-boundaries` | Synchronous bounded dispatch, correct snapd change polling, reusable timers, `clkTck` comment |
| 6 - Protocol semantics | `research/06-protocol-semantics` | Measurements and approved decisions; no wire change |
| 7 - Device identity | `research/07-device-identity` | Feasibility report and approved onboarding protocol or a documented blocker |

Phase 5 is cut from `fix/04-p2-hygiene`. Phase 6 is independent research and may
run after Phase 5; it must not block merging Phase 5. Phase 7 is independent of
Phase 6 and may run in parallel once ownership contacts and an Ubuntu Core test
device are available.

## Verification Strategy

Phase 5 uses fail-first Go tests and ends with formatting, vet, lint, compatibility,
and repeated race-enabled tests. Its tests assert sequential callback handoff,
panic containment, snapd completion/error decoding, cancellation, and ping timer
semantics.

Phase 6 records commands, fixtures, dates, device/kernel versions, server endpoint
behavior, and named decision owners. Results from a general Linux workstation may
exercise the tools but do not substitute for representative Ubuntu Core data.

Phase 7 uses synthetic assertions in committed tests only after a protocol is
approved. Real device assertions, registration keys, secure IDs, exchange tokens,
private keys, and server nonces must never be committed. Exploratory captures are
redacted and kept outside the repository.

## Out of Scope

- Reimplementing any Phase 0-4 item solely because the August document repeats it.
- Collapsing the 15 monitor goroutines into one scheduler.
- State-write batching without target-device evidence.
- A unilateral switch from HTTP to HTTPS ping.
- A unilateral change to memory, disk, or CPU values.
- Client-side assertion trust decisions before the server protocol is defined.
- A snapd signing API implementation in this repository.
