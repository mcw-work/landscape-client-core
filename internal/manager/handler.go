package manager

import (
	"context"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// Handler handles a specific inbound message type from the Landscape server.
type Handler interface {
	MessageType() string
	Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error
}
