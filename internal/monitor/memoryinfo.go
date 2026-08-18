package monitor

import (
	"bufio"
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

// MemoryInfo monitors free memory and swap by reading /proc/meminfo.
// Field names in messages match the Python memoryinfo.py plugin exactly.
type MemoryInfo struct {
	heartbeatSource
	procMeminfoPath string
	interval        time.Duration // sampling interval
	sendInterval    time.Duration // how often buffered points are sent
}

// NewMemoryInfo returns a MemoryInfo plugin with default settings.
func NewMemoryInfo() *MemoryInfo {
	return &MemoryInfo{
		procMeminfoPath: "/proc/meminfo",
		interval:        15 * time.Second,
		sendInterval:    5 * time.Minute,
	}
}

// Name returns the Landscape message type string.
func (p *MemoryInfo) Name() string { return "memory-info" }

func (p *MemoryInfo) Interval() time.Duration { return p.interval }

// Run starts the periodic memory information collection loop.
func (p *MemoryInfo) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	acc := newAccumulator(p.sendInterval, time.Now)

	runTicker(ctx, p.interval, false, staggerFor(p.interval), func(ctx context.Context, t time.Time) {
		p.beat(p.Name())
		freeMemMB, freeSwapMB, err := p.sample()
		if err != nil {
			slog.Warn("memory-info: cannot sample", "error", err)
			return
		}
		acc.add(bpickle.Tuple{t.Unix(), freeMemMB, freeSwapMB})

		points := acc.drainIfDue()
		if points == nil {
			return
		}
		msg := exchange.Message{
			"type":        "memory-info",
			"memory-info": points,
		}
		if err := sink.Send(ctx, msg); err != nil {
			slog.Warn("memory-info: send failed", "error", err)
		}
	})
	return nil
}

// sample reads /proc/meminfo and returns free memory and free swap in megabytes.
// Free memory matches the Python client: (MemFree + Buffers + Cached) / 1024.
func (p *MemoryInfo) sample() (freeMemMB, freeSwapMB int64, err error) {
	f, err := os.Open(p.procMeminfoPath)
	if err != nil {
		return 0, 0, fmt.Errorf("cannot open %s: %w", p.procMeminfoPath, err)
	}
	defer func() {
		_ = f.Close()
	}()

	var memFreeKB, buffersKB, cachedKB, swapFreeKB int64
	var foundMem, foundBuffers, foundCached, foundSwap bool
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		val, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemFree":
			memFreeKB = val
			foundMem = true
		case "Buffers":
			buffersKB = val
			foundBuffers = true
		case "Cached":
			cachedKB = val
			foundCached = true
		case "SwapFree":
			swapFreeKB = val
			foundSwap = true
		}
		if foundMem && foundBuffers && foundCached && foundSwap {
			break
		}
	}

	if !foundMem || !foundSwap {
		return 0, 0, fmt.Errorf("MemFree or SwapFree not found in %s", p.procMeminfoPath)
	}
	freeMemMB = (memFreeKB + buffersKB + cachedKB) / 1024
	return freeMemMB, swapFreeKB / 1024, nil
}
