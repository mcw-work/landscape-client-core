package manager_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/manager"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// mockResultSink records calls to SendResult.
type mockResultSink struct {
	mu    sync.Mutex
	calls []resultCall
}

type resultCall struct {
	opID       int64
	status     int
	resultCode int64 // 0 means not set
	output     string
}

func (m *mockResultSink) SendResult(_ context.Context, opID int64, status int, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, resultCall{opID: opID, status: status, resultCode: 0, output: output})
	return nil
}

func (m *mockResultSink) SendResultCode(_ context.Context, opID int64, status int, resultCode int64, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, resultCall{opID: opID, status: status, resultCode: resultCode, output: output})
	return nil
}

func (m *mockResultSink) lastCall() (resultCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return resultCall{}, false
	}
	return m.calls[len(m.calls)-1], true
}

// -- InstallSnapHandler --

func TestInstallSnapHandler_MessageType(t *testing.T) {
	h := &manager.InstallSnapHandler{Snapd: &snapd.MockClient{}}
	if got := h.MessageType(); got != "install-snap" {
		t.Errorf("MessageType() = %q, want %q", got, "install-snap")
	}
}

func TestInstallSnapHandler_Success(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "42"}
	sink := &mockResultSink{}
	h := &manager.InstallSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "mysnap",
		"operation-id": int64(1),
		"channel":      "stable",
		"classic":      false,
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusSucceeded)
	}
	if call.output != "" {
		t.Errorf("output = %q, want empty", call.output)
	}
	if len(mc.InstallCalls) != 1 || mc.InstallCalls[0] != "mysnap" {
		t.Errorf("InstallCalls = %v, want [mysnap]", mc.InstallCalls)
	}
}

func TestInstallSnapHandler_SnapdError(t *testing.T) {
	mc := &snapd.MockClient{Err: fmt.Errorf("install failed")}
	sink := &mockResultSink{}
	h := &manager.InstallSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "mysnap",
		"operation-id": int64(1),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
	if call.output == "" {
		t.Error("expected non-empty error output")
	}
}

func TestInstallSnapHandler_WaitForChangeTimeout(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "42", ChangeErr: context.DeadlineExceeded}
	sink := &mockResultSink{}
	h := &manager.InstallSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "mysnap",
		"operation-id": int64(1),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
}

// -- RemoveSnapHandler --

func TestRemoveSnapHandler_MessageType(t *testing.T) {
	h := &manager.RemoveSnapHandler{Snapd: &snapd.MockClient{}}
	if got := h.MessageType(); got != "remove-snap" {
		t.Errorf("MessageType() = %q, want %q", got, "remove-snap")
	}
}

func TestRemoveSnapHandler_Success(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "99"}
	sink := &mockResultSink{}
	h := &manager.RemoveSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "removeme",
		"operation-id": int64(2),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusSucceeded)
	}
	if call.output != "" {
		t.Errorf("output = %q, want empty", call.output)
	}
	if len(mc.RemoveCalls) != 1 || mc.RemoveCalls[0] != "removeme" {
		t.Errorf("RemoveCalls = %v, want [removeme]", mc.RemoveCalls)
	}
}

func TestRemoveSnapHandler_SnapdError(t *testing.T) {
	mc := &snapd.MockClient{Err: fmt.Errorf("remove failed")}
	sink := &mockResultSink{}
	h := &manager.RemoveSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "removeme",
		"operation-id": int64(2),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
	if call.output == "" {
		t.Error("expected non-empty error output")
	}
}

func TestRemoveSnapHandler_WaitForChangeTimeout(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "99", ChangeErr: context.DeadlineExceeded}
	sink := &mockResultSink{}
	h := &manager.RemoveSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "removeme",
		"operation-id": int64(2),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
}

// -- RefreshSnapHandler --

func TestRefreshSnapHandler_MessageType(t *testing.T) {
	h := &manager.RefreshSnapHandler{Snapd: &snapd.MockClient{}}
	if got := h.MessageType(); got != "refresh-snap" {
		t.Errorf("MessageType() = %q, want %q", got, "refresh-snap")
	}
}

func TestRefreshSnapHandler_Success(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "77"}
	sink := &mockResultSink{}
	h := &manager.RefreshSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "refreshme",
		"operation-id": int64(3),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusSucceeded)
	}
	if call.output != "" {
		t.Errorf("output = %q, want empty", call.output)
	}
	if len(mc.RefreshCalls) != 1 || mc.RefreshCalls[0] != "refreshme" {
		t.Errorf("RefreshCalls = %v, want [refreshme]", mc.RefreshCalls)
	}
}

func TestRefreshSnapHandler_SnapdError(t *testing.T) {
	mc := &snapd.MockClient{Err: fmt.Errorf("refresh failed")}
	sink := &mockResultSink{}
	h := &manager.RefreshSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "refreshme",
		"operation-id": int64(3),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
	if call.output == "" {
		t.Error("expected non-empty error output")
	}
}

func TestRefreshSnapHandler_WaitForChangeTimeout(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "77", ChangeErr: context.DeadlineExceeded}
	sink := &mockResultSink{}
	h := &manager.RefreshSnapHandler{Snapd: mc}
	msg := exchange.Message{
		"snap-name":    "refreshme",
		"operation-id": int64(3),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
}

// -- StartServiceHandler --

func TestStartServiceHandler_MessageType(t *testing.T) {
	h := &manager.StartServiceHandler{Snapd: &snapd.MockClient{}}
	if got := h.MessageType(); got != "start-snap-service" {
		t.Errorf("MessageType() = %q, want %q", got, "start-snap-service")
	}
}

func TestStartServiceHandler_Success(t *testing.T) {
	mc := &snapd.MockClient{}
	sink := &mockResultSink{}
	h := &manager.StartServiceHandler{Snapd: mc}
	msg := exchange.Message{
		"snap":         "mysnap",
		"service":      "myservice",
		"operation-id": int64(4),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusSucceeded)
	}
	if call.output != "" {
		t.Errorf("output = %q, want empty", call.output)
	}
	if len(mc.ServiceActions) != 1 || mc.ServiceActions[0] != "start:mysnap.myservice" {
		t.Errorf("ServiceActions = %v, want [start:mysnap.myservice]", mc.ServiceActions)
	}
}

func TestStartServiceHandler_SnapdError(t *testing.T) {
	mc := &snapd.MockClient{Err: fmt.Errorf("start failed")}
	sink := &mockResultSink{}
	h := &manager.StartServiceHandler{Snapd: mc}
	msg := exchange.Message{
		"snap":         "mysnap",
		"service":      "myservice",
		"operation-id": int64(4),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
	if call.output == "" {
		t.Error("expected non-empty error output")
	}
}

// -- StopServiceHandler --

func TestStopServiceHandler_MessageType(t *testing.T) {
	h := &manager.StopServiceHandler{Snapd: &snapd.MockClient{}}
	if got := h.MessageType(); got != "stop-snap-service" {
		t.Errorf("MessageType() = %q, want %q", got, "stop-snap-service")
	}
}

func TestStopServiceHandler_Success(t *testing.T) {
	mc := &snapd.MockClient{}
	sink := &mockResultSink{}
	h := &manager.StopServiceHandler{Snapd: mc}
	msg := exchange.Message{
		"snap":         "mysnap",
		"service":      "myservice",
		"operation-id": int64(5),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusSucceeded)
	}
	if len(mc.ServiceActions) != 1 || mc.ServiceActions[0] != "stop:mysnap.myservice" {
		t.Errorf("ServiceActions = %v, want [stop:mysnap.myservice]", mc.ServiceActions)
	}
}

func TestStopServiceHandler_SnapdError(t *testing.T) {
	mc := &snapd.MockClient{Err: fmt.Errorf("stop failed")}
	sink := &mockResultSink{}
	h := &manager.StopServiceHandler{Snapd: mc}
	msg := exchange.Message{
		"snap":         "mysnap",
		"service":      "myservice",
		"operation-id": int64(5),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
}

// -- RestartServiceHandler --

func TestRestartServiceHandler_MessageType(t *testing.T) {
	h := &manager.RestartServiceHandler{Snapd: &snapd.MockClient{}}
	if got := h.MessageType(); got != "restart-snap-service" {
		t.Errorf("MessageType() = %q, want %q", got, "restart-snap-service")
	}
}

func TestRestartServiceHandler_Success(t *testing.T) {
	mc := &snapd.MockClient{}
	sink := &mockResultSink{}
	h := &manager.RestartServiceHandler{Snapd: mc}
	msg := exchange.Message{
		"snap":         "mysnap",
		"service":      "myservice",
		"operation-id": int64(6),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusSucceeded {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusSucceeded)
	}
	if len(mc.ServiceActions) != 1 || mc.ServiceActions[0] != "restart:mysnap.myservice" {
		t.Errorf("ServiceActions = %v, want [restart:mysnap.myservice]", mc.ServiceActions)
	}
}

func TestRestartServiceHandler_SnapdError(t *testing.T) {
	mc := &snapd.MockClient{Err: fmt.Errorf("restart failed")}
	sink := &mockResultSink{}
	h := &manager.RestartServiceHandler{Snapd: mc}
	msg := exchange.Message{
		"snap":         "mysnap",
		"service":      "myservice",
		"operation-id": int64(6),
	}

	if err := h.Handle(context.Background(), msg, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call, ok := sink.lastCall()
	if !ok {
		t.Fatal("expected SendResult to be called")
	}
	if call.status != exchange.StatusFailed {
		t.Errorf("status = %d, want %d", call.status, exchange.StatusFailed)
	}
}
