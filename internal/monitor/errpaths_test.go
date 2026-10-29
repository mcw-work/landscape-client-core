package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// errSink records every message it is offered and rejects all of them.
type errSink struct {
	mu   sync.Mutex
	msgs []exchange.Message
}

func (s *errSink) Send(_ context.Context, msg exchange.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return errors.New("sink unavailable")
}

func (s *errSink) Attempts() []exchange.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

// waitUntil polls cond until it holds or a 2s deadline expires.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// corruptAccessor returns an accessor whose state file cannot be decoded and
// has no backup, so both GetPluginState and SetPluginState fail.
func corruptAccessor(t *testing.T, name string) *persist.PluginStateAccessor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	writeFixture(t, path, "{not json")
	return persist.New(path).Accessor(name, nil)
}

// seededStore returns a store with v already persisted under name.
func seededStore(t *testing.T, name string, v any) *persist.Store {
	t.Helper()
	store := newStore(t)
	if err := store.Accessor(name, nil).SetPluginState(v); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return store
}

// countBeats wires a heartbeat whose clock counts calls, i.e. ticks started.
func countBeats(p interface{ setHeartbeat(*Heartbeat) }) *atomic.Int64 {
	var n atomic.Int64
	p.setHeartbeat(NewHeartbeat(func() time.Time {
		n.Add(1)
		return time.Now()
	}))
	return &n
}

// waitTicks waits until n ticks have started, so n-1 have fully completed.
func waitTicks(t *testing.T, beats *atomic.Int64, n int64) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("%d ticks", n), func() bool { return beats.Load() >= n })
}

// startPlugin runs p in the background and returns an idempotent stop.
func startPlugin(t *testing.T, p Plugin, sink exchange.MessageSink, state *persist.PluginStateAccessor) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, sink, state) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run returned %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func attemptTypes(msgs []exchange.Message) []string {
	types := make([]string, 0, len(msgs))
	for _, m := range msgs {
		types = append(types, fmt.Sprint(m["type"]))
	}
	return types
}

// ─── ActiveProcessInfo ─────────────────────────────────────────────────────

func newProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "stat"), "cpu 1 2 3 4\nbtime 1000\n")
	writeFixture(t, filepath.Join(root, "uptime"), "500.0 100.0\n")
	writeProc(t, root, 1, procStatLine(1, "init", nil), procStatus(100))
	return root
}

func TestActiveProcessInfo_SendFailureIsTolerated(t *testing.T) {
	p := &ActiveProcessInfo{procRoot: newProcFixture(t), interval: 5 * time.Millisecond}
	sink := &errSink{}
	stop := startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	stop()
	if got := sink.Attempts()[0]["kill-all-processes"]; got != true {
		t.Errorf("first attempt kill-all-processes: want true, got %v", got)
	}
}

func TestActiveProcessInfo_UnchangedTableSendsNothing(t *testing.T) {
	p := &ActiveProcessInfo{procRoot: newProcFixture(t), interval: 5 * time.Millisecond}
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 4)
	stop()
	if n := len(sink.Messages()); n != 1 {
		t.Errorf("want only the initial snapshot, got %d messages", n)
	}
}

// ─── ComputerInfo ──────────────────────────────────────────────────────────

func TestComputerInfo_StateAndSendFailuresStillReportEverything(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	osRelease := filepath.Join(dir, "os-release")
	machineID := filepath.Join(dir, "machine-id")
	writeFixture(t, meminfo, "garbage\nMemTotal: lots kB\nMemTotal: 2048 kB\nSwapTotal: 1024 kB\n")
	writeFixture(t, osRelease, "# no separator here\nNAME=\"Ubuntu\"\n")
	writeFixture(t, machineID, "abc\n")

	p := &ComputerInfo{
		meminfoPath:   meminfo,
		osReleasePath: osRelease,
		machineIDPath: machineID,
		snapdClient:   &snapd.MockClient{AssertionsData: &snapd.Assertions{Serial: "SER", Model: "MOD", Brand: "BR"}},
		getHostname:   func() (string, error) { return "host", nil },
		interval:      time.Hour,
	}
	sink := &errSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitUntil(t, "three send attempts", func() bool { return len(sink.Attempts()) >= 3 })
	stop()

	msgs := sink.Attempts()
	if got, want := attemptTypes(msgs), []string{"computer-info", "distribution-info", "snap-info"}; !slices.Equal(got, want) {
		t.Fatalf("attempted types: want %v, got %v", want, got)
	}
	if got := msgs[0]["total-memory"]; got != int64(2) {
		t.Errorf("total-memory: want 2, got %v", got)
	}
	if got := msgs[0]["total-swap"]; got != int64(1) {
		t.Errorf("total-swap: want 1, got %v", got)
	}
	if got := msgs[1]["distributor-id"]; got != "Ubuntu" {
		t.Errorf("distributor-id: want Ubuntu, got %v", got)
	}
	if got := msgs[2]["serial"]; got != "SER" {
		t.Errorf("serial: want SER, got %v", got)
	}
}

func TestComputerInfo_SnapdErrorYieldsNoAssertions(t *testing.T) {
	p := &ComputerInfo{snapdClient: &snapd.MockClient{Err: errors.New("snapd down")}}
	serial, model, brand := p.readSnapAssertions(context.Background())
	if serial != "" || model != "" || brand != "" {
		t.Errorf("want empty assertions on error, got %q %q %q", serial, model, brand)
	}
}

// ─── CPU / load / memory / network-activity samplers ───────────────────────

func TestCPUUsage_NoDeltaSendsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	writeFixture(t, path, "cpu 1 1 1 1\n")
	p := &CPUUsage{procStatPath: path, interval: 5 * time.Millisecond}
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("unchanged counters must not produce samples, got %d messages", n)
	}
}

func TestCPUUsage_SendFailureIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	writeFixture(t, path, "cpu 0 0 0 0\n")
	p := &CPUUsage{procStatPath: path, interval: 5 * time.Millisecond}
	sink := &errSink{}
	startPlugin(t, p, sink, nil)
	i := 0
	waitUntil(t, "a send attempt", func() bool {
		i++
		atomicWriteFixture(t, path, fmt.Sprintf("cpu %d 0 0 %d\n", i*10, i*10))
		return len(sink.Attempts()) >= 1
	})
	if got := sink.Attempts()[0]["type"]; got != "cpu-usage" {
		t.Errorf("type: want cpu-usage, got %v", got)
	}
}

func TestLoadAverage_BadFileSendsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loadavg")
	writeFixture(t, path, "not-a-number\n")
	p := &LoadAverage{procLoadavgPath: path, interval: 5 * time.Millisecond}
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("want no messages, got %d", n)
	}
}

func TestLoadAverage_SendFailureIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loadavg")
	writeFixture(t, path, "0.50 0.40 0.30 1/100 123\n")
	p := &LoadAverage{procLoadavgPath: path, interval: 5 * time.Millisecond}
	sink := &errSink{}
	startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	if got := sink.Attempts()[0]["type"]; got != "load-average" {
		t.Errorf("type: want load-average, got %v", got)
	}
}

func TestMemoryInfo_BadFileSendsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	writeFixture(t, path, "MemFree: 1 kB\n")
	p := &MemoryInfo{procMeminfoPath: path, interval: 5 * time.Millisecond}
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("want no messages, got %d", n)
	}
}

func TestMemoryInfo_SendFailureIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	writeFixture(t, path, "MemFree: 1024 kB\nSwapFree: 1024 kB\n")
	p := &MemoryInfo{procMeminfoPath: path, interval: 5 * time.Millisecond}
	sink := &errSink{}
	startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	if got := sink.Attempts()[0]["type"]; got != "memory-info" {
		t.Errorf("type: want memory-info, got %v", got)
	}
}

func TestNetworkActivity_SendFailureIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	devContent := func(n int) string {
		return fmt.Sprintf("h1\nh2\n eth0: %d 0 0 0 0 0 0 0 %d 0\n", n, n)
	}
	writeFixture(t, path, devContent(0))
	p := &NetworkActivity{procNetDevPath: path, interval: 5 * time.Millisecond}
	sink := &errSink{}
	startPlugin(t, p, sink, nil)
	i := 0
	waitUntil(t, "a send attempt", func() bool {
		i++
		atomicWriteFixture(t, path, devContent(i*100))
		return len(sink.Attempts()) >= 1
	})
	if got := sink.Attempts()[0]["type"]; got != "network-activity" {
		t.Errorf("type: want network-activity, got %v", got)
	}
}

// Samples within the send window are buffered, not sent one per tick.
func TestSamplers_BufferUntilSendWindow(t *testing.T) {
	dir := t.TempDir()
	loadPath := filepath.Join(dir, "loadavg")
	memPath := filepath.Join(dir, "meminfo")
	cpuPath := filepath.Join(dir, "stat")
	writeFixture(t, loadPath, "0.50 0.40 0.30 1/100 123\n")
	writeFixture(t, memPath, "MemFree: 1024 kB\nSwapFree: 1024 kB\n")
	writeFixture(t, cpuPath, "cpu 0 0 0 0\n")

	load := &LoadAverage{procLoadavgPath: loadPath, interval: 5 * time.Millisecond, sendInterval: time.Hour}
	mem := &MemoryInfo{procMeminfoPath: memPath, interval: 5 * time.Millisecond, sendInterval: time.Hour}
	cpu := &CPUUsage{procStatPath: cpuPath, interval: 5 * time.Millisecond, sendInterval: time.Hour}
	loadBeats, memBeats, cpuBeats := countBeats(load), countBeats(mem), countBeats(cpu)
	sink := &mockSink{}
	stops := []func(){
		startPlugin(t, load, sink, nil),
		startPlugin(t, mem, sink, nil),
		startPlugin(t, cpu, sink, nil),
	}
	i := 0
	waitUntil(t, "several ticks of every sampler", func() bool {
		i++
		atomicWriteFixture(t, cpuPath, fmt.Sprintf("cpu %d 0 0 %d\n", i*10, i*10))
		return loadBeats.Load() >= 3 && memBeats.Load() >= 3 && cpuBeats.Load() >= 4
	})
	for _, stop := range stops {
		stop()
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("samples inside the send window must be buffered, got %d messages", n)
	}
}

// ─── HardwareInfo ──────────────────────────────────────────────────────────

func TestHardwareInfo_RetriesOnTickAfterRunFailure(t *testing.T) {
	var calls atomic.Int32
	p := &HardwareInfo{
		interval: 5 * time.Millisecond,
		run: func(context.Context) ([]byte, error) {
			if calls.Add(1) == 1 {
				return nil, errors.New("lshw exploded")
			}
			return []byte("<list><node id=\"x\"/></list>"), nil
		},
	}
	sink := &errSink{}
	stop := startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	stop()
	if calls.Load() < 2 {
		t.Errorf("want a periodic retry after the startup failure, got %d runs", calls.Load())
	}
	if got := string(sink.Attempts()[0]["data"].([]byte)); got != "<list><node id=\"x\"/></list>" {
		t.Errorf("data: got %q", got)
	}
}

func TestNewHardwareInfo_RunHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHardwareInfo().run(ctx); err == nil {
		t.Error("want an error running lshw with a cancelled context")
	}
}

// ─── MountInfo ─────────────────────────────────────────────────────────────

func newMountFixture(t *testing.T) *MountInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mounts")
	writeFixture(t, path, "/dev/sda1 / ext4 rw 0 0\n")
	return &MountInfo{mountsPath: path, statvfs: makeMockStatvfs(100, 50), interval: 5 * time.Millisecond}
}

func TestMountInfo_PersistsStateEvenWhenSendFails(t *testing.T) {
	p := newMountFixture(t)
	store := newStore(t)
	sink := &errSink{}
	stop := startPlugin(t, p, sink, store.Accessor(p.Name(), nil))
	waitUntil(t, "two send attempts", func() bool { return len(sink.Attempts()) >= 2 })
	stop()

	if got, want := attemptTypes(sink.Attempts()), []string{"mount-info", "free-space"}; !slices.Equal(got, want) {
		t.Errorf("attempted types: want %v, got %v", want, got)
	}
	var saved mountInfoState
	if err := store.Accessor(p.Name(), nil).GetPluginState(&saved); err != nil {
		t.Fatalf("GetPluginState: %v", err)
	}
	if saved.Hash == "" || saved.FreeSpaceHash == "" {
		t.Errorf("hashes not persisted: %+v", saved)
	}
}

func TestMountInfo_SaveFailureSuppressesSend(t *testing.T) {
	p := newMountFixture(t)
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("unsaved change must not be sent, got %d messages", n)
	}
}

// ─── NetworkDevice ─────────────────────────────────────────────────────────

func TestNetworkDevice_PersistsHashEvenWhenSendFails(t *testing.T) {
	p, _ := makeNetMock(t, t.TempDir())
	store := newStore(t)
	sink := &errSink{}
	stop := startPlugin(t, p, sink, store.Accessor(p.Name(), nil))
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	stop()

	var saved networkDeviceState
	if err := store.Accessor(p.Name(), nil).GetPluginState(&saved); err != nil {
		t.Fatalf("GetPluginState: %v", err)
	}
	if saved.Hash == "" {
		t.Error("hash not persisted")
	}
}

func TestNetworkDevice_SaveFailureSuppressesSend(t *testing.T) {
	p, _ := makeNetMock(t, t.TempDir())
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("unsaved change must not be sent, got %d messages", n)
	}
}

func TestNetworkDevice_InterfaceListErrorSendsNothing(t *testing.T) {
	p := &NetworkDevice{
		interval:      5 * time.Millisecond,
		getInterfaces: func() ([]net.Interface, error) { return nil, errors.New("netlink down") },
	}
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("want no messages, got %d", n)
	}
}

func TestNewNetworkDevice_GetAddrsMatchesInterfaceAddrs(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		t.Skipf("no interfaces available: %v", err)
	}
	want, wantErr := ifaces[0].Addrs()
	got, gotErr := NewNetworkDevice().getAddrs(&ifaces[0])
	if (wantErr == nil) != (gotErr == nil) || len(got) != len(want) {
		t.Errorf("getAddrs(%s): want %v/%v, got %v/%v", ifaces[0].Name, want, wantErr, got, gotErr)
	}
}

// ─── ProcessorInfo ─────────────────────────────────────────────────────────

func TestProcessorInfo_StateAndSendFailuresAreTolerated(t *testing.T) {
	p := &ProcessorInfo{cpuinfoPath: writeTemp(t, cpuInfoX86Fixture), interval: time.Hour}
	sink := &errSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	stop()
	msg := sink.Attempts()[0]
	if msg["type"] != "processor-info" {
		t.Errorf("type: want processor-info, got %v", msg["type"])
	}
	if procs, _ := msg["processors"].([]map[string]any); len(procs) != 2 {
		t.Errorf("processors: want 2, got %v", msg["processors"])
	}
}

// ─── RebootRequired ────────────────────────────────────────────────────────

func TestRebootRequired_PersistedSameValueIsNotResent(t *testing.T) {
	p := NewRebootRequired(&snapd.MockClient{RebootRequired: true})
	p.interval = 5 * time.Millisecond
	store := seededStore(t, p.Name(), rebootState{Initialized: true, Flag: true})
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, store.Accessor(p.Name(), nil))
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("value unchanged since restart, want no messages, got %d", n)
	}
}

func TestRebootRequired_PersistedChangeIsSentAndSaved(t *testing.T) {
	p := NewRebootRequired(&snapd.MockClient{RebootRequired: true})
	p.interval = time.Hour
	store := seededStore(t, p.Name(), rebootState{Initialized: true, Flag: false})
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, store.Accessor(p.Name(), nil))
	msgs := waitForMessages(t, sink, 1, 2*time.Second)
	stop()
	if msgs[0]["flag"] != true {
		t.Errorf("flag: want true, got %v", msgs[0]["flag"])
	}
	var saved rebootState
	if err := store.Accessor(p.Name(), nil).GetPluginState(&saved); err != nil {
		t.Fatalf("GetPluginState: %v", err)
	}
	if !saved.Initialized || !saved.Flag {
		t.Errorf("persisted state: want initialized flag=true, got %+v", saved)
	}
}

func TestRebootRequired_SaveFailureSuppressesSend(t *testing.T) {
	p := NewRebootRequired(&snapd.MockClient{RebootRequired: true})
	p.interval = 5 * time.Millisecond
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("unsaved change must not be sent, got %d messages", n)
	}
}

func TestRebootRequired_SendFailureIsTolerated(t *testing.T) {
	p := NewRebootRequired(&snapd.MockClient{RebootRequired: true})
	p.interval = time.Hour
	sink := &errSink{}
	stop := startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	stop()
	if got := sink.Attempts()[0]["type"]; got != "reboot-required-info" {
		t.Errorf("type: want reboot-required-info, got %v", got)
	}
}

// ─── SnapPackages / SnapServices ───────────────────────────────────────────

func TestSnapPackages_SendNowUsesFallbackIDForDevmodeSnaps(t *testing.T) {
	p := NewSnapPackages(&snapd.MockClient{Snaps: []snapd.SnapInfo{{Name: "devsnap", Version: "1"}}})
	sink := &mockSink{}
	p.SendNow(context.Background(), sink)
	msgs := sink.Messages()
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	installed := msgs[0]["snaps"].(map[string]any)["installed"].([]any)
	if got := installed[0].(map[string]any)["id"]; got != "no-serial-devsnap" {
		t.Errorf("id: want no-serial-devsnap, got %v", got)
	}
}

func TestSnapPackages_SendNowToleratesSendFailure(t *testing.T) {
	p := NewSnapPackages(&snapd.MockClient{})
	sink := &errSink{}
	p.SendNow(context.Background(), sink)
	if got := attemptTypes(sink.Attempts()); !slices.Equal(got, []string{"snaps"}) {
		t.Errorf("attempts: want [snaps], got %v", got)
	}
}

func snapServicesFixture() []snapd.ServiceInfo {
	return []snapd.ServiceInfo{{Snap: "a", Name: "a.svc", Enabled: true, Active: true}}
}

func TestSnapServices_PersistedHashIsNotResent(t *testing.T) {
	p := NewSnapServices(&snapd.MockClient{Services: snapServicesFixture()})
	p.interval = 5 * time.Millisecond
	store := seededStore(t, p.Name(), snapServicesState{Hash: hashSnapServices(snapServicesFixture())})
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, store.Accessor(p.Name(), nil))
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("services unchanged since restart, want no messages, got %d", n)
	}
}

func TestSnapServices_SaveFailureSuppressesSend(t *testing.T) {
	p := NewSnapServices(&snapd.MockClient{Services: snapServicesFixture()})
	p.interval = 5 * time.Millisecond
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("unsaved change must not be sent, got %d messages", n)
	}
}

func TestSnapServices_SendFailureIsTolerated(t *testing.T) {
	p := NewSnapServices(&snapd.MockClient{Services: snapServicesFixture()})
	p.interval = 5 * time.Millisecond
	sink := &errSink{}
	startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	if got := sink.Attempts()[0]["type"]; got != "snap-services" {
		t.Errorf("type: want snap-services, got %v", got)
	}
}

// ─── Temperature ───────────────────────────────────────────────────────────

func TestTemperature_BadGlobSendsNothing(t *testing.T) {
	p := &Temperature{sysfsPath: filepath.Join(t.TempDir(), "["), interval: 5 * time.Millisecond}
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("want no messages, got %d", n)
	}
}

func TestTemperature_SendFailureIsTolerated(t *testing.T) {
	dir := t.TempDir()
	zone := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(zone, "temp"), "42000\n")
	p := &Temperature{sysfsPath: dir, interval: 5 * time.Millisecond}
	sink := &errSink{}
	startPlugin(t, p, sink, nil)
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	if got := sink.Attempts()[0]["thermal-zone"]; got != "thermal_zone0" {
		t.Errorf("thermal-zone: want thermal_zone0, got %v", got)
	}
}

// ─── Users ─────────────────────────────────────────────────────────────────

func newUsersFixture(t *testing.T) *UserMonitor {
	t.Helper()
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	writeFixture(t, passwd, passwdFixture)
	writeFixture(t, group, groupFixture)
	return &UserMonitor{passwdPath: passwd, groupPath: group, interval: 5 * time.Millisecond}
}

func TestUsers_StateAndSendFailuresAreTolerated(t *testing.T) {
	p := newUsersFixture(t)
	sink := &errSink{}
	stop := startPlugin(t, p, sink, corruptAccessor(t, p.Name()))
	waitUntil(t, "a send attempt", func() bool { return len(sink.Attempts()) >= 1 })
	stop()
	msg := sink.Attempts()[0]
	if msg["type"] != "users" {
		t.Errorf("type: want users, got %v", msg["type"])
	}
	if creates, _ := msg["create-users"].([]map[string]any); len(creates) != 3 {
		t.Errorf("create-users: want 3, got %v", msg["create-users"])
	}
}

func TestUsers_MissingGroupFileSendsNothing(t *testing.T) {
	p := newUsersFixture(t)
	p.groupPath = filepath.Join(t.TempDir(), "absent")
	beats := countBeats(p)
	sink := &mockSink{}
	stop := startPlugin(t, p, sink, nil)
	waitTicks(t, beats, 3)
	stop()
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("want no messages without a group file, got %d", n)
	}
}

// ─── Runner ────────────────────────────────────────────────────────────────

// A real plugin run by the Runner must be wired to the runner's heartbeat, and
// an unreadable store must not stop it from running.
func TestRunner_WiresHeartbeatIntoRealPlugins(t *testing.T) {
	dir := t.TempDir()
	zone := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(zone, "temp"), "42000\n")
	plugin := &Temperature{sysfsPath: dir, interval: 5 * time.Millisecond}

	statePath := filepath.Join(t.TempDir(), "state.json")
	writeFixture(t, statePath, "{not json")
	sink := &mockSink{}
	r := New([]Plugin{plugin}, sink, persist.New(statePath))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitUntil(t, "a heartbeat from temperature", func() bool {
		return slices.Contains(r.heartbeat.Stale(-time.Hour), "temperature")
	})
	waitForMessages(t, sink, 1, 2*time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
