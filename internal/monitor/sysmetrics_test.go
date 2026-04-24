package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/snapd"
)

// ─── Reboot test helper ────────────────────────────────────────────────────

// rebootSeqClient implements snapd.Client for reboot-required tests.
// GetRebootRequired returns values from vals in order; once exhausted it
// repeats the last value. All other methods are no-op stubs.
type rebootSeqClient struct {
	mu   sync.Mutex
	vals []bool
	idx  int
	err  error
}

func (c *rebootSeqClient) GetRebootRequired(_ context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	if len(c.vals) == 0 {
		return false, nil
	}
	if c.idx >= len(c.vals) {
		return c.vals[len(c.vals)-1], nil
	}
	v := c.vals[c.idx]
	c.idx++
	return v, nil
}

func (c *rebootSeqClient) ListSnaps(_ context.Context) ([]snapd.SnapInfo, error) { return nil, nil }
func (c *rebootSeqClient) ListServices(_ context.Context) ([]snapd.ServiceInfo, error) {
	return nil, nil
}
func (c *rebootSeqClient) InstallSnap(_ context.Context, _ string, _ snapd.InstallOptions) (string, error) {
	return "", nil
}
func (c *rebootSeqClient) RemoveSnap(_ context.Context, _ string) (string, error)  { return "", nil }
func (c *rebootSeqClient) RefreshSnap(_ context.Context, _ string) (string, error) { return "", nil }
func (c *rebootSeqClient) StartService(_ context.Context, _, _ string) error       { return nil }
func (c *rebootSeqClient) StopService(_ context.Context, _, _ string) error        { return nil }
func (c *rebootSeqClient) RestartService(_ context.Context, _, _ string) error     { return nil }
func (c *rebootSeqClient) WaitForChange(_ context.Context, _ string) error         { return nil }
func (c *rebootSeqClient) GetAssertions(_ context.Context) (*snapd.Assertions, error) {
	return nil, nil
}

// ─── NetworkActivity tests ─────────────────────────────────────────────────

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
  eth0:    1000      10    0    0    0     0          0         0      500       5    0    0    0     0       0          0
`

const netDevUpdated = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
  eth0:    2500      25    0    0    0     0          0         0     1800      18    0    0    0     0       0          0
`

func TestNetworkActivity_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev")
	writeFixture(t, path, netDevFixture)

	p := &NetworkActivity{procNetDevPath: path, interval: 5 * time.Millisecond}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Wait for the goroutine to prime the baseline, then update the file.
	time.Sleep(20 * time.Millisecond)
	writeFixture(t, path, netDevUpdated)

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	msg := msgs[0]
	if got := msg["type"]; got != "network-activity" {
		t.Errorf("type: want %q, got %q", "network-activity", got)
	}
	acts, ok := msg["activities"].(map[string][]any)
	if !ok {
		t.Fatalf("activities: expected map[string][]any, got %T", msg["activities"])
	}
	eth0, ok := acts["eth0"]
	if !ok || len(eth0) == 0 {
		t.Fatalf("activities: expected eth0 key with entries, got %v", acts)
	}
	entry, ok := eth0[0].([]any)
	if !ok || len(entry) < 3 {
		t.Fatalf("eth0[0]: expected []any with ≥3 elements, got %T %v", eth0[0], eth0[0])
	}
	// eth0 rx delta = 2500-1000 = 1500, tx delta = 1800-500 = 1300
	rxDelta, ok := entry[1].(int64)
	if !ok {
		t.Fatalf("eth0[0][1]: expected int64, got %T", entry[1])
	}
	txDelta, ok := entry[2].(int64)
	if !ok {
		t.Fatalf("eth0[0][2]: expected int64, got %T", entry[2])
	}
	if rxDelta != 1500 {
		t.Errorf("rx delta: want 1500, got %d", rxDelta)
	}
	if txDelta != 1300 {
		t.Errorf("tx delta: want 1300, got %d", txDelta)
	}
}

func TestNetworkActivity_MissingFile(t *testing.T) {
	p := &NetworkActivity{
		procNetDevPath: filepath.Join(t.TempDir(), "nonexistent"),
		interval:       5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on missing file, got %d", n)
	}
}

func TestNetworkActivity_ContextCancel(t *testing.T) {
	p := &NetworkActivity{procNetDevPath: "/proc/net/dev", interval: time.Hour}
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

// ─── ActiveProcessInfo tests ───────────────────────────────────────────────

// makeProcTree creates a minimal /proc-like directory tree under dir.
// procStat should be the full content for the proc/stat file (must contain a
// "btime <seconds>" line). uptimeStr is the content for proc/uptime.
// pids is a map of pid → fake stat-line content.
func makeProcTree(t *testing.T, dir string, procStatContent, uptimeStr string, pids map[int64]struct{ stat, status string }) {
	t.Helper()
	writeFixture(t, filepath.Join(dir, "stat"), procStatContent)
	writeFixture(t, filepath.Join(dir, "uptime"), uptimeStr)
	for pid, files := range pids {
		pidDir := filepath.Join(dir, fmt.Sprintf("%d", pid))
		if err := os.MkdirAll(pidDir, 0700); err != nil {
			t.Fatalf("makeProcTree: mkdir %s: %v", pidDir, err)
		}
		writeFixture(t, filepath.Join(pidDir, "stat"), files.stat)
		writeFixture(t, filepath.Join(pidDir, "status"), files.status)
	}
}

func TestActiveProcessInfo_HappyPath(t *testing.T) {
	dir := t.TempDir()
	makeProcTree(t, dir,
		"cpu  0 0 0 0 0 0 0 0 0 0\nbtime 1609459200\n",
		"12345.0 98765.43\n",
		map[int64]struct{ stat, status string }{
			123: {
				// Fields after "(bash) ": state ppid pgrp sess tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime pri nice nthreads itreal starttime vsize
				stat:   "123 (bash) S 0 0 0 0 -1 0 0 0 0 0 10 5 0 0 20 0 1 0 100 1048576\n",
				status: "Name:\tbash\nState:\tS (sleeping)\nUid:\t1000 1000 1000 1000\nGid:\t1000 1000 1000 1000\nVmSize:\t 1024 kB\n",
			},
		},
	)

	p := &ActiveProcessInfo{procRoot: dir, interval: 5 * time.Millisecond}
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
	if got := msg["type"]; got != "active-process-info" {
		t.Errorf("type: want %q, got %q", "active-process-info", got)
	}
	if got := msg["kill-all-processes"]; got != true {
		t.Errorf("kill-all-processes: want true, got %v", got)
	}
	adds, ok := msg["add-processes"].([]any)
	if !ok || len(adds) == 0 {
		t.Fatalf("add-processes: expected non-empty []any, got %T %v", msg["add-processes"], msg["add-processes"])
	}
	proc, ok := adds[0].(map[string]any)
	if !ok {
		t.Fatalf("add-processes[0]: expected map[string]any, got %T", adds[0])
	}
	if got := proc["pid"]; got != int64(123) {
		t.Errorf("pid: want 123, got %v", got)
	}
	if got := proc["name"]; got != "bash" {
		t.Errorf("name: want %q, got %q", "bash", got)
	}
	if got := proc["state"]; got != "S" {
		t.Errorf("state: want %q, got %q", "S", got)
	}
}

func TestActiveProcessInfo_MissingProcRoot(t *testing.T) {
	p := &ActiveProcessInfo{
		procRoot: filepath.Join(t.TempDir(), "nonexistent"),
		interval: 5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on missing proc root, got %d", n)
	}
}

func TestActiveProcessInfo_ContextCancel(t *testing.T) {
	p := &ActiveProcessInfo{procRoot: t.TempDir(), interval: time.Hour}
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

// ─── Temperature tests ─────────────────────────────────────────────────────

func TestTemperature_HappyPath(t *testing.T) {
	dir := t.TempDir()
	// Create a fake thermal_zone0 directory with a temp file (42°C = 42000 millideg).
	zoneDir := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zoneDir, 0700); err != nil {
		t.Fatalf("mkdir %s: %v", zoneDir, err)
	}
	writeFixture(t, filepath.Join(zoneDir, "temp"), "42000\n")

	p := &Temperature{sysfsPath: dir, interval: 5 * time.Millisecond}
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
	if got := msg["type"]; got != "temperature" {
		t.Errorf("type: want %q, got %q", "temperature", got)
	}
	if got := msg["thermal-zone"]; got != "thermal_zone0" {
		t.Errorf("thermal-zone: want %q, got %q", "thermal_zone0", got)
	}
	temps, ok := msg["temperatures"].([]any)
	if !ok || len(temps) == 0 {
		t.Fatalf("temperatures: expected non-empty []any, got %T %v", msg["temperatures"], msg["temperatures"])
	}
	entry, ok := temps[0].([]any)
	if !ok || len(entry) < 2 {
		t.Fatalf("temperatures[0]: expected []any with ≥2 elements, got %T %v", temps[0], temps[0])
	}
	temp, ok := entry[1].(float64)
	if !ok {
		t.Fatalf("temperatures[0][1]: expected float64, got %T", entry[1])
	}
	if temp < 41.99 || temp > 42.01 {
		t.Errorf("temperature: want 42.0, got %f", temp)
	}
}

func TestTemperature_NoZones(t *testing.T) {
	// Empty directory: no thermal_zone* entries → no messages, no error.
	p := &Temperature{sysfsPath: t.TempDir(), interval: 5 * time.Millisecond}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages with no thermal zones, got %d", n)
	}
}

func TestTemperature_ContextCancel(t *testing.T) {
	p := &Temperature{sysfsPath: "/sys/class/thermal", interval: time.Hour}
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

// ─── RebootRequired tests ──────────────────────────────────────────────────

func TestRebootRequired_HappyPath(t *testing.T) {
	client := &rebootSeqClient{vals: []bool{true}}
	p := &RebootRequiredPlugin{snapd: client, interval: 5 * time.Millisecond}
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
	if got := msg["type"]; got != "reboot-required-info" {
		t.Errorf("type: want %q, got %q", "reboot-required-info", got)
	}
	flag, ok := msg["flag"].(bool)
	if !ok {
		t.Fatalf("flag: expected bool, got %T", msg["flag"])
	}
	if !flag {
		t.Errorf("flag: want true, got false")
	}
}

func TestRebootRequired_SnapdError(t *testing.T) {
	// When snapd returns errors, no messages should be sent.
	client := &rebootSeqClient{err: fmt.Errorf("snapd unavailable")}
	p := &RebootRequiredPlugin{snapd: client, interval: 5 * time.Millisecond}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on snapd error, got %d", n)
	}
}

func TestRebootRequired_ContextCancel(t *testing.T) {
	client := &rebootSeqClient{vals: []bool{false}}
	p := &RebootRequiredPlugin{snapd: client, interval: time.Hour}
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

// TestRebootRequired_SameValueNoRepeat verifies the data-watcher property:
// sending the same value repeatedly must not produce more than one message.
func TestRebootRequired_SameValueNoRepeat(t *testing.T) {
	// Always returns false — only the first sample should generate a message.
	client := &rebootSeqClient{vals: []bool{false}}
	p := &RebootRequiredPlugin{snapd: client, interval: 5 * time.Millisecond}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Let it run until context expires (many ticks, all returning false).
	p.Run(ctx, sink, nil) //nolint:errcheck

	msgs := sink.Messages()
	if len(msgs) != 1 {
		t.Errorf("want exactly 1 message for repeated same value, got %d", len(msgs))
	}
}

// TestRebootRequired_PackagesFieldPresent verifies that every outbound message
// contains a "packages" key with value []any{}.
func TestRebootRequired_PackagesFieldPresent(t *testing.T) {
	client := &rebootSeqClient{vals: []bool{true}}
	p := &RebootRequiredPlugin{snapd: client, interval: 5 * time.Millisecond}
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
	pkgs, ok := msg["packages"]
	if !ok {
		t.Fatalf("packages key missing from reboot-required-info message")
	}
	pkgSlice, ok := pkgs.([]any)
	if !ok {
		t.Fatalf("packages: expected []any, got %T", pkgs)
	}
	if len(pkgSlice) != 0 {
		t.Errorf("packages: want empty slice, got %v", pkgSlice)
	}
}

// TestRebootRequired_ValueChange verifies that a value change produces a second message.
func TestRebootRequired_ValueChange(t *testing.T) {
	// First call returns false, subsequent calls return true.
	client := &rebootSeqClient{vals: []bool{false, true}}
	p := &RebootRequiredPlugin{snapd: client, interval: 5 * time.Millisecond}
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
		t.Fatalf("want ≥2 messages after value change, got %d", len(msgs))
	}
	flag0, _ := msgs[0]["flag"].(bool)
	flag1, _ := msgs[1]["flag"].(bool)
	if flag0 != false {
		t.Errorf("msgs[0].flag: want false, got %v", flag0)
	}
	if flag1 != true {
		t.Errorf("msgs[1].flag: want true, got %v", flag1)
	}
}
