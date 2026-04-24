package monitor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// Temperature reads thermal zone temperatures from sysfs and sends per-zone
// messages. Message fields match the Python temperature.py plugin exactly.
// If no thermal zones are found the plugin runs silently without sending messages.
type Temperature struct {
	sysfsPath string
	interval  time.Duration
}

// NewTemperature returns a Temperature plugin with default settings.
func NewTemperature() *Temperature {
	return &Temperature{
		sysfsPath: "/sys/class/thermal",
		interval:  5 * time.Minute,
	}
}

// Name returns the Landscape message type string.
func (p *Temperature) Name() string { return "temperature" }

// Run starts the periodic temperature collection loop.
func (p *Temperature) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-ticker.C:
			zones, err := p.readZones()
			if err != nil {
				log.Printf("temperature: %v", err)
				continue
			}
			for zone, temp := range zones {
				msg := exchange.Message{
					"type":         "temperature",
					"thermal-zone": zone,
					"temperatures": []any{[]any{t.Unix(), temp}},
				}
				if err := sink.Send(ctx, msg); err != nil {
					log.Printf("temperature: send: %v", err)
				}
			}
		}
	}
}

// readZones globs sysfsPath/thermal_zone* directories and reads each zone's
// temperature file. Values are in millidegrees Celsius and are divided by 1000.
// Zones without a readable temp file are silently skipped.
func (p *Temperature) readZones() (map[string]float64, error) {
	pattern := filepath.Join(p.sysfsPath, "thermal_zone*")
	dirs, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", pattern, err)
	}
	zones := make(map[string]float64, len(dirs))
	for _, dir := range dirs {
		zoneName := filepath.Base(dir)
		data, err := os.ReadFile(filepath.Join(dir, "temp"))
		if err != nil {
			continue
		}
		milliDeg, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}
		zones[zoneName] = milliDeg / 1000.0
	}
	return zones, nil
}
