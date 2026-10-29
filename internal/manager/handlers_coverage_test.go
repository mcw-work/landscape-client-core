package manager_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/manager"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// blockingSnapd blocks in WaitForChange and service calls until ctx is done,
// signalling started first, so tests can cancel an in-flight operation.
type blockingSnapd struct {
	*snapd.MockClient
	started chan struct{}
}

func newBlockingSnapd() *blockingSnapd {
	return &blockingSnapd{MockClient: &snapd.MockClient{ChangeID: "1"}, started: make(chan struct{}, 1)}
}

func (b *blockingSnapd) block(ctx context.Context) error {
	b.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingSnapd) WaitForChange(ctx context.Context, _ string) error { return b.block(ctx) }
func (b *blockingSnapd) StartService(ctx context.Context, _, _ string) error {
	return b.block(ctx)
}
func (b *blockingSnapd) StopService(ctx context.Context, _, _ string) error {
	return b.block(ctx)
}
func (b *blockingSnapd) RestartService(ctx context.Context, _, _ string) error {
	return b.block(ctx)
}

// optsSnapd records InstallSnap options.
type optsSnapd struct {
	*snapd.MockClient
	opts []snapd.InstallOptions
}

func (o *optsSnapd) InstallSnap(ctx context.Context, name string, opts snapd.InstallOptions) (string, error) {
	o.opts = append(o.opts, opts)
	return o.MockClient.InstallSnap(ctx, name, opts)
}

type cancellableHandler interface {
	manager.Handler
	SetOperationContextManager(*manager.OperationContextManager)
}

type snapListHandlerCase struct {
	name  string
	make  func(c snapd.Client, onComplete func()) cancellableHandler
	calls func(mc *snapd.MockClient) []string
}

var snapListHandlers = []snapListHandlerCase{
	{
		name: "install",
		make: func(c snapd.Client, f func()) cancellableHandler {
			return &manager.InstallSnapHandler{Snapd: c, OnComplete: f}
		},
		calls: func(mc *snapd.MockClient) []string { return mc.InstallCalls },
	},
	{
		name: "remove",
		make: func(c snapd.Client, f func()) cancellableHandler {
			return &manager.RemoveSnapHandler{Snapd: c, OnComplete: f}
		},
		calls: func(mc *snapd.MockClient) []string { return mc.RemoveCalls },
	},
	{
		name: "refresh",
		make: func(c snapd.Client, f func()) cancellableHandler {
			return &manager.RefreshSnapHandler{Snapd: c, OnComplete: f}
		},
		calls: func(mc *snapd.MockClient) []string { return mc.RefreshCalls },
	},
}

func TestSnapListHandlers_MissingOperationID(t *testing.T) {
	for _, hc := range snapListHandlers {
		t.Run(hc.name, func(t *testing.T) {
			mc := &snapd.MockClient{ChangeID: "1"}
			sink := &mockResultSink{}
			h := hc.make(mc, nil)
			msg := exchange.Message{"snaps": []any{map[string]any{"name": "hello"}}}
			if err := h.Handle(context.Background(), msg, sink); err == nil {
				t.Error("expected error for missing operation-id")
			}
			if len(sink.calls) != 0 || len(hc.calls(mc)) != 0 {
				t.Errorf("no result or snapd call expected, got results=%v calls=%v", sink.calls, hc.calls(mc))
			}
		})
	}
}

func TestSnapListHandlers_InvalidSnaps(t *testing.T) {
	cases := []struct {
		name    string
		snaps   any
		present bool
		wantErr string
	}{
		{"missing", nil, false, "missing required field"},
		{"not a list", "hello", true, "expected list"},
		{"entry not dict", []any{"hello"}, true, "expected dict"},
		{"entry missing name", []any{map[string]any{"args": map[string]any{}}}, true, "missing \"name\""},
		{"entry name wrong type", []any{map[string]any{"name": int64(3)}}, true, "missing \"name\""},
	}
	for _, hc := range snapListHandlers {
		for _, tc := range cases {
			t.Run(hc.name+"/"+tc.name, func(t *testing.T) {
				mc := &snapd.MockClient{ChangeID: "1"}
				sink := &mockResultSink{}
				completed := false
				h := hc.make(mc, func() { completed = true })
				msg := exchange.Message{"operation-id": int64(7)}
				if tc.present {
					msg["snaps"] = tc.snaps
				}
				if err := h.Handle(context.Background(), msg, sink); err != nil {
					t.Fatalf("Handle: %v", err)
				}
				call, ok := sink.lastCall()
				if !ok {
					t.Fatal("no result sent")
				}
				if call.opID != 7 || call.status != exchange.StatusFailed {
					t.Errorf("result = %+v, want op 7 StatusFailed", call)
				}
				if !strings.Contains(call.output, tc.wantErr) {
					t.Errorf("output = %q, want containing %q", call.output, tc.wantErr)
				}
				if got := hc.calls(mc); len(got) != 0 {
					t.Errorf("snapd calls = %v, want none", got)
				}
				if completed {
					t.Error("OnComplete called on failure")
				}
			})
		}
	}
}

// TestSnapListHandlers_StopsAtFirstFailure verifies a failing first snap
// prevents the second from being processed.
func TestSnapListHandlers_StopsAtFirstFailure(t *testing.T) {
	for _, hc := range snapListHandlers {
		t.Run(hc.name, func(t *testing.T) {
			mc := &snapd.MockClient{ChangeID: "1", ChangeErr: errors.New("change 1 failed")}
			sink := &mockResultSink{}
			h := hc.make(mc, nil)
			msg := exchange.Message{
				"operation-id": int64(8),
				"snaps":        []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
			}
			if err := h.Handle(context.Background(), msg, sink); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := hc.calls(mc); len(got) != 1 || got[0] != "a" {
				t.Errorf("snapd calls = %v, want [a]", got)
			}
			call, _ := sink.lastCall()
			if call.status != exchange.StatusFailed || call.output != "change 1 failed" {
				t.Errorf("result = %+v, want StatusFailed %q", call, "change 1 failed")
			}
			if len(sink.calls) != 1 {
				t.Errorf("results = %d, want exactly 1", len(sink.calls))
			}
		})
	}
}

func TestSnapListHandlers_CancelledViaOperationContextManager(t *testing.T) {
	for _, hc := range snapListHandlers {
		t.Run(hc.name, func(t *testing.T) {
			bs := newBlockingSnapd()
			sink := &mockResultSink{}
			completed := false
			h := hc.make(bs, func() { completed = true })
			mgr := manager.NewOperationContextManager()
			h.SetOperationContextManager(mgr)

			done := make(chan error, 1)
			go func() {
				done <- h.Handle(context.Background(), exchange.Message{
					"operation-id": int64(9),
					"snaps":        []any{map[string]any{"name": "hello"}},
				}, sink)
			}()

			select {
			case <-bs.started:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not reach WaitForChange")
			}
			mgr.Cancel(9)

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not return after cancellation")
			}

			call, ok := sink.lastCall()
			if !ok {
				t.Fatal("no result sent")
			}
			if call.opID != 9 || call.status != exchange.StatusFailed {
				t.Errorf("result = %+v, want op 9 StatusFailed", call)
			}
			if !strings.Contains(call.output, context.Canceled.Error()) {
				t.Errorf("output = %q, want context canceled", call.output)
			}
			if completed {
				t.Error("OnComplete called after cancellation")
			}
		})
	}
}

func TestInstallSnapHandler_PassesChannelAndClassic(t *testing.T) {
	oc := &optsSnapd{MockClient: &snapd.MockClient{ChangeID: "1"}}
	sink := &mockResultSink{}
	h := &manager.InstallSnapHandler{Snapd: oc}
	msg := exchange.Message{
		"operation-id": int64(10),
		"snaps": []any{
			map[string]any{"name": "a", "args": map[string]any{"channel": "edge", "classic": true}},
			map[string]any{"name": "b"},
		},
	}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(oc.opts) != 2 {
		t.Fatalf("InstallSnap calls = %d, want 2", len(oc.opts))
	}
	if oc.opts[0] != (snapd.InstallOptions{Channel: "edge", Classic: true}) {
		t.Errorf("opts[0] = %+v, want edge/classic", oc.opts[0])
	}
	if oc.opts[1] != (snapd.InstallOptions{}) {
		t.Errorf("opts[1] = %+v, want zero options", oc.opts[1])
	}
	if call, _ := sink.lastCall(); call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want StatusSucceeded", call.status)
	}
}

type serviceHandlerCase struct {
	name   string
	make   func(c snapd.Client) cancellableHandler
	action string
}

var serviceHandlers = []serviceHandlerCase{
	{"start", func(c snapd.Client) cancellableHandler { return &manager.StartServiceHandler{Snapd: c} }, "start"},
	{"stop", func(c snapd.Client) cancellableHandler { return &manager.StopServiceHandler{Snapd: c} }, "stop"},
	{"restart", func(c snapd.Client) cancellableHandler { return &manager.RestartServiceHandler{Snapd: c} }, "restart"},
}

func TestServiceHandlers_MissingFields(t *testing.T) {
	full := exchange.Message{"snap": "hello", "service": "svc", "operation-id": int64(1)}
	cases := []struct {
		name   string
		mutate func(exchange.Message)
	}{
		{"missing snap", func(m exchange.Message) { delete(m, "snap") }},
		{"snap wrong type", func(m exchange.Message) { m["snap"] = int64(1) }},
		{"missing service", func(m exchange.Message) { delete(m, "service") }},
		{"missing operation-id", func(m exchange.Message) { delete(m, "operation-id") }},
	}
	for _, hc := range serviceHandlers {
		for _, tc := range cases {
			t.Run(hc.name+"/"+tc.name, func(t *testing.T) {
				msg := exchange.Message{}
				for k, v := range full {
					msg[k] = v
				}
				tc.mutate(msg)
				mc := &snapd.MockClient{}
				sink := &mockResultSink{}
				if err := hc.make(mc).Handle(context.Background(), msg, sink); err == nil {
					t.Error("expected error")
				}
				if len(mc.ServiceActions) != 0 || len(sink.calls) != 0 {
					t.Errorf("no action/result expected, got actions=%v results=%v", mc.ServiceActions, sink.calls)
				}
			})
		}
	}
}

func TestServiceHandlers_CancelledViaOperationContextManager(t *testing.T) {
	for _, hc := range serviceHandlers {
		t.Run(hc.name, func(t *testing.T) {
			bs := newBlockingSnapd()
			sink := &mockResultSink{}
			h := hc.make(bs)
			mgr := manager.NewOperationContextManager()
			h.SetOperationContextManager(mgr)

			done := make(chan error, 1)
			go func() {
				done <- h.Handle(context.Background(), exchange.Message{
					"snap": "hello", "service": "svc", "operation-id": int64(11),
				}, sink)
			}()

			select {
			case <-bs.started:
			case <-time.After(2 * time.Second):
				t.Fatalf("handler did not call %s", hc.action)
			}
			mgr.Cancel(11)

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not return after cancellation")
			}

			call, ok := sink.lastCall()
			if !ok {
				t.Fatal("no result sent")
			}
			if call.opID != 11 || call.status != exchange.StatusFailed || !strings.Contains(call.output, context.Canceled.Error()) {
				t.Errorf("result = %+v, want op 11 StatusFailed context canceled", call)
			}
		})
	}
}

// ---- ShutdownHandler edge cases ----

func TestShutdownHandler_MessageType(t *testing.T) {
	if got := manager.NewShutdownHandler().MessageType(); got != "shutdown" {
		t.Errorf("MessageType() = %q, want %q", got, "shutdown")
	}
}

func TestShutdownHandler_MissingOperationIDDoesNotShutdown(t *testing.T) {
	h := manager.NewShutdownHandler()
	called := false
	h.Shutdown = func(context.Context, bool) error { called = true; return nil }
	sink := &mockResultSink{}

	if err := h.Handle(context.Background(), exchange.Message{"reboot": true}, sink); err == nil {
		t.Error("expected error for missing operation-id")
	}
	if called || len(sink.calls) != 0 {
		t.Errorf("shutdown called=%v results=%v, want neither", called, sink.calls)
	}
}

// A result that cannot be delivered must not be followed by a shutdown, or
// the server would never learn the operation ran.
func TestShutdownHandler_SendResultErrorDoesNotShutdown(t *testing.T) {
	h := manager.NewShutdownHandler()
	called := false
	h.Shutdown = func(context.Context, bool) error { called = true; return nil }
	sendErr := errors.New("exchange down")
	sink := &funcSink{fn: func(int64, int, string) error { return sendErr }}

	err := h.Handle(context.Background(), exchange.Message{"operation-id": int64(1)}, sink)
	if !errors.Is(err, sendErr) {
		t.Errorf("Handle err = %v, want %v", err, sendErr)
	}
	if called {
		t.Error("Shutdown called despite SendResult failure")
	}
}

// ---- ScriptExecHandler edge cases ----

func TestScriptExecHandler_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		msg  exchange.Message
	}{
		{"missing operation-id", exchange.Message{"code": "echo hi"}},
		{"missing code", exchange.Message{"operation-id": int64(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapCommon := t.TempDir()
			h := manager.NewScriptExecHandler(snapCommon, nil)
			sink := &mockResultSink{}
			if err := h.Handle(context.Background(), tc.msg, sink); err == nil {
				t.Error("expected error")
			}
			if len(sink.calls) != 0 {
				t.Errorf("results = %v, want none", sink.calls)
			}
			if _, err := os.Stat(filepath.Join(snapCommon, "scripts")); !os.IsNotExist(err) {
				t.Errorf("scripts dir should not be created, stat err = %v", err)
			}
		})
	}
}

func TestScriptExecHandler_ScriptDirCreateError(t *testing.T) {
	snapCommon := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(snapCommon, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h := manager.NewScriptExecHandler(snapCommon, nil)
	sink := &mockResultSink{}

	err := h.Handle(context.Background(), exchange.Message{"operation-id": int64(1), "code": "echo hi"}, sink)
	if err == nil {
		t.Fatal("expected error when $SNAP_COMMON is not a directory")
	}
	if len(sink.calls) != 0 {
		t.Errorf("results = %v, want none", sink.calls)
	}
}

func TestScriptExecHandler_ScriptWriteError(t *testing.T) {
	snapCommon := t.TempDir()
	// A directory where the script file should go makes WriteFile fail.
	if err := os.MkdirAll(filepath.Join(snapCommon, "scripts", "1", "script"), 0700); err != nil {
		t.Fatal(err)
	}
	h := manager.NewScriptExecHandler(snapCommon, nil)
	sink := &mockResultSink{}

	err := h.Handle(context.Background(), exchange.Message{"operation-id": int64(1), "code": "echo hi"}, sink)
	if err == nil {
		t.Fatal("expected error when script cannot be written")
	}
	if _, statErr := os.Stat(filepath.Join(snapCommon, "scripts", "1")); !os.IsNotExist(statErr) {
		t.Errorf("script dir should be cleaned up, stat err = %v", statErr)
	}
}

func TestScriptExecHandler_AttachmentWriteError(t *testing.T) {
	h := manager.NewScriptExecHandler(t.TempDir(), &fakeAttachmentFetcher{data: map[int64][]byte{5: []byte("x")}})
	sink := &mockResultSink{}

	// Stays inside the script dir but its parent directory does not exist.
	msg := exchange.Message{
		"operation-id": int64(1),
		"code":         "echo hi",
		"attachments":  map[string]any{"nodir/file.txt": int64(5)},
	}
	if err := h.Handle(context.Background(), msg, sink); err == nil {
		t.Fatal("expected error when attachment cannot be written")
	}
	if len(sink.calls) != 0 {
		t.Errorf("results = %v, want none (script must not run)", sink.calls)
	}
}

// An executable interpreter that the kernel cannot exec (ENOEXEC) fails in
// Start, not with an exit status, so the reason must be reported.
func TestScriptExecHandler_InterpreterExecFormatError(t *testing.T) {
	interp := filepath.Join(t.TempDir(), "bogus")
	if err := os.WriteFile(interp, []byte("\x00\x01\x02not an executable"), 0700); err != nil {
		t.Fatal(err)
	}
	h := manager.NewScriptExecHandler(t.TempDir(), nil)
	sink := &mockResultSink{}

	msg := exchange.Message{"operation-id": int64(1), "code": "echo hi", "interpreter": interp}
	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("no result sent")
	}
	if call.status != exchange.StatusFailed || call.resultCode != 103 {
		t.Errorf("result = %+v, want StatusFailed code 103", call)
	}
	if !strings.Contains(call.output, "cannot run interpreter "+interp) {
		t.Errorf("output = %q, want cannot-run reason", call.output)
	}
}
