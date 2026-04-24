package monitor

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// CPUUsage monitors CPU utilization by reading /proc/stat.
// Field names in messages match the Python cpuusage.py plugin exactly.
type CPUUsage struct {
	procStatPath string
	interval     time.Duration
	prevTotal    int64
	prevIdle     int64
	hasPrev      bool
}

// NewCPUUsage returns a CPUUsage plugin with default settings.
func NewCPUUsage() *CPUUsage {
	return &CPUUsage{
		procStatPath: "/proc/stat",
		interval:     30 * time.Second,
	}
}

// Name returns the Landscape message type string.
func (p *CPUUsage) Name() string { return "cpu-usage" }

// Run starts the periodic CPU usage collection loop. An initial baseline
// sample is taken synchronously before the ticker starts so the first tick
// can always produce a real delta.
func (p *CPUUsage) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	// Prime the baseline before starting the ticker.
	if _, err := p.sample(); err != nil {
		log.Printf("cpu-usage: priming baseline: %v", err)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-ticker.C:
			usage, err := p.sample()
			if err != nil {
				log.Printf("cpu-usage: %v", err)
				continue
			}
			if usage < 0 {
				// No delta available yet.
				continue
			}
			msg := exchange.Message{
				"type":       "cpu-usage",
				"cpu-usages": []any{bpickle.Tuple{t.Unix(), usage}},
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("cpu-usage: send: %v", err)
			}
		}
	}
}

// sample reads /proc/stat and returns the CPU active fraction (0–1).
// Returns (-1, nil) on the first call (baseline only) or when total delta ≤ 0.
func (p *CPUUsage) sample() (float64, error) {
	f, err := os.Open(p.procStatPath)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", p.procStatPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, fmt.Errorf("reading first line of %s", p.procStatPath)
	}
	// Line format: "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, fmt.Errorf("unexpected format in %s", p.procStatPath)
	}

	var total int64
	for _, s := range fields[1:] {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing field in %s: %w", p.procStatPath, err)
		}
		total += v
	}
	// fields[4] is the idle counter (user=1, nice=2, system=3, idle=4).
	idle, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing idle from %s: %w", p.procStatPath, err)
	}

	if !p.hasPrev {
		p.prevTotal = total
		p.prevIdle = idle
		p.hasPrev = true
		return -1, nil // first call: baseline only
	}

	totalDelta := total - p.prevTotal
	idleDelta := idle - p.prevIdle
	p.prevTotal = total
	p.prevIdle = idle

	if totalDelta <= 0 {
		return -1, nil // no change or counter overflow
	}
	// (total_delta - idle_delta) / total_delta  — matches Python formula exactly.
	return float64(totalDelta-idleDelta) / float64(totalDelta), nil
}
