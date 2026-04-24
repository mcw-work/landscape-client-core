package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

type networkDeviceState struct {
	Hash string `json:"hash"`
}

type NetworkDevice struct {
	interval      time.Duration
	getInterfaces func() ([]net.Interface, error)
	getAddrs      func(iface *net.Interface) ([]net.Addr, error)
	sysNetPath    string
}

func NewNetworkDevice() *NetworkDevice {
	return &NetworkDevice{
		interval:      30 * time.Second,
		getInterfaces: net.Interfaces,
		getAddrs:      func(iface *net.Interface) ([]net.Addr, error) { return iface.Addrs() },
		sysNetPath:    "/sys/class/net",
	}
}

func (p *NetworkDevice) Name() string { return "network-device" }

func (p *NetworkDevice) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var saved networkDeviceState
	if state != nil {
		_ = state.GetPluginState(&saved)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			devices, speeds, err := p.collect()
			if err != nil {
				log.Printf("network-device: %v", err)
				continue
			}
			combined := []any{devices, speeds}
			data, err := json.Marshal(combined)
			if err != nil {
				log.Printf("network-device: marshal: %v", err)
				continue
			}
			hash := fmt.Sprintf("%x", sha256.Sum256(data))
			if hash == saved.Hash {
				continue
			}
			saved.Hash = hash
			if state != nil {
				_ = state.SetPluginState(saved)
			}
			msg := exchange.Message{
				"type":          "network-device",
				"devices":       devices,
				"device-speeds": speeds,
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("network-device: send: %v", err)
			}
		}
	}
}

func (p *NetworkDevice) collect() ([]map[string]any, []map[string]any, error) {
	ifaces, err := p.getInterfaces()
	if err != nil {
		return nil, nil, fmt.Errorf("listing interfaces: %w", err)
	}

	sort.Slice(ifaces, func(i, j int) bool {
		return ifaces[i].Name < ifaces[j].Name
	})

	var devices []map[string]any
	var speeds []map[string]any

	for idx := range ifaces {
		iface := &ifaces[idx]
		name := iface.Name
		if name == "lo" {
			continue
		}
		if strings.Contains(name, ".") || strings.Contains(name, ":") {
			continue
		}
		if strings.HasPrefix(name, "tap") {
			continue
		}

		mac := iface.HardwareAddr.String()
		flags := int(iface.Flags)

		ipAddr := ""
		broadcastAddr := "0.0.0.0"
		netmask := "0.0.0.0"

		addrs, err := p.getAddrs(iface)
		if err != nil {
			log.Printf("network-device: getting addrs for %s: %v", name, err)
		} else {
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok {
					continue
				}
				ip4 := ipNet.IP.To4()
				if ip4 == nil {
					continue
				}
				ipAddr = ip4.String()
				mask := ipNet.Mask
				netmask = fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
				broadcast := make(net.IP, 4)
				for i := 0; i < 4; i++ {
					broadcast[i] = ip4[i] | ^mask[i]
				}
				broadcastAddr = broadcast.String()
				break
			}
		}

		devices = append(devices, map[string]any{
			"interface":         name,
			"ip_address":        ipAddr,
			"mac_address":       mac,
			"broadcast_address": broadcastAddr,
			"netmask":           netmask,
			"flags":             flags,
		})

		speed := p.readSpeed(name)
		duplex := p.readDuplex(name)
		speeds = append(speeds, map[string]any{
			"interface": name,
			"speed":     speed,
			"duplex":    duplex,
		})
	}

	if devices == nil {
		devices = []map[string]any{}
	}
	if speeds == nil {
		speeds = []map[string]any{}
	}
	return devices, speeds, nil
}

func (p *NetworkDevice) readSpeed(iface string) int {
	path := fmt.Sprintf("%s/%s/speed", p.sysNetPath, iface)
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return n
}

func (p *NetworkDevice) readDuplex(iface string) bool {
	path := fmt.Sprintf("%s/%s/duplex", p.sysNetPath, iface)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "full"
}
