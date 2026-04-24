package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// Runner manages a set of monitor plugins, running each in its own goroutine
// with panic recovery and exponential backoff on failure.
type Runner struct {
	plugins []Plugin
	sink    exchange.MessageSink
	store   *persist.Store
}

var (
	runnerInitialBackoff   = time.Second
	runnerMaxBackoff       = 5 * time.Minute
	runnerHealthyRunWindow = 30 * time.Second
)

// New returns a Runner that will run the given plugins, sending messages to
// sink and loading/saving per-plugin state via store.
func New(plugins []Plugin, sink exchange.MessageSink, store *persist.Store) *Runner {
	return &Runner{
		plugins: plugins,
		sink:    sink,
		store:   store,
	}
}

// Run starts one goroutine per plugin and blocks until all goroutines have
// exited. It always returns nil.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, p := range r.plugins {
		wg.Add(1)
		go func(plugin Plugin) {
			defer wg.Done()
			r.runPlugin(ctx, plugin)
		}(p)
	}
	wg.Wait()
	return nil
}

// runPlugin runs a single plugin in a loop, recovering from panics and applying
// exponential backoff before each restart. It returns when ctx is cancelled.
func (r *Runner) runPlugin(ctx context.Context, plugin Plugin) {
	backoff := runnerInitialBackoff
	for {
		started := time.Now()
		var runErr error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runErr = fmt.Errorf("panic: %v", rec)
					log.Printf("monitor: plugin %s panicked: %v\n%s", plugin.Name(), rec, debug.Stack())
				}
			}()
			state, err := r.store.Load()
			if err != nil {
				log.Printf("monitor: plugin %s: loading state: %v; using empty state", plugin.Name(), err)
				state = &persist.State{PluginState: make(map[string]json.RawMessage)}
			}
			accessor := r.store.Accessor(plugin.Name(), state)
			runErr = plugin.Run(ctx, r.sink, accessor)
		}()

		// Don't restart if context was cancelled.
		if ctx.Err() != nil {
			return
		}

		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			log.Printf("monitor: plugin %s failed: %v", plugin.Name(), runErr)
		}

		if time.Since(started) >= runnerHealthyRunWindow {
			backoff = runnerInitialBackoff
		}

		// Exponential backoff before restart.
		log.Printf("monitor: plugin %s restarting in %v", plugin.Name(), backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > runnerMaxBackoff {
			backoff = runnerMaxBackoff
		}
	}
}
