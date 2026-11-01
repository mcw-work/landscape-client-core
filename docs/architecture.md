# Architecture

`landscape-client-core` is one daemon process (`cmd/landscape-client-core`)
plus an interactive configuration wizard (`cmd/landscape-client-core-config`).
The daemon talks to the Landscape server over HTTPS and to the device through
snapd, procfs/sysfs, logind and `lshw`.

## Components

```mermaid
flowchart LR
    server[("Landscape server<br/>/message-system<br/>/ping<br/>/attachment/ID")]

    subgraph daemon["landscape-client-core daemon"]
        direction LR
        monitor["monitor.Runner<br/>15 plugins"]
        exchange["exchange.Exchange<br/>queue, sequencing, dispatch"]
        manager["manager.Runner<br/>command handlers"]
        ping["ping.Pinger"]
        watchdog["watchdog"]
        transport["transport.Client<br/>HTTPS, proxy, CA pinning"]
        bpickle["bpickle codec"]
    end

    subgraph disk["$SNAP_COMMON"]
        state[("state.json<br/>identity, sequences,<br/>plugin state")]
        queue[("queue.json<br/>unsent messages")]
        scripts[("scripts/OP-ID/")]
    end

    subgraph device["Device"]
        snapd["snapd<br/>/run/snapd.socket"]
        procfs["/proc, /sys,<br/>extrausers"]
        logind["logind<br/>D-Bus"]
        lshw["lshw"]
    end

    monitor -- "Send(msg)" --> exchange
    exchange -- "Subscribe callbacks" --> manager
    manager -- "SendResult" --> exchange
    ping -- "TriggerExchange()" --> exchange
    watchdog -. "StaleSources()" .-> monitor

    exchange --> bpickle
    exchange --> transport
    ping --> transport
    manager -- "attachments" --> transport
    transport <--> server

    exchange <--> state
    exchange <--> queue
    monitor <--> state
    manager --> scripts

    monitor --> procfs
    monitor --> snapd
    monitor --> lshw
    manager --> snapd
    manager --> logind
```

| Package | Responsibility |
|---|---|
| `cmd/landscape-client-core` | Loads config, wires every component, runs them, handles `SIGTERM`/`SIGINT`. Also implements the `--validate-config` and `--sync-confdb` modes used by the configure hook. |
| `cmd/landscape-client-core-config` | Interactive wizard that writes all settings in one `snapctl set`. |
| `internal/config` | Reads snap config through `snapctl get`, applies defaults, validates. |
| `internal/exchange` | Outbound queue, message sequencing, registration, the exchange loop, inbound dispatch. |
| `internal/monitor` | Plugin runner and the 15 telemetry plugins. |
| `internal/manager` | Handlers for inbound commands (snaps, services, shutdown, scripts, cancel). |
| `internal/ping` | Cheap periodic poll that wakes the exchange when the server has messages. |
| `internal/transport` | HTTP client: TLS 1.2+, optional custom CA, proxies, timeouts, response size cap. |
| `internal/bpickle` | Encoder/decoder for Landscape's wire format. |
| `internal/persist` | Atomic JSON state file with backup recovery. |
| `internal/snapd` | Minimal snapd REST client over the Unix socket. |
| `internal/runcmd` | Runs external commands with a timeout. |
| `internal/version` | Version string and `User-Agent`, injected at build time. |

## Goroutines and lifecycle

`run()` in [main.go](../cmd/landscape-client-core/main.go) starts four
long-lived goroutines in an `errgroup`. If any of them returns an error the
group context is cancelled and the process exits non-zero, so snapd's
`restart-condition: on-failure` restarts it.

```mermaid
flowchart TB
    main["realMain<br/>config.Load, persist.New, transport.New"] --> run["run()"]
    run --> eg{{"errgroup"}}
    eg --> ex["exchange.Run"]
    eg --> mon["monitor.Runner.Run"]
    eg --> pg["ping.Pinger.Run"]
    eg --> wd["watchdog, every 30 s"]
    mon --> p1["plugin goroutine x15<br/>restart with backoff 1 s to 5 min"]
    ex -- "inbound command" --> mgr["manager handler goroutine<br/>bounded by semaphore, default 100"]
```

- **Monitor plugins** each run in their own goroutine. A plugin that returns an
  error or panics is restarted after a backoff that starts at 1 s and doubles to
  a 5 min cap. The backoff resets after a run that lasted at least 30 s.
- **Manager handlers** run one goroutine per command. A weighted semaphore caps
  concurrency (`--handler-concurrency`, default 100) and a `WaitGroup` tracks
  them for shutdown.
- **Watchdog.** Every plugin records a heartbeat on each tick. Every 30 s the
  watchdog flags any plugin that has not beaten for `max(3 × interval, 2 min)`.
  A stale plugin means a goroutine is wedged (for example blocked in a
  syscall), so the daemon exits non-zero and snapd restarts it.

### Shutdown

```mermaid
sequenceDiagram
    participant OS as snapd / signal
    participant Main as run()
    participant Ex as exchange
    participant Mgr as manager
    OS->>Main: SIGTERM
    Main->>Ex: cancel context
    Ex->>Ex: final exchange (5 s budget)
    Ex->>Ex: write queue.json
    Main->>Mgr: WaitWithTimeout(5 s)
    Main->>Main: wait for errgroup (5 s)
    Main-->>OS: exit
```

## Outbound: telemetry to the server

```mermaid
sequenceDiagram
    participant P as plugin
    participant Ex as exchange
    participant Disk as state.json / queue.json
    participant S as server

    P->>Ex: Send(msg) — queued, does not wake the loop
    Note over Ex: timer fires, or woken by ping,<br/>an operation result, or a backlog
    Ex->>Ex: prepend "register" if not registered
    Ex->>Ex: take up to 100 messages of accepted types,<br/>hold the rest in the queue
    Ex->>S: POST /message-system (bpickle)
    alt transport error or non-2xx
        Ex->>Ex: re-queue the batch
    else 200
        S-->>Ex: next-expected-sequence, token, messages
        Ex->>Ex: re-queue anything not ACKed
        Ex->>Ex: handle and dispatch inbound messages
        Ex->>Disk: update state.json
    end
    Ex->>Disk: write queue.json
```

The next exchange is scheduled as follows:

| Condition | Delay |
|---|---|
| Default | `exchange-interval` (900 s) |
| An `operation-result` is queued, the device is not registered, or it has just registered | `urgent-exchange-interval` (60 s) |
| The server returned 429 or 5xx | Backoff of 5 min doubling to 2 h, plus up to 20% jitter, if longer than the above |
| Messages are still queued after a successful exchange | Immediately |

The in-memory and on-disk queue is capped at 1,000 messages. When it overflows
the oldest telemetry is dropped first; operation results are never dropped.

## Inbound: commands from the server

```mermaid
sequenceDiagram
    participant S as server
    participant Ex as exchange
    participant R as manager.Runner
    participant H as handler goroutine
    participant D as snapd / logind / shell

    S-->>Ex: exchange response with messages
    alt protocol message (set-id, accepted-types, ...)
        Ex->>Ex: handled inline
    else command
        Ex->>R: subscriber callback (in server order)
        R->>R: acquire semaphore
        R->>H: go Handle(ctx, msg)
        H->>D: perform operation
        H->>Ex: SendResult / SendResultCode
        Ex->>Ex: queue operation-result and wake loop
    end
    Ex->>S: operation-result on the next exchange
```

Protocol messages handled inside the exchange are listed in
[protocol.md](protocol.md#messages-handled-by-the-exchange). Commands handled
by the manager are in [management-commands.md](management-commands.md). A
`set-intervals` message is handled by a subscriber in `main` that updates the
ping interval.

## Ping

```mermaid
sequenceDiagram
    participant P as ping.Pinger
    participant S as server /ping
    participant Ex as exchange
    loop every ping-interval (30 s)
        alt not registered (no insecure-id)
            P->>P: skip
        else registered
            P->>S: POST insecure_id=...
            S-->>P: {messages: true|false}
            opt messages: true
                P->>Ex: TriggerExchange()
            end
        end
    end
```

## Registration

```mermaid
stateDiagram-v2
    [*] --> Unregistered
    Unregistered --> Unregistered: exchange sends "register"<br/>(every urgent interval)
    Unregistered --> Registered: set-id<br/>(store secure-id, insecure-id,<br/>clear plugin state)
    Registered --> Unregistered: unknown-id<br/>(clear both IDs)
    Registered --> Registered: resynchronize<br/>(clear plugin state, ack)
```

## Persistent state

All files live in `$SNAP_COMMON` (`/var/snap/landscape-client-core/common`).

| File | Contents | Written by |
|---|---|---|
| `state.json` | `secure_id`, `insecure_id`, `server_uuid`, `outbound_sequence`, `next_expected_from_server`, `exchange_token`, `accepted_types`, `accepted_types_hash`, `plugin_state` | exchange after every exchange; plugins via `SetPluginState` |
| `state.json.old` | Previous good copy, used if `state.json` is corrupt at load | every save |
| `queue.json` | Unsent outbound messages | exchange after every attempt and at shutdown |
| `scripts/<operation-id>/` | Script and attachments while a script runs; removed afterwards | `execute-script` handler |

Every write goes to a temp file, is `fsync`ed and then renamed over the target.
All writes to `state.json` are read-modify-write under one mutex, so the
exchange and plugins never overwrite each other's fields.

## Snap packaging

[snapcraft.yaml](../snap/snapcraft.yaml) defines:

- **`landscape-client-core`** — the daemon. Installed disabled
  (`install-mode: disable`); the configure hook enables it once configured.
  Runs with `GOMEMLIMIT=64MiB` and `GOMAXPROCS=2`.
- **`config`** — the wizard.
- **Plugs:** `network`, `snapd-control`, `system-observe`, `hardware-observe`,
  `shutdown`, `log-observe`, `mount-observe`, plus two confdb custodian plugs
  (`landscape-client-admin`, `landscape-client-state`).
- **Hooks:** `install`, `post-refresh`, `configure`, and
  `change-view-landscape-client-admin`. See
  [configuration.md](configuration.md#snap-hooks).

Under strict confinement the daemon runs as root and cannot switch users, so
the `username` field of `execute-script` is ignored.
