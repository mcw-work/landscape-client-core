package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// maxScriptOutputBytes is the maximum number of bytes captured from a script's
// combined stdout+stderr before output is discarded.
const maxScriptOutputBytes = 5 * 1024 * 1024

const truncationMarker = "\n**OUTPUT TRUNCATED**"

// dbusShutdown calls org.freedesktop.login1.Manager Reboot or PowerOff via DBus.
// interactive is passed as false (non-interactive, matches Python client's True
// which means "allow polkit interactive auth" — on Ubuntu Core this is fine either way).
// ctx bounds both the bus connection and the method call: an unresponsive logind
// would otherwise hang this handler indefinitely, and the manager semaphore means
// a wedged handler eventually starves all manager operations.
func dbusShutdown(ctx context.Context, reboot bool) error {
	// godbus v5.2.2 has no ConnectSystemBusWithContext; WithContext binds ctx to the conn.
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")

	method := "org.freedesktop.login1.Manager.PowerOff"
	if reboot {
		method = "org.freedesktop.login1.Manager.Reboot"
	}

	return obj.CallWithContext(ctx, method, 0, false).Store()
}

// ShutdownHandler handles "shutdown" commands.
type ShutdownHandler struct {
	// Shutdown is the function used to trigger a shutdown or reboot.
	// Defaults to dbusShutdown; injectable for testing.
	Shutdown func(ctx context.Context, reboot bool) error
}

// NewShutdownHandler creates a ShutdownHandler with the default DBus executor.
func NewShutdownHandler() *ShutdownHandler {
	return &ShutdownHandler{Shutdown: dbusShutdown}
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

	action := "poweroff"
	if reboot {
		action = "reboot"
	}
	slog.Info("shutdown: requesting via DBus", "action", action)
	dbusCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := h.Shutdown(dbusCtx, reboot); err != nil {
		slog.Error("shutdown: request failed", "action", action, "error", err)
		_ = result.SendResult(ctx, opID, exchange.StatusFailed, err.Error())
	}
	return nil
}

// limitWriter caps combined writes to n bytes total, appends a truncation marker
// once when the cap is reached, then silently discards all subsequent writes.
// It is safe for concurrent use (stdout and stderr copy goroutines).
type limitWriter struct {
	// Kept as a mutex because stdout/stderr are copied concurrently and both the
	// remaining byte budget and truncated flag must stay consistent; see docs/concurrency.md.
	mu        sync.Mutex
	w         io.Writer
	n         int
	truncated bool
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.truncated {
		return len(p), nil
	}
	if len(p) <= lw.n {
		n, err := lw.w.Write(p)
		lw.n -= n
		if err != nil {
			return n, err
		}
		return len(p), nil
	}
	// Write remaining capacity, then append the truncation marker.
	if lw.n > 0 {
		if _, err := lw.w.Write(p[:lw.n]); err != nil {
			return 0, err
		}
		lw.n = 0
	}
	_, _ = lw.w.Write([]byte(truncationMarker))
	lw.truncated = true
	return len(p), nil
}

// AttachmentFetcher downloads attachment content from the Landscape server by ID.
type AttachmentFetcher interface {
	FetchAttachment(ctx context.Context, id int64) ([]byte, error)
}

// ScriptExecHandler handles "execute-script" commands.
type ScriptExecHandler struct {
	snapCommon string
	fetcher    AttachmentFetcher // nil = attachments not supported
	opCtxMgr   *OperationContextManager
}

// NewScriptExecHandler creates a ScriptExecHandler.
// snapCommon is the $SNAP_COMMON directory; use t.TempDir() in tests.
// fetcher may be nil if attachment support is not needed.
func NewScriptExecHandler(snapCommon string, fetcher AttachmentFetcher) *ScriptExecHandler {
	return &ScriptExecHandler{snapCommon: snapCommon, fetcher: fetcher}
}

// SetOperationContextManager configures operation cancellation tracking.
func (h *ScriptExecHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *ScriptExecHandler) MessageType() string { return "execute-script" }

func (h *ScriptExecHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	code, err := getString(msg, "code")
	if err != nil {
		return err
	}

	// Whitespace-only is absent: strings.Fields returns an empty slice for " ",
	// "\t" and "\n", which the == "" check does not catch.
	interpreter, _ := getString(msg, "interpreter")
	if strings.TrimSpace(interpreter) == "" {
		interpreter = "/bin/sh"
	}

	// Split interpreter into binary path and optional arguments (e.g. "/usr/bin/env python3").
	interpreterFields := strings.Fields(interpreter)
	if len(interpreterFields) == 0 {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			"execute-script: cannot determine interpreter")
		return nil
	}
	interpreterBin := interpreterFields[0]
	interpreterArgs := interpreterFields[1:]

	// username switching is unsupported under strict confinement — log and ignore.
	if username, _ := getString(msg, "username"); username != "" {
		slog.Warn("execute-script: username switching not supported under strict confinement, ignoring username", "username", username)
	}

	// os.Stat passes for directories and non-executable files, which then fail
	// later at fork/exec with a less specific message.
	if fi, err := os.Stat(interpreterBin); err != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			fmt.Sprintf("execute-script: interpreter not found: %s", interpreterBin))
		return nil
	} else if fi.IsDir() {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			fmt.Sprintf("execute-script: interpreter %s is not executable: is a directory", interpreterBin))
		return nil
	}
	if err := unix.Access(interpreterBin, unix.X_OK); err != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103,
			fmt.Sprintf("execute-script: interpreter %s is not executable: %v", interpreterBin, err))
		return nil
	}

	// time-limit of 0 means no limit.
	timeLimit, _ := getInt64(msg, "time-limit")

	// Create per-operation script directory.
	scriptDir := filepath.Join(h.snapCommon, "scripts", fmt.Sprintf("%d", opID))
	if err := os.MkdirAll(scriptDir, 0700); err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(scriptDir)
	}()

	// Write script with shebang.
	scriptPath := filepath.Join(scriptDir, "script")
	scriptContent := "#!" + interpreter + "\n" + code
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0700); err != nil {
		return err
	}

	// Download attachments if present.
	attachments := getAttachments(msg)
	if len(attachments) > 0 {
		if h.fetcher == nil {
			_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 104,
				"execute-script: attachment fetching not configured")
			return nil
		}
		for filename, attachID := range attachments {
			data, err := h.fetcher.FetchAttachment(operationCtx, attachID)
			if err != nil {
				_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 104,
					fmt.Sprintf("execute-script: fetching attachment %q: %v", filename, err))
				return nil
			}
			destPath := filepath.Join(scriptDir, filename)
			// Guard against path traversal: ensure destPath is within scriptDir.
			cleanDest := filepath.Clean(destPath)
			if !strings.HasPrefix(cleanDest+string(os.PathSeparator), filepath.Clean(scriptDir)+string(os.PathSeparator)) {
				_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 104,
					fmt.Sprintf("execute-script: attachment filename %q is invalid", filename))
				return nil
			}
			if err := os.WriteFile(destPath, data, 0600); err != nil {
				return err
			}
		}
	}

	// Build command environment.
	var cmdEnv []string
	if len(attachments) > 0 {
		cmdEnv = append(os.Environ(), "LANDSCAPE_ATTACHMENTS="+scriptDir)
	}

	// Build execution context.
	execCtx := operationCtx
	if timeLimit > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(operationCtx, time.Duration(timeLimit)*time.Second)
		defer cancel()
	}

	// Run the script.
	slog.Info("execute-script: running", "op", opID, "interpreter", interpreter, "script", scriptPath, "time_limit", timeLimit)
	cmd := exec.CommandContext(execCtx, interpreterBin, append(interpreterArgs, scriptPath)...)
	// Run the script in its own process group so a timeout kills grandchildren
	// too. Without this they survive as orphans holding the stdout pipe, which
	// blocks cmd.Wait indefinitely — the pipe exists because Stdout is an
	// io.Writer rather than an *os.File.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Bound Wait even if something still holds the pipe open.
	cmd.WaitDelay = 5 * time.Second
	if len(cmdEnv) > 0 {
		cmd.Env = cmdEnv
	}
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: maxScriptOutputBytes} // 5 MiB shared cap for stdout+stderr
	cmd.Stdout = lw
	cmd.Stderr = lw

	runErr := cmd.Run()
	// Reap the whole process group. cmd.Cancel and WaitDelay only target the
	// direct child, and os/exec's context watcher stops once the leader is
	// reaped — so a script that backgrounds a process and exits immediately
	// (e.g. "sleep 30 &") leaves that grandchild orphaned, holding the snap's
	// descriptors. Signalling the negative pgid sweeps any survivor. ESRCH
	// (group already gone) is expected and ignored.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Sanitize output to valid UTF-8, replacing invalid bytes with the Unicode
	// replacement character — matching the Python client's
	// data.decode("utf-8", "replace") behaviour. This ensures the bpickle u-type
	// field in the operation-result message is always valid UTF-8 so the
	// Landscape server can parse the exchange payload without error.
	output := strings.ToValidUTF8(buf.String(), string(utf8.RuneError))
	slog.Info("execute-script: run complete", "op", opID, "error", runErr, "output_bytes", len(output))

	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 102, output)
		return nil
	}
	if runErr != nil {
		text := output
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			// The script ran and failed: keep its output, append the exit status
			// so the operator can tell 42 from 1.
			text = fmt.Sprintf("%s\nexit status %d", output, exitErr.ExitCode())
		default:
			// The interpreter could not be executed at all, so there is no script
			// output to report — without this the Landscape UI shows a blank
			// failure with no explanation.
			text = fmt.Sprintf("execute-script: cannot run interpreter %s: %v", interpreterBin, runErr)
		}
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103, text)
		return nil
	}

	_ = result.SendResult(ctx, opID, exchange.StatusSucceeded, output)
	return nil
}
