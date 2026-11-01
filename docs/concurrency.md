# Concurrency

Rules for adding or changing concurrent code. The runtime structure itself is
described in [architecture.md](architecture.md#goroutines-and-lifecycle).

## Ownership

- Every long-running component exposes `Run(ctx)` and returns promptly when
  `ctx` is done. `main` starts them in one `errgroup`; a returned error stops
  the whole daemon so snapd restarts it.
- Every other goroutine is either tracked (manager handlers in a `WaitGroup`,
  plugin goroutines in the monitor's `errgroup`) or short-lived with a single
  owner.
- Shutdown waits are bounded (5 s each), so a stuck goroutine cannot block
  exit.

## Contexts

- The daemon context from `main` controls component lifetime.
- Inbound commands run on the daemon context, not a per-exchange one, because
  handlers outlive the exchange that delivered them.
- Each operation derives its own cancellable context and registers it with
  `OperationContextManager`, so `cancel-operation`, timeouts and shutdown all
  stop it the same way.
- Every I/O call takes a context. Calls to snapd, logind and `lshw` add their
  own timeouts because the daemon context never expires on its own.
- Do not store contexts in structs for reuse. The one exception is the
  exchange's dispatch context, set once in `Run`.

## Inbound dispatch

The exchange calls subscriber callbacks synchronously, in server order. A
callback must return quickly. `manager.Runner` acquires its semaphore in the
callback and then hands the work to a goroutine. There is no extra queue
between the exchange and the runner; the semaphore is the back-pressure.

## Choosing a primitive

| Need | Use | Example |
|---|---|---|
| Cap concurrent work | `semaphore.Weighted` | manager handlers |
| Wait for background work at shutdown | `sync.WaitGroup` | manager handlers |
| Related goroutines that fail together | `errgroup` | `run()`, monitor runner |
| A single value read often, written rarely | `atomic.Value` | ping interval |
| Several fields that must change together | `sync.Mutex` | see below |

### Mutexes in use

| Location | Protects | Why not something lighter |
|---|---|---|
| `exchange.Exchange.mu` | `pending` queue, `handlers` map, `insecureID`, `dispatchCtx` | Slice and map updates are compound operations. |
| `persist.Store.mu` | Read-modify-write of `state.json` | The exchange and plugins update different fields of one file. |
| `manager.limitWriter.mu` | Remaining output budget and truncation flag | stdout and stderr write concurrently; the marker must be added exactly once. |
| `manager.OperationContextManager.mu` | Operation ID to cancel function map | Map access from many handlers. |
| `monitor.Heartbeat.mu` | Last-beat times | Plugins write while the watchdog reads. |

Keep critical sections short, and put a one-line comment on each lock saying
what it guards.

## Testing

Run `go test -race ./...` for any concurrency change. Timing-sensitive tests
should also pass with `-count=3`.
