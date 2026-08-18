package monitor

import (
	"testing"
	"time"
)

func TestAccumulator_BuffersUntilTheSendWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAccumulator(60*time.Second, func() time.Time { return now })

	a.add("point-1")
	if got := a.drainIfDue(); got != nil {
		t.Errorf("drained before the send window elapsed: %v", got)
	}

	now = time.Unix(1030, 0)
	a.add("point-2")
	if got := a.drainIfDue(); got != nil {
		t.Errorf("drained at 30s with a 60s window: %v", got)
	}

	now = time.Unix(1061, 0)
	a.add("point-3")
	got := a.drainIfDue()
	if len(got) != 3 {
		t.Fatalf("want 3 buffered points, got %d: %v", len(got), got)
	}
}

func TestAccumulator_DrainEmptiesTheBuffer(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAccumulator(10*time.Second, func() time.Time { return now })

	a.add("a")
	now = time.Unix(1011, 0)
	if got := a.drainIfDue(); len(got) != 1 {
		t.Fatalf("want 1 point, got %d", len(got))
	}

	now = time.Unix(1022, 0)
	if got := a.drainIfDue(); got != nil {
		t.Errorf("second drain returned stale points: %v", got)
	}
}

func TestAccumulator_ZeroWindowSendsEveryPoint(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAccumulator(0, func() time.Time { return now })

	a.add("a")
	if got := a.drainIfDue(); len(got) != 1 {
		t.Errorf("a zero window should send immediately, got %d points", len(got))
	}
}
