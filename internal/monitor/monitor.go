// Package monitor provides system information collection plugins that
// periodically gather data and send messages to the Landscape server.
package monitor

import (
	"context"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// Plugin is the interface every monitor plugin implements.
type Plugin interface {
	Name() string
	Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error
}
