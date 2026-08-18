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

// MemoryInfo monitors free memory and swap by reading /proc/meminfo.
// Field names in messages match the Python memoryinfo.py plugin exactly.
type MemoryInfo struct {
	heartbeatSource
	procMeminfoPath string
	interval        time.Duration
}

// NewMemoryInfo returns a MemoryInfo plugin with default settings.
func NewMemoryInfo() *MemoryInfo {
	return &MemoryInfo{
		procMeminfoPath: "/proc/meminfo",
		interval:        15 * time.Second,
	}
}

// Name returns the Landscape message type string.
func (p *MemoryInfo) Name() string { return "memory-info" }

func (p *MemoryInfo) Interval() time.Duration { return p.interval }

// Run starts the periodic memory information collection loop.
func (p *MemoryInfo) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	runTicker(ctx, p.interval, false, 0, func(ctx context.Context, t time.Time) {
		p.beat(p.Name())
		freeMemMB, freeSwapMB, err := p.sample()
		if err != nil {
			log.Printf("memory-info: %v", err)
			return
		}
		msg := exchange.Message{
			"type":        "memory-info",
			"memory-info": []any{bpickle.Tuple{t.Unix(), freeMemMB, freeSwapMB}},
		}
		if err := sink.Send(ctx, msg); err != nil {
			log.Printf("memory-info: send: %v", err)
		}
	})
	return nil
}

// sample reads /proc/meminfo and returns free memory and free swap in megabytes.
// Free memory matches the Python client: (MemFree + Buffers + Cached) / 1024.
func (p *MemoryInfo) sample() (freeMemMB, freeSwapMB int64, err error) {
	f, err := os.Open(p.procMeminfoPath)
	if err != nil {
		return 0, 0, fmt.Errorf("opening %s: %w", p.procMeminfoPath, err)
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
