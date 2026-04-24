package monitor

import (
	"context"
	"log"
	"os/exec"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// HardwareInfo collects hardware information via lshw and reports it to the
// Landscape server. The message type and field names match the Python
// HardwareInfo manager plugin exactly: {"type": "hardware-info", "data": <xml bytes>}.
type HardwareInfo struct {
	interval time.Duration
}

// NewHardwareInfo returns a HardwareInfo with a 24-hour collection interval.
func NewHardwareInfo() *HardwareInfo {
	return &HardwareInfo{interval: 24 * time.Hour}
}

func (p *HardwareInfo) Name() string { return "hardware-info" }

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
			p.tick(ctx, sink)
		}
	}
}

func (p *HardwareInfo) tick(ctx context.Context, sink exchange.MessageSink) {
	out, err := exec.CommandContext(ctx, "lshw", "-xml", "-quiet").Output()
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
