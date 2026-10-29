package monitor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/snapd"
)

func TestWatchdogThreshold(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{15 * time.Second, 2 * time.Minute},
		{40 * time.Second, 2 * time.Minute},
		{time.Minute, 3 * time.Minute},
		{time.Hour, 3 * time.Hour},
	}
	for _, tc := range tests {
		if got := watchdogThreshold(tc.interval); got != tc.want {
			t.Errorf("watchdogThreshold(%s) = %s, want %s", tc.interval, got, tc.want)
		}
	}
}

func TestHeartbeat_StaleSourcesUsesPerSourceThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	hb.Beat("fast")
	hb.Beat("slow")
	hb.Beat("untracked")

	now = now.Add(5 * time.Minute)
	got := hb.StaleSources(map[string]time.Duration{
		"fast":  2 * time.Minute,
		"slow":  time.Hour,
		"never": time.Second,
	})
	if want := []string{"fast"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestHeartbeat_NilIsSafe(t *testing.T) {
	var hb *Heartbeat
	hb.Beat("x")
	if got := hb.Stale(time.Second); got != nil {
		t.Errorf("Stale on nil = %v", got)
	}
	if got := hb.StaleSources(map[string]time.Duration{"x": time.Second}); got != nil {
		t.Errorf("StaleSources on nil = %v", got)
	}
}

func TestRunner_StaleSourcesJudgesEachPluginByItsInterval(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New([]Plugin{&fakePlugin{name: "wedged"}, &fakePlugin{name: "healthy"}}, nil, nil)
	r.heartbeat = NewHeartbeat(func() time.Time { return now })
	r.heartbeat.Beat("wedged")
	r.heartbeat.Beat("healthy")

	// fakePlugin has a 1s interval, so the 2-minute floor applies.
	now = now.Add(3 * time.Minute)
	r.heartbeat.Beat("healthy")

	if got, want := r.StaleSources(), []string{"wedged"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseARM64_ModernKernel(t *testing.T) {
	cpuinfo := "processor\t: 0\nBogoMIPS\t: 108.00\nFeatures\t: fp asimd\n\n" +
		"processor\t: 1\nBogoMIPS\t: 108.00\n\n"
	p := &ProcessorInfo{cpuinfoPath: writeTemp(t, cpuinfo)}

	got := p.parseARM64()
	want := []map[string]any{
		{"processor-id": 0, "model": "arm"},
		{"processor-id": 1, "model": "arm"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Blocks with no "processor" line must get distinct sequential ids.
func TestParseARM64_LegacyBlocksWithoutProcessorLine(t *testing.T) {
	cpuinfo := "Processor\t: AArch64 Processor rev 4\nCache size\t: 512\n\n" +
		"Processor\t: Other\n"
	p := &ProcessorInfo{cpuinfoPath: writeTemp(t, cpuinfo)}

	got := p.parseARM64()
	want := []map[string]any{
		{"processor-id": 0, "model": "AArch64 Processor rev 4", "cache-size": 512},
		{"processor-id": 1, "model": "Other"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseARM64_ExplicitModelIsNotOverwritten(t *testing.T) {
	p := &ProcessorInfo{cpuinfoPath: writeTemp(t, "Processor\t: Cortex-A72\nprocessor\t: 3\n")}

	got := p.parseARM64()
	want := []map[string]any{{"processor-id": 3, "model": "Cortex-A72"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseARM64_MissingFile(t *testing.T) {
	p := &ProcessorInfo{cpuinfoPath: filepath.Join(t.TempDir(), "missing")}
	if got := p.parseARM64(); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestPluginConstructors_NamesAndIntervals(t *testing.T) {
	client := &snapd.MockClient{}
	plugins := []Plugin{
		NewActiveProcessInfo(),
		NewComputerInfo(client),
		NewCPUUsage(),
		NewHardwareInfo(),
		NewLoadAverage(),
		NewMemoryInfo(),
		NewMountInfo(),
		NewNetworkActivity(),
		NewNetworkDevice(),
		NewProcessorInfo(),
		NewRebootRequired(client),
		NewSnapPackages(client),
		NewSnapServices(client),
		NewTemperature(),
		NewUsers(),
	}
	seen := make(map[string]bool)
	for _, p := range plugins {
		name := p.Name()
		if name == "" {
			t.Errorf("%T has an empty name", p)
		}
		if seen[name] {
			t.Errorf("duplicate plugin name %q", name)
		}
		seen[name] = true
		if p.Interval() <= 0 {
			t.Errorf("%s: interval %s must be positive", name, p.Interval())
		}
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
