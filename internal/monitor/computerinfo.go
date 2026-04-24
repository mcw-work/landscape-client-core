package monitor

import (
	"bufio"
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

type ciSavedState struct {
	Initialized   bool   `json:"initialized"`
	Hostname      string `json:"hostname"`
	TotalMemory   int64  `json:"total_memory"`
	TotalSwap     int64  `json:"total_swap"`
	MachineID     string `json:"machine_id"`
	DistributorID string `json:"distributor_id"`
	Description   string `json:"description"`
	Release       string `json:"release"`
	CodeName      string `json:"code_name"`
	Serial        string `json:"serial"`
	SnapModel     string `json:"snap_model"`
	Brand         string `json:"brand"`
}

type ComputerInfo struct {
	meminfoPath   string
	osReleasePath string
	machineIDPath string
	snapdClient   snapd.Client
	interval      time.Duration
}

func NewComputerInfo(client snapd.Client) *ComputerInfo {
	return &ComputerInfo{
		meminfoPath:   "/proc/meminfo",
		osReleasePath: "/etc/os-release",
		machineIDPath: "/etc/machine-id",
		snapdClient:   client,
		interval:      5 * time.Minute,
	}
}

func (p *ComputerInfo) Name() string { return "computer-info" }

func (p *ComputerInfo) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var prev ciSavedState
	if state != nil {
		if err := state.GetPluginState(&prev); err != nil {
			log.Printf("computer-info: loading state: %v", err)
		}
	}

	p.tick(ctx, sink, state, &prev)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.tick(ctx, sink, state, &prev)
		}
	}
}

func (p *ComputerInfo) tick(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor, prev *ciSavedState) {
	hostname, _ := os.Hostname()
	totalMemMB, totalSwapMB := p.readMeminfo()
	machineID := p.readMachineID()
	distID, description, release, codeName := p.readOSRelease()
	serial, snapModel, brand := p.readSnapAssertions(ctx)

	compMsg := exchange.Message{}
	if !prev.Initialized || hostname != prev.Hostname {
		compMsg["hostname"] = hostname
	}
	if !prev.Initialized || totalMemMB != prev.TotalMemory {
		compMsg["total-memory"] = totalMemMB
	}
	if !prev.Initialized || totalSwapMB != prev.TotalSwap {
		compMsg["total-swap"] = totalSwapMB
	}
	if !prev.Initialized || machineID != prev.MachineID {
		compMsg["machine-id"] = machineID
	}
	if len(compMsg) > 0 {
		compMsg["type"] = "computer-info"
		if err := sink.Send(ctx, compMsg); err != nil {
			log.Printf("computer-info: send: %v", err)
		}
	}

	distMsg := exchange.Message{}
	if !prev.Initialized || distID != prev.DistributorID {
		distMsg["distributor-id"] = distID
	}
	if !prev.Initialized || description != prev.Description {
		distMsg["description"] = description
	}
	if !prev.Initialized || release != prev.Release {
		distMsg["release"] = release
	}
	if !prev.Initialized || codeName != prev.CodeName {
		distMsg["code-name"] = codeName
	}
	if len(distMsg) > 0 {
		distMsg["type"] = "distribution-info"
		if err := sink.Send(ctx, distMsg); err != nil {
			log.Printf("computer-info: send distribution-info: %v", err)
		}
	}

	snapMsg := exchange.Message{}
	if !prev.Initialized || serial != prev.Serial {
		snapMsg["serial"] = serial
	}
	if !prev.Initialized || snapModel != prev.SnapModel {
		snapMsg["model"] = snapModel
	}
	if !prev.Initialized || brand != prev.Brand {
		snapMsg["brand"] = brand
	}
	// Only send snap-info on Ubuntu Core devices (serial assertion only exists on UC).
	// Classic Ubuntu has generic model/brand but no serial assertion.
	if len(snapMsg) > 0 && serial != "" {
		snapMsg["type"] = "snap-info"
		if err := sink.Send(ctx, snapMsg); err != nil {
			log.Printf("computer-info: send snap-info: %v", err)
		}
	}

	next := ciSavedState{
		Initialized:   true,
		Hostname:      hostname,
		TotalMemory:   totalMemMB,
		TotalSwap:     totalSwapMB,
		MachineID:     machineID,
		DistributorID: distID,
		Description:   description,
		Release:       release,
		CodeName:      codeName,
		Serial:        serial,
		SnapModel:     snapModel,
		Brand:         brand,
	}
	*prev = next
	if state != nil {
		if err := state.SetPluginState(next); err != nil {
			log.Printf("computer-info: saving state: %v", err)
		}
	}
}

func (p *ComputerInfo) readMeminfo() (totalMemMB, totalSwapMB int64) {
	f, err := os.Open(p.meminfoPath)
	if err != nil {
		log.Printf("computer-info: opening %s: %v", p.meminfoPath, err)
		return
	}
	defer f.Close()
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
		case "MemTotal":
			totalMemMB = val / 1024
		case "SwapTotal":
			totalSwapMB = val / 1024
		}
	}
	return
}

func (p *ComputerInfo) readMachineID() string {
	data, err := os.ReadFile(p.machineIDPath)
	if err != nil {
		log.Printf("computer-info: reading machine-id: %v", err)
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (p *ComputerInfo) readOSRelease() (distributorID, description, release, codeName string) {
	f, err := os.Open(p.osReleasePath)
	if err != nil {
		log.Printf("computer-info: opening %s: %v", p.osReleasePath, err)
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"`)
		switch key {
		case "NAME":
			distributorID = value
		case "PRETTY_NAME":
			description = value
		case "VERSION_ID":
			release = value
		case "VERSION_CODENAME":
			codeName = value
		}
	}
	return
}

func (p *ComputerInfo) readSnapAssertions(ctx context.Context) (serial, model, brand string) {
	if p.snapdClient == nil {
		return
	}
	assertions, err := p.snapdClient.GetAssertions(ctx)
	if err != nil {
		log.Printf("computer-info: getting snap assertions: %v", err)
		return
	}
	if assertions == nil {
		return
	}
	return assertions.Serial, assertions.Model, assertions.Brand
}
