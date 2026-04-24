# Configuration

## Configuration Keys

Configuration is stored as snap configuration and managed via `snapctl`. All keys use the prefix `landscape-client-core:` when accessed externally (e.g. `snap get landscape-client-core account-name`).

### Required Keys

All four required keys must be set before the daemon will start.

| Key | Description | Example |
|---|---|---|
| `account-name` | Landscape account name | `my-org` |
| `registration-key` | Registration key for the account (shared secret) | `abc123` |
| `computer-title` | Display name for this device in the Landscape dashboard | `"Factory Floor Pi #42"` |
| `url` | Landscape message system URL | `https://landscape.canonical.com/message-system` |

### Optional Keys

| Key | Default | Description |
|---|---|---|
| `exchange-interval` | `15m` | Interval between regular exchanges with the server |
| `urgent-exchange-interval` | `1m` | Interval used when messages are pending or the device is unregistered |
| `ping-interval` | `30s` | Interval between lightweight ping polls |
| `ping-url` | *(derived)* | Ping endpoint URL. If empty, derived from `url` (e.g. `https://host/message-system` → `http://host/ping`) |
| `ssl-public-key` | — | Path to a CA certificate file for TLS verification (replaces system CAs) |
| `http-proxy` | — | HTTP proxy URL (e.g. `http://proxy.example.com:3128`) |
| `https-proxy` | — | HTTPS proxy URL (e.g. `https://proxy.example.com:3128`) |
| `access-group` | — | Landscape access group for this device |
| `tags` | — | Comma-separated tags (e.g. `production,arm64`) |
| `log-level` | `info` | Log verbosity: `debug`, `info`, `warning`, or `error` |

### Duration Format

Duration values (`exchange-interval`, `urgent-exchange-interval`, `ping-interval`) use Go's duration string format:

- `30s` — 30 seconds
- `5m` — 5 minutes
- `1h30m` — 1 hour 30 minutes

## Setting Configuration

### Via snap set

```bash
sudo snap set landscape-client-core \
  account-name=my-account \
  registration-key=my-key \
  computer-title="My Device" \
  url=https://landscape.canonical.com/message-system
```

Multiple keys can be set in a single command. The configure hook validates the configuration after each `snap set` call.

### Via the Interactive Wizard

The `config` command provides a step-by-step interactive wizard:

```bash
landscape-client-core.config
```

The wizard:

1. Asks whether you're using Canonical Hosted Landscape or a self-hosted server.
2. Prompts for computer title, account name, and registration key (password-masked).
3. Optionally collects HTTP/HTTPS proxy URLs, access group, and tags.
4. Displays a summary for review.
5. Writes all settings in a single batched `snapctl set` call.
6. Restarts the daemon.

### Reading Current Configuration

```bash
snap get landscape-client-core
```

Or for a specific key:

```bash
snap get landscape-client-core account-name
```

## Configure Hook Validation

The snap `configure` hook runs `landscape-client-core --validate-config` on every `snap set` call. Validation behaviour:

- **0 required keys set** (fresh install): passes silently — allows partial configuration.
- **1–2 required keys set**: passes — still in-progress configuration.
- **3+ required keys set**: full validation is run; all required keys must be present and valid.
- **All 4 required keys set**: full validation; durations are parsed and URL format is checked.

This means you can set keys incrementally without errors until you have all required keys present.

## Configuration File Location

Configuration is managed entirely through snapctl and the snap configuration system. The raw values are stored by snapd, not in a user-visible file. Derived runtime state (device IDs, sequence numbers) is stored separately in:

```
/var/snap/landscape-client-core/common/client.state
```

This file is JSON and is written atomically. Do not edit it manually.

## Self-Hosted Landscape

For a self-hosted Landscape installation:

```bash
sudo snap set landscape-client-core \
  account-name=standalone \
  registration-key="" \
  computer-title="My Server" \
  url=https://my-landscape.example.com/message-system \
  ssl-public-key=/var/snap/landscape-client-core/common/my-ca.crt
```

If your server uses a self-signed or private CA certificate, copy the certificate to `$SNAP_COMMON` and set `ssl-public-key` to its path.

## Proxy Configuration

```bash
sudo snap set landscape-client-core \
  https-proxy=https://proxy.example.com:3128
```

The client uses the `https-proxy` for HTTPS URLs and `http-proxy` for HTTP URLs. If neither is set, `HTTP_PROXY`/`HTTPS_PROXY` environment variables are honoured.

## Tag Format

Tags must match the pattern `[a-zA-Z0-9][a-zA-Z0-9-]*` (alphanumeric, hyphens allowed after the first character). Multiple tags are comma-separated with no spaces:

```bash
sudo snap set landscape-client-core tags=production,arm64,factory-floor
```
