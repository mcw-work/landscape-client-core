package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// Executor runs an external command. Injectable for testing.
type Executor func(ctx context.Context, name string, args ...string) error

func defaultExecutor(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// ShutdownHandler handles "shutdown" commands.
type ShutdownHandler struct {
	Exec Executor
}

// NewShutdownHandler creates a ShutdownHandler with the default executor.
func NewShutdownHandler() *ShutdownHandler {
	return &ShutdownHandler{Exec: defaultExecutor}
}

func (h *ShutdownHandler) MessageType() string { return "shutdown" }

func (h *ShutdownHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}
	reboot := getBool(msg, "reboot")

	// Send result BEFORE executing the command — reboot will kill the process.
	if err := result.SendResult(ctx, opID, exchange.StatusSucceeded, ""); err != nil {
		return err
	}

	subcmd := "poweroff"
	if reboot {
		subcmd = "reboot"
	}

	if err := h.Exec(ctx, "systemctl", subcmd); err != nil {
		_ = result.SendResult(ctx, opID, exchange.StatusFailed, err.Error())
	}
	return nil
}

// limitWriter caps combined writes to n bytes total, silently discarding the
// rest. It is safe for concurrent use (stdout and stderr copy goroutines).
type limitWriter struct {
	mu sync.Mutex
	w  io.Writer
	n  int
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.n <= 0 {
		return len(p), nil
	}
	toWrite := p
	if len(toWrite) > lw.n {
		toWrite = p[:lw.n]
	}
	n, err := lw.w.Write(toWrite)
	lw.n -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// ScriptExecHandler handles "execute-script" commands.
type ScriptExecHandler struct {
	snapCommon string
}

// NewScriptExecHandler creates a ScriptExecHandler.
// snapCommon is the $SNAP_COMMON directory; use t.TempDir() in tests.
func NewScriptExecHandler(snapCommon string) *ScriptExecHandler {
	return &ScriptExecHandler{snapCommon: snapCommon}
}

func (h *ScriptExecHandler) MessageType() string { return "execute-script" }

func (h *ScriptExecHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}
	code, err := getString(msg, "code")
	if err != nil {
		return err
	}

	// username switching is unsupported under strict confinement — log and ignore.
	if username, _ := getString(msg, "username"); username != "" {
		log.Printf("execute-script: username switching not supported under strict confinement, ignoring username %q", username)
	}

	// time-limit of 0 means no limit.
	timeLimit, _ := getInt64(msg, "time-limit")

	// Create per-operation script directory.
	scriptDir := filepath.Join(h.snapCommon, "scripts", fmt.Sprintf("%d", opID))
	if err := os.MkdirAll(scriptDir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(scriptDir)

	scriptPath := filepath.Join(scriptDir, "script.sh")
	if err := os.WriteFile(scriptPath, []byte(code), 0700); err != nil {
		return err
	}

	// Build execution context.
	execCtx := ctx
	if timeLimit > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeLimit)*time.Second)
		defer cancel()
	}

	// Run the script via sh. Use os/exec directly (not the injectable Executor).
	cmd := exec.CommandContext(execCtx, "sh", scriptPath)
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: 1 << 20} // 1 MiB shared cap for stdout+stderr
	cmd.Stdout = lw
	cmd.Stderr = lw

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			if exitCode == 0 {
				// Killed by signal reports ExitCode -1, but guard for safety.
				exitCode = 1
			}
		} else {
			exitCode = 1
		}
	}

	status := exchange.StatusSucceeded
	if exitCode != 0 {
		status = exchange.StatusFailed
	}

	_ = result.SendResult(ctx, opID, status, buf.String())
	return nil
}
