package monitor

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// SnapPackagesPlugin monitors installed snaps by querying snapd.
// A full list is sent on every tick (not a diff). Message type and fields
// match the Python snaps plugin exactly.
type SnapPackagesPlugin struct {
	interval    time.Duration
	snapdClient snapd.Client
}

// NewSnapPackages returns a SnapPackagesPlugin with default settings.
func NewSnapPackages(client snapd.Client) *SnapPackagesPlugin {
	return &SnapPackagesPlugin{
		interval:    30 * time.Minute,
		snapdClient: client,
	}
}

// Name returns the Landscape message type string.
func (p *SnapPackagesPlugin) Name() string { return "snaps" }

// Run starts the periodic snap list collection loop. A full installed list is
// sent on every tick. On snapd error the message is sent with an empty
// installed list to keep the server in sync.
func (p *SnapPackagesPlugin) Run(ctx context.Context, sink exchange.MessageSink, _ *persist.PluginStateAccessor) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			installed := p.collect(ctx)
			msg := exchange.Message{
				"type": "snaps",
				"snaps": map[string]any{
					"installed": installed,
				},
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("snaps: send: %v", err)
			}
		}
	}
}

// collect queries snapd for installed snaps and converts them to the wire
// format. On error it logs and returns an empty slice.
func (p *SnapPackagesPlugin) collect(ctx context.Context) []any {
	snaps, err := p.snapdClient.ListSnaps(ctx)
	if err != nil {
		log.Printf("snaps: listing snaps: %v", err)
		return []any{}
	}
	result := make([]any, 0, len(snaps))
	for _, s := range snaps {
		result = append(result, map[string]any{
			"id":               s.Name,
			"name":             s.Name,
			"version":          s.Version,
			"revision":         fmt.Sprintf("%d", s.Revision),
			"tracking-channel": s.Channel,
			"publisher": map[string]any{
				"username":   s.Developer,
				"validation": "",
			},
			"confinement": "",
		})
	}
	return result
}
