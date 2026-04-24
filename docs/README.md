# landscape-client-core Documentation

`landscape-client-core` is the Go implementation of the Landscape management client for Ubuntu Core 24 devices. It is fully wire-compatible with the Landscape server and replaces the Python-based `landscape-client` on Ubuntu Core.

## Documentation Index

| Document | Description |
|---|---|
| [Architecture](architecture.md) | Component overview, data flow, and package relationships |
| [Configuration](configuration.md) | All configuration keys, defaults, validation, and the interactive wizard |
| [Monitoring Plugins](monitoring-plugins.md) | Every system metric plugin and what it reports |
| [Management Commands](management-commands.md) | Inbound commands the client handles (snap, service, shutdown) |
| [Remote Script Execution](remote-script-execution.md) | Detailed reference for the `execute-script` handler |
| [Wire Protocol](protocol.md) | bpickle encoding, exchange protocol, and message formats |

## Quick Start

### Install

```bash
sudo snap install landscape-client-core
```

### Configure

Use the interactive wizard:

```bash
landscape-client-core.config
```

Or set configuration keys directly:

```bash
sudo snap set landscape-client-core \
  account-name=my-account \
  registration-key=my-key \
  computer-title="My Device" \
  url=https://landscape.canonical.com/message-system
```

### Status

The client runs as a background daemon. Check its status with:

```bash
sudo snap services landscape-client-core
```

View logs:

```bash
sudo snap logs landscape-client-core -f
```

## What It Does

Once configured and running the client:

1. **Registers** with the Landscape server using the account name and registration key.
2. **Collects** system metrics (CPU, memory, network, snap packages, hardware info, etc.) and reports them to the server on each exchange.
3. **Listens** for management commands from the Landscape dashboard (install/remove snaps, manage services, reboot, execute scripts).
4. **Executes** received commands and reports results back to the server.
5. **Pings** the server periodically to detect pending messages between full exchanges.
