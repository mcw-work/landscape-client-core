package monitor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
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

func (p *ProcessorInfo) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var saved processorInfoState
	if state != nil {
		_ = state.GetPluginState(&saved)
	}

	if p.delay > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.delay):
		}
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	doSend := func() {
		processors := p.parseProcessors()
		if processors == nil {
			return
		}
		sort.Slice(processors, func(i, j int) bool {
			idI, _ := processors[i]["processor-id"].(int)
			idJ, _ := processors[j]["processor-id"].(int)
			return idI < idJ
		})
		data, err := json.Marshal(processors)
		if err != nil {
			log.Printf("processor-info: marshal: %v", err)
			return
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(data))
		if hash == saved.Hash {
			return
		}
		saved.Hash = hash
		if state != nil {
			if err := state.SetPluginState(saved); err != nil {
				log.Printf("processor-info: saving state: %v", err)
			}
		}
		msg := exchange.Message{
			"type":       "processor-info",
			"processors": processors,
		}
		if err := sink.Send(ctx, msg); err != nil {
			log.Printf("processor-info: send: %v", err)
		}
	}

	doSend()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			doSend()
		}
	}
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
		log.Printf("processor-info: opening %s: %v", p.cpuinfoPath, err)
		return nil
	}
	defer f.Close()

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
		log.Printf("processor-info: opening %s: %v", p.cpuinfoPath, err)
		return nil
	}
	defer f.Close()

	var processors []map[string]any
	var current map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if current != nil {
				if _, ok := current["processor-id"]; !ok {
					current["processor-id"] = 0
				}
				processors = append(processors, current)
				current = nil
			}
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
	if current != nil {
		if _, ok := current["processor-id"]; !ok {
			current["processor-id"] = 0
		}
		processors = append(processors, current)
	}
	return processors
}
