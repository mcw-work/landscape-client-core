package monitor

import (
	"context"
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
	p.send(ctx, sink)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.send(ctx, sink)
		}
	}
}

// SendNow immediately collects and sends the current snap list to sink.
// Called by snap manager handlers after install/remove/refresh to give
// the server an up-to-date snap list without waiting for the next tick.
func (p *SnapPackagesPlugin) SendNow(ctx context.Context, sink exchange.MessageSink) {
	p.send(ctx, sink)
}

func (p *SnapPackagesPlugin) send(ctx context.Context, sink exchange.MessageSink) {
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
		snapID := s.ID
		// Devmode snaps have no store ID — use a name-based fallback so the
		// server can still index them (matches Python client behaviour).
		if snapID == "" {
			snapID = "no-serial-" + s.Name
		}
		result = append(result, map[string]any{
			"id":               snapID,
			"name":             s.Name,
			"version":          s.Version,
			"revision":         s.Revision,
			"tracking-channel": s.Channel,
			"publisher": map[string]any{
				"username":   s.Developer,
				"validation": "",
			},
			"confinement": s.Confinement,
			"summary":     s.Summary,
		})
	}
	return result
}
