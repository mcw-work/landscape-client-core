package monitor

import (
	"context"
	"log"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

// rebootState is the persisted state for RebootRequiredPlugin.
type rebootState struct {
	Initialized bool `json:"initialized"`
	Flag        bool `json:"flag"`
}

// RebootRequiredPlugin monitors whether the system requires a reboot via the
// snapd API. It acts as a data-watcher: a message is sent only when the value
// changes from the previously reported value. Message type and fields match the
// Python rebootrequired.py plugin exactly.
type RebootRequiredPlugin struct {
	heartbeatSource
	interval time.Duration
	snapd    snapd.Client
}

// NewRebootRequired returns a RebootRequiredPlugin with default settings.
func NewRebootRequired(client snapd.Client) *RebootRequiredPlugin {
	return &RebootRequiredPlugin{
		interval: 5 * time.Minute,
		snapd:    client,
	}
}

// Name returns the Landscape message type string.
func (p *RebootRequiredPlugin) Name() string { return "reboot-required-info" }

func (p *RebootRequiredPlugin) Interval() time.Duration { return p.interval }

// Run starts the periodic reboot-required check loop. A message is emitted
// only when the reboot flag changes from its previously reported value.
// The last reported value is persisted via state so it survives restarts.
func (p *RebootRequiredPlugin) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	// Load the last reported flag from persisted state if available.
	var prevFlag *bool
	if state != nil {
		var saved rebootState
		if err := state.GetPluginState(&saved); err == nil && saved.Initialized {
			f := saved.Flag
			prevFlag = &f
		}
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.beat(p.Name())
			callCtx, cancel := context.WithTimeout(ctx, snapdCallTimeout)
			flag, err := p.snapd.GetRebootRequired(callCtx)
			cancel()
			if err != nil {
				log.Printf("reboot-required: %v", err)
				continue
			}
			if prevFlag != nil && *prevFlag == flag {
				continue // no change, skip
			}
			// Value changed (or first sample): persist, then update the tracker
			// only on a successful save so a failure is re-detected next tick.
			if state != nil {
				if err := state.SetPluginState(rebootState{Initialized: true, Flag: flag}); err != nil {
					log.Printf("%s: saving state: %v; will retry next tick", p.Name(), err)
					continue
				}
			}
			f := flag
			prevFlag = &f
			msg := exchange.Message{
				"type":     "reboot-required-info",
				"flag":     flag,
				"packages": []any{},
			}
			if err := sink.Send(ctx, msg); err != nil {
				log.Printf("reboot-required: send: %v", err)
			}
		}
	}
}
