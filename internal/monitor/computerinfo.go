package monitor

import (
	"bufio"
	"context"
	"log/slog"
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
	heartbeatSource
	meminfoPath   string
	osReleasePath string
	machineIDPath string
	snapdClient   snapd.Client
	getHostname   func() (string, error)
	interval      time.Duration
}

func NewComputerInfo(client snapd.Client) *ComputerInfo {
	return &ComputerInfo{
		meminfoPath:   "/proc/meminfo",
		osReleasePath: "/etc/os-release",
		machineIDPath: "/etc/machine-id",
		snapdClient:   client,
		getHostname:   os.Hostname,
		interval:      5 * time.Minute,
	}
}

func (p *ComputerInfo) Name() string { return "computer-info" }

func (p *ComputerInfo) Interval() time.Duration { return p.interval }

func (p *ComputerInfo) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var prev ciSavedState
	if state != nil {
		if err := state.GetPluginState(&prev); err != nil {
			slog.Warn("computer-info: cannot load state", "error", err)
		}
	}

	p.tick(ctx, sink, state, &prev)

	runTicker(ctx, p.interval, false, staggerFor(p.interval), func(ctx context.Context, _ time.Time) {
		p.beat(p.Name())
		p.tick(ctx, sink, state, &prev)
	})
	return nil
}

func (p *ComputerInfo) tick(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor, prev *ciSavedState) {
	getHostname := p.getHostname
	if getHostname == nil {
		getHostname = os.Hostname
	}
	hostname, hostnameErr := getHostname()
	if hostnameErr != nil {
		// Omit the field rather than sending "", which the server reads as
		// "this device has no hostname".
		slog.Warn("computer-info: cannot read hostname", "error", hostnameErr)
	}
	totalMemMB, totalSwapMB := p.readMeminfo()
	machineID := p.readMachineID()
	distID, description, release, codeName := p.readOSRelease()
	serial, snapModel, brand := p.readSnapAssertions(ctx)

	compMsg := exchange.Message{}
	if hostnameErr == nil && (!prev.Initialized || hostname != prev.Hostname) {
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
			slog.Warn("computer-info: send failed", "error", err)
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
			slog.Warn("computer-info: send distribution-info failed", "error", err)
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
			slog.Warn("computer-info: send snap-info failed", "error", err)
		}
	}

	// On a failed lookup keep the last known hostname so a transient error does
	// not churn state or re-send once it recovers.
	storedHostname := prev.Hostname
	if hostnameErr == nil {
		storedHostname = hostname
	}

	next := ciSavedState{
		Initialized:   true,
		Hostname:      storedHostname,
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
			slog.Error("computer-info: cannot save state", "error", err)
		}
	}
}

func (p *ComputerInfo) readMeminfo() (totalMemMB, totalSwapMB int64) {
	f, err := os.Open(p.meminfoPath)
	if err != nil {
		slog.Warn("computer-info: cannot open meminfo", "path", p.meminfoPath, "error", err)
		return
	}
	defer func() {
		_ = f.Close()
	}()
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
		slog.Warn("computer-info: cannot read machine-id", "error", err)
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (p *ComputerInfo) readOSRelease() (distributorID, description, release, codeName string) {
	f, err := os.Open(p.osReleasePath)
	if err != nil {
		slog.Warn("computer-info: cannot open os-release", "path", p.osReleasePath, "error", err)
		return distributorID, description, release, codeName
	}
	defer func() {
		_ = f.Close()
	}()
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
	return distributorID, description, release, codeName
}

func (p *ComputerInfo) readSnapAssertions(ctx context.Context) (serial, model, brand string) {
	if p.snapdClient == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, snapdCallTimeout)
	assertions, err := p.snapdClient.GetAssertions(callCtx)
	cancel()
	if err != nil {
		slog.Warn("computer-info: cannot get snap assertions", "error", err)
		return
	}
	if assertions == nil {
		return
	}
	return assertions.Serial, assertions.Model, assertions.Brand
}
