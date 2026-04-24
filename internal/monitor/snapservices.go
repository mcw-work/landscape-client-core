package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// snapServicesState is the persisted state for SnapServicesPlugin.
type snapServicesState struct {
	Hash string `json:"hash"`
}

// SnapServicesPlugin monitors snap services by querying snapd. It acts as a
// data-watcher: a message is sent only when the services list changes from the
// previously reported value. Message type and fields match the Python
// snap-services plugin exactly.
type SnapServicesPlugin struct {
	interval    time.Duration
	snapdClient snapd.Client
}

// NewSnapServices returns a SnapServicesPlugin with default settings.
func NewSnapServices(client snapd.Client) *SnapServicesPlugin {
	return &SnapServicesPlugin{
		interval:    time.Minute,
		snapdClient: client,
	}
}

// Name returns the Landscape message type string.
func (p *SnapServicesPlugin) Name() string { return "snap-services" }

// Run starts the periodic snap services collection loop. A message is emitted
// only when the services list changes from its previously reported value.
// The last reported hash is persisted via state so it survives restarts.
func (p *SnapServicesPlugin) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var prevHash string
	if state != nil {
		var saved snapServicesState
		if err := state.GetPluginState(&saved); err == nil {
			prevHash = saved.Hash
		}
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			services, err := p.snapdClient.ListServices(ctx)
			if err != nil {
				log.Printf("snap-services: listing services: %v", err)
				continue
			}
			// Sort by name for stable hashing and consistent output.
			sort.Slice(services, func(i, j int) bool {
				return services[i].Name < services[j].Name
			})
			hash := hashSnapServices(services)
			if hash == prevHash {
				continue // no change, skip
			}
			prevHash = hash
			if state != nil {
				_ = state.SetPluginState(snapServicesState{Hash: hash})
			}
			running := make([]any, 0, len(services))
			for _, svc := range services {
				running = append(running, map[string]any{
					"name":    svc.Name,
					"snap":    svc.Snap,
					"enabled": svc.Enabled,
					"active":  svc.Active,
				})
			}
			msg := exchange.Message{
				"type": "snap-services",
				"services": map[string]any{
					"running": running,
				},
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("snap-services: send: %v", err)
			}
		}
	}
}

// hashSnapServices returns a hex SHA-256 digest of the JSON-encoded services
// slice. The caller must sort the slice before calling this function.
func hashSnapServices(services []snapd.ServiceInfo) string {
	data, _ := json.Marshal(services)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
