# Concurrency Design Guide

This document describes the concurrency patterns used in landscape-client-core and why each pattern is used.

## Goals

- Keep goroutine lifetimes explicit and bounded.
- Make shutdown deterministic.
- Avoid unbounded work queues and hidden background work.
- Prefer simple ownership and cancellation rules over ad-hoc locking.

## Goroutine Lifecycle Management

Long-running components (exchange loop, monitor runner, ping loop) are started with context-aware `Run(ctx)` methods.

Rules:

- Each loop listens for `ctx.Done()` and exits quickly.
- Shutdown paths use bounded waits where needed (for example, grace windows).
- Background goroutines are either tracked (WaitGroup/errgroup) or are short-lived fire-once goroutines with clear ownership.

## Context Usage and Cancellation

Contexts carry cancellation and deadlines through component boundaries.

Patterns used:

- Parent context from main controls service lifetime.
- Child contexts are used for bounded operations (timeouts or final drain steps).
- Operation-scoped contexts can be canceled when the server requests cancellation.

Guidelines:

- Always pass context into I/O and network operations.
- Do not store contexts on structs for long-term reuse.
- Derive child contexts close to the operation that needs them.

## Semaphore Bounding (Plan 1)

Handler execution can be bounded with a semaphore to prevent unbounded goroutine growth under bursty inbound traffic.

Benefits:

- Caps concurrent handler execution.
- Provides backpressure instead of memory growth.
- Works with context cancellation to avoid indefinite waits.

Use this for work pools where throughput is important but hard upper limits are required.

## WaitGroup Tracking (Plan 2)

WaitGroups track in-flight asynchronous work so shutdown can wait for completion.

Benefits:

- No fire-and-forget behavior during shutdown.
- Deterministic teardown and easier tests.
- Explicit ownership of background tasks.

Use WaitGroup when you only need completion tracking and not first-error propagation.

## Operation Cancellation (Plan 3)

Operation handlers can run with operation-scoped contexts that are tracked by operation ID.

Benefits:

- Mid-flight cancellation for long-running tasks.
- Unified behavior for timeout, shutdown, and server-requested cancel.
- Reduced wasted work after operation is no longer relevant.

Design notes:

- Register cancel funcs before dispatch.
- Unregister cancel funcs when operation completes.
- Return cancellation errors as operation results when appropriate.

## errgroup Error Handling (Plan 4)

`errgroup` is used when a set of related goroutines should fail together and report the first error.

Benefits:

- Shared cancellation across related goroutines.
- Error propagation without custom channels.
- Cleaner orchestration code than manual WaitGroup plus error plumbing.

Use `errgroup` for tightly coupled goroutines where one failure should stop the group.

## Atomic Patterns (Plan 5)

Simple independently updated values are good candidates for atomic access.

Current usage:

- Ping interval uses `atomic.Value` with `time.Duration` in [internal/ping/ping.go](../internal/ping/ping.go).

Why atomic is a fit here:

- Single value with no multi-field invariant.
- Frequent reads and occasional writes.
- Lock-free access reduces synchronization overhead and complexity.

Constraints:

- Store/load must use a consistent concrete type (`time.Duration`).
- Initialize atomic storage before first load.

## Why Remaining Mutexes Are Retained

Not all mutexes should be removed. Two mutexes are intentionally kept because they protect multi-step invariants.

### Exchange mutex

Location: [internal/exchange/exchange.go](../internal/exchange/exchange.go)

Protects:

- `pending` outbound queue
- `handlers` subscription map
- `insecureID` shared state

Reason:

- These values are read and written from multiple goroutines.
- Operations involve compound steps (append/copy/read) that must be consistent.
- Atomic primitives do not replace map/slice coordination safely.

### limitWriter mutex

Location: [internal/manager/system.go](../internal/manager/system.go)

Protects:

- remaining byte budget `n`
- truncation state `truncated`
- marker insertion behavior

Reason:

- stdout/stderr copy goroutines can call `Write` concurrently.
- The writer must guarantee the truncation marker is appended once and state stays coherent.
- This is a multi-variable invariant, so a mutex is the correct primitive.

## Best Practices

- Prefer context cancellation and ownership boundaries before adding locks.
- Use semaphores for concurrency limits and WaitGroup/errgroup for lifecycle control.
- Use atomics only for single-value state with clear type invariants.
- Keep mutex critical sections small and focused on shared invariants.
- Document why each lock exists and what it protects.
- Verify concurrency changes with `go test -race`.
