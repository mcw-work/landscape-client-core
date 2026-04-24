package monitor

import (
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

// LoadAverage monitors system load by reading /proc/loadavg.
// Field names in messages match the Python loadaverage.py plugin exactly.
type LoadAverage struct {
	procLoadavgPath string
	interval        time.Duration
}

// NewLoadAverage returns a LoadAverage plugin with default settings.
func NewLoadAverage() *LoadAverage {
	return &LoadAverage{
		procLoadavgPath: "/proc/loadavg",
		interval:        5 * time.Minute,
	}
}

// Name returns the Landscape message type string.
func (p *LoadAverage) Name() string { return "load-average" }

// Run starts the periodic load average collection loop.
func (p *LoadAverage) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-ticker.C:
			load, err := p.sample()
			if err != nil {
				log.Printf("load-average: %v", err)
				continue
			}
			msg := exchange.Message{
				"type":          "load-average",
				"load-averages": []any{bpickle.Tuple{t.Unix(), load}},
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("load-average: send: %v", err)
			}
		}
	}
}

// sample reads /proc/loadavg and returns the 1-minute load average,
// matching Python's os.getloadavg()[0].
func (p *LoadAverage) sample() (float64, error) {
	data, err := os.ReadFile(p.procLoadavgPath)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", p.procLoadavgPath, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected format in %s", p.procLoadavgPath)
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing load average from %s: %w", p.procLoadavgPath, err)
	}
	return load, nil
}
