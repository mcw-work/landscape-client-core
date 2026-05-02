package manager

import (
	"context"
	"fmt"
	"log"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"golang.org/x/sync/semaphore"
)

// Runner registers Handlers with a CommandSource and dispatches inbound
// messages to the appropriate handler in a dedicated goroutine.
type Runner struct {
	handlers []Handler
	source   exchange.CommandSource
	result   exchange.ResultSink
	sem      *semaphore.Weighted
}

// NewRunner constructs a Runner that will dispatch messages from source to
// handlers and send results via result. maxConcurrency limits the number of
// concurrent handler executions.
func NewRunner(handlers []Handler, source exchange.CommandSource, result exchange.ResultSink, maxConcurrency int) *Runner {
	return &Runner{
		handlers: handlers,
		source:   source,
		result:   result,
		sem:      semaphore.NewWeighted(int64(maxConcurrency)),
	}
}

// Register subscribes each handler to its message type on the CommandSource.
// Each inbound message is dispatched in a new goroutine. Panics inside
// handlers are recovered, logged, and reported as StatusFailed results.
// Handler concurrency is bounded by the semaphore.
func (r *Runner) Register() {
	for _, h := range r.handlers {
		handler := h // capture loop variable
		r.source.Subscribe(handler.MessageType(), func(ctx context.Context, msg exchange.Message) {
			if err := r.sem.Acquire(ctx, 1); err != nil {
				opID, _ := msg["operation-id"].(int64)
				log.Printf("manager: handler %s semaphore acquire error: %v", handler.MessageType(), err)
				_ = r.result.SendResult(ctx, opID, exchange.StatusFailed, fmt.Sprintf("semaphore acquire failed: %v", err))
				return
			}
			go func() {
				defer r.sem.Release(1)
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
