# Phase 3 — P1 Efficiency Implementation Plan

> **For agentic workers:** REQUIRED: Use the `subagent-driven-development` agent (recommended) or `executing-plans` agent to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the client honour its configured exchange interval, survive server outages and restarts without losing messages, and stop burning CPU and bandwidth on per-sample messages and per-tick allocation.

**Architecture:** Five commits rewrite `internal/exchange`'s send and queue semantics as one coherent sequence: message classification (urgent vs scheduled), then backoff, then a per-exchange cap, then durability, then the O(n) prepend removal. Three commits fix monitor scheduling behind one extracted `runTicker` helper. Two commits cut allocation and fix the §7.2 ordering and error-discard regressions.

**Tech Stack:** Go 1.25, `context`, `encoding/json`, `math/rand`, `slices`, stdlib `testing`

**Spec:** [docs/superpowers/specs/2026-08-17-code-review-remediation-design.md](../specs/2026-08-17-code-review-remediation-design.md)

**Branch:** `fix/03-p1-efficiency`, cut from `fix/02-p1-reliability`

---

## Wire-compatibility note

Read this before writing any code in this phase.

All four batchable plugins already send a **list** of tuples containing exactly
one point:

| Plugin | Field | Current value |
|---|---|---|
| `cpuusage.go` | `cpu-usages` | `[]any{bpickle.Tuple{t.Unix(), usage}}` |
| `memoryinfo.go` | `memory-info` | `[]any{bpickle.Tuple{t.Unix(), freeMemMB, freeSwapMB}}` |
| `loadaverage.go` | `load-averages` | `[]any{bpickle.Tuple{t.Unix(), load}}` |
| `temperature.go` | `temperatures` | `[]any{bpickle.Tuple{t.Unix(), temp}}` |

So batching is **purely additive**: the same field carries N tuples instead of 1,
and no server-side change is implied. Do not rename fields, do not change tuple
shape, and do not change reported *values* — free memory, free disk and guest CPU
are a separate decision request in spec §7.

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `internal/exchange/exchange.go` | Modify | Urgent classification, backoff, cap, durability, prepend removal |
| `internal/exchange/exchange_test.go` | Modify | Interval, backoff and cap tests |
| `internal/exchange/queue.go` | Create | Durable pending-message spool |
| `internal/exchange/queue_test.go` | Create | Spool round-trip, cap and drop-policy tests |
| `internal/monitor/monitor.go` | Modify | `runTicker` helper |
| `internal/monitor/ticker_test.go` | Create | Initial-tick, stagger and cancellation tests |
| `internal/monitor/*.go` (15 plugins) | Modify | Migrate to `runTicker` |
| `internal/monitor/accumulator.go` | Create | Buffer data points between sends |
| `internal/monitor/accumulator_test.go` | Create | Batching tests |
| `internal/monitor/activeprocessinfo.go` | Modify | Allocation reuse; `diffProcesses` fix |
| `internal/monitor/temperature.go` | Modify | Deterministic zone ordering |
| `internal/monitor/processorinfo.go` | Modify | Stable sort, real identifiers |
| `internal/monitor/mountinfo.go` | Modify | Free-space change detection |
| `internal/monitor/computerinfo.go` | Modify | Stop discarding `os.Hostname` error |
| `cmd/landscape-client-core/main.go` | Modify | Pass the spool path; wire urgent sends |

---

## Task 0: Create the branch

- [ ] **Step 1: Cut the branch**

```bash
git checkout fix/02-p1-reliability
git checkout -b fix/03-p1-efficiency
```

- [ ] **Step 2: Confirm a clean starting point**

Run: `go build ./... && go test -race ./...`
Expected: all packages `ok` or `no test files`.

---

## Task 1: Distinguish urgent from scheduled sends

`Exchange.Send` unconditionally calls `TriggerExchange()`, so every plugin message
forces an immediate HTTP exchange. Measured with a 1-hour interval configured and
a 50ms send cadence:

```
plugin sends=24, HTTP exchanges=24 (exchange-interval was 1h)
```

1:1 — the interval has no effect. With `memory-info` at 15s that is roughly 60×
the configured rate, which on a metered or cellular Core fleet is a 60× bandwidth
and wakeup regression.

**There is a second half that is easy to miss.** `Run`'s interval selection reads:

```go
		if hasPending || state.SecureID == "" || justRegistered {
			interval = e.cfg.UrgentExchangeInterval
		}
```

So removing the trigger from `Send` is not enough — queued telemetry would still
poll at `UrgentExchangeInterval` (default 1 minute). The condition must become
"has **urgent** pending", which means messages carry a classification.

Python's semantics: `send_message(..., urgent=False)` is the default
(`landscape/client/broker/server.py:200-210`); only genuinely urgent events opt in.

**Files:**
- Modify: `internal/exchange/exchange.go`
- Modify: `internal/exchange/exchange_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/exchange/exchange_test.go` (package `exchange`):

```go
// TestSend_DoesNotForceAnExchange asserts plugin telemetry queues for the next
// scheduled exchange instead of triggering one per message. Measured before this
// fix: 24 sends produced 24 exchanges with a 1-hour interval configured.
func TestSend_DoesNotForceAnExchange(t *testing.T) {
	srv := &fakeServer{}
	for i := 0; i < 10; i++ {
		srv.push(map[string]any{
			"next-expected-sequence": int64(0),
			"next-exchange-token":    "tok",
			"messages":               []any{},
		})
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	st, _ := store.Load()
	st.SecureID = "already-registered"
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := &config.Config{
		URL:                    ts.URL,
		AccountName:            "acc",
		ExchangeInterval:       time.Hour,
		UrgentExchangeInterval: time.Hour,
	}
	exc := New(cfg, store, tc)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- exc.Run(ctx) }()

	// Let the initial exchange happen.
	time.Sleep(200 * time.Millisecond)
	baseline := srv.count()

	for i := 0; i < 20; i++ {
		if err := exc.Send(ctx, Message{"type": "cpu-usage"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	extra := srv.count() - baseline
	cancel()
	<-runDone

	if extra > 0 {
		t.Errorf("20 non-urgent sends triggered %d extra exchanges; exchange-interval was 1h", extra)
	}
}

// TestSendUrgent_TriggersAnExchange asserts operation results are not delayed
// until the next scheduled exchange — the server is waiting on those.
func TestSendUrgent_TriggersAnExchange(t *testing.T) {
	srv := &fakeServer{}
	for i := 0; i < 5; i++ {
		srv.push(map[string]any{
			"next-expected-sequence": int64(0),
			"next-exchange-token":    "tok",
			"messages":               []any{},
		})
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	st, _ := store.Load()
	st.SecureID = "already-registered"
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := &config.Config{
		URL:                    ts.URL,
		AccountName:            "acc",
		ExchangeInterval:       time.Hour,
		UrgentExchangeInterval: time.Hour,
	}
	exc := New(cfg, store, tc)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- exc.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	baseline := srv.count()

	if err := exc.SendResult(ctx, 42, StatusSucceeded, "done"); err != nil {
		t.Fatalf("SendResult: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	extra := srv.count() - baseline
	cancel()
	<-runDone

	if extra == 0 {
		t.Error("an operation-result did not trigger an exchange")
	}
}
```

`fakeServer` with `push()` and `count()` already exists in
`internal/exchange/exchange_test.go`. Reuse it; do not add a second one.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/exchange/ -run TestSend_DoesNotForceAnExchange -v`
Expected: FAIL with `20 non-urgent sends triggered 20 extra exchanges; exchange-interval was 1h`.

- [ ] **Step 3: Add the classification**

Urgency is derived from the message type rather than tracked as a separate flag: a
flag would have to be maintained through every re-queue path — transport failure,
partial ACK, `resynchronize` — and can drift out of sync with the queue. The queue
is capped at 100 by Task 3, so scanning it is trivial.

Add to `internal/exchange/exchange.go`:

```go
// isUrgentType reports whether a message must not wait for the next scheduled
// exchange. The server blocks on operation results.
func isUrgentType(msgType string) bool {
	return msgType == "operation-result"
}

// hasUrgentPending reports whether the queue holds a message that should shorten
// the next exchange interval. Caller must hold e.mu.
func (e *Exchange) hasUrgentPendingLocked() bool {
	for _, m := range e.pending {
		t, _ := m["type"].(string)
		if isUrgentType(t) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Split `Send` and `SendUrgent`**

```go
// Send enqueues a message for the next scheduled exchange. It does NOT wake the
// exchange loop: plugin telemetry that triggered an exchange per message made
// exchange-interval meaningless — measured 60x the configured rate on a device
// with memory-info at 15s. Matches Python's send_message(urgent=False) default.
func (e *Exchange) Send(_ context.Context, msg Message) error {
	e.mu.Lock()
	e.pending = append(e.pending, msg)
	e.mu.Unlock()
	return nil
}

// SendUrgent enqueues a message and wakes the exchange loop immediately. Use it
// only for messages the server is waiting on, such as operation results.
func (e *Exchange) SendUrgent(_ context.Context, msg Message) error {
	e.mu.Lock()
	e.pending = append(e.pending, msg)
	e.mu.Unlock()
	e.TriggerExchange()
	return nil
}
```

- [ ] **Step 5: Route operation results through the urgent path**

In `sendOperationResult`, change the final line from `return e.Send(ctx, msg)` to
`return e.SendUrgent(ctx, msg)`. Both `SendResult` and `SendResultCode` route
through it, so this is the only edit needed.

- [ ] **Step 6: Fix the interval selection**

In `Run`, replace:

```go
		e.mu.Lock()
		hasPending := len(e.pending) > 0
		e.mu.Unlock()
```

with:

```go
		e.mu.Lock()
		hasUrgent := e.hasUrgentPendingLocked()
		e.mu.Unlock()
```

and the condition:

```go
		if hasUrgent || state.SecureID == "" || justRegistered {
			interval = e.cfg.UrgentExchangeInterval
		}
```

No flag needs clearing anywhere: draining the queue in `performExchange` removes
the urgent messages, and a re-queue restores them, so the derived answer is always
correct.

- [ ] **Step 7: Run to verify both tests pass**

Run: `go test -race ./internal/exchange/ -v`
Expected: PASS, including the existing exchange tests. Any existing test that
relied on `Send` triggering an exchange must be updated to call `SendUrgent` or to
call `TriggerExchange()` explicitly — check each failure rather than reverting.

- [ ] **Step 8: Confirm plugins use the non-urgent path**

Run: `grep -rn 'SendUrgent' internal/monitor/ || echo "monitor uses scheduled sends only"`
Expected: `monitor uses scheduled sends only`.

`MessageSink` is the interface plugins hold; leaving `SendUrgent` off that
interface makes the correct path the only reachable one for telemetry. Check
`internal/exchange/exchange.go`'s `MessageSink` definition and keep it that way.

- [ ] **Step 9: Commit**

```bash
git add internal/exchange/exchange.go internal/exchange/exchange_test.go
git commit -m "feat(exchange): distinguish urgent from scheduled sends

Send unconditionally called TriggerExchange, so every plugin message forced
an immediate HTTP exchange: measured 1:1, 24 sends producing 24 exchanges
with a 1-hour interval configured. With memory-info at 15s that is ~60x the
configured rate, and on a metered Core fleet a 60x bandwidth regression.

The interval selection also treated any pending message as urgent, so simply
dropping the trigger would still have polled at UrgentExchangeInterval.
Urgency is now derived from the message type; operation results, which the
server blocks on, remain immediate.

MessageSink deliberately exposes only the scheduled path, so plugin telemetry
cannot opt into urgency."
```

---

## Task 2: Exponential backoff with jitter on 429 and 5xx

The Go client retries at a fixed interval and treats all failures identically —
`performExchange`'s error is logged and the loop continues. Python backs off
300→7200s with jitter on HTTP 429 and 5xx
(`landscape/client/broker/exchange.py:418,629-639,700-705`), explicitly to shed
load from an overloaded server.

A fleet of Core devices hitting a struggling server will hammer it — and before
Task 1, each device retried on every plugin tick.

The 404 server-API-downgrade path (`exchange.py:617-625`) is also absent entirely.

`internal/transport/transport.go` already returns a typed `*HTTPError` carrying
`StatusCode`, so the classification is `errors.As`, not string matching.

**Files:**
- Modify: `internal/exchange/exchange.go`
- Modify: `internal/exchange/exchange_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestBackoff_EscalatesOn5xx asserts repeated server errors increase the delay
// rather than retrying at a fixed interval, which lets a fleet hammer an
// already-struggling server.
func TestBackoff_EscalatesOn5xx(t *testing.T) {
	b := newBackoff()

	if d := b.current(); d != 0 {
		t.Errorf("initial backoff should be zero, got %v", d)
	}

	b.failure(500)
	first := b.current()
	if first < 300*time.Second || first > 360*time.Second {
		t.Errorf("first backoff should be ~300s with jitter, got %v", first)
	}

	b.failure(500)
	second := b.current()
	if second <= first {
		t.Errorf("backoff should escalate: first=%v second=%v", first, second)
	}

	for i := 0; i < 20; i++ {
		b.failure(503)
	}
	if capped := b.current(); capped > 7200*time.Second {
		t.Errorf("backoff should cap at 7200s, got %v", capped)
	}
}

func TestBackoff_DecaysOnSuccess(t *testing.T) {
	b := newBackoff()
	b.failure(500)
	b.failure(500)
	if b.current() == 0 {
		t.Fatal("expected a non-zero backoff after failures")
	}

	b.success()
	if d := b.current(); d != 0 {
		t.Errorf("backoff should reset on success, got %v", d)
	}
}

func TestBackoff_IgnoresClientErrors(t *testing.T) {
	b := newBackoff()

	// A 400 is our bug, not server overload; backing off does not help.
	b.failure(400)
	if d := b.current(); d != 0 {
		t.Errorf("4xx other than 429 should not back off, got %v", d)
	}

	b.failure(429)
	if d := b.current(); d == 0 {
		t.Error("429 should back off")
	}
}

func TestBackoff_JitterVaries(t *testing.T) {
	seen := make(map[time.Duration]bool)
	for i := 0; i < 20; i++ {
		b := newBackoff()
		b.failure(500)
		seen[b.current()] = true
	}
	if len(seen) < 2 {
		t.Error("backoff has no jitter; a fleet would retry in lockstep")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/exchange/ -run TestBackoff -v`
Expected: FAIL — compile error, `undefined: newBackoff`.

- [ ] **Step 3: Implement the backoff**

Create the type in `internal/exchange/exchange.go` (or a small `backoff.go` in the
same package):

```go
const (
	backoffMin = 300 * time.Second
	backoffMax = 7200 * time.Second
)

// backoff sheds load from an overloaded server. Without it a fleet of devices
// retries at a fixed interval against a server that is already struggling.
// Ported from Python's ExponentialBackoff.
type backoff struct {
	delay time.Duration
	rand  *rand.Rand
}

func newBackoff() *backoff {
	return &backoff{rand: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// failure records an HTTP status. Only 429 and 5xx back off: a 4xx other than
// 429 is our own bad request, and waiting does not help.
func (b *backoff) failure(status int) {
	if status != 429 && (status < 500 || status > 599) {
		return
	}
	if b.delay == 0 {
		b.delay = backoffMin
	} else {
		b.delay *= 2
	}
	if b.delay > backoffMax {
		b.delay = backoffMax
	}
}

func (b *backoff) success() {
	b.delay = 0
}

// current returns the delay with up to 20% jitter, so a fleet does not retry in
// lockstep.
func (b *backoff) current() time.Duration {
	if b.delay == 0 {
		return 0
	}
	jitter := time.Duration(b.rand.Int63n(int64(b.delay) / 5))
	return b.delay + jitter
}
```

Add `"math/rand"` to the imports.

- [ ] **Step 4: Apply it in the exchange loop**

In `Run`, after `performExchange` returns:

```go
		prevSecureID := state.SecureID
		err := e.performExchange(ctx, state)
		if err != nil {
			log.Printf("exchange: exchange failed: %v", err)
			var httpErr *transport.HTTPError
			if errors.As(err, &httpErr) {
				bo.failure(httpErr.StatusCode)
			}
		} else {
			bo.success()
		}
```

with `bo := newBackoff()` created once before the loop. Then in the interval
selection, take the larger of the scheduled interval and the backoff:

```go
		if d := bo.current(); d > interval {
			log.Printf("exchange: backing off for %v after server errors", d)
			interval = d
		}
```

Placing the backoff *after* the urgent-interval selection is deliberate: a server
that is returning 503 should not be polled at the urgent interval just because an
operation result is queued.

- [ ] **Step 5: Handle the 404 API downgrade**

Where the HTTP error is classified, add:

```go
			if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
				// An older server does not know this API version. Python drops to
				// the previous version rather than failing permanently.
				if downgraded := previousAPIVersion(state.ServerAPI); downgraded != "" {
					log.Printf("exchange: server returned 404; downgrading API %s -> %s", state.ServerAPI, downgraded)
					state.ServerAPI = downgraded
				}
			}
```

This needs a `ServerAPI` field on `persist.State` (defaulting to `apiVersion`) and
a `previousAPIVersion` function. Read `internal/exchange/exchange.go`'s
`apiVersion` constant and the payload assembly first — the payload currently
hard-codes `"server-api": apiVersion`, which must become `state.ServerAPI`.

If the codebase supports only one API version, implement `previousAPIVersion` to
return `""` and log the 404 explicitly rather than inventing a version ladder;
state that choice in the commit message.

- [ ] **Step 6: Run to verify it passes**

Run: `go test -race ./internal/exchange/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/exchange/
git commit -m "feat(exchange): exponential backoff with jitter on 429 and 5xx

All failures were treated identically and retried at a fixed interval, so a
fleet of Core devices would hammer an already-struggling server. Ports
Python's 300->7200s randomised backoff, escalating on 429/5xx and resetting
on success. 4xx other than 429 does not back off: that is our bad request,
and waiting does not help.

Backoff is applied after the urgent-interval selection, so a server returning
503 is not polled urgently just because a result is queued.

Also adds the 404 server-API-downgrade path, which was absent entirely."
```

---

## Task 3: Cap messages per exchange at 100

Python caps at `max_messages=100` per exchange
(`landscape/client/broker/exchange.py:386,752`). Go sends the entire queue in one
request. This must land before durability, so a spool restored after a long outage
cannot produce one enormous first request.

**Files:**
- Modify: `internal/exchange/exchange.go`
- Modify: `internal/exchange/exchange_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestPerformExchange_CapsMessagesPerRequest asserts a large backlog is drained
// across several exchanges rather than sent as one enormous request.
func TestPerformExchange_CapsMessagesPerRequest(t *testing.T) {
	srv := &fakeServer{}
	for i := 0; i < 10; i++ {
		srv.push(map[string]any{
			"next-expected-sequence": int64(0),
			"next-exchange-token":    "tok",
			"messages":               []any{},
		})
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	st, _ := store.Load()
	st.SecureID = "already-registered"
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := &config.Config{URL: ts.URL, AccountName: "acc"}
	exc := New(cfg, store, tc)

	for i := 0; i < 250; i++ {
		if err := exc.Send(context.Background(), Message{"type": "cpu-usage"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	state, _ := store.Load()
	if err := exc.performExchange(context.Background(), state); err != nil {
		t.Fatalf("performExchange: %v", err)
	}

	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	msgs, ok := reqs[0].payload["messages"].([]any)
	if !ok {
		t.Fatalf("messages: unexpected type %T", reqs[0].payload["messages"])
	}
	if len(msgs) > 100 {
		t.Errorf("sent %d messages in one exchange; the cap is 100", len(msgs))
	}

	// The remainder must still be queued, not dropped.
	exc.mu.Lock()
	remaining := len(exc.pending)
	exc.mu.Unlock()
	if remaining != 150 {
		t.Errorf("want 150 messages still queued, got %d", remaining)
	}
}
```

`fakeServer` records `received []receivedRequest`; check the accessor name — it
may be a method or a mutex-guarded field. Use what exists.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/exchange/ -run TestPerformExchange_CapsMessagesPerRequest -v`
Expected: FAIL with `sent 250 messages in one exchange; the cap is 100`.

- [ ] **Step 3: Apply the cap when snapshotting**

Replace the snapshot block in `performExchange`:

```go
	e.mu.Lock()
	snapshot := make([]Message, len(e.pending))
	copy(snapshot, e.pending)
	e.pending = nil
	e.mu.Unlock()
```

with:

```go
	// Drain at most maxMessagesPerExchange, matching Python's max_messages. A
	// restored spool after a long outage would otherwise produce one enormous
	// request.
	e.mu.Lock()
	n := len(e.pending)
	if n > maxMessagesPerExchange {
		n = maxMessagesPerExchange
	}
	snapshot := make([]Message, n)
	copy(snapshot, e.pending[:n])
	e.pending = e.pending[n:]
	e.mu.Unlock()
```

and add the constant:

```go
// maxMessagesPerExchange matches the Python client's max_messages.
const maxMessagesPerExchange = 100
```

Note `e.pending = e.pending[n:]` retains the backing array. Task 5 replaces the
queue representation; until then this is acceptable because the queue is bounded
by Task 4.

- [ ] **Step 4: Trigger a follow-up exchange when a backlog remains**

At the end of a successful `performExchange`, if messages are still queued, wake
the loop so the backlog drains promptly rather than one batch per interval:

```go
	e.mu.Lock()
	backlog := len(e.pending) > 0
	e.mu.Unlock()
	if backlog {
		e.TriggerExchange()
	}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test -race ./internal/exchange/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/exchange/
git commit -m "feat(exchange): cap messages per exchange at 100

Matches Python's max_messages. Go sent the entire queue in one request, which
becomes a problem as soon as the queue is durable: a spool restored after a
long outage would produce one enormous first request.

A remaining backlog wakes the loop so it drains promptly rather than one
batch per interval."
```

---

## Task 4: Persist the pending queue and bound its size

`Exchange.pending` is an in-memory slice, and `persist.State` has no queue field.
Two consequences:

- **Data loss.** Every daemon restart — including every `snap refresh` — silently drops all unsent messages, including `operation-result`s the server is waiting on. Python persists to a spool directory (`landscape/client/broker/store.py`).
- **Unbounded growth.** With no cap and no durability, a long server outage grows `pending` without limit. On a 512 MB Core device that is an OOM path.

**Design decisions (from spec §3):** a single JSON file in `$SNAP_COMMON`, written
with the temp-file + `fsync` + rename pattern `persist.Store.saveLocked` already
uses. It must be a **separate file from `state.json`** so a queue write can never
touch `SecureID` or the sequence number — that is the same failure mode Phase 2
Task 7 removed. Drop policy: never drop `operation-result`, drop oldest telemetry
first, one warning per drop episode rather than per message.

**Files:**
- Create: `internal/exchange/queue.go`
- Create: `internal/exchange/queue_test.go`
- Modify: `internal/exchange/exchange.go`
- Modify: `cmd/landscape-client-core/main.go`

- [ ] **Step 1: Write the failing test**

Create `internal/exchange/queue_test.go`:

```go
package exchange

import (
	"path/filepath"
	"testing"
)

func TestSpool_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	s := newSpool(path)

	msgs := []Message{
		{"type": "cpu-usage"},
		{"type": "operation-result", "operation-id": int64(42)},
	}
	if err := s.save(msgs); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got))
	}
	if got[1]["type"] != "operation-result" {
		t.Errorf("message order or content lost: %v", got)
	}
}

func TestSpool_MissingFileIsEmptyNotAnError(t *testing.T) {
	s := newSpool(filepath.Join(t.TempDir(), "does-not-exist.json"))
	got, err := s.load()
	if err != nil {
		t.Fatalf("first run must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want an empty queue, got %d messages", len(got))
	}
}

func TestSpool_CorruptFileIsEmptyNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := newSpool(path).load()
	if err != nil {
		t.Fatalf("a corrupt spool must not block startup: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want an empty queue, got %d messages", len(got))
	}
}

func TestEnforceQueueBound_DropsOldestTelemetryFirst(t *testing.T) {
	msgs := make([]Message, 0, maxQueuedMessages+10)
	// Oldest telemetry first, then a result, then more telemetry.
	for i := 0; i < maxQueuedMessages+9; i++ {
		msgs = append(msgs, Message{"type": "cpu-usage", "n": int64(i)})
	}
	msgs = append(msgs, Message{"type": "operation-result", "operation-id": int64(7)})

	kept, dropped := enforceQueueBound(msgs)

	if len(kept) > maxQueuedMessages {
		t.Errorf("queue not bounded: %d messages kept", len(kept))
	}
	if dropped == 0 {
		t.Error("expected some messages to be dropped")
	}

	var foundResult bool
	for _, m := range kept {
		if m["type"] == "operation-result" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("operation-result was dropped; the server is blocking on it")
	}

	// The oldest telemetry must be the part that went.
	if n, ok := kept[0]["n"].(int64); ok && n == 0 {
		t.Error("dropped newest telemetry instead of oldest")
	}
}

func TestEnforceQueueBound_NeverDropsResultsEvenWhenFull(t *testing.T) {
	msgs := make([]Message, 0, maxQueuedMessages+5)
	for i := 0; i < maxQueuedMessages+5; i++ {
		msgs = append(msgs, Message{"type": "operation-result", "operation-id": int64(i)})
	}

	kept, _ := enforceQueueBound(msgs)
	for _, m := range kept {
		if m["type"] != "operation-result" {
			t.Fatal("unexpected message type in kept set")
		}
	}
	if len(kept) != len(msgs) {
		t.Errorf("results must never be dropped: kept %d of %d", len(kept), len(msgs))
	}
}
```

Add `"os"` to the imports.

The last test encodes a deliberate decision: results are never dropped **even if
that exceeds the bound**. An unbounded number of pending results implies the
server has been unreachable for a very long time, and losing them is worse than
the memory. If you disagree, raise it rather than silently changing the test.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/exchange/ -run 'TestSpool|TestEnforceQueueBound' -v`
Expected: FAIL — compile error, `undefined: newSpool`.

- [ ] **Step 3: Implement the spool**

Create `internal/exchange/queue.go`:

```go
package exchange

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// maxQueuedMessages bounds the in-memory and on-disk queue. With no cap, a long
// server outage grows the queue without limit, which on a 512 MB Core device is
// an OOM path.
const maxQueuedMessages = 1000

// spool persists unsent messages across restarts. Without it every restart —
// including every snap refresh — silently drops unsent messages, including
// operation results the server is waiting on.
//
// It is deliberately a separate file from state.json: a queue write must never
// be able to touch SecureID or the outbound sequence number.
type spool struct {
	path string
}

func newSpool(path string) *spool {
	return &spool{path: path}
}

// load returns the persisted queue. A missing or corrupt spool yields an empty
// queue rather than an error: losing queued telemetry is bad, but refusing to
// start is worse.
func (s *spool) load() ([]Message, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("exchange: cannot read message spool: %w", err)
	}

	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		log.Printf("exchange: message spool %s is corrupt (%v); starting with an empty queue", s.path, err)
		return nil, nil
	}
	return msgs, nil
}

// save writes the queue atomically, using the same temp-file + fsync + rename
// sequence as persist.Store.
func (s *spool) save(msgs []Message) error {
	data, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("exchange: cannot encode message spool: %w", err)
	}

	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".queue-*.tmp")
	if err != nil {
		return fmt.Errorf("exchange: cannot create spool temp file: %w", err)
	}
	tmpPath := f.Name()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot write spool temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot sync spool temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot close spool temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot rename spool temp file: %w", err)
	}
	return nil
}

// enforceQueueBound trims the queue to maxQueuedMessages, dropping the oldest
// non-urgent messages first. Operation results are never dropped: the server is
// blocking on them, and telemetry is resampled anyway.
//
// Returns the retained messages and the number dropped.
func enforceQueueBound(msgs []Message) ([]Message, int) {
	if len(msgs) <= maxQueuedMessages {
		return msgs, 0
	}

	excess := len(msgs) - maxQueuedMessages
	kept := make([]Message, 0, len(msgs))
	dropped := 0

	for _, m := range msgs {
		msgType, _ := m["type"].(string)
		if dropped < excess && !isUrgentType(msgType) {
			dropped++
			continue
		}
		kept = append(kept, m)
	}
	return kept, dropped
}
```

- [ ] **Step 4: Wire the spool into `Exchange`**

Add a `spool *spool` field. Load it in `Run` before the loop:

```go
	if e.spool != nil {
		restored, err := e.spool.load()
		if err != nil {
			log.Printf("exchange: %v", err)
		} else if len(restored) > 0 {
			e.mu.Lock()
			e.pending = append(restored, e.pending...)
			e.mu.Unlock()
			log.Printf("exchange: restored %d queued message(s) from the spool", len(restored))
		}
	}
```

Persist after every queue mutation that matters — the simplest correct points are
at the end of `performExchange` (after the snapshot is taken and after any
re-queue) and on the shutdown path before `Run` returns. Add a helper:

```go
// persistQueue bounds and writes the queue. Called after every mutation so an
// unexpected restart loses at most the messages queued since the last exchange.
func (e *Exchange) persistQueue() {
	if e.spool == nil {
		return
	}
	e.mu.Lock()
	kept, dropped := enforceQueueBound(e.pending)
	e.pending = kept
	snapshot := make([]Message, len(kept))
	copy(snapshot, kept)
	e.mu.Unlock()

	if dropped > 0 {
		log.Printf("exchange: queue full; dropped %d oldest telemetry message(s), operation results retained", dropped)
	}
	if err := e.spool.save(snapshot); err != nil {
		log.Printf("exchange: %v", err)
	}
}
```

One log line per drop episode, not per message — a full queue would otherwise
produce hundreds of lines.

- [ ] **Step 5: Construct the spool in `run()`**

In `cmd/landscape-client-core/main.go`:

```go
	exc := exchange.New(d.cfg, d.store, d.transport)
	exc.SetSpool(filepath.Join(d.snapCommon, "queue.json"))
```

Or add the path as a parameter to `exchange.New` — prefer whichever keeps
`New`'s signature honest. If `New` gains a parameter, update every test that calls
it; `t.TempDir()` is the right value there.

- [ ] **Step 6: Write the restart-durability test**

```go
// TestExchange_QueueSurvivesRestart asserts unsent messages are not lost when
// the daemon restarts — including on every snap refresh.
func TestExchange_QueueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "queue.json")

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(filepath.Join(dir, "state.json"))
	cfg := &config.Config{URL: "http://127.0.0.1:1", AccountName: "acc"}

	first := New(cfg, store, tc)
	first.SetSpool(spoolPath)
	if err := first.SendResult(context.Background(), 42, StatusSucceeded, "done"); err != nil {
		t.Fatalf("SendResult: %v", err)
	}
	first.persistQueue()

	// Simulate a restart.
	second := New(cfg, store, tc)
	second.SetSpool(spoolPath)
	restored, err := second.spool.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("want 1 restored message, got %d", len(restored))
	}
	if restored[0]["type"] != "operation-result" {
		t.Errorf("wrong message restored: %v", restored[0])
	}
}
```

Note `json.Unmarshal` decodes numbers into `float64`, so an `operation-id` written
as `int64` returns as `float64` after a round trip. Either assert only on `type`,
as above, or normalise numeric fields on load. **Decide explicitly and state the
choice in the commit message** — a silently changed type would break bpickle
encoding on the next exchange. Normalising on load is the safer option; if you
take it, add a test asserting `operation-id` is `int64` after a round trip.

- [ ] **Step 7: Run to verify it passes**

Run: `go test -race ./internal/exchange/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/exchange/ cmd/landscape-client-core/main.go
git commit -m "feat(exchange): persist the pending queue and bound its size

The queue was memory-only, so every restart — including every snap refresh —
silently dropped unsent messages, including operation results the server was
waiting on. With no cap, a long outage grew it without limit, which on a
512 MB Core device is an OOM path.

Single JSON spool in SNAP_COMMON using the same temp+fsync+rename sequence as
persist.Store, deliberately separate from state.json so a queue write can
never touch SecureID or the sequence number.

Drop policy: operation results are never dropped, oldest telemetry goes
first, one warning per drop episode. A missing or corrupt spool yields an
empty queue rather than blocking startup."
```

---

## Task 5: Remove O(n) queue prepends

Four sites do `append([]Message{x}, e.pending...)` — a full reallocation and copy
of the queue per re-queue, O(n) per operation:

| Site | Purpose |
|---|---|
| registration message injection | prepend the `register` message |
| transport failure re-queue | restore the unsent snapshot |
| partial-ACK re-queue | restore unacknowledged messages |
| `resynchronize` ack | prepend the ack |

**Files:**
- Modify: `internal/exchange/exchange.go`

- [ ] **Step 1: Confirm the sites**

Run: `grep -n 'append(\[\]Message{' internal/exchange/exchange.go`
Expected: four matches. If Tasks 1–4 changed the count, use what the grep reports.

- [ ] **Step 2: Replace with `slices.Insert`**

For a single-message prepend:

```go
		e.mu.Lock()
		e.pending = slices.Insert(e.pending, 0, regMsg)
		e.mu.Unlock()
```

For a slice prepend:

```go
		e.mu.Lock()
		e.pending = slices.Insert(e.pending, 0, snapshot...)
		e.mu.Unlock()
```

Add `"slices"` to the imports.

`slices.Insert` still shifts elements, but it grows the existing backing array
rather than allocating a new slice per call, and it makes the intent explicit. The
queue is bounded at 1000 by Task 4, so a ring buffer would be over-engineering
here — note that reasoning in the commit message so a future reader does not
"improve" it.

- [ ] **Step 3: Verify behaviour is unchanged**

Run: `go test -race ./internal/exchange/ -v`
Expected: PASS, including the partial-ACK re-queue ordering test — message order
matters for sequencing and must be identical.

- [ ] **Step 4: Commit**

```bash
git add internal/exchange/exchange.go
git commit -m "perf(exchange): remove O(n) queue prepends

Four sites did append([]Message{x}, e.pending...), reallocating and copying
the whole queue per re-queue. slices.Insert grows the existing backing array
instead.

Not a ring buffer: the queue is bounded at 1000, so the added complexity
would not pay for itself."
```

---

## Task 6: Extract a `runTicker` helper

All 15 plugins hand-roll the same loop. Credit where due: **every one has
`ctx.Done()` as the first select arm and every one defers the ticker stop** — the
shape is correct throughout. The problem is that every cross-cutting fix has 15
edit sites, which is how both the missing stagger and the missing per-tick
timeouts arose.

This commit changes no behaviour.

**Files:**
- Modify: `internal/monitor/monitor.go`
- Create: `internal/monitor/ticker_test.go`
- Modify: all 15 plugin files

- [ ] **Step 1: Write the failing test**

Create `internal/monitor/ticker_test.go` (package `monitor`):

```go
package monitor

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRunTicker_CallsOnEachTick(t *testing.T) {
	var mu sync.Mutex
	var calls int

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	runTicker(ctx, 20*time.Millisecond, false, 0, func(context.Context, time.Time) {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if calls < 3 {
		t.Errorf("want at least 3 ticks in 200ms at 20ms, got %d", calls)
	}
}

func TestRunTicker_RunImmediatelyFiresBeforeTheFirstInterval(t *testing.T) {
	fired := make(chan struct{}, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go runTicker(ctx, time.Hour, true, 0, func(context.Context, time.Time) {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runImmediately did not fire before the first interval")
	}
}

func TestRunTicker_WithoutRunImmediatelyWaits(t *testing.T) {
	fired := make(chan struct{}, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go runTicker(ctx, time.Hour, false, 0, func(context.Context, time.Time) {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	select {
	case <-fired:
		t.Fatal("fired before the first interval without runImmediately")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRunTicker_ReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		runTicker(ctx, time.Hour, false, 0, func(context.Context, time.Time) {})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTicker did not return on cancellation")
	}
}

func TestRunTicker_StaggerDelaysStartWithinBound(t *testing.T) {
	fired := make(chan time.Time, 1)
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go runTicker(ctx, 50*time.Millisecond, true, 200*time.Millisecond, func(_ context.Context, _ time.Time) {
		select {
		case fired <- time.Now():
		default:
		}
	})

	select {
	case at := <-fired:
		if d := at.Sub(start); d > 400*time.Millisecond {
			t.Errorf("stagger exceeded its bound: first tick after %v", d)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("never fired")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestRunTicker -v`
Expected: FAIL — compile error, `undefined: runTicker`.

- [ ] **Step 3: Implement the helper**

Add to `internal/monitor/monitor.go`:

```go
// runTicker drives a plugin's periodic work until ctx is cancelled.
//
// Every plugin previously hand-rolled this loop, so each cross-cutting change —
// an initial tick, a launch stagger, a per-tick timeout — meant 15 edits.
//
// stagger, when non-zero, delays the start by a random duration up to that
// bound. Without it all plugins start together and, because several share an
// interval, re-converge on the same tick forever: a periodic CPU spike and a
// burst of simultaneous sends. Mirrors Python's stagger_launch.
func runTicker(ctx context.Context, interval time.Duration, runImmediately bool, stagger time.Duration, fn func(context.Context, time.Time)) {
	if stagger > 0 {
		delay := time.Duration(rand.Int63n(int64(stagger)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}

	if runImmediately {
		fn(ctx, time.Now())
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			fn(ctx, t)
		}
	}
}
```

Add `"math/rand"` to the imports.

- [ ] **Step 4: Migrate all 15 plugins**

For each plugin, replace the hand-rolled loop body. Example for `cpuusage.go`:

```go
func (p *CPUUsage) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	// Prime the baseline before starting the ticker.
	if _, err := p.sample(); err != nil {
		log.Printf("cpu-usage: priming baseline: %v", err)
	}

	runTicker(ctx, p.interval, false, 0, func(ctx context.Context, t time.Time) {
		usage, err := p.sample()
		if err != nil {
			log.Printf("cpu-usage: %v", err)
			return
		}
		if usage < 0 {
			return
		}
		msg := exchange.Message{
			"type":       "cpu-usage",
			"cpu-usages": []any{bpickle.Tuple{t.Unix(), usage}},
		}
		if err := sink.Send(ctx, msg); err != nil {
			log.Printf("cpu-usage: send: %v", err)
		}
	})
	return nil
}
```

Note `continue` inside the old loop becomes `return` inside the callback. Check
every migrated plugin for that substitution — a missed `continue` will not
compile, but a `break` would silently change behaviour.

Pass `false, 0` for every plugin in this commit. `hardwareinfo.go`,
`computerinfo.go`, `processorinfo.go` and `snappackages.go` already do work before
their loop; keep that as an explicit call before `runTicker`, exactly as
`cpuusage.go` above keeps its baseline priming, rather than switching them to
`runImmediately` here — that is Task 7's change, and mixing them makes this commit
unreviewable.

- [ ] **Step 5: Run the full monitor suite**

Run: `go test -race ./internal/monitor/ -v`
Expected: PASS, unchanged. This commit is behaviour-preserving; any failure is a
migration error, not a test that needs updating.

- [ ] **Step 6: Confirm no hand-rolled loops remain**

Run: `grep -rn 'time.NewTicker' internal/monitor/ | grep -v monitor.go | grep -v _test.go`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/
git commit -m "refactor(monitor): extract runTicker helper

All 15 plugins hand-rolled the same ticker loop. The shape was correct in
every one — ctx.Done() first, ticker stop deferred — but it meant 15 edit
sites for every cross-cutting change, which is how the missing launch stagger
and missing per-tick timeouts arose.

No behaviour change: every plugin passes runImmediately=false and stagger=0."
```

---

## Task 7: Run `rebootrequired` immediately and stagger plugin launch

Be precise about what is actually a regression here.

**Initial tick.** `BrokerClientPlugin.run_immediately` defaults to `False` in
Python (`landscape/client/broker/client.py:42`), so most plugins waiting a full
interval is **faithful**, not a bug. The real regression is narrow:
`landscape/client/monitor/rebootrequired.py` sets `run_immediately = True` and Go's
`rebootrequired.go` does not. On a device that reboots frequently, the server can
wait 5 minutes to learn a reboot is still required.

**Launch stagger — Go-only regression.** Python delays each plugin's loop start by
`random.random() * run_interval * config.stagger_launch`
(`client.py:117-121`). Go has no equivalent — grepping for `stagger|jitter|rand\.`
across `internal/` and `cmd/` returns nothing. All 15 plugins start at once and
share intervals (five at 30s, five at 5m), so they re-converge on the same tick
forever: a periodic CPU spike plus a burst of simultaneous sends.

**Files:**
- Modify: `internal/monitor/rebootrequired.go`
- Modify: all 15 plugin files (stagger argument)
- Modify: `internal/monitor/sysinfo_test.go` or `snapplugins_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestRebootRequired_ReportsImmediately asserts the plugin does not wait a full
// interval before its first report, matching Python's run_immediately = True.
// On a device that reboots often, the server would otherwise wait 5 minutes to
// learn a reboot is still required.
func TestRebootRequired_ReportsImmediately(t *testing.T) {
	mock := &snapd.MockClient{}
	// Set whatever field makes GetRebootRequired return true — check mock.go.

	p := NewRebootRequired(mock)
	p.interval = time.Hour // must not be waited for

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 1*time.Second)
	cancel()
	<-errCh

	if len(msgs) == 0 {
		t.Fatal("no message before the first interval elapsed")
	}
}
```

Check `snapd.MockClient`'s field names in `internal/snapd/mock.go` and set the one
that drives `GetRebootRequired`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestRebootRequired_ReportsImmediately -v`
Expected: FAIL — `waitForMessages` times out.

- [ ] **Step 3: Set `runImmediately` for `rebootrequired`**

In `internal/monitor/rebootrequired.go`, change the `runTicker` call's third
argument from `false` to `true`, with a comment:

```go
	// Python's rebootrequired sets run_immediately = True: a device that has just
	// rebooted should not wait 5 minutes to tell the server it still needs one.
	runTicker(ctx, p.interval, true, staggerFor(p.interval), func(ctx context.Context, t time.Time) {
```

Leave every other plugin at `false` — that matches Python's default and is not a
regression.

- [ ] **Step 4: Add the stagger to all 15 plugins**

Add the helper next to `runTicker`:

```go
// staggerLaunchFraction mirrors Python's config.stagger_launch: each plugin's
// first tick is delayed by a random fraction of its own interval, so plugins
// sharing an interval do not converge on the same tick forever.
const staggerLaunchFraction = 0.1

func staggerFor(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * staggerLaunchFraction)
}
```

Then pass `staggerFor(p.interval)` as the fourth argument in all 15 plugins.

- [ ] **Step 5: Check the tests tolerate the stagger**

A 10% stagger on a 5ms test interval is 0.5ms — negligible. But several tests use
`waitForMessages` with a 500ms budget against a 5ms interval, which still leaves
plenty of headroom. Run the suite and check for new flakiness:

Run: `go test -race -count=5 ./internal/monitor/`
Expected: PASS all five runs. If a test becomes flaky, raise its timeout rather
than removing the stagger.

- [ ] **Step 6: Commit**

```bash
git add internal/monitor/
git commit -m "fix(monitor): run rebootrequired immediately and stagger plugin launch

Two separate issues, only one of which is a regression.

run_immediately defaults to False in Python, so most plugins waiting a full
interval is faithful. Only rebootrequired sets it True, and Go did not — a
device that has just rebooted waited 5 minutes to tell the server.

The stagger is a genuine Go-only regression: Python delays each plugin by a
random fraction of its interval, Go had no equivalent, so all 15 started
together and — with five sharing 30s and five sharing 5m — re-converged on
the same tick forever."
```

---

## Task 8: Separate sample interval from send interval

The Go plugins conflate *sampling* with *sending*: one message per sample, each
carrying a single data point. Python accumulates points and sends one message per
hour (`landscape/client/accumulate.py:75-103`,
`landscape/client/monitor/cpuusage.py:24-25,50-55`).

Result today: far more messages, each with full bpickle envelope overhead, and —
before Task 1 — an HTTP exchange per message.

As noted at the top of this plan, all four affected plugins already send a *list*
of tuples, so this is additive on the wire.

**Files:**
- Create: `internal/monitor/accumulator.go`
- Create: `internal/monitor/accumulator_test.go`
- Modify: `internal/monitor/cpuusage.go`, `memoryinfo.go`, `loadaverage.go`, `networkactivity.go`

- [ ] **Step 1: Write the failing test**

Create `internal/monitor/accumulator_test.go`:

```go
package monitor

import (
	"testing"
	"time"
)

func TestAccumulator_BuffersUntilTheSendWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAccumulator(60*time.Second, func() time.Time { return now })

	a.add("point-1")
	if got := a.drainIfDue(); got != nil {
		t.Errorf("drained before the send window elapsed: %v", got)
	}

	now = time.Unix(1030, 0)
	a.add("point-2")
	if got := a.drainIfDue(); got != nil {
		t.Errorf("drained at 30s with a 60s window: %v", got)
	}

	now = time.Unix(1061, 0)
	a.add("point-3")
	got := a.drainIfDue()
	if len(got) != 3 {
		t.Fatalf("want 3 buffered points, got %d: %v", len(got), got)
	}
}

func TestAccumulator_DrainEmptiesTheBuffer(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAccumulator(10*time.Second, func() time.Time { return now })

	a.add("a")
	now = time.Unix(1011, 0)
	if got := a.drainIfDue(); len(got) != 1 {
		t.Fatalf("want 1 point, got %d", len(got))
	}

	now = time.Unix(1022, 0)
	if got := a.drainIfDue(); got != nil {
		t.Errorf("second drain returned stale points: %v", got)
	}
}

func TestAccumulator_ZeroWindowSendsEveryPoint(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAccumulator(0, func() time.Time { return now })

	a.add("a")
	if got := a.drainIfDue(); len(got) != 1 {
		t.Errorf("a zero window should send immediately, got %d points", len(got))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestAccumulator -v`
Expected: FAIL — compile error, `undefined: newAccumulator`.

- [ ] **Step 3: Implement the accumulator**

Create `internal/monitor/accumulator.go`:

```go
package monitor

import "time"

// accumulator buffers data points between sends. The plugins previously
// conflated sampling interval with send interval, producing one message per
// sample with a single point and a full bpickle envelope each. Python separates
// the two and sends one message per window.
//
// now is injectable so the window can be tested without sleeping.
type accumulator struct {
	window   time.Duration
	now      func() time.Time
	lastSend time.Time
	points   []any
}

func newAccumulator(window time.Duration, now func() time.Time) *accumulator {
	if now == nil {
		now = time.Now
	}
	return &accumulator{
		window:   window,
		now:      now,
		lastSend: now(),
	}
}

func (a *accumulator) add(point any) {
	a.points = append(a.points, point)
}

// drainIfDue returns the buffered points and resets the window, or nil if the
// window has not elapsed. A zero window sends every point immediately.
func (a *accumulator) drainIfDue() []any {
	if len(a.points) == 0 {
		return nil
	}
	if a.window > 0 && a.now().Sub(a.lastSend) < a.window {
		return nil
	}
	points := a.points
	a.points = nil
	a.lastSend = a.now()
	return points
}
```

- [ ] **Step 4: Use it in `cpuusage.go`**

```go
type CPUUsage struct {
	procStatPath string
	interval     time.Duration // sampling interval
	sendInterval time.Duration // how often buffered points are sent
	prevTotal    int64
	prevIdle     int64
	hasPrev      bool
}

func NewCPUUsage() *CPUUsage {
	return &CPUUsage{
		procStatPath: "/proc/stat",
		interval:     30 * time.Second,
		sendInterval: 5 * time.Minute,
	}
}
```

and in `Run`:

```go
	acc := newAccumulator(p.sendInterval, time.Now)

	runTicker(ctx, p.interval, false, staggerFor(p.interval), func(ctx context.Context, t time.Time) {
		usage, err := p.sample()
		if err != nil {
			log.Printf("cpu-usage: %v", err)
			return
		}
		if usage < 0 {
			return
		}
		acc.add(bpickle.Tuple{t.Unix(), usage})

		points := acc.drainIfDue()
		if points == nil {
			return
		}
		msg := exchange.Message{
			"type":       "cpu-usage",
			"cpu-usages": points,
		}
		if err := sink.Send(ctx, msg); err != nil {
			log.Printf("cpu-usage: send: %v", err)
		}
	})
```

Apply the same shape to `memoryinfo.go` (`memory-info`), `loadaverage.go`
(`load-averages`) and `networkactivity.go` (per-interface, so one accumulator per
interface — check its `bpickle.BytesDict` shape first and keep it).

Buffered points are lost if the daemon stops mid-window. That is acceptable for
resampled telemetry and is why this treatment is limited to these four plugins —
`operation-result` and inventory messages are never buffered. State that in the
commit message.

- [ ] **Step 5: Assert the wire shape did not change**

```go
// TestCPUUsage_BatchedMessageShapeIsUnchanged asserts batching is additive: the
// same field carries N tuples instead of 1, with identical tuple shape.
func TestCPUUsage_BatchedMessageShapeIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	writeFixture(t, statPath, "cpu  100 0 100 800 0 0 0 0 0 0\n")

	p := &CPUUsage{
		procStatPath: statPath,
		interval:     5 * time.Millisecond,
		sendInterval: 20 * time.Millisecond,
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 1*time.Second)
	cancel()
	<-errCh

	points, ok := msgs[0]["cpu-usages"].([]any)
	if !ok {
		t.Fatalf("cpu-usages: want []any, got %T", msgs[0]["cpu-usages"])
	}
	if len(points) == 0 {
		t.Fatal("no points in the batched message")
	}
	if _, ok := points[0].(bpickle.Tuple); !ok {
		t.Errorf("point 0: want bpickle.Tuple, got %T", points[0])
	}
}
```

The fixture must produce a changing CPU delta across ticks; if a static
`/proc/stat` yields `usage < 0` forever, write a fixture-updating helper or accept
a single point and assert only the shape.

- [ ] **Step 6: Run to verify it passes**

Run: `go test -race ./internal/monitor/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/
git commit -m "perf(monitor): separate sample interval from send interval

The plugins conflated sampling with sending: one message per sample carrying
a single data point, each with a full bpickle envelope. Python separates the
two via Accumulator/step-size and sends one message per window.

Additive on the wire: all four plugins already sent a list of tuples holding
exactly one point, so the same field now carries N with identical tuple
shape.

Buffered points are lost if the daemon stops mid-window. Acceptable for
resampled telemetry, which is why this is limited to cpu-usage, memory-info,
load-average and network-activity — results and inventory are never
buffered."
```

---

## Task 9: Cut per-tick allocation and fix `diffProcesses`

Two problems in `internal/monitor/activeprocessinfo.go`.

**Allocation.** The plugin runs every 30s and rebuilds the full process table each
tick: `os.ReadDir(/proc)` plus one `os.ReadFile` per PID, a fresh
`map[int64]processInfo`, and a `map[string]any` per process via `processToMap`. On
a device with a few hundred processes that is a few hundred small maps and slices
every 30s — the main steady-state GC pressure in the daemon, and probably why
`GOGC=50` was reached for.

The slice capacity hints already in the file are **correct** and stay; this is
about map and interface allocation, not sizing.

**Degenerate diff.** `diffProcesses` compares whole structs with `oldInfo !=
newInfo`, including `percentCPU` rounded to 0.1. Any process doing work changes
that field, so `update-processes` carries a large share of the process table every
30s and the diff has stopped saving anything.

**Files:**
- Modify: `internal/monitor/activeprocessinfo.go`
- Modify: `internal/monitor/sysinfo_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestDiffProcesses_IgnoresCPUJitter asserts a process whose only change is its
// CPU percentage is not reported as updated. Comparing whole structs made the
// diff degenerate: any process doing work counted as changed.
func TestDiffProcesses_IgnoresCPUJitter(t *testing.T) {
	old := map[int64]processInfo{
		1: {pid: 1, name: "init", state: "S", percentCPU: 0.1},
		2: {pid: 2, name: "worker", state: "R", percentCPU: 3.4},
	}
	updated := map[int64]processInfo{
		1: {pid: 1, name: "init", state: "S", percentCPU: 0.2},
		2: {pid: 2, name: "worker", state: "R", percentCPU: 7.9},
	}

	added, changed, removed := diffProcesses(old, updated)

	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("want no additions or removals, got %d/%d", len(added), len(removed))
	}
	if len(changed) != 0 {
		t.Errorf("CPU-only changes should not be reported as updates, got %d", len(changed))
	}
}

// TestDiffProcesses_ReportsRealChanges guards against over-correcting.
func TestDiffProcesses_ReportsRealChanges(t *testing.T) {
	old := map[int64]processInfo{
		1: {pid: 1, name: "init", state: "S", percentCPU: 0.1},
	}
	updated := map[int64]processInfo{
		1: {pid: 1, name: "init", state: "Z", percentCPU: 0.1},
		2: {pid: 2, name: "new", state: "R"},
	}

	added, changed, removed := diffProcesses(old, updated)

	if len(added) != 1 {
		t.Errorf("want 1 added process, got %d", len(added))
	}
	if len(changed) != 1 {
		t.Errorf("a state change S->Z must be reported, got %d changes", len(changed))
	}
	if len(removed) != 0 {
		t.Errorf("want no removals, got %d", len(removed))
	}
}
```

Read `processInfo`'s actual field names and `diffProcesses`'s actual signature
first — the return shape above is a guess and must be corrected to match.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestDiffProcesses -v`
Expected: FAIL with `CPU-only changes should not be reported as updates, got 2`.

- [ ] **Step 3: Compare the fields that matter**

Replace the whole-struct comparison with an explicit predicate:

```go
// processChanged reports whether a process differs in a way worth telling the
// server about. percentCPU is deliberately excluded: it is a lifetime average
// that moves on every tick for any process doing work, so including it made
// update-processes carry most of the table every 30s and the diff saved nothing.
func processChanged(old, updated processInfo) bool {
	return old.name != updated.name ||
		old.state != updated.state ||
		old.vsize != updated.vsize ||
		old.uid != updated.uid ||
		old.startTime != updated.startTime
}
```

and use `if processChanged(oldInfo, newInfo)` in `diffProcesses`. Adjust the field
list to the real struct — include every field that appears in the outbound message
except `percentCPU`.

Note this changes what `update-processes` reports: a process whose CPU changed but
whose identity did not no longer generates an update. That is the intended saving,
and it does not change any *value* the server receives for processes that are
reported.

- [ ] **Step 4: Reuse the process map across ticks**

Add a reusable map to the struct and `clear()` it per tick instead of allocating:

```go
	// Reused across ticks: rebuilding this map every 30s was the daemon's main
	// steady-state GC pressure.
	scratch map[int64]processInfo
```

```go
	if p.scratch == nil {
		p.scratch = make(map[int64]processInfo, 256)
	} else {
		clear(p.scratch)
	}
```

Take care: the previous tick's map is still needed for the diff, so alternate
between two maps rather than clearing the one being compared against. Swap them
after each diff.

- [ ] **Step 5: Reuse the per-PID read buffer**

Replace the per-PID `os.ReadFile` with an `os.Open` plus a reused `[]byte` on the
struct:

```go
	// Reused across PIDs and ticks; /proc/<pid>/stat is well under 4 KiB.
	buf []byte
```

```go
	if p.buf == nil {
		p.buf = make([]byte, 4096)
	}
	f, err := os.Open(statPath)
	if err != nil {
		continue
	}
	n, err := f.Read(p.buf)
	_ = f.Close()
	if err != nil && n == 0 {
		continue
	}
	data := p.buf[:n]
```

A short read is fine here — `/proc/<pid>/stat` is a single line well under 4 KiB —
but assert that in a test rather than assuming it.

- [ ] **Step 6: Add an allocation benchmark**

```go
func BenchmarkActiveProcessInfo_Collect(b *testing.B) {
	p := NewActiveProcessInfo()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.collect()
	}
}
```

Adapt to the real collection method's name and signature.

Run: `go test ./internal/monitor/ -bench BenchmarkActiveProcessInfo -benchmem -run '^$'`
Record the before and after `allocs/op` in the commit message. A meaningful
reduction is the point of this commit; if there is none, say so rather than
claiming one.

- [ ] **Step 7: Run to verify it passes**

Run: `go test -race ./internal/monitor/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/monitor/activeprocessinfo.go internal/monitor/sysinfo_test.go
git commit -m "perf(monitor): cut per-tick allocation and fix diffProcesses

The plugin rebuilt the whole process table every 30s — one ReadFile per PID,
a fresh map, and a map[string]any per process — which was the daemon's main
steady-state GC pressure and probably why GOGC=50 was reached for. Maps and
the read buffer are now reused; the existing slice capacity hints were
already correct and are unchanged.

Separately, diffProcesses compared whole structs including percentCPU rounded
to 0.1, so any process doing work counted as changed and update-processes
carried most of the table every 30s. percentCPU is a lifetime average, not
instantaneous usage, so excluding it from the change test costs nothing.

allocs/op: <before> -> <after>"
```

Fill in the benchmark numbers before committing.

---

## Task 10: Remaining ordering and error-discard regressions

Four §7.2 items with no Python excuse.

**Files:**
- Modify: `internal/monitor/temperature.go`, `processorinfo.go`, `mountinfo.go`, `activeprocessinfo.go`, `computerinfo.go`

- [ ] **Step 1: Write the failing tests**

```go
// TestTemperature_ZoneOrderIsDeterministic asserts multi-zone devices emit in a
// stable order. Iterating a map produced a different order every tick.
func TestTemperature_ZoneOrderIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	for _, z := range []struct{ name, temp string }{
		{"thermal_zone0", "45000"},
		{"thermal_zone1", "50000"},
		{"thermal_zone2", "55000"},
	} {
		zoneDir := filepath.Join(dir, z.name)
		if err := os.MkdirAll(zoneDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFixture(t, filepath.Join(zoneDir, "temp"), z.temp+"\n")
		writeFixture(t, filepath.Join(zoneDir, "type"), z.name+"\n")
	}

	var orders [][]string
	for run := 0; run < 5; run++ {
		p := &Temperature{interval: 5 * time.Millisecond, thermalPath: dir}
		sink := &mockSink{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)

		errCh := make(chan error, 1)
		go func() { errCh <- p.Run(ctx, sink, nil) }()
		msgs := waitForMessages(t, sink, 3, 500*time.Millisecond)
		cancel()
		<-errCh

		order := make([]string, 0, 3)
		for _, m := range msgs[:3] {
			order = append(order, m["thermal-zone"].(string))
		}
		orders = append(orders, order)
	}

	for i := 1; i < len(orders); i++ {
		for j := range orders[0] {
			if orders[i][j] != orders[0][j] {
				t.Fatalf("zone order varies between runs: %v vs %v", orders[0], orders[i])
			}
		}
	}
}

// TestComputerInfo_HostnameErrorIsNotSentAsEmpty asserts a failed hostname
// lookup is not reported as an empty hostname. `hostname, _ := os.Hostname()`
// sent "", which the server reads as "this device has no hostname".
func TestComputerInfo_HostnameErrorIsNotSentAsEmpty(t *testing.T) {
	p := NewComputerInfo(&snapd.MockClient{})
	p.interval = 5 * time.Millisecond
	p.getHostname = func() (string, error) {
		return "", errors.New("simulated hostname failure")
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-errCh

	for _, msg := range sink.messages() {
		v, ok := msg["hostname"]
		if !ok {
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			t.Error("sent an empty hostname after a failed lookup")
		}
		if b, isBytes := v.([]byte); isBytes && len(b) == 0 {
			t.Error("sent an empty hostname after a failed lookup")
		}
	}
}

func TestComputerInfo_HostnameIsSentWhenAvailable(t *testing.T) {
	p := NewComputerInfo(&snapd.MockClient{})
	p.interval = 5 * time.Millisecond
	p.getHostname = func() (string, error) { return "test-host", nil }

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 1*time.Second)
	cancel()
	<-errCh

	var found bool
	for _, msg := range msgs {
		if v, ok := msg["hostname"]; ok {
			found = true
			if fmt.Sprintf("%s", v) != "test-host" {
				t.Errorf("hostname: want test-host, got %v", v)
			}
		}
	}
	if !found {
		t.Error("no message carried a hostname")
	}
}
```

The `thermalPath` field name and the `thermal-zone` value type (`string` or
`[]byte`) must be checked against `internal/monitor/temperature.go` — the message
currently sets `"thermal-zone": zone`, so match whatever `zone`'s type is.

`getHostname` does not exist yet; adding it is part of Step 6. Declare it as
`getHostname func() (string, error)` on `ComputerInfo`, defaulted to `os.Hostname`
in `NewComputerInfo`. That is the only way to cover this case, and it is a
one-line change.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run 'TestTemperature_ZoneOrder' -v`
Expected: FAIL with `zone order varies between runs`, though it may pass by chance
— run with `-count=10` to confirm.

- [ ] **Step 3: Sort the temperature zones**

Collect zone names into a slice, `slices.Sort` it, then iterate the slice rather
than ranging over the map.

- [ ] **Step 4: Stabilise `processorinfo` identifiers**

An ARM `/proc/cpuinfo` block with no `processor` line currently defaults to
`processor-id: 0`, so multiple such blocks collide and the `sort.Slice` is unstable
across them. Assign a sequential index as the block is parsed, and use
`slices.SortStableFunc` so equal keys retain input order.

- [ ] **Step 5: Add change detection to `mountinfo` free-space**

`free-space` is sent unconditionally every 5 minutes — 288 messages/day from this
plugin alone. Hash the entries the same way the plugin already hashes its mount
layout, and skip the send when unchanged. Free space genuinely does change, so
expect this to reduce rather than eliminate the traffic; do not add a change
threshold, because that would alter reported values.

- [ ] **Step 6: Stop discarding errors that fabricate data**

| Site | Current | Change |
|---|---|---|
| `activeprocessinfo.go` `utime`/`stime`/`starttime`/`vsize` | four `_ =` `ParseInt`s | skip the process on parse failure rather than reporting 0% CPU with a boot-time start |
| `activeprocessinfo.go` scanner | ignores `scanner.Err()` | log and skip the tick |
| `computerinfo.go` | `hostname, _ := os.Hostname()` | log and skip the field rather than sending an empty hostname |
| `mountinfo.go` | `layoutData, _ := json.Marshal(...)` | log and skip the tick rather than hashing `null` |

- [ ] **Step 7: Run to verify it passes**

Run: `go test -race -count=10 ./internal/monitor/ -run 'TestTemperature|TestProcessorInfo|TestMountInfo|TestComputerInfo|TestActiveProcess' -v`
Expected: PASS all ten runs.

- [ ] **Step 8: Commit**

```bash
git add internal/monitor/
git commit -m "fix(monitor): ordering and error-discard regressions

temperature iterated a map, so multi-zone devices emitted in a different
order every tick. processorinfo defaulted ARM blocks with no 'processor' line
to id 0, so they collided under an unstable sort. mountinfo sent free-space
unconditionally every 5 minutes — 288 messages/day.

Four ParseInts, scanner.Err(), os.Hostname and a json.Marshal all had their
errors discarded in ways that fabricate data: a malformed field reported a
process as 0% CPU with a boot-time start, and a failed marshal hashed 'null'.

None of these have a Python excuse."
```

---

## Task 11: Verify the phase

- [ ] **Step 1: Full verification**

Run:

```bash
gofmt -l .
go vet ./...
go test -race -count=3 ./...
golangci-lint run
```

Expected: all clean across three runs — this phase touches scheduling and
concurrency, so a single green run is not enough.

- [ ] **Step 2: Confirm the exchange interval is honoured**

Run: `go test ./internal/exchange/ -run 'TestSend_DoesNotForceAnExchange|TestSendUrgent' -v`
Expected: PASS.

- [ ] **Step 3: Confirm no hand-rolled ticker loops remain**

Run: `grep -rn 'time.NewTicker' internal/monitor/ | grep -v monitor.go | grep -v _test.go`
Expected: no output.

- [ ] **Step 4: Record the allocation improvement**

Run: `go test ./internal/monitor/ -bench . -benchmem -run '^$'`
Expected: `activeprocessinfo` allocations materially below the pre-phase baseline.
Put the numbers in the PR description.

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin fix/03-p1-efficiency
```

PR title: `Phase 3: P1 efficiency — exchange rework, scheduling, allocation`

The PR description should state explicitly that no reported *value* changed, only
message cadence and grouping, and link to spec §7 for the values that are under
separate consideration.

---

## Done when

- Non-urgent plugin sends do not trigger an exchange, and `exchange-interval` is observed.
- `operation-result` still triggers an immediate exchange.
- Repeated 429/5xx responses escalate the retry delay with jitter and decay on success.
- No exchange carries more than 100 messages.
- Queued messages survive a restart, and a full queue drops oldest telemetry while retaining every `operation-result`.
- All 15 plugins use `runTicker`; `rebootrequired` reports immediately; every plugin's first tick is staggered.
- `cpu-usage`, `memory-info`, `load-average` and `network-activity` batch multiple points per message with unchanged tuple shape.
- `diffProcesses` no longer reports CPU-only changes, and `activeprocessinfo` allocates measurably less per tick.
- Temperature zones and processor IDs are deterministically ordered, and no discarded error fabricates data.
