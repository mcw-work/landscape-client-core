package snapd_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/snapd"
)

// startUnixServer creates a test HTTP server listening on a temp Unix socket.
func startUnixServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "snapd.socket")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen unix: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, socketPath
}

func syncResponse(result any) []byte {
	raw, _ := json.Marshal(result)
	resp := map[string]any{
		"type":        "sync",
		"status-code": 200,
		"result":      json.RawMessage(raw),
	}
	data, _ := json.Marshal(resp)
	return data
}

func asyncResponse(changeID string) []byte {
	resp := map[string]any{
		"type":        "async",
		"status-code": 202,
		"change":      changeID,
	}
	data, _ := json.Marshal(resp)
	return data
}

func errorResponse(code int, msg string) []byte {
	resp := map[string]any{
		"type":        "error",
		"status-code": code,
		"result":      map[string]string{"message": msg},
	}
	data, _ := json.Marshal(resp)
	return data
}

// Test 1: ListSnaps returns correct []SnapInfo.
func TestListSnaps(t *testing.T) {
	want := []snapd.SnapInfo{
		{Name: "core20", Version: "20", Revision: "1234", Channel: "stable", Status: "active", Developer: "canonical"},
		{Name: "lxd", Version: "5.0", Revision: "5678", Channel: "latest/stable", Status: "active", Developer: "canonical"},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/snaps" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(want))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	got, err := client.ListSnaps(context.Background())
	if err != nil {
		t.Fatalf("ListSnaps: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d snaps, want %d", len(got), len(want))
	}
	for i, s := range got {
		if s.Name != want[i].Name || s.Version != want[i].Version {
			t.Errorf("snap[%d]: got %+v, want %+v", i, s, want[i])
		}
	}
}

// Test 2: ListServices returns correct []ServiceInfo.
func TestListServices(t *testing.T) {
	want := []snapd.ServiceInfo{
		{Snap: "lxd", Name: "lxd.daemon", Enabled: true, Active: true},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/apps" || r.URL.Query().Get("select") != "service" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(want))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	got, err := client.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(got) != 1 || got[0].Name != "lxd.daemon" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Test 3: InstallSnap sends correct JSON body and returns changeID.
func TestInstallSnap(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/snaps/mysnap" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(asyncResponse("change-1"))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	changeID, err := client.InstallSnap(context.Background(), "mysnap", snapd.InstallOptions{Channel: "beta"})
	if err != nil {
		t.Fatalf("InstallSnap: %v", err)
	}
	if changeID != "change-1" {
		t.Errorf("changeID: got %q, want %q", changeID, "change-1")
	}
	if gotBody["action"] != "install" {
		t.Errorf("action: got %v, want install", gotBody["action"])
	}
	if gotBody["channel"] != "beta" {
		t.Errorf("channel: got %v, want beta", gotBody["channel"])
	}
}

// Test 4: RemoveSnap sends correct action and returns changeID.
func TestRemoveSnap(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(asyncResponse("change-2"))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	changeID, err := client.RemoveSnap(context.Background(), "mysnap")
	if err != nil {
		t.Fatalf("RemoveSnap: %v", err)
	}
	if changeID != "change-2" {
		t.Errorf("changeID: got %q, want %q", changeID, "change-2")
	}
	if gotBody["action"] != "remove" {
		t.Errorf("action: got %v, want remove", gotBody["action"])
	}
}

// Test 5: RefreshSnap sends correct action and returns changeID.
func TestRefreshSnap(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(asyncResponse("change-3"))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	changeID, err := client.RefreshSnap(context.Background(), "mysnap")
	if err != nil {
		t.Fatalf("RefreshSnap: %v", err)
	}
	if changeID != "change-3" {
		t.Errorf("changeID: got %q, want %q", changeID, "change-3")
	}
	if gotBody["action"] != "refresh" {
		t.Errorf("action: got %v, want refresh", gotBody["action"])
	}
}

// Test 6: StartService sends correct body with full service name.
func TestStartService(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(nil))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	err := client.StartService(context.Background(), "lxd", "daemon")
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}
	if gotBody["action"] != "start" {
		t.Errorf("action: got %v, want start", gotBody["action"])
	}
	names, _ := gotBody["names"].([]any)
	if len(names) != 1 || names[0] != "lxd.daemon" {
		t.Errorf("names: got %v, want [lxd.daemon]", names)
	}
}

// Test 7: StopService sends correct body.
func TestStopService(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(nil))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	err := client.StopService(context.Background(), "lxd", "daemon")
	if err != nil {
		t.Fatalf("StopService: %v", err)
	}
	if gotBody["action"] != "stop" {
		t.Errorf("action: got %v, want stop", gotBody["action"])
	}
}

// Test 8: RestartService sends correct body.
func TestRestartService(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(nil))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	err := client.RestartService(context.Background(), "lxd", "daemon")
	if err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	if gotBody["action"] != "restart" {
		t.Errorf("action: got %v, want restart", gotBody["action"])
	}
}

// Test 9: WaitForChange polls until "Done".
func TestWaitForChange_Done(t *testing.T) {
	var callCount atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		status := "Doing"
		if n >= 3 {
			status = "Done"
		}
		result := map[string]any{"status": status}
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(result))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	err := client.WaitForChange(context.Background(), "change-99")
	if err != nil {
		t.Fatalf("WaitForChange: %v", err)
	}
	if n := callCount.Load(); n < 3 {
		t.Errorf("expected at least 3 polls, got %d", n)
	}
}

// Test 10: WaitForChange returns error when status is "Error".
func TestWaitForChange_Error(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := map[string]any{
			"status": "Error",
			"err":    map[string]string{"message": "something went wrong"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(result))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	err := client.WaitForChange(context.Background(), "change-bad")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error %q does not contain expected message", err.Error())
	}
}

// Test 11: GetAssertions parses serial, model, brand correctly.
func TestGetAssertions(t *testing.T) {
	serialAssertion := "type: serial\nserial: abc-123\nbrand-id: canonical\nmodel: ubuntu-core-20\n\n<body>"
	modelAssertion := "type: model\nmodel: ubuntu-core-20\nbrand-id: canonical\n\n<body>"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/assertions/serial":
			w.Header().Set("Content-Type", "application/x.ubuntu.assertion")
			fmt.Fprint(w, serialAssertion)
		case "/v2/assertions/model":
			w.Header().Set("Content-Type", "application/x.ubuntu.assertion")
			fmt.Fprint(w, modelAssertion)
		default:
			http.NotFound(w, r)
		}
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	a, err := client.GetAssertions(context.Background())
	if err != nil {
		t.Fatalf("GetAssertions: %v", err)
	}
	if a.Serial != "abc-123" {
		t.Errorf("Serial: got %q, want %q", a.Serial, "abc-123")
	}
	if a.Model != "ubuntu-core-20" {
		t.Errorf("Model: got %q, want %q", a.Model, "ubuntu-core-20")
	}
	if a.Brand != "canonical" {
		t.Errorf("Brand: got %q, want %q", a.Brand, "canonical")
	}
}

// Test 12: GetRebootRequired returns true when refresh.pending == "restart".
func TestGetRebootRequired_True(t *testing.T) {
	result := map[string]any{
		"refresh": map[string]string{"pending": "restart"},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(result))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	got, err := client.GetRebootRequired(context.Background())
	if err != nil {
		t.Fatalf("GetRebootRequired: %v", err)
	}
	if !got {
		t.Error("expected true, got false")
	}
}

// Test 13: GetRebootRequired returns false when field is absent.
func TestGetRebootRequired_False(t *testing.T) {
	result := map[string]any{
		"version": "2.57",
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(result))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	got, err := client.GetRebootRequired(context.Background())
	if err != nil {
		t.Fatalf("GetRebootRequired: %v", err)
	}
	if got {
		t.Error("expected false, got true")
	}
}

// Test 14: Context cancellation propagates to in-flight request.
func TestContextCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
		}
		w.Write(syncResponse([]snapd.SnapInfo{}))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.ListSnaps(ctx)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}

// Test 15: WaitForChange returns promptly with context error when context is cancelled during sleep.
func TestWaitForChange_ContextCancel(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := map[string]any{"status": "Doing"}
		w.Header().Set("Content-Type", "application/json")
		w.Write(syncResponse(result))
	})
	_, socketPath := startUnixServer(t, handler)
	client := snapd.New(socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.WaitForChange(ctx, "change-cancel")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected context error, got %v", err)
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("WaitForChange took too long: %v", elapsed)
	}
}

// MockClient tests.

func TestMockClient_InstallCalls(t *testing.T) {
	mc := &snapd.MockClient{ChangeID: "mock-change"}
	id, err := mc.InstallSnap(context.Background(), "mysnap", snapd.InstallOptions{})
	if err != nil {
		t.Fatalf("InstallSnap: %v", err)
	}
	if id != "mock-change" {
		t.Errorf("changeID: got %q, want %q", id, "mock-change")
	}
	if len(mc.InstallCalls) != 1 || mc.InstallCalls[0] != "mysnap" {
		t.Errorf("InstallCalls: got %v, want [mysnap]", mc.InstallCalls)
	}
}

func TestMockClient_ErrPropagation(t *testing.T) {
	sentinel := errors.New("mock error")
	mc := &snapd.MockClient{Err: sentinel}

	if _, err := mc.ListSnaps(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("ListSnaps: got %v, want sentinel", err)
	}
	if _, err := mc.ListServices(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("ListServices: got %v, want sentinel", err)
	}
	if _, err := mc.InstallSnap(context.Background(), "snap", snapd.InstallOptions{}); !errors.Is(err, sentinel) {
		t.Errorf("InstallSnap: got %v, want sentinel", err)
	}
	if _, err := mc.RemoveSnap(context.Background(), "snap"); !errors.Is(err, sentinel) {
		t.Errorf("RemoveSnap: got %v, want sentinel", err)
	}
	if _, err := mc.RefreshSnap(context.Background(), "snap"); !errors.Is(err, sentinel) {
		t.Errorf("RefreshSnap: got %v, want sentinel", err)
	}
	if err := mc.StartService(context.Background(), "snap", "svc"); !errors.Is(err, sentinel) {
		t.Errorf("StartService: got %v, want sentinel", err)
	}
	if _, err := mc.GetAssertions(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("GetAssertions: got %v, want sentinel", err)
	}
	if _, err := mc.GetRebootRequired(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("GetRebootRequired: got %v, want sentinel", err)
	}
}

func TestMockClient_ServiceActions(t *testing.T) {
	mc := &snapd.MockClient{}
	mc.StartService(context.Background(), "lxd", "daemon")
	mc.StopService(context.Background(), "lxd", "daemon")
	mc.RestartService(context.Background(), "lxd", "daemon")

	want := []string{"start:lxd.daemon", "stop:lxd.daemon", "restart:lxd.daemon"}
	if len(mc.ServiceActions) != len(want) {
		t.Fatalf("ServiceActions: got %v, want %v", mc.ServiceActions, want)
	}
	for i, a := range mc.ServiceActions {
		if a != want[i] {
			t.Errorf("ServiceActions[%d]: got %q, want %q", i, a, want[i])
		}
	}
}
