# Architecture

## Overview

`landscape-client-core` is structured as three concurrent subsystems coordinated by the main entry point. Each subsystem runs its own goroutine loop and communicates with the others through well-defined interfaces.

```
┌─────────────────────────────────────────────────────────────┐
│  main.go                                                    │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │   Exchange   │  │   Monitor    │  │      Ping        │  │
│  │   Runner     │  │   Runner     │  │      Loop        │  │
│  └──────┬───────┘  └──────┬───────┘  └────────┬─────────┘  │
└─────────┼────────────────┼──────────────────┼─────────────┘
          │                │                  │
          │◄───────────────┘ MessageSink       │ TriggerExchange
          │                                   │
    ┌─────▼─────────────────────────────────◄─┘
    │  internal/exchange                       │
    │  • Outbound queue                        │
    │  • Sequence tracking                     │
    │  • Message dispatch                      │
    └──┬──────────────┬───────────────────────┘
       │              │
  transport       manager.Runner
  (HTTP POST)     • InstallSnap
       │          • RemoveSnap
  bpickle         • RefreshSnap
  encode/decode   • StartService
                  • StopService
                  • RestartService
                  • Shutdown
                  • ScriptExec
                       │
                  transport
                  (attachment fetch)
```

## Packages

### `cmd/landscape-client-core`

The daemon entry point. Responsible for:

- Loading configuration via `snapctl`
- Constructing and wiring all subsystems
- Starting the three concurrent goroutine groups (exchange, monitor, ping)
- Handling `SIGTERM`/`SIGINT` for graceful shutdown with a 5-second grace period

The binary also accepts a `--validate-config` flag, used by the snap `configure` hook to check configuration without starting the daemon.

### `cmd/landscape-client-core-config`

An interactive terminal wizard for configuring the client. Collects all settings (account name, registration key, server URL, proxy, tags, etc.) then writes them with a single batched `snapctl set` call to avoid triggering the configure hook in an incomplete state.

### `internal/exchange`

The core exchange loop. Runs on a ticker (normal: 15 min, urgent: 1 min). On each tick it:

1. Builds the outbound payload from queued messages.
2. bpickle-encodes the payload and POST's it to the server.
3. Decodes the response and dispatches inbound messages to registered handlers.
4. Persists updated sequence numbers and device identities.

Provides three key interfaces consumed by the other subsystems:

- `MessageSink` — for monitor plugins to enqueue outbound messages.
- `CommandSource` — for the manager to subscribe to inbound message types.
- `ResultSink` — for handlers to send `operation-result` messages back to the server.

### `internal/manager`

Receives inbound management commands from the exchange and dispatches them to `Handler` implementations. Each handler runs in its own goroutine. Panics are caught and reported as failures. See [Management Commands](management-commands.md).

### `internal/monitor`

Runs a collection of system-metric plugins. Each plugin loops independently with exponential backoff on failure (1 s → 5 min). Plugins post metrics to the exchange `MessageSink`. See [Monitoring Plugins](monitoring-plugins.md).

### `internal/ping`

Lightweight periodic poll. POSTs the device's insecure-id to the ping endpoint every interval (default 30 s). If the server indicates pending messages, calls `Exchange.TriggerExchange()` to wake the exchange loop immediately.

### `internal/transport`

HTTPS client supporting TLS certificate pinning, HTTP/HTTPS proxies, configurable timeouts, and a custom `User-Agent` header. Used by exchange (for full message POST), ping (for ping form POST), and the script handler (for attachment GET).

### `internal/config`

Reads configuration from snapctl and returns a typed `Config` struct. Validates all required fields and parses duration strings. Used by main and the configure hook.

### `internal/persist`

Atomic JSON state file for device identity (secure-id, insecure-id), sequence numbers, accepted-types hash, and per-plugin state. Writes are done via `rename(2)` to prevent partial writes.

### `internal/bpickle`

Encoder/decoder for Landscape's binary pickle wire format. See [Wire Protocol](protocol.md).

### `internal/snapd`

Snapd REST API client. Used by manager handlers (snap install/remove/refresh/service) and monitor plugins (RebootRequired, ComputerInfo, SnapPackages, SnapServices).

## Data Flow

### Startup

```
main() → config.Load()
       → persist.Store{}
       → transport.New()
       → snapd.New()
       → exchange.New() ──── subscribes manager handlers
       → monitor.Runner.Register() ── passes exchange as MessageSink
       → ping.New()
       → goroutine: exchange.Run()
       → goroutine: monitor.Runner.Run()
       → goroutine: ping.Pinger.Run()
       → wait for SIGTERM/SIGINT → cancel context → 5s grace
```

### Outbound (metrics → server)

```
plugin.Run() → exchange.Send(msg)
             [queued in Exchange.pending]
exchange tick → build payload → bpickle.Marshal()
             → transport.Post() → HTTP 200
             → bpickle.Unmarshal(response)
             → store.Update(sequence++)
```

### Inbound (commands → results)

```
exchange.performExchange() → dispatch inbound msg
→ manager.Runner.dispatch()
→ handler.Handle(ctx, msg, resultSink)
→ snapd operation / script exec
→ resultSink.SendResult() or SendResultCode()
→ exchange queues "operation-result" message
→ delivered on next exchange
```

### Ping → urgent exchange

```
ping.doPing() → POST insecure_id
→ response {"messages": true}
→ exchange.TriggerExchange()   ← wakes exchange immediately
```

## State Persistence

The persist store lives at `$SNAP_COMMON/client.state` (default `/var/snap/landscape-client-core/common/client.state`). It is a single JSON file updated atomically via temp-file rename. Fields:

| Field | Purpose |
|---|---|
| `SecureID` | Device identity issued by server on registration |
| `InsecureID` | Public device identifier used for ping |
| `ServerUUID` | Server instance identity |
| `ExchangeToken` | Rotating token for exchange authentication |
| `OutboundSequence` | Count of messages sent so far |
| `NextExpectedFromServer` | ACK for inbound messages |
| `AcceptedTypes` | List of message types the server accepts |
| `AcceptedTypesHash` | MD5 of accepted types (avoids re-sending if unchanged) |
| `PluginState` | Per-plugin state keyed by plugin name |

## Snap Confinement

The snap uses **strict confinement**. Relevant implications:

- All persistent data is under `$SNAP_COMMON`.
- Configuration is read and written via `snapctl`, not flat files.
- Network access is provided by the `network` plug.
- Snap management requires the `snapd-control` plug.
- Hardware introspection uses `system-observe` and `hardware-observe`.
- Shutdown/reboot requires the `shutdown` plug.
- Mount point enumeration requires `mount-observe`.
- User switching in script execution is **not supported** under strict confinement; the `username` field is ignored.
