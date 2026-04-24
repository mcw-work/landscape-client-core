package monitor

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// processInfo mirrors the fields sent in active-process-info messages.
// Field names match the Python ProcessInformation output exactly.
type processInfo struct {
	pid        int64
	name       string
	state      string
	uid        int64
	gid        int64
	vmSize     int64   // kB
	startTime  int64   // Unix timestamp
	percentCPU float64 // 0–99
}

// ActiveProcessInfo monitors running processes by diffing /proc entries on each
// tick. Message fields match the Python activeprocessinfo.py plugin exactly.
// On the first tick it sends kill-all-processes plus all current processes;
// on subsequent ticks it sends only creates, updates, and deletes.
type ActiveProcessInfo struct {
	procRoot    string
	interval    time.Duration
	initialized bool
	previous    map[int64]processInfo
}

// NewActiveProcessInfo returns an ActiveProcessInfo plugin with default settings.
func NewActiveProcessInfo() *ActiveProcessInfo {
	return &ActiveProcessInfo{
		procRoot: "/proc",
		interval: 30 * time.Second,
	}
}

// Name returns the Landscape message type string.
func (p *ActiveProcessInfo) Name() string { return "active-process-info" }

// Run starts the periodic process info collection loop.
func (p *ActiveProcessInfo) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			msg, err := p.buildMessage()
			if err != nil {
				log.Printf("active-process-info: %v", err)
				continue
			}
			if msg == nil {
				continue
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("active-process-info: send: %v", err)
			}
		}
	}
}

// buildMessage reads current processes and constructs a diff message.
// Returns nil if there are no changes to report (after the first run).
func (p *ActiveProcessInfo) buildMessage() (exchange.Message, error) {
	bootTime, err := p.readBootTime()
	if err != nil {
		return nil, fmt.Errorf("reading boot time: %w", err)
	}
	uptime, err := p.readUptime()
	if err != nil {
		return nil, fmt.Errorf("reading uptime: %w", err)
	}
	current, err := p.readAllProcesses(bootTime, uptime)
	if err != nil {
		return nil, fmt.Errorf("reading processes: %w", err)
	}

	msg := exchange.Message{"type": "active-process-info"}

	if !p.initialized {
		// First run: instruct server to discard any stale process state.
		msg["kill-all-processes"] = true
		if len(current) > 0 {
			adds := make([]any, 0, len(current))
			for _, info := range current {
				adds = append(adds, processToMap(info))
			}
			msg["add-processes"] = adds
		}
		p.initialized = true
		p.previous = current
		return msg, nil
	}

	creates, updates, deletes := diffProcesses(p.previous, current)
	p.previous = current

	if len(creates) == 0 && len(updates) == 0 && len(deletes) == 0 {
		return nil, nil
	}
	if len(creates) > 0 {
		adds := make([]any, 0, len(creates))
		for _, info := range creates {
			adds = append(adds, processToMap(info))
		}
		msg["add-processes"] = adds
	}
	if len(updates) > 0 {
		upds := make([]any, 0, len(updates))
		for _, info := range updates {
			upds = append(upds, processToMap(info))
		}
		msg["update-processes"] = upds
	}
	if len(deletes) > 0 {
		killed := make([]any, 0, len(deletes))
		for _, pid := range deletes {
			killed = append(killed, pid)
		}
		msg["kill-processes"] = killed
	}
	return msg, nil
}

// readBootTime reads the system boot time in Unix seconds from procRoot/stat.
func (p *ActiveProcessInfo) readBootTime() (int64, error) {
	f, err := os.Open(filepath.Join(p.procRoot, "stat"))
	if err != nil {
		return 0, fmt.Errorf("opening proc/stat: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "btime ") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("malformed btime line in proc/stat")
			}
			t, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing btime: %w", err)
			}
			return t, nil
		}
	}
	return 0, fmt.Errorf("btime not found in proc/stat")
}

// readUptime reads the system uptime in seconds from procRoot/uptime.
func (p *ActiveProcessInfo) readUptime() (float64, error) {
	data, err := os.ReadFile(filepath.Join(p.procRoot, "uptime"))
	if err != nil {
		return 0, fmt.Errorf("reading proc/uptime: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("malformed proc/uptime")
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing uptime: %w", err)
	}
	return uptime, nil
}

// readAllProcesses iterates numeric subdirectories of procRoot and collects
// process info, skipping processes that have already exited.
func (p *ActiveProcessInfo) readAllProcesses(bootTime int64, uptime float64) (map[int64]processInfo, error) {
	entries, err := os.ReadDir(p.procRoot)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p.procRoot, err)
	}
	processes := make(map[int64]processInfo)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue // not a PID directory
		}
		info, err := p.readProcessInfo(pid, bootTime, uptime)
		if err != nil {
			continue // process exited between listing and reading
		}
		if info.state == "X" {
			continue // dead process
		}
		processes[pid] = *info
	}
	return processes, nil
}

// clkTck is the kernel timer frequency; 100 Hz is universal on modern Linux.
const clkTck = 100

// readProcessInfo parses /proc/<pid>/stat and /proc/<pid>/status to build a
// processInfo. Fields match the Python ProcessInformation.get_process_info output.
func (p *ActiveProcessInfo) readProcessInfo(pid int64, bootTime int64, uptime float64) (*processInfo, error) {
	pidStr := strconv.FormatInt(pid, 10)
	statPath := filepath.Join(p.procRoot, pidStr, "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return nil, err
	}

	line := strings.TrimSpace(string(data))
	// comm may contain spaces and parentheses; find the last ')'.
	firstParen := strings.Index(line, "(")
	lastParen := strings.LastIndex(line, ")")
	if firstParen < 0 || lastParen <= firstParen {
		return nil, fmt.Errorf("malformed stat line for pid %d", pid)
	}
	comm := line[firstParen+1 : lastParen]

	// Fields after "(comm) ": [0]=state … [11]=utime [12]=stime … [19]=starttime [20]=vsize
	rest := strings.Fields(line[lastParen+1:])
	if len(rest) < 21 {
		return nil, fmt.Errorf("too few fields in stat for pid %d", pid)
	}
	state := rest[0]
	utime, _ := strconv.ParseInt(rest[11], 10, 64)
	stime, _ := strconv.ParseInt(rest[12], 10, 64)
	startJiffies, _ := strconv.ParseInt(rest[19], 10, 64)
	vsize, _ := strconv.ParseInt(rest[20], 10, 64)

	startTimeSec := bootTime + startJiffies/clkTck
	totalTime := utime + stime
	seconds := uptime - float64(startJiffies)/clkTck
	var pcpu float64
	if seconds > 0 {
		pcpu = float64(totalTime) * 100.0 / clkTck / seconds
		pcpu = math.Round(max(min(pcpu, 99.0), 0.0)*10) / 10
	}

	info := &processInfo{
		pid:        pid,
		name:       comm,
		state:      state,
		startTime:  startTimeSec,
		vmSize:     vsize / 1024, // bytes → kB (fallback if status unavailable)
		percentCPU: pcpu,
	}
	p.readStatusInto(pid, info)
	return info, nil
}

// readStatusInto supplements info with uid, gid, and vm-size from
// /proc/<pid>/status. Errors are silently ignored since stat fields serve
// as fallbacks.
func (p *ActiveProcessInfo) readStatusInto(pid int64, info *processInfo) {
	statusPath := filepath.Join(p.procRoot, strconv.FormatInt(pid, 10), "status")
	f, err := os.Open(statusPath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "Uid:"):
			if fields := strings.Fields(line); len(fields) >= 2 {
				info.uid, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		case strings.HasPrefix(line, "Gid:"):
			if fields := strings.Fields(line); len(fields) >= 2 {
				info.gid, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		case strings.HasPrefix(line, "VmSize:"):
			// VmSize is already in kB in /proc/<pid>/status.
			if fields := strings.Fields(line); len(fields) >= 2 {
				info.vmSize, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}
}

// processToMap converts a processInfo to the map structure expected by the
// Landscape server, matching Python's process_info dict keys.
func processToMap(info processInfo) map[string]any {
	return map[string]any{
		"pid":         info.pid,
		"name":        info.name,
		"state":       []byte(info.state),
		"uid":         info.uid,
		"gid":         info.gid,
		"vm-size":     info.vmSize,
		"start-time":  info.startTime,
		"percent-cpu": info.percentCPU,
	}
}

// diffProcesses computes the three-way diff between old and current process maps.
// creates: PIDs present in current but not old.
// updates: PIDs present in both but with changed fields.
// deletes: PIDs present in old but not current.
func diffProcesses(old, current map[int64]processInfo) (creates, updates map[int64]processInfo, deletes []int64) {
	creates = make(map[int64]processInfo)
	updates = make(map[int64]processInfo)
	for pid, newInfo := range current {
		oldInfo, existed := old[pid]
		if !existed {
			creates[pid] = newInfo
		} else if oldInfo != newInfo {
			updates[pid] = newInfo
		}
	}
	for pid := range old {
		if _, exists := current[pid]; !exists {
			deletes = append(deletes, pid)
		}
	}
	return creates, updates, deletes
}
