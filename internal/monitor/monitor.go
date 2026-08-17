// Package monitor provides system information collection plugins that
// periodically gather data and send messages to the Landscape server.
package monitor

import (
	"context"
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
	Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error
}
