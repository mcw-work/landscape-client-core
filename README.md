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

The client is configured via `snap set`:

```bash
# Set the Landscape server URL
sudo snap set landscape-client-core url="https://landscape.example.com"

# Set the account name
sudo snap set landscape-client-core account-name="my-account"

# Set the registration key (if required)
sudo snap set landscape-client-core registration-key="my-key"
```

Configuration is validated by the snap `configure` hook and stored under
`$SNAP_COMMON`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
