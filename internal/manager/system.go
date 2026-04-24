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
	"strings"
	"sync"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// maxScriptOutputBytes is the maximum number of bytes captured from a script's
// combined stdout+stderr before output is discarded.
const maxScriptOutputBytes = 5 * 1024 * 1024

const truncationMarker = "\n**OUTPUT TRUNCATED**"

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

// limitWriter caps combined writes to n bytes total, appends a truncation marker
// once when the cap is reached, then silently discards all subsequent writes.
// It is safe for concurrent use (stdout and stderr copy goroutines).
type limitWriter struct {
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
}

// NewScriptExecHandler creates a ScriptExecHandler.
// snapCommon is the $SNAP_COMMON directory; use t.TempDir() in tests.
// fetcher may be nil if attachment support is not needed.
func NewScriptExecHandler(snapCommon string, fetcher AttachmentFetcher) *ScriptExecHandler {
	return &ScriptExecHandler{snapCommon: snapCommon, fetcher: fetcher}
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

	// interpreter defaults to /bin/sh if not provided.
	interpreter, _ := getString(msg, "interpreter")
	if interpreter == "" {
		interpreter = "/bin/sh"
	}

	// Split interpreter into binary path and optional arguments (e.g. "/usr/bin/env python3").
	interpreterFields := strings.Fields(interpreter)
	interpreterBin := interpreterFields[0]
	interpreterArgs := interpreterFields[1:]

	// username switching is unsupported under strict confinement — log and ignore.
	if username, _ := getString(msg, "username"); username != "" {
		log.Printf("execute-script: username switching not supported under strict confinement, ignoring username %q", username)
	}

	// Verify interpreter binary exists.
	if _, err := os.Stat(interpreterBin); err != nil {
		_ = result.SendResult(ctx, opID, exchange.StatusFailed,
			fmt.Sprintf("execute-script: interpreter not found: %s", interpreterBin))
		return nil
	}

	// time-limit of 0 means no limit.
	timeLimit, _ := getInt64(msg, "time-limit")

	// Create per-operation script directory.
	scriptDir := filepath.Join(h.snapCommon, "scripts", fmt.Sprintf("%d", opID))
	if err := os.MkdirAll(scriptDir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(scriptDir)

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
			data, err := h.fetcher.FetchAttachment(ctx, attachID)
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
	execCtx := ctx
	if timeLimit > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeLimit)*time.Second)
		defer cancel()
	}

	// Run the script.
	cmd := exec.CommandContext(execCtx, interpreterBin, append(interpreterArgs, scriptPath)...)
	if len(cmdEnv) > 0 {
		cmd.Env = cmdEnv
	}
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: maxScriptOutputBytes} // 5 MiB shared cap for stdout+stderr
	cmd.Stdout = lw
	cmd.Stderr = lw

	runErr := cmd.Run()
	output := buf.String()

	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 102, output)
		return nil
	}
	if runErr != nil {
		_ = result.SendResultCode(ctx, opID, exchange.StatusFailed, 103, output)
		return nil
	}

	_ = result.SendResult(ctx, opID, exchange.StatusSucceeded, output)
	return nil
}
