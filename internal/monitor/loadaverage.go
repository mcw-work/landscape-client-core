package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// LoadAverage monitors system load by reading /proc/loadavg.
// Field names in messages match the Python loadaverage.py plugin exactly.
type LoadAverage struct {
	heartbeatSource
	procLoadavgPath string
	interval        time.Duration // sampling interval
	sendInterval    time.Duration // how often buffered points are sent
}

// NewLoadAverage returns a LoadAverage plugin with default settings.
func NewLoadAverage() *LoadAverage {
	return &LoadAverage{
		procLoadavgPath: "/proc/loadavg",
		interval:        5 * time.Minute,
		sendInterval:    5 * time.Minute,
	}
}

// Name returns the Landscape message type string.
func (p *LoadAverage) Name() string { return "load-average" }

func (p *LoadAverage) Interval() time.Duration { return p.interval }

// Run starts the periodic load average collection loop.
func (p *LoadAverage) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	acc := newAccumulator(p.sendInterval, time.Now)

	runTicker(ctx, p.interval, false, staggerFor(p.interval), func(ctx context.Context, t time.Time) {
		p.beat(p.Name())
		load, err := p.sample()
		if err != nil {
			slog.Warn("load-average: cannot sample", "error", err)
			return
		}
		acc.add(bpickle.Tuple{t.Unix(), load})

		points := acc.drainIfDue()
		if points == nil {
			return
		}
		msg := exchange.Message{
			"type":          "load-average",
			"load-averages": points,
		}
		if err := sink.Send(ctx, msg); err != nil {
			slog.Warn("load-average: send failed", "error", err)
		}
	})
	return nil
}

// sample reads /proc/loadavg and returns the 1-minute load average,
// matching Python's os.getloadavg()[0].
func (p *LoadAverage) sample() (float64, error) {
	data, err := os.ReadFile(p.procLoadavgPath)
	if err != nil {
		return 0, fmt.Errorf("cannot read %s: %w", p.procLoadavgPath, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected format in %s", p.procLoadavgPath)
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse load average from %s: %w", p.procLoadavgPath, err)
	}
	return load, nil
}
