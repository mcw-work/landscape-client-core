# landscape-client-core

A Go implementation of the Landscape client targeting Ubuntu Core 24 devices.
It is a full replacement for the Python-based `landscape-client` on Ubuntu Core
and is exactly wire-compatible with the Landscape Server (no server-side changes
required).

## Requirements

- Go 1.22 or later
- Ubuntu Core 24 (target platform)

## Building

```bash
make build   # go build ./...
make test    # go test ./...
make vet     # go vet ./...
make lint    # golangci-lint run ./... (requires golangci-lint)
```

## Configuration

The client is configured via `snap set`. Available keys:

| Key | Required | Default | Description |
|-----|----------|---------|-------------|
| `account-name` | yes | — | Landscape account name |
| `registration-key` | no | — | Registration key for the account |
| `computer-title` | yes | — | Display name for this device |
| `url` | yes | — | Landscape message system URL (e.g. `https://landscape.canonical.com/message-system`) |
| `exchange-interval` | no | `15m` | Interval between regular exchanges |
| `urgent-exchange-interval` | no | `1m` | Interval for urgent exchanges |
| `ping-interval` | no | `30s` | Interval between ping messages |
| `ssl-public-key` | no | — | Path to CA certificate for SSL verification |
| `http-proxy` | no | — | HTTP proxy URL |
| `https-proxy` | no | — | HTTPS proxy URL |
| `access-group` | no | — | Landscape access group |
| `tags` | no | — | Comma-separated tags for this device |
| `log-level` | no | `info` | Log level (`debug`, `info`, `warning`, `error`) |

Example:

```
sudo snap set landscape-client-core \
  account-name=my-account \
  computer-title="My Device" \
  url=https://landscape.canonical.com/message-system
```

Note: `registration-key` is optional and only needed if the account requires explicit key authentication.

Configuration is validated by the snap `configure` hook and stored under
`$SNAP_COMMON`.

## Wire Compatibility Tests

The `internal/bpickle` package includes tests (build tag `compat`) that
cross-validate Go bpickle encoding against the Python `landscape-client`
implementation by running Python as a subprocess.

**Prerequisites:**

1. Clone `canonical/landscape-client` alongside this repo:
   ```bash
   git clone https://github.com/canonical/landscape-client ../landscape-client
   ```
2. Ensure Python 3 is available (`python3 --version`). No additional pip
   packages are required — `landscape/lib/bpickle.py` uses only stdlib.

**Run the compat tests:**

```bash
LANDSCAPE_CLIENT_PATH=../landscape-client \
  go test -tags compat -v ./internal/bpickle/...
```

These tests are also run automatically by the
`.github/workflows/compat.yml` CI workflow on every push to `main` and on
pull requests that touch `internal/bpickle/`.

## Building the Snap

**Prerequisites:** [snapcraft](https://snapcraft.io/docs/snapcraft-overview)
installed (`sudo snap install snapcraft --classic`).

**Build locally (destructive mode — fastest, runs directly on the host):**

```bash
snapcraft --destructive-mode
```

This produces `landscape-client-core_<version>_amd64.snap` (or the host
arch) in the current directory.

**Build in a clean LXD container (recommended for release builds):**

```bash
snapcraft
```

Snapcraft will spin up a `core24` LXD container automatically. Requires LXD
to be initialised (`sudo lxd init --auto`).

**Install the built snap on a Core 24 device:**

```bash
sudo snap install landscape-client-core_*.snap --dangerous
```

`--dangerous` is required because the snap is unsigned. Production snaps are
distributed via the Snap Store and install without this flag.

**Validate the snapcraft.yaml without building:**

```bash
snapcraft lint
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
