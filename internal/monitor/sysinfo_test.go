package monitor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

const ciMeminfoFixture = `MemTotal:       8192000 kB
MemFree:        4096000 kB
Buffers:          204800 kB
Cached:          819200 kB
SwapCached:           0 kB
SwapTotal:      2097152 kB
SwapFree:       2097152 kB
`

const osReleaseFixture = `NAME="Ubuntu"
PRETTY_NAME="Ubuntu 24.04 LTS"
VERSION_ID="24.04"
VERSION_CODENAME=noble
`

const machineIDFixture = "aabbccddeeff00112233445566778899\n"

const cpuInfoX86Fixture = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz
cache size	: 12288 KB

processor	: 1
vendor_id	: GenuineIntel
model name	: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz
cache size	: 12288 KB

`

const mountsFixture = `/dev/sda1 / ext4 rw,relatime 0 0
tmpfs /tmp tmpfs rw,nosuid,nodev 0 0
`

const passwdFixture = `root:x:0:0:root:/root:/bin/bash
alice:x:1000:1000:Alice Smith,Office,555-1234,555-5678:/home/alice:/bin/bash
bob:x:1001:1001:Bob Jones,,, :/home/bob:/bin/bash
`

const groupFixture = `root:x:0:
alice:x:1000:alice
staff:x:50:alice,bob
`

// atomicWriteFixture atomically replaces the file at path with content by
// writing to a sibling temp file and renaming. This prevents the plugin from
// reading a partially-written (truncated) file when both goroutines race.
func atomicWriteFixture(t *testing.T, path, content string) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fixture-*")
	if err != nil {
		t.Fatalf("atomicWriteFixture: create temp: %v", err)
	}
	name := tmp.Name()
	if _, err := fmt.Fprint(tmp, content); err != nil {
		tmp.Close()
		os.Remove(name)
		t.Fatalf("atomicWriteFixture: write: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		t.Fatalf("atomicWriteFixture: close: %v", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		t.Fatalf("atomicWriteFixture: rename: %v", err)
	}
}

// ─── ComputerInfo tests ────────────────────────────────────────────────────

func TestComputerInfo_HappyPath(t *testing.T) {
	dir := t.TempDir()
	meminfoPath := filepath.Join(dir, "meminfo")
	osReleasePath := filepath.Join(dir, "os-release")
	machineIDPath := filepath.Join(dir, "machine-id")
	writeFixture(t, meminfoPath, ciMeminfoFixture)
	writeFixture(t, osReleasePath, osReleaseFixture)
	writeFixture(t, machineIDPath, machineIDFixture)

	mockSnapd := &snapd.MockClient{
		AssertionsData: &snapd.Assertions{
			Serial: "serial-123",
			Model:  "my-model",
			Brand:  "my-brand",
		},
	}

	p := &ComputerInfo{
		meminfoPath:   meminfoPath,
		osReleasePath: osReleasePath,
		machineIDPath: machineIDPath,
		snapdClient:   mockSnapd,
		interval:      5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Wait for the three sub-messages (computer-info, distribution-info, snap-info).
	msgs := waitForMessages(t, sink, 3, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	byType := make(map[string]map[string]any)
	for _, m := range msgs {
		if tp, ok := m["type"].(string); ok {
			byType[tp] = m
		}
	}

	ci, ok := byType["computer-info"]
	if !ok {
		t.Fatal("expected computer-info message")
	}
	if ci["total-memory"] != int64(8000) {
		t.Errorf("total-memory: want 8000, got %v", ci["total-memory"])
	}
	if ci["total-swap"] != int64(2048) {
		t.Errorf("total-swap: want 2048, got %v", ci["total-swap"])
	}
	if ci["machine-id"] != "aabbccddeeff00112233445566778899" {
		t.Errorf("machine-id: want aabbccddeeff00112233445566778899, got %v", ci["machine-id"])
	}

	di, ok := byType["distribution-info"]
	if !ok {
		t.Fatal("expected distribution-info message")
	}
	if di["distributor-id"] != "Ubuntu" {
		t.Errorf("distributor-id: want Ubuntu, got %v", di["distributor-id"])
	}
	if di["description"] != "Ubuntu 24.04 LTS" {
		t.Errorf("description: want Ubuntu 24.04 LTS, got %v", di["description"])
	}
	if di["release"] != "24.04" {
		t.Errorf("release: want 24.04, got %v", di["release"])
	}
	if di["code-name"] != "noble" {
		t.Errorf("code-name: want noble, got %v", di["code-name"])
	}

	si, ok := byType["snap-info"]
	if !ok {
		t.Fatal("expected snap-info message")
	}
	if si["serial"] != "serial-123" {
		t.Errorf("serial: want serial-123, got %v", si["serial"])
	}
	if si["model"] != "my-model" {
		t.Errorf("model: want my-model, got %v", si["model"])
	}
	if si["brand"] != "my-brand" {
		t.Errorf("brand: want my-brand, got %v", si["brand"])
	}
}

func TestComputerInfo_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	p := &ComputerInfo{
		meminfoPath:   filepath.Join(dir, "noexist-meminfo"),
		osReleasePath: filepath.Join(dir, "noexist-osrelease"),
		machineIDPath: filepath.Join(dir, "noexist-machineid"),
		snapdClient:   nil,
		interval:      5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
}

func TestComputerInfo_ContextCancel(t *testing.T) {
	p := &ComputerInfo{
		meminfoPath:   "/proc/meminfo",
		osReleasePath: "/etc/os-release",
		machineIDPath: "/etc/machine-id",
		snapdClient:   nil,
		interval:      time.Hour,
	}
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

func TestComputerInfo_DataWatcher(t *testing.T) {
	dir := t.TempDir()
	meminfoPath := filepath.Join(dir, "meminfo")
	osReleasePath := filepath.Join(dir, "os-release")
	machineIDPath := filepath.Join(dir, "machine-id")
	writeFixture(t, meminfoPath, ciMeminfoFixture)
	writeFixture(t, osReleasePath, osReleaseFixture)
	writeFixture(t, machineIDPath, machineIDFixture)

	p := &ComputerInfo{
		meminfoPath:   meminfoPath,
		osReleasePath: osReleasePath,
		machineIDPath: machineIDPath,
		snapdClient:   nil,
		interval:      5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Wait for initial messages (computer-info + distribution-info; snap-info omitted since snapdClient=nil with empty values still sends on first run).
	waitForMessages(t, sink, 2, 500*time.Millisecond)

	// Let multiple ticks pass with the same data.
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// With same data, only the initial messages should have been sent.
	// The snap-info might or might not be sent depending on the nil client returning empty strings.
	// At minimum, computer-info and distribution-info should each appear exactly once.
	countByType := make(map[string]int)
	for _, m := range sink.Messages() {
		if tp, ok := m["type"].(string); ok {
			countByType[tp]++
		}
	}
	if countByType["computer-info"] != 1 {
		t.Errorf("computer-info count: want 1, got %d", countByType["computer-info"])
	}
	if countByType["distribution-info"] != 1 {
		t.Errorf("distribution-info count: want 1, got %d", countByType["distribution-info"])
	}
}

func TestComputerInfo_DataWatcherChange(t *testing.T) {
	dir := t.TempDir()
	meminfoPath := filepath.Join(dir, "meminfo")
	osReleasePath := filepath.Join(dir, "os-release")
	machineIDPath := filepath.Join(dir, "machine-id")
	writeFixture(t, meminfoPath, ciMeminfoFixture)
	writeFixture(t, osReleasePath, osReleaseFixture)
	writeFixture(t, machineIDPath, machineIDFixture)

	p := &ComputerInfo{
		meminfoPath:   meminfoPath,
		osReleasePath: osReleasePath,
		machineIDPath: machineIDPath,
		snapdClient:   nil,
		interval:      5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Wait for initial messages.
	waitForMessages(t, sink, 2, 500*time.Millisecond)
	initialCount := len(sink.Messages())

	// Update meminfo: change total memory. Use atomic write so the plugin
	// never reads a partially-truncated file between ticks.
	atomicWriteFixture(t, meminfoPath, `MemTotal:       16384000 kB
MemFree:        8192000 kB
Buffers:          204800 kB
Cached:          819200 kB
SwapCached:           0 kB
SwapTotal:      2097152 kB
SwapFree:       2097152 kB
`)

	// Wait for a new computer-info message.
	waitForMessages(t, sink, initialCount+1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var gotCompInfo bool
	for _, m := range sink.Messages()[initialCount:] {
		if m["type"] == "computer-info" {
			gotCompInfo = true
			if m["total-memory"] != int64(16000) {
				t.Errorf("updated total-memory: want 16000, got %v", m["total-memory"])
			}
		}
	}
	if !gotCompInfo {
		t.Error("expected a new computer-info message after memory change")
	}
}

// ─── ProcessorInfo tests ───────────────────────────────────────────────────

func TestProcessorInfo_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	writeFixture(t, path, cpuInfoX86Fixture)

	p := &ProcessorInfo{cpuinfoPath: path, interval: 5 * time.Millisecond, delay: 0}
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
	if msg["type"] != "processor-info" {
		t.Errorf("type: want processor-info, got %v", msg["type"])
	}
	procs, ok := msg["processors"].([]map[string]any)
	if !ok || len(procs) != 2 {
		t.Fatalf("processors: expected 2-element []map[string]any, got %T len=%d", msg["processors"], len(procs))
	}
	p0 := procs[0]
	if p0["processor-id"] != 0 {
		t.Errorf("processor-id: want 0, got %v", p0["processor-id"])
	}
	if p0["vendor"] != "GenuineIntel" {
		t.Errorf("vendor: want GenuineIntel, got %v", p0["vendor"])
	}
	if p0["model"] != "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz" {
		t.Errorf("model: want Intel(R) ..., got %v", p0["model"])
	}
	if p0["cache-size"] != 12288 {
		t.Errorf("cache-size: want 12288, got %v", p0["cache-size"])
	}
}

func TestProcessorInfo_MissingFile(t *testing.T) {
	p := &ProcessorInfo{
		cpuinfoPath: filepath.Join(t.TempDir(), "nonexistent"),
		interval:    5 * time.Millisecond,
		delay:       0,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on missing file, got %d", n)
	}
}

func TestProcessorInfo_ContextCancel(t *testing.T) {
	p := &ProcessorInfo{cpuinfoPath: "/proc/cpuinfo", interval: time.Hour, delay: time.Hour}
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

func TestProcessorInfo_DataWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	writeFixture(t, path, cpuInfoX86Fixture)

	p := &ProcessorInfo{cpuinfoPath: path, interval: 5 * time.Millisecond, delay: 0}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	waitForMessages(t, sink, 1, 500*time.Millisecond)
	// Let several more ticks pass — same data should not trigger new messages.
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if n := len(sink.Messages()); n != 1 {
		t.Errorf("data-watcher: want 1 message, got %d", n)
	}
}

func TestProcessorInfo_DataWatcherChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	writeFixture(t, path, cpuInfoX86Fixture)

	p := &ProcessorInfo{cpuinfoPath: path, interval: 5 * time.Millisecond, delay: 0}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	waitForMessages(t, sink, 1, 500*time.Millisecond)

	// Change the cpuinfo (different model name).
	atomicWriteFixture(t, path, `processor	: 0
vendor_id	: AuthenticAMD
model name	: AMD Ryzen 9 5900X
cache size	: 32768 KB

`)
	waitForMessages(t, sink, 2, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// ─── NetworkDevice tests ───────────────────────────────────────────────────

func makeNetMock(t *testing.T, sysNetPath string) (*NetworkDevice, func() ([]net.Interface, error)) {
	t.Helper()
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	iface := net.Interface{
		Index:        99,
		Name:         "eth0",
		HardwareAddr: mac,
		Flags:        net.FlagUp | net.FlagBroadcast,
	}

	if err := os.MkdirAll(filepath.Join(sysNetPath, "eth0"), 0755); err != nil {
		t.Fatalf("mkdir eth0: %v", err)
	}
	writeFixture(t, filepath.Join(sysNetPath, "eth0", "speed"), "1000\n")
	writeFixture(t, filepath.Join(sysNetPath, "eth0", "duplex"), "full\n")

	fn := func() ([]net.Interface, error) {
		return []net.Interface{iface}, nil
	}
	p := &NetworkDevice{
		interval:      5 * time.Millisecond,
		getInterfaces: fn,
		getAddrs: func(_ *net.Interface) ([]net.Addr, error) {
			ipnet := &net.IPNet{
				IP:   net.ParseIP("192.168.1.10").To4(),
				Mask: net.CIDRMask(24, 32),
			}
			return []net.Addr{ipnet}, nil
		},
		sysNetPath: sysNetPath,
	}
	return p, fn
}

func TestNetworkDevice_HappyPath(t *testing.T) {
	dir := t.TempDir()
	p, _ := makeNetMock(t, dir)
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
	if msg["type"] != "network-device" {
		t.Errorf("type: want network-device, got %v", msg["type"])
	}
	devices, ok := msg["devices"].([]map[string]any)
	if !ok || len(devices) == 0 {
		t.Fatalf("devices: expected non-empty []map[string]any, got %T", msg["devices"])
	}
	d := devices[0]
	if string(d["interface"].([]byte)) != "eth0" {
		t.Errorf("interface: want eth0, got %v", d["interface"])
	}
	if string(d["ip_address"].([]byte)) != "192.168.1.10" {
		t.Errorf("ip_address: want 192.168.1.10, got %v", d["ip_address"])
	}
	if string(d["mac_address"].([]byte)) != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("mac_address: want aa:bb:cc:dd:ee:ff, got %v", d["mac_address"])
	}

	speeds, ok := msg["device-speeds"].([]map[string]any)
	if !ok || len(speeds) == 0 {
		t.Fatalf("device-speeds: expected non-empty, got %T", msg["device-speeds"])
	}
	s := speeds[0]
	if s["speed"] != 1000 {
		t.Errorf("speed: want 1000, got %v", s["speed"])
	}
	if s["duplex"] != true {
		t.Errorf("duplex: want true, got %v", s["duplex"])
	}
}

func TestNetworkDevice_ContextCancel(t *testing.T) {
	p := &NetworkDevice{
		interval:      time.Hour,
		getInterfaces: net.Interfaces,
		getAddrs:      func(iface *net.Interface) ([]net.Addr, error) { return iface.Addrs() },
		sysNetPath:    "/sys/class/net",
	}
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

func TestNetworkDevice_DataWatcher(t *testing.T) {
	dir := t.TempDir()
	p, _ := makeNetMock(t, dir)
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	waitForMessages(t, sink, 1, 500*time.Millisecond)
	// Same data — no further messages.
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if n := len(sink.Messages()); n != 1 {
		t.Errorf("data-watcher: want 1 message, got %d", n)
	}
}

func TestNetworkDevice_DataWatcherChange(t *testing.T) {
	dir := t.TempDir()

	// First: speed=1000
	if err := os.MkdirAll(filepath.Join(dir, "eth0"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "eth0", "speed"), "1000\n")
	writeFixture(t, filepath.Join(dir, "eth0", "duplex"), "full\n")

	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	iface := net.Interface{Index: 99, Name: "eth0", HardwareAddr: mac, Flags: net.FlagUp}
	p := &NetworkDevice{
		interval:      5 * time.Millisecond,
		getInterfaces: func() ([]net.Interface, error) { return []net.Interface{iface}, nil },
		getAddrs:      func(_ *net.Interface) ([]net.Addr, error) { return nil, nil },
		sysNetPath:    dir,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	waitForMessages(t, sink, 1, 500*time.Millisecond)

	// Change speed.
	writeFixture(t, filepath.Join(dir, "eth0", "speed"), "100\n")

	waitForMessages(t, sink, 2, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// ─── MountInfo tests ───────────────────────────────────────────────────────

func makeMockStatvfs(totalMB, freeMB int64) func(string) (syscall.Statfs_t, error) {
	return func(_ string) (syscall.Statfs_t, error) {
		const bsize = int64(4096)
		return syscall.Statfs_t{
			Bsize:  bsize,
			Blocks: uint64(totalMB * 1024 * 1024 / bsize),
			Bfree:  uint64(freeMB * 1024 * 1024 / bsize),
		}, nil
	}
}

func TestMountInfo_HappyPath(t *testing.T) {
	dir := t.TempDir()
	mountsPath := filepath.Join(dir, "mounts")
	writeFixture(t, mountsPath, mountsFixture)

	p := &MountInfo{
		mountsPath: mountsPath,
		statvfs:    makeMockStatvfs(100, 50),
		interval:   5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// Expect mount-info + free-space on first tick.
	msgs := waitForMessages(t, sink, 2, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	byType := make(map[string]map[string]any)
	for _, m := range msgs {
		if tp, ok := m["type"].(string); ok {
			byType[tp] = m
		}
	}

	mi, ok := byType["mount-info"]
	if !ok {
		t.Fatal("expected mount-info message")
	}
	entries, ok := mi["mount-info"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("mount-info entries: expected non-empty []any, got %T", mi["mount-info"])
	}
	tuple, ok := entries[0].(bpickle.Tuple)
	if !ok || len(tuple) < 2 {
		t.Fatalf("mount-info entry: expected bpickle.Tuple with 2 elements, got %v", entries[0])
	}
	info, ok := tuple[1].(map[string]any)
	if !ok {
		t.Fatalf("mount-info map: expected map[string]any, got %T", tuple[1])
	}
	if info["device"] != "/dev/sda1" {
		t.Errorf("device: want /dev/sda1, got %v", info["device"])
	}
	if info["filesystem"] != "ext4" {
		t.Errorf("filesystem: want ext4, got %v", info["filesystem"])
	}
	if info["total-space"] != int64(100) {
		t.Errorf("total-space: want 100, got %v", info["total-space"])
	}

	fs, ok := byType["free-space"]
	if !ok {
		t.Fatal("expected free-space message")
	}
	fsEntries, ok := fs["free-space"].([]any)
	if !ok || len(fsEntries) == 0 {
		t.Fatalf("free-space entries: expected non-empty, got %T", fs["free-space"])
	}
	fsTuple, ok := fsEntries[0].(bpickle.Tuple)
	if !ok || len(fsTuple) < 3 {
		t.Fatalf("free-space entry: expected bpickle.Tuple with 3 elements, got %v", fsEntries[0])
	}
	if fsTuple[2] != int64(50) {
		t.Errorf("free-space value: want 50, got %v", fsTuple[2])
	}
}

func TestMountInfo_MissingFile(t *testing.T) {
	p := &MountInfo{
		mountsPath: filepath.Join(t.TempDir(), "nonexistent"),
		statvfs:    makeMockStatvfs(100, 50),
		interval:   5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on missing file, got %d", n)
	}
}

func TestMountInfo_ContextCancel(t *testing.T) {
	p := &MountInfo{
		mountsPath: "/proc/mounts",
		statvfs:    defaultStatfs,
		interval:   time.Hour,
	}
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

func TestMountInfo_DataWatcher(t *testing.T) {
	dir := t.TempDir()
	mountsPath := filepath.Join(dir, "mounts")
	writeFixture(t, mountsPath, mountsFixture)

	p := &MountInfo{
		mountsPath: mountsPath,
		statvfs:    makeMockStatvfs(100, 50),
		interval:   5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// First tick: 1 mount-info + 1 free-space = 2 messages.
	waitForMessages(t, sink, 2, 500*time.Millisecond)

	// Let several more ticks pass; free-space is sent every tick,
	// but mount-info should not repeat since the layout didn't change.
	time.Sleep(60 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	countByType := make(map[string]int)
	for _, m := range sink.Messages() {
		if tp, ok := m["type"].(string); ok {
			countByType[tp]++
		}
	}
	if countByType["mount-info"] != 1 {
		t.Errorf("mount-info count: want 1 (data-watcher), got %d", countByType["mount-info"])
	}
	if countByType["free-space"] < 2 {
		t.Errorf("free-space count: want ≥2, got %d", countByType["free-space"])
	}
}

func TestMountInfo_DataWatcherChange(t *testing.T) {
	dir := t.TempDir()
	mountsPath := filepath.Join(dir, "mounts")
	writeFixture(t, mountsPath, mountsFixture)

	p := &MountInfo{
		mountsPath: mountsPath,
		statvfs:    makeMockStatvfs(100, 50),
		interval:   5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	waitForMessages(t, sink, 2, 500*time.Millisecond)
	initialMountInfoCount := 0
	for _, m := range sink.Messages() {
		if m["type"] == "mount-info" {
			initialMountInfoCount++
		}
	}

	// Change mount layout: add a second ext4 device.
	atomicWriteFixture(t, mountsPath, `/dev/sda1 / ext4 rw,relatime 0 0
/dev/sdb1 /data ext4 rw,relatime 0 0
`)
	waitForMessages(t, sink, len(sink.Messages())+1, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	finalMountInfoCount := 0
	for _, m := range sink.Messages() {
		if m["type"] == "mount-info" {
			finalMountInfoCount++
		}
	}
	if finalMountInfoCount <= initialMountInfoCount {
		t.Errorf("expected new mount-info after layout change; count before=%d after=%d", initialMountInfoCount, finalMountInfoCount)
	}
}

// ─── Users tests ───────────────────────────────────────────────────────────

func TestUsers_HappyPath(t *testing.T) {
	dir := t.TempDir()
	passwdPath := filepath.Join(dir, "passwd")
	groupPath := filepath.Join(dir, "group")
	writeFixture(t, passwdPath, passwdFixture)
	writeFixture(t, groupPath, groupFixture)

	p := &UserMonitor{passwdPath: passwdPath, groupPath: groupPath, interval: 5 * time.Millisecond}
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
	if msg["type"] != "users" {
		t.Errorf("type: want users, got %v", msg["type"])
	}
	creates, ok := msg["create-users"].([]map[string]any)
	if !ok || len(creates) == 0 {
		t.Fatalf("create-users: expected non-empty, got %T", msg["create-users"])
	}
	// Find alice in create-users.
	var alice map[string]any
	for _, u := range creates {
		if u["username"] == "alice" {
			alice = u
		}
	}
	if alice == nil {
		t.Fatal("alice not found in create-users")
	}
	if alice["name"] != "Alice Smith" {
		t.Errorf("alice name: want Alice Smith, got %v", alice["name"])
	}
	if alice["uid"] != 1000 {
		t.Errorf("alice uid: want 1000, got %v", alice["uid"])
	}
	if alice["primary-gid"] != 1000 {
		t.Errorf("alice primary-gid: want 1000, got %v", alice["primary-gid"])
	}
	if alice["location"] != "Office" {
		t.Errorf("alice location: want Office, got %v", alice["location"])
	}
	if alice["work-phone"] != "555-1234" {
		t.Errorf("alice work-phone: want 555-1234, got %v", alice["work-phone"])
	}
	if alice["home-phone"] != "555-5678" {
		t.Errorf("alice home-phone: want 555-5678, got %v", alice["home-phone"])
	}
	if alice["enabled"] != true {
		t.Errorf("alice enabled: want true, got %v", alice["enabled"])
	}
}

func TestUsers_MissingFile(t *testing.T) {
	p := &UserMonitor{
		passwdPath: filepath.Join(t.TempDir(), "nonexistent-passwd"),
		groupPath:  filepath.Join(t.TempDir(), "nonexistent-group"),
		interval:   5 * time.Millisecond,
	}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx, sink, nil); err != nil {
		t.Errorf("Run should return nil on context cancel, got: %v", err)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("expected 0 messages on missing file, got %d", n)
	}
}

func TestUsers_ContextCancel(t *testing.T) {
	p := &UserMonitor{
		passwdPath: "/etc/passwd",
		groupPath:  "/etc/group",
		interval:   time.Hour,
	}
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

func TestUsers_CreateUpdateDelete(t *testing.T) {
	dir := t.TempDir()
	passwdPath := filepath.Join(dir, "passwd")
	groupPath := filepath.Join(dir, "group")

	// Initial state: alice and bob.
	atomicWriteFixture(t, passwdPath, `alice:x:1000:1000:Alice:/home/alice:/bin/bash
bob:x:1001:1001:Bob:/home/bob:/bin/bash
`)
	atomicWriteFixture(t, groupPath, `staff:x:50:alice,bob
`)

	p := &UserMonitor{passwdPath: passwdPath, groupPath: groupPath, interval: 5 * time.Millisecond}
	sink := &mockSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(ctx, sink, nil) }()

	// First tick: creates alice, bob, staff.
	waitForMessages(t, sink, 1, 500*time.Millisecond)

	// Update fixture: alice's name changes, bob is removed, carol is added.
	atomicWriteFixture(t, passwdPath, `alice:x:1000:1000:Alice Updated:/home/alice:/bin/bash
carol:x:1002:1002:Carol:/home/carol:/bin/bash
`)
	atomicWriteFixture(t, groupPath, `staff:x:50:alice,carol
`)

	// Second tick: update alice, delete bob, create carol.
	waitForMessages(t, sink, 2, 500*time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Second message is the diff.
	secondMsg := sink.Messages()[1]
	if secondMsg["type"] != "users" {
		t.Errorf("type: want users, got %v", secondMsg["type"])
	}

	// delete-users should contain bob.
	delUsers, ok := secondMsg["delete-users"].([]string)
	if !ok {
		t.Fatalf("delete-users: expected []string, got %T", secondMsg["delete-users"])
	}
	foundBob := false
	for _, u := range delUsers {
		if u == "bob" {
			foundBob = true
		}
	}
	if !foundBob {
		t.Errorf("expected bob in delete-users, got %v", delUsers)
	}

	// update-users should contain alice with updated name.
	updateUsers, ok := secondMsg["update-users"].([]map[string]any)
	if !ok {
		t.Fatalf("update-users: expected []map[string]any, got %T", secondMsg["update-users"])
	}
	foundAlice := false
	for _, u := range updateUsers {
		if u["username"] == "alice" && u["name"] == "Alice Updated" {
			foundAlice = true
		}
	}
	if !foundAlice {
		t.Errorf("expected updated alice in update-users, got %v", updateUsers)
	}

	// create-users should contain carol.
	createUsers, ok := secondMsg["create-users"].([]map[string]any)
	if !ok {
		t.Fatalf("create-users: expected []map[string]any, got %T", secondMsg["create-users"])
	}
	foundCarol := false
	for _, u := range createUsers {
		if u["username"] == "carol" {
			foundCarol = true
		}
	}
	if !foundCarol {
		t.Errorf("expected carol in create-users, got %v", createUsers)
	}
}
