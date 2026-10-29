package monitor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// procStatLine builds a /proc/<pid>/stat line; overrides replaces fields after
// "(comm) " by index (0=state, 11=utime, 12=stime, 19=starttime, 20=vsize).
func procStatLine(pid int64, comm string, overrides map[int]string) string {
	rest := strings.Fields("S 0 0 0 0 -1 0 0 0 0 0 10 5 0 0 20 0 1 0 100 1048576")
	for i, v := range overrides {
		rest[i] = v
	}
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(rest, " "))
}

func procStatus(vmSizeKB int) string {
	return fmt.Sprintf("Uid:\t1000 1000 1000 1000\nGid:\t1000 1000 1000 1000\nVmSize:\t %d kB\n", vmSizeKB)
}

// writeProc creates root/<pid>/stat and, when status is non-empty, root/<pid>/status.
func writeProc(t *testing.T, root string, pid int64, stat, status string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	atomicWriteFixture(t, filepath.Join(dir, "stat"), stat)
	if status != "" {
		atomicWriteFixture(t, filepath.Join(dir, "status"), status)
	}
}

func pidsOf(t *testing.T, v any) []int64 {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("want []any, got %T", v)
	}
	var pids []int64
	for _, e := range list {
		switch e := e.(type) {
		case int64:
			pids = append(pids, e)
		case map[string]any:
			pids = append(pids, e["pid"].(int64))
		default:
			t.Fatalf("unexpected entry %T", e)
		}
	}
	slices.Sort(pids)
	return pids
}

func TestActiveProcessInfo_DiffReportsCreatesUpdatesAndKills(t *testing.T) {
	root := newProcFixture(t) // pid 1
	writeProc(t, root, 2, procStatLine(2, "doomed", nil), procStatus(10))
	p := &ActiveProcessInfo{procRoot: root}

	first, err := p.buildMessage()
	if err != nil {
		t.Fatal(err)
	}
	if first["kill-all-processes"] != true {
		t.Errorf("first message must reset the server table: %v", first)
	}
	if got := pidsOf(t, first["add-processes"]); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("initial adds: want [1 2], got %v", got)
	}

	second, err := p.buildMessage()
	if err != nil || second != nil {
		t.Fatalf("unchanged table: want nil message, got %v (err %v)", second, err)
	}

	writeProc(t, root, 1, procStatLine(1, "init", nil), procStatus(200))
	if err := os.RemoveAll(filepath.Join(root, "2")); err != nil {
		t.Fatal(err)
	}
	writeProc(t, root, 3, procStatLine(3, "newcomer", nil), procStatus(30))

	third, err := p.buildMessage()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := third["kill-all-processes"]; ok {
		t.Error("kill-all-processes must only be sent on the first run")
	}
	if got := pidsOf(t, third["add-processes"]); !slices.Equal(got, []int64{3}) {
		t.Errorf("add-processes: want [3], got %v", got)
	}
	if got := pidsOf(t, third["update-processes"]); !slices.Equal(got, []int64{1}) {
		t.Errorf("update-processes: want [1], got %v", got)
	}
	if vm := third["update-processes"].([]any)[0].(map[string]any)["vm-size"]; vm != int64(200) {
		t.Errorf("updated vm-size: want 200, got %v", vm)
	}
	if got := pidsOf(t, third["kill-processes"]); !slices.Equal(got, []int64{2}) {
		t.Errorf("kill-processes: want [2], got %v", got)
	}
}

func TestActiveProcessInfo_SkipsUnreadableAndMalformedProcesses(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"self", "10", "11/stat"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(t, root, 12, "12 noparens S\n", "")
	writeProc(t, root, 13, "13 (short) S 1 2\n", "")
	writeProc(t, root, 14, procStatLine(14, "bad-utime", map[int]string{11: "x"}), "")
	writeProc(t, root, 15, procStatLine(15, "bad-stime", map[int]string{12: "x"}), "")
	writeProc(t, root, 16, procStatLine(16, "bad-start", map[int]string{19: "x"}), "")
	writeProc(t, root, 17, procStatLine(17, "bad-vsize", map[int]string{20: "x"}), "")
	writeProc(t, root, 18, procStatLine(18, "dead", map[int]string{0: "X"}), "")
	writeProc(t, root, 19, procStatLine(19, "no-status", nil), "")
	writeProc(t, root, 20, procStatLine(20, "long-status", nil), "Uid:\t5 5 5 5\n"+strings.Repeat("x", 70*1024)+"\n")

	p := &ActiveProcessInfo{procRoot: root}
	dst := make(map[int64]processInfo)
	if err := p.readAllProcessesInto(dst, 1000, 500); err != nil {
		t.Fatal(err)
	}
	var pids []int64
	for pid := range dst {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	if !slices.Equal(pids, []int64{19, 20}) {
		t.Fatalf("want only pids [19 20], got %v", pids)
	}
	if got := dst[19]; got.vmSize != 1024 || got.startTime != 1001 || got.uid != 0 {
		t.Errorf("pid 19 must fall back to stat values: %+v", got)
	}
	if got := dst[20].uid; got != 5 {
		t.Errorf("pid 20 uid parsed before the overlong line: want 5, got %d", got)
	}
}

func TestActiveProcessInfo_BuildMessageErrors(t *testing.T) {
	const absent = "\x00absent"
	cases := []struct {
		name, stat, uptime, want string
	}{
		{"missing stat", absent, "1 1", "cannot open proc/stat"},
		{"btime without value", "btime \n", "1 1", "malformed btime"},
		{"btime not numeric", "btime soon\n", "1 1", "cannot parse btime"},
		{"btime absent", "cpu 1 2 3 4\n", "1 1", "btime not found"},
		{"overlong stat line", "cpu " + strings.Repeat("1 ", 40000) + "\nbtime 1\n", "1 1", "cannot scan proc/stat"},
		{"missing uptime", "btime 1\n", absent, "cannot read proc/uptime"},
		{"empty uptime", "btime 1\n", "", "malformed proc/uptime"},
		{"uptime not numeric", "btime 1\n", "later 1", "cannot parse uptime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.stat != absent {
				writeFixture(t, filepath.Join(root, "stat"), tc.stat)
			}
			if tc.uptime != absent {
				writeFixture(t, filepath.Join(root, "uptime"), tc.uptime)
			}
			p := &ActiveProcessInfo{procRoot: root}
			msg, err := p.buildMessage()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v (msg %v)", tc.want, err, msg)
			}
		})
	}
}

func TestActiveProcessInfo_UnlistableProcRootIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permission")
	}
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "stat"), "btime 1\n")
	writeFixture(t, filepath.Join(root, "uptime"), "1 1\n")
	if err := os.Chmod(root, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	p := &ActiveProcessInfo{procRoot: root}
	if _, err := p.buildMessage(); err == nil || !strings.Contains(err.Error(), "cannot read processes") {
		t.Errorf("want a process listing error, got %v", err)
	}
}

func TestComputerInfo_ParsersSkipMalformedLines(t *testing.T) {
	dir := t.TempDir()
	p := &ComputerInfo{
		meminfoPath:   filepath.Join(dir, "meminfo"),
		osReleasePath: filepath.Join(dir, "os-release"),
	}
	writeFixture(t, p.meminfoPath, "lonely\nMemTotal: many kB\nMemTotal: 4096 kB\nSwapTotal: 2048 kB\n")
	writeFixture(t, p.osReleasePath, "no separator\nVERSION_ID=\"24.04\"\n")
	if mem, swap := p.readMeminfo(); mem != 4 || swap != 2 {
		t.Errorf("readMeminfo: want 4/2, got %d/%d", mem, swap)
	}
	if _, _, release, _ := p.readOSRelease(); release != "24.04" {
		t.Errorf("readOSRelease release: want 24.04, got %q", release)
	}
}

func TestSamplers_RejectMalformedInput(t *testing.T) {
	cases := []struct {
		name, content, want string
		sample              func(path string) error
	}{
		{"cpu empty", "", "cannot read first line", func(path string) error {
			_, err := (&CPUUsage{procStatPath: path}).sample()
			return err
		}},
		{"cpu wrong line", "intr 1 2 3 4\n", "unexpected format", func(path string) error {
			_, err := (&CPUUsage{procStatPath: path}).sample()
			return err
		}},
		{"cpu not numeric", "cpu 1 2 x 4\n", "cannot parse field", func(path string) error {
			_, err := (&CPUUsage{procStatPath: path}).sample()
			return err
		}},
		{"loadavg empty", "", "unexpected format", func(path string) error {
			_, err := (&LoadAverage{procLoadavgPath: path}).sample()
			return err
		}},
		{"loadavg not numeric", "busy 1 1\n", "cannot parse load average", func(path string) error {
			_, err := (&LoadAverage{procLoadavgPath: path}).sample()
			return err
		}},
		{"meminfo missing swap", "MemFree: 1 kB\n", "MemFree or SwapFree not found", func(path string) error {
			_, _, err := (&MemoryInfo{procMeminfoPath: path}).sample()
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f")
			writeFixture(t, path, tc.content)
			if err := tc.sample(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMemoryInfo_SampleSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	writeFixture(t, path, "junk\nMemFree: lots kB\nMemFree: 1024 kB\nBuffers: 1024 kB\nCached: 1024 kB\nSwapFree: 2048 kB\n")
	mem, swap, err := (&MemoryInfo{procMeminfoPath: path}).sample()
	if err != nil || mem != 3 || swap != 2 {
		t.Errorf("want 3/2, got %d/%d (err %v)", mem, swap, err)
	}
}

func TestNetworkActivity_ReadDevSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	writeFixture(t, path, "h1\nh2\nno colon here\n eth0: 1 2\n eth1: x 0 0 0 0 0 0 0 9\n eth2: 100 0 0 0 0 0 0 0 200 0\n")
	rx, tx, err := (&NetworkActivity{procNetDevPath: path}).readDev()
	if err != nil {
		t.Fatal(err)
	}
	if len(rx) != 1 || rx["eth2"] != 100 || len(tx) != 1 || tx["eth2"] != 200 {
		t.Errorf("want only eth2 100/200, got rx=%v tx=%v", rx, tx)
	}
}

func TestNetworkActivity_DeltaOmitsIdleInterfaces(t *testing.T) {
	p := &NetworkActivity{lastRx: map[string]int64{"eth0": 5}, lastTx: map[string]int64{"eth0": 5}}
	got := p.delta(1, map[string]int64{"eth0": 5, "eth9": 7}, map[string]int64{"eth0": 5, "eth9": 7})
	if len(got) != 0 {
		t.Errorf("idle and newly-appeared interfaces must be omitted, got %v", got)
	}
}

func TestMountInfo_DefaultStatfs(t *testing.T) {
	stat, err := defaultStatfs(t.TempDir())
	if err != nil {
		t.Fatalf("defaultStatfs: %v", err)
	}
	if stat.Bsize <= 0 || stat.Blocks == 0 {
		t.Errorf("implausible statfs result: bsize=%d blocks=%d", stat.Bsize, stat.Blocks)
	}
	if _, err := defaultStatfs(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("want an error for a missing path")
	}
}

func TestMountInfo_ReadMountsFiltersEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mounts")
	writeFixture(t, path, strings.Join([]string{
		"short line",
		"/dev/sda2 /dev/shm ext4 rw 0 0",
		"/dev/sdb1 /mnt/net nfs rw 0 0",
		"/dev/sdc1 /broken ext4 rw 0 0",
		"/dev/sda1 / ext4 rw 0 0",
	}, "\n")+"\n")
	ok := makeMockStatvfs(100, 50)
	p := &MountInfo{mountsPath: path, statvfs: func(mp string) (syscall.Statfs_t, error) {
		if mp == "/broken" {
			return syscall.Statfs_t{}, errors.New("stale handle")
		}
		return ok(mp)
	}}
	mounts, err := p.readMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0]["mount-point"] != "/" || mounts[0]["total-space"] != int64(100) {
		t.Errorf("want only / with 100MB, got %v", mounts)
	}
}

func TestNetworkDevice_CollectFiltersAndDescribesInterfaces(t *testing.T) {
	sys := t.TempDir()
	for _, name := range []string{"eth1", "eth3"} {
		if err := os.MkdirAll(filepath.Join(sys, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(sys, "eth1", "speed"), "1000\n")
	writeFixture(t, filepath.Join(sys, "eth1", "duplex"), "full\n")
	writeFixture(t, filepath.Join(sys, "eth3", "speed"), "unknown\n")

	names := []string{"wlan0", "eth3", "lo", "eth0.1", "eth0:1", "tap0", "eth2", "eth1"}
	ifaces := make([]net.Interface, 0, len(names))
	for _, n := range names {
		ifaces = append(ifaces, net.Interface{Name: n, Flags: net.FlagUp})
	}
	p := &NetworkDevice{
		sysNetPath:    sys,
		getInterfaces: func() ([]net.Interface, error) { return ifaces, nil },
		getAddrs: func(iface *net.Interface) ([]net.Addr, error) {
			switch iface.Name {
			case "wlan0":
				return nil, errors.New("permission denied")
			case "eth1":
				return []net.Addr{
					&net.IPAddr{IP: net.ParseIP("10.9.9.9")},
					&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
					&net.IPNet{IP: net.ParseIP("10.0.0.5").To4(), Mask: net.CIDRMask(16, 32)},
				}, nil
			}
			return nil, nil
		},
	}
	devices, speeds, err := p.collect()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range devices {
		got = append(got, string(d["interface"].([]byte)))
	}
	if want := []string{"eth1", "eth2", "eth3", "wlan0"}; !slices.Equal(got, want) {
		t.Fatalf("interfaces: want %v, got %v", want, got)
	}
	eth1 := devices[0]
	if string(eth1["ip_address"].([]byte)) != "10.0.0.5" ||
		string(eth1["netmask"].([]byte)) != "255.255.0.0" ||
		string(eth1["broadcast_address"].([]byte)) != "10.0.255.255" {
		t.Errorf("eth1 addressing: %v", eth1)
	}
	if wlan := devices[3]; string(wlan["ip_address"].([]byte)) != "" || string(wlan["netmask"].([]byte)) != "0.0.0.0" {
		t.Errorf("wlan0 without addrs must use defaults: %v", wlan)
	}
	wantSpeeds := []struct {
		speed  int
		duplex bool
	}{{1000, true}, {-1, false}, {-1, false}, {-1, false}}
	for i, w := range wantSpeeds {
		if speeds[i]["speed"] != w.speed || speeds[i]["duplex"] != w.duplex {
			t.Errorf("%s speed/duplex: want %d/%v, got %v/%v", got[i], w.speed, w.duplex, speeds[i]["speed"], speeds[i]["duplex"])
		}
	}
}

func TestNetworkDevice_CollectWithOnlyIgnoredInterfacesIsEmptyNotNil(t *testing.T) {
	p := &NetworkDevice{
		sysNetPath:    t.TempDir(),
		getInterfaces: func() ([]net.Interface, error) { return []net.Interface{{Name: "lo"}}, nil },
	}
	devices, speeds, err := p.collect()
	if err != nil || devices == nil || speeds == nil || len(devices) != 0 || len(speeds) != 0 {
		t.Errorf("want empty non-nil slices, got %v %v (err %v)", devices, speeds, err)
	}
}

func TestTranslateGoFlags_MapsEveryFlag(t *testing.T) {
	all := net.FlagUp | net.FlagBroadcast | net.FlagLoopback | net.FlagPointToPoint | net.FlagRunning | net.FlagMulticast
	if got, want := translateGoFlags(all), iffUp|iffBroadcast|iffLoopback|iffPointToPoint|iffRunning|iffMulticast; got != want {
		t.Errorf("want %#x, got %#x", want, got)
	}
	if got := translateGoFlags(0); got != 0 {
		t.Errorf("no flags: want 0, got %#x", got)
	}
}

func TestProcessorInfo_ParseX86SkipsBadProcessorIDs(t *testing.T) {
	p := &ProcessorInfo{cpuinfoPath: writeTemp(t, "processor\t: x\nvendor_id\t: Orphan\nprocessor\t: 3\nmodel name\t: M\n")}
	procs := p.parseX86()
	if len(procs) != 1 || procs[0]["processor-id"] != 3 || procs[0]["model"] != "M" {
		t.Errorf("want one processor 3/M, got %v", procs)
	}
	if _, ok := procs[0]["vendor"]; ok {
		t.Error("vendor before a valid processor line must not be attributed")
	}
}

func TestProcessorInfo_ParseARM64HandlesFieldsBeforeProcessorLine(t *testing.T) {
	p := &ProcessorInfo{cpuinfoPath: writeTemp(t, "no colon\nprocessor\t: x\nCache size\t: 512\nProcessor\t: ARMv7\n")}
	procs := p.parseARM64()
	if len(procs) != 1 {
		t.Fatalf("want 1 processor, got %v", procs)
	}
	got := procs[0]
	if got["processor-id"] != 0 || got["model"] != "ARMv7" || got["cache-size"] != 512 {
		t.Errorf("unexpected processor %v", got)
	}
}

func TestTemperature_ReadZonesSkipsUnreadableZones(t *testing.T) {
	dir := t.TempDir()
	for _, z := range []string{"thermal_zone0", "thermal_zone1", "thermal_zone2"} {
		if err := os.MkdirAll(filepath.Join(dir, z), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(dir, "thermal_zone0", "temp"), "45000\n")
	writeFixture(t, filepath.Join(dir, "thermal_zone2", "temp"), "hot\n")
	zones, err := (&Temperature{sysfsPath: dir}).readZones()
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 || zones["thermal_zone0"] != 45 {
		t.Errorf("want only thermal_zone0=45, got %v", zones)
	}
}

func TestTemperature_ReadZonesBadPattern(t *testing.T) {
	if _, err := (&Temperature{sysfsPath: filepath.Join(t.TempDir(), "[")}).readZones(); err == nil {
		t.Error("want a glob error for a malformed path")
	}
}

func TestUsers_ParsersSkipMalformedLines(t *testing.T) {
	p := &UserMonitor{
		passwdPath: writeTemp(t, "# comment\n\nshort:x:1\nbaduid:x:u:1:::\nbadgid:x:1:g:::\nok:x:1000:1000:Ok User:/home/ok:/bin/sh\n"),
	}
	users, err := p.parsePasswd()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users["ok"].Name != "Ok User" {
		t.Fatalf("want only ok, got %v", users)
	}

	p.groupPath = writeTemp(t, "# comment\n\nshort:x\nbad:x:g:\nstaff:x:50:ok,ghost\n")
	groups, err := p.parseGroup(users)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || !slices.Equal(groups["staff"].Members, []string{"ok"}) {
		t.Errorf("want staff with members [ok], got %v", groups)
	}
}

func TestBuildUsersDiff_GroupChanges(t *testing.T) {
	oldGroups := map[string]groupRecord{
		"staff": {Name: "staff", GID: 50, Members: []string{"alice", "bob"}},
		"gone":  {Name: "gone", GID: 60, Members: []string{}},
	}
	newGroups := map[string]groupRecord{
		"staff": {Name: "staff", GID: 51, Members: []string{"alice", "carol"}},
	}
	msg := buildUsersDiff(nil, nil, oldGroups, newGroups)

	updates, _ := msg["update-groups"].([]map[string]any)
	if len(updates) != 1 || updates[0]["gid"] != 51 {
		t.Errorf("update-groups: want staff gid 51, got %v", msg["update-groups"])
	}
	if got := msg["create-group-members"].(map[string][]string)["staff"]; !slices.Equal(got, []string{"carol"}) {
		t.Errorf("create-group-members: want [carol], got %v", got)
	}
	if got := msg["delete-group-members"].(map[string][]string)["staff"]; !slices.Equal(got, []string{"bob"}) {
		t.Errorf("delete-group-members: want [bob], got %v", got)
	}
	if got := msg["delete-groups"].([]string); !slices.Equal(got, []string{"gone"}) {
		t.Errorf("delete-groups: want [gone], got %v", got)
	}
}

func TestNewAccumulator_DefaultsToWallClock(t *testing.T) {
	acc := newAccumulator(0, nil)
	if acc.now == nil {
		t.Fatal("nil now must default to time.Now")
	}
	if d := time.Since(acc.lastSend); d < 0 || d > time.Minute {
		t.Errorf("lastSend not initialised from the wall clock: %v ago", d)
	}
	acc.add(1)
	if got := acc.drainIfDue(); len(got) != 1 {
		t.Errorf("zero window must drain immediately, got %v", got)
	}
}
