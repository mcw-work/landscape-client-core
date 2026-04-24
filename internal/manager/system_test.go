package manager_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	h.Shutdown = func(reboot bool) error {
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
	h.Shutdown = func(reboot bool) error {
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
	h.Shutdown = func(_ bool) error {
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
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

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
