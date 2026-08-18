package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
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
	plugins   []Plugin
	sink      exchange.MessageSink
	store     *persist.Store
	heartbeat *Heartbeat
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
		plugins:   plugins,
		sink:      sink,
		store:     store,
		heartbeat: NewHeartbeat(nil),
	}
}

// watchdogThreshold returns how long a plugin may go without progress before it
// is considered wedged. Intervals range from 15s to 1h, so a single global
// threshold would either miss wedged fast plugins or false-positive slow ones.
func watchdogThreshold(interval time.Duration) time.Duration {
	const minThreshold = 2 * time.Minute
	t := interval * 3
	if t < minThreshold {
		return minThreshold
	}
	return t
}

// StaleSources returns the names of plugins that have stopped making progress,
// each judged against its own per-plugin threshold. A blocked goroutine keeps
// the process alive and healthy-looking, so the supervisor needs this signal to
// distinguish a wedged daemon from a healthy one.
func (r *Runner) StaleSources() []string {
	thresholds := make(map[string]time.Duration, len(r.plugins))
	for _, p := range r.plugins {
		thresholds[p.Name()] = watchdogThreshold(p.Interval())
	}
	return r.heartbeat.StaleSources(thresholds)
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
		slog.Error("monitor: runner group error", "error", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(failed) > 0 {
		names := make([]string, 0, len(failed))
		for name := range failed {
			names = append(names, name)
		}
		slices.Sort(names)
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
					slog.Error("monitor: plugin panicked", "plugin", plugin.Name(), "panic", rec, "stack", string(debug.Stack()))
				}
			}()
			state, err := r.store.Load()
			if err != nil {
				slog.Warn("monitor: cannot load plugin state, using empty state", "plugin", plugin.Name(), "error", err)
				state = &persist.State{PluginState: make(map[string]json.RawMessage)}
			}
			accessor := r.store.Accessor(plugin.Name(), state)
			if hs, ok := plugin.(interface{ setHeartbeat(*Heartbeat) }); ok {
				hs.setHeartbeat(r.heartbeat)
			}
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
			slog.Error("monitor: plugin failed", "plugin", plugin.Name(), "error", runErr)
		}

		if time.Since(started) >= runnerHealthyRunWindow {
			backoff = runnerInitialBackoff
		}

		// Exponential backoff before restart.
		slog.Warn("monitor: plugin restarting", "plugin", plugin.Name(), "backoff", backoff)
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
