package monitor

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// mockSink collects messages sent by plugins.
type mockSink struct {
	mu   sync.Mutex
	msgs []exchange.Message
}

func (m *mockSink) Send(_ context.Context, msg exchange.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

func (m *mockSink) Messages() []exchange.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]exchange.Message, len(m.msgs))
	copy(cp, m.msgs)
	return cp
}

// writeFixture writes content to a file in a temp directory.
func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writeFixture: %v", err)
	}
}

// waitForMessages polls sink until at least n messages arrive or the deadline
// (relative to call time) expires.
func waitForMessages(t *testing.T, sink *mockSink, n int, deadline time.Duration) []exchange.Message {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		msgs := sink.Messages()
		if len(msgs) >= n {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %d message(s); got %d", deadline, n, len(sink.Messages()))
	return nil
}

// ---- CPUUsage tests --------------------------------------------------------

func TestCPUUsage_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")

	// Initial /proc/stat content (cpu line only — parser only reads first line).
	// Fields after "cpu": user nice system idle iowait irq softirq steal guest guest_nice
	// total = 10000+0+5000+30000 = 45000, idle = 30000
	writeFixture(t, path, "cpu  10000 0 5000 30000 0 0 0 0 0 0\n")

	p := &CPUUsage{procStatPath: path, interval: 5 * time.Millisecond}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Give the goroutine time to start and prime the baseline sample.
	time.Sleep(20 * time.Millisecond)

	// Update /proc/stat: total = 10600+0+5400+30600 = 46600, idle = 30600
	// total_delta = 1600, idle_delta = 600 → usage = 1000/1600 = 0.625
	writeFixture(t, path, "cpu  10600 0 5400 30600 0 0 0 0 0 0\n")

	msgs := waitForMessages(t, sink, 1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	msg := msgs[0]
	if got := msg["type"]; got != "cpu-usage" {
		t.Errorf("type: want %q, got %q", "cpu-usage", got)
	}
	points, ok := msg["cpu-usages"].([]any)
	if !ok || len(points) == 0 {
		t.Fatalf("cpu-usages: expected non-empty []any, got %T %v", msg["cpu-usages"], msg["cpu-usages"])
	}
	point, ok := points[0].(bpickle.Tuple)
	if !ok || len(point) < 2 {
		t.Fatalf("cpu-usages[0]: expected bpickle.Tuple with ≥2 elements, got %T %v", points[0], points[0])
	}
	usage, ok := point[1].(float64)
	if !ok {
		t.Fatalf("cpu-usages[0][1]: expected float64, got %T", point[1])
	}
	if usage < 0.624 || usage > 0.626 {
		t.Errorf("cpu usage: want ~0.625, got %f", usage)
	}
}

func TestCPUUsage_MissingFile(t *testing.T) {
	p := &CPUUsage{
		procStatPath: filepath.Join(t.TempDir(), "nonexistent"),
		interval:     5 * time.Millisecond,
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

func TestCPUUsage_ContextCancel(t *testing.T) {
	p := &CPUUsage{procStatPath: "/proc/stat", interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run is even called

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, &mockSink{}, (*persist.PluginStateAccessor)(nil)) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Run did not return after context cancel")
	}
}

// TestCPUUsage_BatchedMessageShapeIsUnchanged asserts batching is additive: the
// same field carries N tuples instead of 1, with identical tuple shape.
func TestCPUUsage_BatchedMessageShapeIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	writeFixture(t, statPath, "cpu  100 0 100 800 0 0 0 0 0 0\n")

	p := &CPUUsage{
		procStatPath: statPath,
		interval:     5 * time.Millisecond,
		sendInterval: 20 * time.Millisecond,
	}

	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// A static /proc/stat yields usage<0 forever, so nudge the counters once to
	// produce a real point; the accumulator then drains it after the window.
	time.Sleep(20 * time.Millisecond)
	writeFixture(t, statPath, "cpu  200 0 200 1400 0 0 0 0 0 0\n")

	msgs := waitForMessages(t, sink, 1, 1*time.Second)
	cancel()
	<-errCh

	points, ok := msgs[0]["cpu-usages"].([]any)
	if !ok {
		t.Fatalf("cpu-usages: want []any, got %T", msgs[0]["cpu-usages"])
	}
	if len(points) == 0 {
		t.Fatal("no points in the batched message")
	}
	if _, ok := points[0].(bpickle.Tuple); !ok {
		t.Errorf("point 0: want bpickle.Tuple, got %T", points[0])
	}
}

// ---- MemoryInfo tests -------------------------------------------------------

const meminfoFixture = `MemTotal:       16384 kB
MemFree:         8192 kB
MemAvailable:   10000 kB
Buffers:          512 kB
Cached:          2048 kB
SwapCached:         0 kB
SwapTotal:       4096 kB
SwapFree:        2048 kB
`

func TestMemoryInfo_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	writeFixture(t, path, meminfoFixture)
	// Expected: freeMemMB = (8192+512+2048)/1024 = 10, freeSwapMB = 2048/1024 = 2

	p := &MemoryInfo{procMeminfoPath: path, interval: 5 * time.Millisecond}
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
	if got := msg["type"]; got != "memory-info" {
		t.Errorf("type: want %q, got %q", "memory-info", got)
	}
	points, ok := msg["memory-info"].([]any)
	if !ok || len(points) == 0 {
		t.Fatalf("memory-info: expected non-empty []any, got %T %v", msg["memory-info"], msg["memory-info"])
	}
	point, ok := points[0].(bpickle.Tuple)
	if !ok || len(point) < 3 {
		t.Fatalf("memory-info[0]: expected bpickle.Tuple with ≥3 elements, got %T %v", points[0], points[0])
	}
	freeMemMB, ok := point[1].(int64)
	if !ok {
		t.Fatalf("memory-info[0][1]: expected int64, got %T", point[1])
	}
	freeSwapMB, ok := point[2].(int64)
	if !ok {
		t.Fatalf("memory-info[0][2]: expected int64, got %T", point[2])
	}
	if freeMemMB != 10 {
		t.Errorf("freeMemMB: want 10, got %d", freeMemMB)
	}
	if freeSwapMB != 2 {
		t.Errorf("freeSwapMB: want 2, got %d", freeSwapMB)
	}
}

func TestMemoryInfo_MissingFile(t *testing.T) {
	p := &MemoryInfo{
		procMeminfoPath: filepath.Join(t.TempDir(), "nonexistent"),
		interval:        5 * time.Millisecond,
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

func TestMemoryInfo_ContextCancel(t *testing.T) {
	p := &MemoryInfo{procMeminfoPath: "/proc/meminfo", interval: time.Hour}
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

// ---- LoadAverage tests ------------------------------------------------------

func TestLoadAverage_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loadavg")
	// /proc/loadavg format: "1min 5min 15min runnable/total last_pid"
	writeFixture(t, path, "0.50 0.75 1.00 1/100 12345\n")

	p := &LoadAverage{procLoadavgPath: path, interval: 5 * time.Millisecond}
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
	if got := msg["type"]; got != "load-average" {
		t.Errorf("type: want %q, got %q", "load-average", got)
	}
	points, ok := msg["load-averages"].([]any)
	if !ok || len(points) == 0 {
		t.Fatalf("load-averages: expected non-empty []any, got %T %v", msg["load-averages"], msg["load-averages"])
	}
	point, ok := points[0].(bpickle.Tuple)
	if !ok || len(point) < 2 {
		t.Fatalf("load-averages[0]: expected bpickle.Tuple with ≥2 elements, got %T %v", points[0], points[0])
	}
	load, ok := point[1].(float64)
	if !ok {
		t.Fatalf("load-averages[0][1]: expected float64, got %T", point[1])
	}
	if load < 0.49 || load > 0.51 {
		t.Errorf("load average: want ~0.50, got %f", load)
	}
}

func TestLoadAverage_MissingFile(t *testing.T) {
	p := &LoadAverage{
		procLoadavgPath: filepath.Join(t.TempDir(), "nonexistent"),
		interval:        5 * time.Millisecond,
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

func TestLoadAverage_ContextCancel(t *testing.T) {
	p := &LoadAverage{procLoadavgPath: "/proc/loadavg", interval: time.Hour}
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

func TestCounterDelta_NormalIncrement(t *testing.T) {
	if got := counterDelta(2000, 1000); got != 1000 {
		t.Errorf("want 1000, got %d", got)
	}
}

// TestCounterDelta_32BitRollover: on armhf the kernel counter wraps at 2^32, so
// a negative raw delta with a previous value near MaxUint32 is a wrap, not a
// reset. Clamping it to zero loses 4 GiB of traffic per interface per wrap.
func TestCounterDelta_32BitRollover(t *testing.T) {
	const maxUint32 = int64(1) << 32

	prev := maxUint32 - 1000 // 4294966296
	now := int64(500)        // wrapped

	want := int64(1500) // 1000 to the wrap point, then 500 more
	if got := counterDelta(now, prev); got != want {
		t.Errorf("want %d, got %d", want, got)
	}
}

// TestCounterDelta_GenuineReset: an interface re-created from scratch has a low
// previous value, so a negative delta there is a reset and must still clamp.
func TestCounterDelta_GenuineReset(t *testing.T) {
	prev := int64(5000)
	now := int64(10)

	if got := counterDelta(now, prev); got != 0 {
		t.Errorf("a genuine counter reset should clamp to 0, got %d", got)
	}
}

func TestCounterDelta_NoChange(t *testing.T) {
	if got := counterDelta(1000, 1000); got != 0 {
		t.Errorf("want 0, got %d", got)
	}
}
