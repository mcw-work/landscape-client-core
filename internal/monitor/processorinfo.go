package monitor

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

type processorInfoState struct {
	Hash string `json:"hash"`
}

type ProcessorInfo struct {
	heartbeatSource
	cpuinfoPath string
	interval    time.Duration
	delay       time.Duration
}

func NewProcessorInfo() *ProcessorInfo {
	return &ProcessorInfo{
		cpuinfoPath: "/proc/cpuinfo",
		interval:    time.Hour,
		delay:       2 * time.Second,
	}
}

func (p *ProcessorInfo) Name() string { return "processor-info" }

func (p *ProcessorInfo) Interval() time.Duration { return p.interval }

func (p *ProcessorInfo) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var saved processorInfoState
	if state != nil {
		if err := state.GetPluginState(&saved); err != nil {
			// Zero state is not equivalent to "no changes yet": for users it
			// re-sends every account as a create.
			slog.Warn("processor-info: cannot load state, treating as first run", "error", err)
		}
	}

	if p.delay > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.delay):
		}
	}

	doSend := func() {
		processors := p.parseProcessors()
		if processors == nil {
			return
		}
		slices.SortStableFunc(processors, func(a, b map[string]any) int {
			idA, _ := a["processor-id"].(int)
			idB, _ := b["processor-id"].(int)
			return cmp.Compare(idA, idB)
		})
		data, err := json.Marshal(processors)
		if err != nil {
			slog.Warn("processor-info: cannot marshal", "error", err)
			return
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(data))
		if hash == saved.Hash {
			return
		}
		saved.Hash = hash
		if state != nil {
			if err := state.SetPluginState(saved); err != nil {
				slog.Error("processor-info: cannot save state", "error", err)
			}
		}
		msg := exchange.Message{
			"type":       "processor-info",
			"processors": processors,
		}
		if err := sink.Send(ctx, msg); err != nil {
			slog.Warn("processor-info: send failed", "error", err)
		}
	}

	doSend()

	runTicker(ctx, p.interval, false, staggerFor(p.interval), func(context.Context, time.Time) {
		p.beat(p.Name())
		doSend()
	})
	return nil
}

func (p *ProcessorInfo) parseProcessors() []map[string]any {
	switch runtime.GOARCH {
	case "arm64":
		return p.parseARM64()
	default:
		return p.parseX86()
	}
}

func (p *ProcessorInfo) parseX86() []map[string]any {
	f, err := os.Open(p.cpuinfoPath)
	if err != nil {
		slog.Warn("processor-info: cannot open cpuinfo", "path", p.cpuinfoPath, "error", err)
		return nil
	}
	defer func() {
		_ = f.Close()
	}()

	var processors []map[string]any
	var current map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case "processor":
			id, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			current = map[string]any{"processor-id": id}
			processors = append(processors, current)
		case "vendor_id":
			if current != nil {
				current["vendor"] = value
			}
		case "model name":
			if current != nil {
				current["model"] = value
			}
		case "cache size":
			if current != nil {
				valueParts := strings.Fields(value)
				if len(valueParts) > 0 {
					if n, err := strconv.Atoi(valueParts[0]); err == nil {
						current["cache-size"] = n
					}
				}
			}
		}
	}
	return processors
}

func (p *ProcessorInfo) parseARM64() []map[string]any {
	f, err := os.Open(p.cpuinfoPath)
	if err != nil {
		slog.Warn("processor-info: cannot open cpuinfo", "path", p.cpuinfoPath, "error", err)
		return nil
	}
	defer func() {
		_ = f.Close()
	}()

	var processors []map[string]any
	var current map[string]any
	// blockIndex gives blocks without a "processor" line a distinct sequential
	// id; defaulting them all to 0 made them collide under the sort.
	blockIndex := 0
	finalize := func() {
		if current == nil {
			return
		}
		if _, ok := current["processor-id"]; !ok {
			current["processor-id"] = blockIndex
		}
		processors = append(processors, current)
		blockIndex++
		current = nil
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			finalize()
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case "processor":
			id, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			if current == nil {
				current = make(map[string]any)
			}
			current["processor-id"] = id
			if _, ok := current["model"]; !ok {
				current["model"] = "arm"
			}
		case "Processor":
			if current == nil {
				current = make(map[string]any)
			}
			current["model"] = value
		case "Cache size":
			if current == nil {
				current = make(map[string]any)
			}
			if n, err := strconv.Atoi(value); err == nil {
				current["cache-size"] = n
			}
		}
	}
	finalize()
	return processors
}
