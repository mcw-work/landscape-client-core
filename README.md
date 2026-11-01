# landscape-client-core

A Go implementation of the Landscape client for Ubuntu Core 26. It replaces the
Python `landscape-client` on Ubuntu Core and is wire-compatible with the
existing Landscape server.

The client registers the device with Landscape, reports system and snap
telemetry, and runs management commands sent from the Landscape dashboard
(snap install/remove/refresh, service control, reboot, scripts).

## Quick start

```bash
sudo snap install landscape-client-core
sudo landscape-client-core.config          # interactive wizard
# or
sudo snap set landscape-client-core \
  account-name=my-account \
  computer-title="My Device" \
  url=https://landscape.canonical.com/message-system
sudo snap logs -f landscape-client-core
```

The daemon stays disabled until `url`, `account-name` and `computer-title` are
set. See [Configuration](docs/configuration.md) for every key.

## Documentation

Start with [Architecture](docs/architecture.md); the full index is in
[docs/README.md](docs/README.md).

## Development

Requires Go 1.25 or later.

```bash
make build   # go build ./...
make test    # go test ./...
make vet     # go vet ./...
make lint    # golangci-lint run ./...
```

### Wire-compatibility tests

Tests with the `compat` build tag cross-check the Go bpickle codec against the
Python implementation. They need a checkout of `canonical/landscape-client` and
`python3` (stdlib only):

```bash
git clone https://github.com/canonical/landscape-client ../landscape-client
make compat LANDSCAPE_CLIENT_PATH=../landscape-client
```

CI runs these on pushes to `main` and on pull requests that touch
`internal/bpickle/` ([compat.yml](.github/workflows/compat.yml)).

### Building the snap

```bash
snapcraft                     # clean build in an LXD container
snapcraft --destructive-mode  # faster, builds on the host (make snap)
sudo snap install landscape-client-core_*.snap --dangerous
```

`--dangerous` is needed for locally built, unsigned snaps.

## License

GPL-2.0-only, matching the Python `landscape-client`. See [LICENSE](LICENSE).
