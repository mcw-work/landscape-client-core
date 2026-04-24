.PHONY: build test vet lint compat snap

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run ./...

compat:
	@test -n "$(LANDSCAPE_CLIENT_PATH)" || { echo "Set LANDSCAPE_CLIENT_PATH to the canonical/landscape-client checkout"; exit 1; }
	LANDSCAPE_CLIENT_PATH=$(LANDSCAPE_CLIENT_PATH) go test -tags compat -v ./internal/bpickle/...

snap:
	snapcraft --destructive-mode
