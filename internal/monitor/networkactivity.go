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

// NetworkActivity monitors network I/O by reading /proc/net/dev and sending
// per-interface byte deltas. Message fields match the Python networkactivity.py plugin.
type NetworkActivity struct {
	heartbeatSource
	procNetDevPath string
	interval       time.Duration
	lastRx         map[string]int64
	lastTx         map[string]int64
}

// NewNetworkActivity returns a NetworkActivity plugin with default settings.
func NewNetworkActivity() *NetworkActivity {
	return &NetworkActivity{
		procNetDevPath: "/proc/net/dev",
		interval:       30 * time.Second,
	}
}

// Name returns the Landscape message type string.
func (p *NetworkActivity) Name() string { return "network-activity" }

func (p *NetworkActivity) Interval() time.Duration { return p.interval }

// Run starts the periodic network activity collection loop. A baseline sample
// is taken synchronously before the ticker starts so the first tick can
// produce a meaningful delta.
func (p *NetworkActivity) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	// Prime baseline.
	rx, tx, err := p.readDev()
	if err != nil {
		log.Printf("network-activity: priming baseline: %v", err)
	} else {
		p.lastRx, p.lastTx = rx, tx
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-ticker.C:
			p.beat(p.Name())
			rx, tx, err := p.readDev()
			if err != nil {
				log.Printf("network-activity: %v", err)
				continue
			}
			activities := p.delta(t.Unix(), rx, tx)
			p.lastRx, p.lastTx = rx, tx
			if len(activities) == 0 {
				continue
			}
			msg := exchange.Message{
				"type":       "network-activity",
				"activities": activities,
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("network-activity: send: %v", err)
			}
		}
	}
}

// readDev parses /proc/net/dev and returns per-interface rx and tx byte counts.
// /proc/net/dev format (after 2 header lines):
//
//	<iface>: rx_bytes rx_pkts rx_errs ... tx_bytes ...
//
// rx_bytes is field index 0 after the colon, tx_bytes is field index 8.
func (p *NetworkActivity) readDev() (rx, tx map[string]int64, err error) {
	f, err := os.Open(p.procNetDevPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", p.procNetDevPath, err)
	}
	defer func() {
		_ = f.Close()
	}()

	rx = make(map[string]int64)
	tx = make(map[string]int64)

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 { // skip two header lines
			continue
		}
		line := scanner.Text()
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colon])
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		rxBytes, err1 := strconv.ParseInt(fields[0], 10, 64)
		txBytes, err2 := strconv.ParseInt(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx[iface] = rxBytes
		tx[iface] = txBytes
	}
	return rx, tx, scanner.Err()
}

// counterWrapThreshold is how close to 2^32 the previous value must be for a
// negative delta to be treated as a wrap rather than a counter reset. A
// re-created interface starts near zero, so its previous value is nowhere near
// the threshold.
const (
	counterWrap          = int64(1) << 32
	counterWrapThreshold = counterWrap - (1 << 24) // within 16 MiB of the wrap point
)

// counterDelta returns the traffic between two samples of a kernel byte counter.
//
// On 32-bit systems (armhf Core devices) the counter wraps at 2^32, so a
// negative raw delta following a near-maximum previous value is a wrap and the
// wrapped amount must be added back — clamping it loses 4 GiB per interface per
// wrap. A negative delta from a low previous value is a genuine reset (interface
// re-created) and still clamps to zero. 64-bit counters do not wrap.
func counterDelta(now, prev int64) int64 {
	d := now - prev
	if d >= 0 {
		return d
	}
	if prev >= counterWrapThreshold {
		return d + counterWrap
	}
	return 0
}

// delta computes per-interface deltas since the last sample. Returns a map of
// interface name (as bytes) → []any{[]any{timestamp, rxDelta, txDelta}} matching the
// Python network-activity message format.
func (p *NetworkActivity) delta(ts int64, rx, tx map[string]int64) bpickle.BytesDict {
	activities := make(bpickle.BytesDict)
	for iface, rxNow := range rx {
		txNow := tx[iface]
		lastRx, hadRx := p.lastRx[iface]
		lastTx, hadTx := p.lastTx[iface]
		if !hadRx || !hadTx {
			continue
		}
		rxDelta := counterDelta(rxNow, lastRx)
		txDelta := counterDelta(txNow, lastTx)
		if rxDelta == 0 && txDelta == 0 {
			continue
		}
		activities[iface] = []any{bpickle.Tuple{ts, rxDelta, txDelta}}
	}
	return activities
}
