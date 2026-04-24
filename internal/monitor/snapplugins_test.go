package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/snapd"
)

// ─── Helpers ─────────────────────────────────────────────────────────────────

// seqServicesClient implements snapd.Client and returns successive service
// lists from vals on each call to ListServices. Once exhausted it repeats the
// last slice. All other methods are no-op stubs.
type seqServicesClient struct {
	mu   sync.Mutex
	vals [][]snapd.ServiceInfo
	idx  int
	err  error
}

func (c *seqServicesClient) ListServices(_ context.Context) ([]snapd.ServiceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if len(c.vals) == 0 {
		return nil, nil
	}
	if c.idx >= len(c.vals) {
		return c.vals[len(c.vals)-1], nil
	}
	v := c.vals[c.idx]
	c.idx++
	return v, nil
}

func (c *seqServicesClient) ListSnaps(_ context.Context) ([]snapd.SnapInfo, error) { return nil, nil }
func (c *seqServicesClient) InstallSnap(_ context.Context, _ string, _ snapd.InstallOptions) (string, error) {
	return "", nil
}
func (c *seqServicesClient) RemoveSnap(_ context.Context, _ string) (string, error)  { return "", nil }
func (c *seqServicesClient) RefreshSnap(_ context.Context, _ string) (string, error) { return "", nil }
func (c *seqServicesClient) StartService(_ context.Context, _, _ string) error       { return nil }
func (c *seqServicesClient) StopService(_ context.Context, _, _ string) error        { return nil }
func (c *seqServicesClient) RestartService(_ context.Context, _, _ string) error     { return nil }
func (c *seqServicesClient) WaitForChange(_ context.Context, _ string) error         { return nil }
func (c *seqServicesClient) GetAssertions(_ context.Context) (*snapd.Assertions, error) {
	return nil, nil
}
func (c *seqServicesClient) GetRebootRequired(_ context.Context) (bool, error) { return false, nil }

// ─── SnapPackages tests ───────────────────────────────────────────────────────

func TestSnapPackages_HappyPath(t *testing.T) {
	client := &snapd.MockClient{
		Snaps: []snapd.SnapInfo{
			{Name: "core22", Version: "20240101", Revision: 1234, Channel: "latest/stable", Developer: "canonical"},
			{Name: "firefox", Version: "125.0", Revision: 4567, Channel: "latest/stable", Developer: "mozilla"},
		},
	}
	p := &SnapPackagesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	msg := msgs[0]
	if got := msg["type"]; got != "snaps" {
		t.Errorf("type: want %q, got %q", "snaps", got)
	}
	snapsMap, ok := msg["snaps"].(map[string]any)
	if !ok {
		t.Fatalf("snaps: expected map[string]any, got %T", msg["snaps"])
	}
	installed, ok := snapsMap["installed"].([]any)
	if !ok {
		t.Fatalf("snaps.installed: expected []any, got %T", snapsMap["installed"])
	}
	if len(installed) != 2 {
		t.Fatalf("snaps.installed: want 2 entries, got %d", len(installed))
	}

	// Check first snap (core22).
	snap0, ok := installed[0].(map[string]any)
	if !ok {
		t.Fatalf("installed[0]: expected map[string]any, got %T", installed[0])
	}
	if snap0["id"] != "core22" {
		t.Errorf("id: want %q, got %q", "core22", snap0["id"])
	}
	if snap0["name"] != "core22" {
		t.Errorf("name: want %q, got %q", "core22", snap0["name"])
	}
	if snap0["version"] != "20240101" {
		t.Errorf("version: want %q, got %q", "20240101", snap0["version"])
	}
	if snap0["revision"] != "1234" {
		t.Errorf("revision: want %q (string), got %v (%T)", "1234", snap0["revision"], snap0["revision"])
	}
	if snap0["tracking-channel"] != "latest/stable" {
		t.Errorf("tracking-channel: want %q, got %q", "latest/stable", snap0["tracking-channel"])
	}
	pub, ok := snap0["publisher"].(map[string]any)
	if !ok {
		t.Fatalf("publisher: expected map[string]any, got %T", snap0["publisher"])
	}
	if pub["username"] != "canonical" {
		t.Errorf("publisher.username: want %q, got %q", "canonical", pub["username"])
	}
	if pub["validation"] != "" {
		t.Errorf("publisher.validation: want %q, got %q", "", pub["validation"])
	}
	if snap0["confinement"] != "" {
		t.Errorf("confinement: want %q, got %q", "", snap0["confinement"])
	}
}

func TestSnapPackages_EmptyList(t *testing.T) {
	client := &snapd.MockClient{Snaps: []snapd.SnapInfo{}}
	p := &SnapPackagesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	snapsMap, ok := msgs[0]["snaps"].(map[string]any)
	if !ok {
		t.Fatalf("snaps: expected map[string]any, got %T", msgs[0]["snaps"])
	}
	installed, ok := snapsMap["installed"].([]any)
	if !ok {
		t.Fatalf("snaps.installed: expected []any, got %T", snapsMap["installed"])
	}
	if len(installed) != 0 {
		t.Errorf("snaps.installed: want empty, got %d entries", len(installed))
	}
}

func TestSnapPackages_SnapdError(t *testing.T) {
	client := &snapd.MockClient{Err: errors.New("snapd unavailable")}
	p := &SnapPackagesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Let a few ticks fire; on each tick the error path sends installed:[].
	// The plugin must not crash and must return nil on context cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
}

func TestSnapPackages_ContextCancel(t *testing.T) {
	p := &SnapPackagesPlugin{interval: time.Hour, snapdClient: &snapd.MockClient{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, &mockSink{}, nil) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Run did not return after context cancel")
	}
}

// ─── SnapServices tests ───────────────────────────────────────────────────────

func TestSnapServices_HappyPath(t *testing.T) {
	services := []snapd.ServiceInfo{
		{Snap: "lxd", Name: "lxd.daemon", Enabled: true, Active: true},
		{Snap: "microk8s", Name: "microk8s.daemon-kubelite", Enabled: true, Active: false},
	}
	client := &seqServicesClient{vals: [][]snapd.ServiceInfo{services}}
	p := &SnapServicesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	msg := msgs[0]
	if got := msg["type"]; got != "snap-services" {
		t.Errorf("type: want %q, got %q", "snap-services", got)
	}
	svcMap, ok := msg["services"].(map[string]any)
	if !ok {
		t.Fatalf("services: expected map[string]any, got %T", msg["services"])
	}
	running, ok := svcMap["running"].([]any)
	if !ok {
		t.Fatalf("services.running: expected []any, got %T", svcMap["running"])
	}
	if len(running) != 2 {
		t.Fatalf("services.running: want 2 entries, got %d", len(running))
	}
	// Services are sorted by name; lxd.daemon < microk8s.daemon-kubelite.
	svc0, ok := running[0].(map[string]any)
	if !ok {
		t.Fatalf("running[0]: expected map[string]any, got %T", running[0])
	}
	if svc0["name"] != "lxd.daemon" {
		t.Errorf("running[0].name: want %q, got %q", "lxd.daemon", svc0["name"])
	}
	if svc0["snap"] != "lxd" {
		t.Errorf("running[0].snap: want %q, got %q", "lxd", svc0["snap"])
	}
	if svc0["enabled"] != true {
		t.Errorf("running[0].enabled: want true, got %v", svc0["enabled"])
	}
	if svc0["active"] != true {
		t.Errorf("running[0].active: want true, got %v", svc0["active"])
	}
}

func TestSnapServices_DataWatcher_NoChangeNoSecondMessage(t *testing.T) {
	services := []snapd.ServiceInfo{
		{Snap: "lxd", Name: "lxd.daemon", Enabled: true, Active: true},
	}
	// The seq client will keep returning the same slice.
	client := &seqServicesClient{vals: [][]snapd.ServiceInfo{services}}
	p := &SnapServicesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Wait for the first message, then allow several more ticks.
	waitForMessages(t, sink, 1, 500*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if n := len(sink.Messages()); n != 1 {
		t.Errorf("expected exactly 1 message (data-watcher), got %d", n)
	}
}

func TestSnapServices_DataWatcher_ChangeTriggersSecondMessage(t *testing.T) {
	first := []snapd.ServiceInfo{
		{Snap: "lxd", Name: "lxd.daemon", Enabled: true, Active: true},
	}
	second := []snapd.ServiceInfo{
		{Snap: "lxd", Name: "lxd.daemon", Enabled: true, Active: false}, // active changed
	}
	client := &seqServicesClient{vals: [][]snapd.ServiceInfo{first, second}}
	p := &SnapServicesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	msgs := waitForMessages(t, sink, 2, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}
	// Second message should reflect the changed service state.
	svcMap, ok := msgs[1]["services"].(map[string]any)
	if !ok {
		t.Fatalf("services in msg[1]: expected map[string]any, got %T", msgs[1]["services"])
	}
	running, ok := svcMap["running"].([]any)
	if !ok || len(running) == 0 {
		t.Fatalf("services.running in msg[1]: expected non-empty []any")
	}
	svc0, ok := running[0].(map[string]any)
	if !ok {
		t.Fatalf("running[0] in msg[1]: expected map[string]any, got %T", running[0])
	}
	if svc0["active"] != false {
		t.Errorf("running[0].active in msg[1]: want false, got %v", svc0["active"])
	}
}

func TestSnapServices_SnapdError(t *testing.T) {
	client := &seqServicesClient{err: errors.New("snapd unavailable")}
	p := &SnapServicesPlugin{interval: 5 * time.Millisecond, snapdClient: client}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Let several ticks fire; the error path must not send any message.
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on snapd error, got %d", n)
	}
}

func TestSnapServices_ContextCancel(t *testing.T) {
	p := &SnapServicesPlugin{interval: time.Hour, snapdClient: &seqServicesClient{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, &mockSink{}, nil) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Run did not return after context cancel")
	}
}
