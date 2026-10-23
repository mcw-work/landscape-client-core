// Package runcmd runs external executables with consistent timeout and error
// handling. Every exec site in the daemon routes through it: .Output() captures
// stderr into ExitError.Stderr, but %v prints only "exit status N", so failures
// were previously logged without the one piece of information that explains them.
package runcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Run executes name with args, bounded by timeout, and returns its stdout.
// A zero timeout means no per-run bound beyond ctx.
//
// The command runs in its own process group so a timeout also kills
// grandchildren, which would otherwise survive holding the stdout pipe and
// block Wait.
func Run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("cannot run %s: executable not found", name)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cannot run %s: %w", name, ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("cannot run %s: %w: %s", name, err, bytes.TrimSpace(ee.Stderr))
	}
	return nil, fmt.Errorf("cannot run %s: %w", name, err)
}
