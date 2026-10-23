package runcmd_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/runcmd"
)

func TestRun_ReturnsStdoutOnSuccess(t *testing.T) {
	t.Parallel()

	out, err := runcmd.Run(context.Background(), 5*time.Second, "/bin/echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("stdout: want %q, got %q", "hello", string(out))
	}
}

func TestRun_SurfacesStderr(t *testing.T) {
	t.Parallel()

	_, err := runcmd.Run(context.Background(), 5*time.Second,
		"/bin/sh", "-c", "echo 'DMI probe failed' >&2; exit 1")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "DMI probe failed") {
		t.Errorf("stderr not surfaced in error: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("exit status not surfaced in error: %v", err)
	}
}

func TestRun_DistinguishesNotFound(t *testing.T) {
	t.Parallel()

	_, err := runcmd.Run(context.Background(), 5*time.Second, "definitely-not-a-real-binary-xyz")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "executable not found") {
		t.Errorf("want an explicit not-found message, got: %v", err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return // also acceptable: the sentinel is preserved
	}
}

func TestRun_EnforcesTimeout(t *testing.T) {
	t.Parallel()

	start := time.Now()
	_, err := runcmd.Run(context.Background(), 200*time.Millisecond, "/bin/sh", "-c", "sleep 30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout not enforced: took %v for a 200ms limit", elapsed)
	}
}

func TestRun_UsesCannotConvention(t *testing.T) {
	t.Parallel()

	_, err := runcmd.Run(context.Background(), 5*time.Second, "/bin/false")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "cannot run ") {
		t.Errorf("error should follow snapd's \"cannot ...\" convention, got: %v", err)
	}
}
