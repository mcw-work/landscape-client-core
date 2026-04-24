package monitor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

// fakePlugin is a test double for Plugin.
type fakePlugin struct {
	name    string
	runFunc func(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error
}

func (f *fakePlugin) Name() string { return f.name }
func (f *fakePlugin) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	return f.runFunc(ctx, sink, state)
}

// newStore returns a persist.Store backed by a temp directory.
func newStore(t *testing.T) *persist.Store {
	t.Helper()
	return persist.New(t.TempDir() + "/state.json")
}

// TestRunner_AllPluginsStarted verifies that Run starts all plugins.
func TestRunner_AllPluginsStarted(t *testing.T) {
	var started int32
	var wg sync.WaitGroup

	makePlugin := func(name string) Plugin {
		wg.Add(1)
		return &fakePlugin{
			name: name,
			runFunc: func(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
				atomic.AddInt32(&started, 1)
				wg.Done()
				// Block until context is cancelled so Run doesn't loop.
				<-ctx.Done()
				return ctx.Err()
			},
		}
	}

	plugins := []Plugin{
		makePlugin("plugin-a"),
		makePlugin("plugin-b"),
		makePlugin("plugin-c"),
	}

	store := newStore(t)
	sink := &mockSink{}
	runner := New(plugins, sink, store)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		runner.Run(ctx) //nolint:errcheck
		close(runDone)
	}()

	// Wait for all 3 plugins to start.
	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for all plugins to start")
	}

	if got := atomic.LoadInt32(&started); got != 3 {
		t.Fatalf("expected 3 plugins started, got %d", got)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Run to return after cancel")
	}
}

// TestRunner_PanicingPluginRestarts verifies that a panicking plugin is
// restarted and runs a second time.
func TestRunner_PanicingPluginRestarts(t *testing.T) {
	var callCount int32

	// Use a very short initial backoff so the test doesn't take 1s.
	// We achieve this by having the plugin itself signal via a channel.
	ran := make(chan struct{}, 2)

	plugin := &fakePlugin{
		name: "panicker",
		runFunc: func(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
			n := atomic.AddInt32(&callCount, 1)
			ran <- struct{}{}
			if n == 1 {
				panic("intentional panic")
			}
			// Second call: block until context cancelled.
			<-ctx.Done()
			return ctx.Err()
		},
	}

	store := newStore(t)
	sink := &mockSink{}
	runner := New([]Plugin{plugin}, sink, store)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go runner.Run(ctx) //nolint:errcheck

	// Wait for first run (panic).
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first plugin run")
	}

	// Wait for second run (after backoff — default 1s).
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for plugin restart after panic")
	}

	if got := atomic.LoadInt32(&callCount); got < 2 {
		t.Fatalf("expected plugin to run at least twice, got %d", got)
	}

	cancel()
}

// TestRunner_ContextCancelStopsAll verifies that cancelling the context
// causes Run to return.
func TestRunner_ContextCancelStopsAll(t *testing.T) {
	makePlugin := func(name string) Plugin {
		return &fakePlugin{
			name: name,
			runFunc: func(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
	}

	plugins := []Plugin{makePlugin("p1"), makePlugin("p2"), makePlugin("p3")}
	store := newStore(t)
	sink := &mockSink{}
	runner := New(plugins, sink, store)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		runner.Run(ctx) //nolint:errcheck
		close(runDone)
	}()

	// Give plugins time to start.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestRunner_NoRestartAfterCancel verifies that a panicking plugin does NOT
// restart if the context is already cancelled.
func TestRunner_NoRestartAfterCancel(t *testing.T) {
	var callCount int32

	ctx, cancel := context.WithCancel(context.Background())

	plugin := &fakePlugin{
		name: "panic-and-cancelled",
		runFunc: func(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
			atomic.AddInt32(&callCount, 1)
			// Cancel the context before panicking so the runner sees ctx.Err() != nil.
			cancel()
			panic("panic while cancelled")
		},
	}

	store := newStore(t)
	sink := &mockSink{}
	runner := New([]Plugin{plugin}, sink, store)

	runDone := make(chan struct{})
	go func() {
		runner.Run(ctx) //nolint:errcheck
		close(runDone)
	}()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel + panic")
	}

	if got := atomic.LoadInt32(&callCount); got != 1 {
		t.Fatalf("expected plugin to run exactly once (no restart), got %d", got)
	}
}
