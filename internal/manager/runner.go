package manager

import (
	"context"
	"fmt"
	"log"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

// Runner registers Handlers with a CommandSource and dispatches inbound
// messages to the appropriate handler in a dedicated goroutine.
type Runner struct {
	handlers []Handler
	source   exchange.CommandSource
	result   exchange.ResultSink
	opCtxMgr *OperationContextManager
}

type operationContextAware interface {
	SetOperationContextManager(*OperationContextManager)
}

// NewRunner constructs a Runner that will dispatch messages from source to
// handlers and send results via result.
func NewRunner(handlers []Handler, source exchange.CommandSource, result exchange.ResultSink) *Runner {
	opCtxMgr := NewOperationContextManager()
	allHandlers := append([]Handler{}, handlers...)
	allHandlers = append(allHandlers, NewCancelHandler(opCtxMgr))
	r := &Runner{
		handlers: allHandlers,
		source:   source,
		result:   result,
		opCtxMgr: opCtxMgr,
	}

	for _, h := range allHandlers {
		if aware, ok := h.(operationContextAware); ok {
			aware.SetOperationContextManager(opCtxMgr)
		}
	}

	return r
}

// Register subscribes each handler to its message type on the CommandSource.
// Each inbound message is dispatched in a new goroutine. Panics inside
// handlers are recovered, logged, and reported as StatusFailed results.
func (r *Runner) Register() {
	for _, h := range r.handlers {
		handler := h // capture loop variable
		r.source.Subscribe(handler.MessageType(), func(ctx context.Context, msg exchange.Message) {
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						opID, _ := msg["operation-id"].(int64)
						log.Printf("manager: handler %s panicked: %v", handler.MessageType(), rec)
						_ = r.result.SendResult(ctx, opID, exchange.StatusFailed, fmt.Sprintf("panic: %v", rec))
					}
				}()
				if err := handler.Handle(ctx, msg, r.result); err != nil {
					log.Printf("manager: handler %s error: %v", handler.MessageType(), err)
				}
			}()
		})
	}
}
