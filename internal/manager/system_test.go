package manager_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func (s *funcSink) SendResultCode(_ context.Context, opID int64, status int, _ int64, output string) error {
	return s.fn(opID, status, output)
}

// fakeAttachmentFetcher returns canned bytes for each attachment ID.
type fakeAttachmentFetcher struct {
	data map[int64][]byte
	err  error
}

func (f *fakeAttachmentFetcher) FetchAttachment(_ context.Context, id int64) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.data[id]
	if !ok {
		return nil, fmt.Errorf("fakeAttachmentFetcher: unknown id %d", id)
	}
	return b, nil
}

// ---- ShutdownHandler tests ----

func TestShutdownHandler_Reboot(t *testing.T) {
	var events []string

	sink := &funcSink{fn: func(opID int64, status int, _ string) error {
		events = append(events, fmt.Sprintf("result:%d", status))
		return nil
	}}

	h := manager.NewShutdownHandler()
	h.Shutdown = func(_ context.Context, reboot bool) error {
		if reboot {
			events = append(events, "shutdown:reboot")
		} else {
			events = append(events, "shutdown:poweroff")
		}
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
		"shutdown:reboot",
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
	var gotReboot *bool

	h := manager.NewShutdownHandler()
	h.Shutdown = func(_ context.Context, reboot bool) error {
		gotReboot = &reboot
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
	if gotReboot == nil || *gotReboot != false {
		t.Errorf("Shutdown called with reboot=%v, want false", gotReboot)
	}
}

func TestShutdownHandler_ExecError(t *testing.T) {
	sink := &mockResultSink{}

	h := manager.NewShutdownHandler()
	h.Shutdown = func(_ context.Context, _ bool) error {
		return fmt.Errorf("shutdown failed")
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
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

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
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

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
	t.Parallel()
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

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
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	// Produce 10 MiB of output; limit is 5 MiB.
	msg := exchange.Message{
		"operation-id": int64(13),
		"code":         "yes x | head -c 10485760",
		"time-limit":   int64(30),
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	const limit = 5 * 1024 * 1024
	if len(call.output) == 0 {
		t.Error("output should not be empty")
	}
	// Allow slightly over limit for the truncation marker.
	if len(call.output) > limit+100 {
		t.Errorf("output not truncated: len = %d, want <= %d (5 MiB + marker)", len(call.output), limit+100)
	}
	tail := call.output
	if len(tail) > 100 {
		tail = tail[len(tail)-100:]
	}
	if !strings.Contains(call.output, "**OUTPUT TRUNCATED**") {
		t.Errorf("output missing truncation marker; last 100 bytes: %q", tail)
	}
}

func TestScriptExecHandler_UsernameWarning(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

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
	h := manager.NewScriptExecHandler(snapCommon, nil)

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

func TestScriptExecHandler_Interpreter(t *testing.T) {
	sink := &mockResultSink{}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping interpreter test")
	}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(20),
		"interpreter":  "/usr/bin/python3",
		"code":         "print('hello from python')",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded; output: %s", call.status, call.output)
	}
	if !strings.Contains(call.output, "hello from python") {
		t.Errorf("output = %q, want to contain %q", call.output, "hello from python")
	}
}

func TestScriptExecHandler_DefaultsToSh(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(21),
		"code":         "echo shell_default",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded", call.status)
	}
	if !strings.Contains(call.output, "shell_default") {
		t.Errorf("output = %q, want to contain %q", call.output, "shell_default")
	}
}

func TestScriptExecHandler_BadInterpreter(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(22),
		"interpreter":  "/nonexistent/interpreter",
		"code":         "echo hi",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
}

func TestScriptExecHandler_TimeoutResultCode(t *testing.T) {
	t.Parallel()
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(30),
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
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 102 {
		t.Errorf("resultCode = %d, want 102 (timeout)", call.resultCode)
	}
}

// TestScriptExec_TimeLimitKillsProcessGroup asserts a script that spawns a
// background child cannot outlive its time-limit. exec.CommandContext alone
// kills only the direct child, and the surviving child holds the stdout pipe
// open, which blocks cmd.Run well past the deadline. The backgrounded child
// also leaves no orphan: it would touch a marker file after the WaitDelay if it
// survived, so the marker's absence proves the process group was reaped.
func TestScriptExec_TimeLimitKillsProcessGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "orphan-marker")
	h := manager.NewScriptExecHandler(dir, nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(1),
		"code":         "(sleep 7; touch " + marker + ") & echo started\n",
		"interpreter":  "/bin/sh",
		"time-limit":   int64(1),
	}

	start := time.Now()
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("time-limit not enforced: Handle took %v for a 1s limit", elapsed)
	}

	if len(sink.calls) != 1 {
		t.Fatalf("want 1 result, got %d", len(sink.calls))
	}
	if sink.calls[0].resultCode != 102 {
		t.Errorf("result-code: want 102 (timeout), got %d", sink.calls[0].resultCode)
	}

	// Wait past the child's 7s touch to prove it was killed with its group,
	// not merely detached from the reaped shell. The process group is reaped by
	// cmd.WaitDelay (5s), so the child must outlive that window for the test to
	// distinguish a real group-kill from the child simply finishing first.
	for time.Since(start) < 9*time.Second {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("orphaned background child survived the time-limit: it created its marker after the process group should have been reaped")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestScriptExec_TimeLimitPreservesPartialOutput guards behaviour that is
// already correct and must not regress.
func TestScriptExec_TimeLimitPreservesPartialOutput(t *testing.T) {
	t.Parallel()
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(2),
		"code":         "echo before-timeout; sleep 30\n",
		"interpreter":  "/bin/sh",
		"time-limit":   int64(1),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(sink.calls) != 1 {
		t.Fatalf("want 1 result, got %d", len(sink.calls))
	}
	if sink.calls[0].resultCode != 102 {
		t.Errorf("result-code: want 102, got %d", sink.calls[0].resultCode)
	}
	if !strings.Contains(sink.calls[0].output, "before-timeout") {
		t.Errorf("partial output lost: got %q", sink.calls[0].output)
	}
}

func TestScriptExecHandler_NonZeroExitResultCode(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(31),
		"code":         "exit 1",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 103 {
		t.Errorf("resultCode = %d, want 103 (process failed)", call.resultCode)
	}
}

func TestScriptExecHandler_SuccessNoResultCode(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(32),
		"code":         "echo ok",
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded", call.status)
	}
	if call.resultCode != 0 {
		t.Errorf("resultCode = %d, want 0 (no code on success)", call.resultCode)
	}
}

func TestScriptExecHandler_Attachments(t *testing.T) {
	fetcher := &fakeAttachmentFetcher{
		data: map[int64][]byte{
			int64(7): []byte("attachment content"),
		},
	}
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), fetcher)

	// The script reads $LANDSCAPE_ATTACHMENTS/myfile.txt and echoes its content.
	msg := exchange.Message{
		"operation-id": int64(40),
		"code":         "cat $LANDSCAPE_ATTACHMENTS/myfile.txt",
		"attachments":  map[string]any{"myfile.txt": int64(7)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded; output: %s", call.status, call.output)
	}
	if !strings.Contains(call.output, "attachment content") {
		t.Errorf("output = %q, want to contain %q", call.output, "attachment content")
	}
}

func TestScriptExecHandler_AttachmentFetchFailed(t *testing.T) {
	fetcher := &fakeAttachmentFetcher{
		err: fmt.Errorf("server returned 403"),
	}
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), fetcher)

	msg := exchange.Message{
		"operation-id": int64(41),
		"code":         "echo hi",
		"attachments":  map[string]any{"file.sh": int64(1)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 104 {
		t.Errorf("resultCode = %d, want 104 (attachment fetch failed)", call.resultCode)
	}
}

func TestScriptExecHandler_NoFetcherWithAttachments(t *testing.T) {
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)

	msg := exchange.Message{
		"operation-id": int64(42),
		"code":         "echo hi",
		"attachments":  map[string]any{"file.sh": int64(1)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed", call.status)
	}
	if call.resultCode != 104 {
		t.Errorf("resultCode = %d, want 104", call.resultCode)
	}
}

func TestScriptExecHandler_AttachmentPathTraversal(t *testing.T) {
	fetcher := &fakeAttachmentFetcher{
		data: map[int64][]byte{
			int64(1): []byte("malicious"),
		},
	}
	sink := &mockResultSink{}
	h := manager.NewScriptExecHandler(t.TempDir(), fetcher)

	msg := exchange.Message{
		"operation-id": int64(43),
		"code":         "echo hi",
		"attachments":  map[string]any{"../../etc/cron.d/evil": int64(1)},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want StatusFailed (path traversal should be rejected)", call.status)
	}
	if call.resultCode != 104 {
		t.Errorf("resultCode = %d, want 104", call.resultCode)
	}
}

// TestScriptExec_ExecFailureReportsReason asserts a fork/exec failure sends the
// reason rather than an empty result-text. On Ubuntu Core the Landscape UI may
// be the operator's only feedback channel.
func TestScriptExec_ExecFailureReportsReason(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(1),
		"code":         "echo hi\n",
		"interpreter":  notExecutable,
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.output == "" {
		t.Fatal("result-text was empty: the operator sees a blank failure")
	}
	if !strings.Contains(call.output, "permission denied") {
		t.Errorf("result-text should explain the failure, got %q", call.output)
	}
}

// TestScriptExec_NonZeroExitReportsCode asserts the exit status reaches the
// server alongside the script's own output.
func TestScriptExec_NonZeroExitReportsCode(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(2),
		"code":         "echo to-stdout; exit 42\n",
		"interpreter":  "/bin/sh",
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if !strings.Contains(call.output, "to-stdout") {
		t.Errorf("script output lost: %q", call.output)
	}
	if !strings.Contains(call.output, "42") {
		t.Errorf("exit status 42 not reported: %q", call.output)
	}
}

// TestScriptExec_WhitespaceInterpreter asserts a whitespace-only interpreter is
// treated as absent rather than panicking. strings.Fields returns an empty
// slice for all of these, and the code indexed [0] unguarded.
func TestScriptExec_WhitespaceInterpreter(t *testing.T) {
	tests := []struct {
		name        string
		interpreter string
	}{
		{"empty", ""},
		{"space", " "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"spaces", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := manager.NewScriptExecHandler(t.TempDir(), nil)
			sink := &mockResultSink{}

			msg := exchange.Message{
				"type":         "execute-script",
				"operation-id": int64(1),
				"code":         "echo ran-ok\n",
				"interpreter":  tt.interpreter,
			}

			if err := h.Handle(context.Background(), msg, sink); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			call, ok := sink.lastCall()
			if !ok {
				t.Fatal("no result sent")
			}
			if strings.Contains(call.output, "panic") {
				t.Fatalf("handler panicked; operator sees a Go runtime error: %q", call.output)
			}
			if !strings.Contains(call.output, "ran-ok") {
				t.Errorf("script should have run under the default interpreter, got %q", call.output)
			}
		})
	}
}

// TestScriptExec_DirectoryInterpreter asserts a directory is rejected before
// exec with a specific reason, rather than passing os.Stat and failing later.
func TestScriptExec_DirectoryInterpreter(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{
		"type":         "execute-script",
		"operation-id": int64(2),
		"code":         "echo hi\n",
		"interpreter":  t.TempDir(),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if !strings.Contains(call.output, "not executable") {
		t.Errorf("want an explicit not-executable message, got %q", call.output)
	}
}
