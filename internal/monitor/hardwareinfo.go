package monitor

import (
	"context"
	"log"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/runcmd"
)

// HardwareInfo collects hardware information via lshw and reports it to the
// Landscape server. The message type and field names match the Python
// HardwareInfo manager plugin exactly: {"type": "hardware-info", "data": <xml bytes>}.
type HardwareInfo struct {
	heartbeatSource
	interval time.Duration
}

// NewHardwareInfo returns a HardwareInfo with a 24-hour collection interval.
func NewHardwareInfo() *HardwareInfo {
	return &HardwareInfo{interval: 24 * time.Hour}
}

func (p *HardwareInfo) Name() string { return "hardware-info" }

func (p *HardwareInfo) Interval() time.Duration { return p.interval }

// Run sends hardware info immediately on startup, then once per day.
func (p *HardwareInfo) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	p.tick(ctx, sink)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.beat(p.Name())
			p.tick(ctx, sink)
		}
	}
}

func (p *HardwareInfo) tick(ctx context.Context, sink exchange.MessageSink) {
	// The per-run lshw timeout is added in Task 12; 0 means bound by ctx only.
	out, err := runcmd.Run(ctx, 0, "lshw", "-xml", "-quiet")
	if err != nil {
		log.Printf("hardware-info: lshw failed: %v", err)
		return
	}
	msg := exchange.Message{
		"type": "hardware-info",
		"data": out,
	}
	if err := sink.Send(ctx, msg); err != nil {
		log.Printf("hardware-info: send: %v", err)
	}
}
