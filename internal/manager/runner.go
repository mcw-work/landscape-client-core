package manager

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

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
	wg       sync.WaitGroup
	opCtxMgr *OperationContextManager
}

type operationContextAware interface {
	SetOperationContextManager(*OperationContextManager)
}

// NewRunner constructs a Runner that will dispatch messages from source to
// handlers and send results via result.
// maxConcurrency is optional and defaults to 100.
func NewRunner(handlers []Handler, source exchange.CommandSource, result exchange.ResultSink, maxConcurrency ...int) *Runner {
	limit := 100
	if len(maxConcurrency) > 0 && maxConcurrency[0] > 0 {
		limit = maxConcurrency[0]
	}

	opCtxMgr := NewOperationContextManager()
	allHandlers := append([]Handler{}, handlers...)
	allHandlers = append(allHandlers, NewCancelHandler(opCtxMgr))
	r := &Runner{
		handlers: allHandlers,
		source:   source,
		result:   result,
		sem:      semaphore.NewWeighted(int64(limit)),
		wg:       sync.WaitGroup{},
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
// Handler concurrency is bounded by the semaphore, and lifecycle is tracked
// by the WaitGroup.
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

			r.wg.Add(1)
			go func() {
				defer r.sem.Release(1)
				defer r.wg.Done()
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

// Wait blocks until all in-flight handler goroutines have completed.
func (r *Runner) Wait() {
	r.wg.Wait()
}

// WaitWithTimeout blocks until all in-flight handler goroutines complete or
// the timeout is reached.
func (r *Runner) WaitWithTimeout(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("manager: wait for handlers timed out after %s", timeout)
	}
}
