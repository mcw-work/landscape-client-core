package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
	"golang.org/x/sync/errgroup"
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
// exited. It returns nil on clean shutdown, and an error naming the plugins that
// failed repeatedly — the supervisor needs to distinguish those two cases.
func (r *Runner) Run(ctx context.Context) error {
	eg, egCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	failed := make(map[string]error)

	for _, p := range r.plugins {
		plugin := p
		eg.Go(func() error {
			if err := r.runPlugin(egCtx, plugin); err != nil {
				mu.Lock()
				failed[plugin.Name()] = err
				mu.Unlock()
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("monitor: runner group error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(failed) > 0 {
		names := make([]string, 0, len(failed))
		for name := range failed {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("monitor: plugins failed: %s (last error for %s: %w)",
			strings.Join(names, ", "), names[0], failed[names[0]])
	}
	return nil
}

// runPlugin runs a single plugin in a loop, recovering from panics and applying
// exponential backoff before each restart. It returns the last non-cancellation
// error observed when ctx is cancelled, or nil on clean shutdown.
func (r *Runner) runPlugin(ctx context.Context, plugin Plugin) error {
	backoff := runnerInitialBackoff
	var lastErr error
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

		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			lastErr = runErr
		}

		// Don't restart if context was cancelled.
		if ctx.Err() != nil {
			return lastErr
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
			return lastErr
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > runnerMaxBackoff {
			backoff = runnerMaxBackoff
		}
	}
}
