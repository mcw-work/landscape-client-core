// Package monitor provides system information collection plugins that
// periodically gather data and send messages to the Landscape server.
package monitor

import (
	"context"
	"math/rand"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// snapdCallTimeout bounds a single snapd request. Plugins hold the
// daemon-lifetime context, so without this a stalled snapd socket wedges the
// plugin permanently.
const snapdCallTimeout = 30 * time.Second

// Plugin is the interface every monitor plugin implements.
type Plugin interface {
	Name() string
	Interval() time.Duration
	Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error
}

// runTicker drives a plugin's periodic work until ctx is cancelled.
//
// Every plugin previously hand-rolled this loop, so each cross-cutting change —
// an initial tick, a launch stagger, a per-tick timeout — meant 15 edits.
//
// stagger, when non-zero, delays the recurring schedule by a random duration up
// to that bound. Without it all plugins start together and, because several
// share an interval, re-converge on the same tick forever: a periodic CPU spike
// and a burst of simultaneous sends. Mirrors Python's stagger_launch.
//
// runImmediately fires before the stagger delay: an immediate report (e.g. a
// just-rebooted device) must not be held back by launch stagger, which only
// exists to spread out the recurring ticks.
func runTicker(ctx context.Context, interval time.Duration, runImmediately bool, stagger time.Duration, fn func(context.Context, time.Time)) {
	if runImmediately {
		fn(ctx, time.Now())
	}

	if stagger > 0 {
		delay := time.Duration(rand.Int63n(int64(stagger)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			fn(ctx, t)
		}
	}
}

// staggerLaunchFraction mirrors Python's config.stagger_launch: each plugin's
// first tick is delayed by a random fraction of its own interval, so plugins
// sharing an interval do not converge on the same tick forever.
const staggerLaunchFraction = 0.1

func staggerFor(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * staggerLaunchFraction)
}
