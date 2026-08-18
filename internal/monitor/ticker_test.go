package monitor

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRunTicker_CallsOnEachTick(t *testing.T) {
	var mu sync.Mutex
	var calls int

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	runTicker(ctx, 20*time.Millisecond, false, 0, func(context.Context, time.Time) {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if calls < 3 {
		t.Errorf("want at least 3 ticks in 200ms at 20ms, got %d", calls)
	}
}

func TestRunTicker_RunImmediatelyFiresBeforeTheFirstInterval(t *testing.T) {
	fired := make(chan struct{}, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go runTicker(ctx, time.Hour, true, 0, func(context.Context, time.Time) {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runImmediately did not fire before the first interval")
	}
}

func TestRunTicker_WithoutRunImmediatelyWaits(t *testing.T) {
	fired := make(chan struct{}, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go runTicker(ctx, time.Hour, false, 0, func(context.Context, time.Time) {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	select {
	case <-fired:
		t.Fatal("fired before the first interval without runImmediately")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRunTicker_ReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		runTicker(ctx, time.Hour, false, 0, func(context.Context, time.Time) {})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTicker did not return on cancellation")
	}
}

func TestRunTicker_StaggerDelaysStartWithinBound(t *testing.T) {
	fired := make(chan time.Time, 1)
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go runTicker(ctx, 50*time.Millisecond, true, 200*time.Millisecond, func(_ context.Context, _ time.Time) {
		select {
		case fired <- time.Now():
		default:
		}
	})

	select {
	case at := <-fired:
		if d := at.Sub(start); d > 400*time.Millisecond {
			t.Errorf("stagger exceeded its bound: first tick after %v", d)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("never fired")
	}
}
