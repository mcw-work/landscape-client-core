package monitor

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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
	// run is injectable so the empty-output and truncated-XML cases — which
	// require an AppArmor denial to reproduce naturally — are testable.
	run func(ctx context.Context) ([]byte, error)
}

// NewHardwareInfo returns a HardwareInfo with a 24-hour collection interval.
func NewHardwareInfo() *HardwareInfo {
	return &HardwareInfo{
		interval: 24 * time.Hour,
		run: func(ctx context.Context) ([]byte, error) {
			return runcmd.Run(ctx, lshwTimeout, "lshw", "-xml", "-quiet")
		},
	}
}

// lshwTimeout bounds a single lshw run. It probes PCI, USB, DMI and SCSI and can
// wedge on a misbehaving device; the plugin holds the daemon-lifetime context,
// so without this the goroutine is blocked for the life of the process.
const lshwTimeout = 2 * time.Minute

func (p *HardwareInfo) Name() string { return "hardware-info" }

func (p *HardwareInfo) Interval() time.Duration { return p.interval }

// Run sends hardware info immediately on startup, then once per day.
func (p *HardwareInfo) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	p.tick(ctx, sink)
	runTicker(ctx, p.interval, false, 0, func(ctx context.Context, _ time.Time) {
		p.beat(p.Name())
		p.tick(ctx, sink)
	})
	return nil
}

func (p *HardwareInfo) tick(ctx context.Context, sink exchange.MessageSink) {
	out, err := p.run(ctx)
	if err != nil {
		log.Printf("hardware-info: %v", err)
		return
	}
	if err := validateLshwXML(out); err != nil {
		// lshw under strict confinement can be partially AppArmor-denied and
		// still exit 0. Sending empty or truncated inventory can make the server
		// overwrite good data with nothing.
		log.Printf("hardware-info: discarding lshw output: %v", err)
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

func validateLshwXML(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("lshw produced no output")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lshw output is not valid XML: %w", err)
		}
	}
}
