package manager_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/manager"
)

// funcSink calls a function on each SendResult — used for ordering assertions.
type funcSink struct {
	fn func(opID int64, status int, output string) error
}

func (s *funcSink) SendResult(_ context.Context, opID int64, status int, output string) error {
	return s.fn(opID, status, output)
}

// ---- ShutdownHandler tests ----

func TestShutdownHandler_Reboot(t *testing.T) {
	var events []string

	sink := &funcSink{fn: func(opID int64, status int, _ string) error {
		events = append(events, fmt.Sprintf("result:%d", status))
		return nil
	}}

	h := manager.NewShutdownHandler()
	h.Exec = func(_ context.Context, name string, args ...string) error {
		events = append(events, "exec:"+name+":"+strings.Join(args, ","))
		return nil
	}

	msg := exchange.Message{
		"operation-id": int64(1),
		"reboot":       true,
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	want := []string{
		fmt.Sprintf("result:%d", exchange.StatusSucceeded),
		"exec:systemctl:reboot",
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestShutdownHandler_Poweroff(t *testing.T) {
	sink := &mockResultSink{}
	var gotName string
	var gotArgs []string

	h := manager.NewShutdownHandler()
	h.Exec = func(_ context.Context, name string, args ...string) error {
		gotName = name
		gotArgs = args
		return nil
	}

	msg := exchange.Message{
		"operation-id": int64(2),
		"reboot":       false,
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded (%d)", call.status, exchange.StatusSucceeded)
	}
	if gotName != "systemctl" || len(gotArgs) != 1 || gotArgs[0] != "poweroff" {
		t.Errorf("exec called with %q %v, want %q %v", gotName, gotArgs, "systemctl", []string{"poweroff"})
	}
}

func TestShutdownHandler_ExecError(t *testing.T) {
	sink := &mockResultSink{}

	h := manager.NewShutdownHandler()
	h.Exec = func(_ context.Context, _ string, _ ...string) error {
		return fmt.Errorf("systemctl failed")
	}

	msg := exchange.Message{
		"operation-id": int64(3),
		"reboot":       false,
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// First result: StatusSucceeded (sent before exec); second: StatusFailed (on error).
	if len(sink.calls) < 2 {
		t.Fatalf("expected at least 2 results, got %d: %v", len(sink.calls), sink.calls)
	}
	if sink.calls[0].status != exchange.StatusSucceeded {
		t.Errorf("calls[0].status = %d, want StatusSucceeded (%d)", sink.calls[0].status, exchange.StatusSucceeded)
	}
	if sink.calls[len(sink.calls)-1].status != exchange.StatusFailed {
		t.Errorf("calls[last].status = %d, want StatusFailed (%d)", sink.calls[len(sink.calls)-1].status, exchange.StatusFailed)
	}
}

// ---- ScriptExecHandler tests ----

func TestScriptExecHandler_Success(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir())

	msg := exchange.Message{
		"operation-id": int64(10),
		"code":         "echo hello",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded (%d)", call.status, exchange.StatusSucceeded)
	}
	if !strings.Contains(call.output, "hello") {
		t.Errorf("output = %q, want to contain %q", call.output, "hello")
	}
}

func TestScriptExecHandler_Failure(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir())

	msg := exchange.Message{
		"operation-id": int64(11),
		"code":         "echo fail_output; exit 1",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed (%d)", call.status, exchange.StatusFailed)
	}
	if !strings.Contains(call.output, "fail_output") {
		t.Errorf("output = %q, want to contain %q", call.output, "fail_output")
	}
}

func TestScriptExecHandler_TimeLimit(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir())

	msg := exchange.Message{
		"operation-id": int64(12),
		"code":         "sleep 10",
		"time-limit":   int64(1),
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed (script should have been killed by timeout)", call.status)
	}
}

func TestScriptExecHandler_OutputTruncated(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir())

	// Produce 2 MiB of output; limit is 1 MiB.
	msg := exchange.Message{
		"operation-id": int64(13),
		"code":         "yes x | head -c 2097152",
		"time-limit":   int64(30),
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if len(call.output) == 0 {
		t.Error("output should not be empty")
	}
	if len(call.output) > 1<<20 {
		t.Errorf("output not truncated: len = %d, want <= %d (1 MiB)", len(call.output), 1<<20)
	}
}

func TestScriptExecHandler_UsernameWarning(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir())

	msg := exchange.Message{
		"operation-id": int64(14),
		"code":         "echo ok",
		"username":     "someuser",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded (%d)", call.status, exchange.StatusSucceeded)
	}
	if !strings.Contains(logBuf.String(), "someuser") {
		t.Errorf("expected username warning in log output, got: %s", logBuf.String())
	}
}

func TestScriptExecHandler_Cleanup(t *testing.T) {
	snapCommon := t.TempDir()
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(snapCommon)

	msg := exchange.Message{
		"operation-id": int64(15),
		"code":         "echo cleanup",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	scriptDir := filepath.Join(snapCommon, "scripts", "15")
	if _, err := os.Stat(scriptDir); !os.IsNotExist(err) {
		t.Errorf("script directory %s should have been removed after execution", scriptDir)
	}
}
