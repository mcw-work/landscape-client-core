package monitor

import (
	"testing"
	"time"
)

func TestHeartbeat_FreshIsNotStale(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	hb.Beat("cpu-usage")

	stale := hb.Stale(60 * time.Second)
	if len(stale) != 0 {
		t.Errorf("want no stale sources, got %v", stale)
	}
}

func TestHeartbeat_DetectsStaleSource(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	hb.Beat("cpu-usage")
	hb.Beat("mount-info")

	// cpu-usage keeps beating; mount-info wedges.
	now = time.Unix(1000+120, 0)
	hb.Beat("cpu-usage")

	stale := hb.Stale(60 * time.Second)
	if len(stale) != 1 || stale[0] != "mount-info" {
		t.Fatalf("want [mount-info], got %v", stale)
	}
}

func TestHeartbeat_StaleIsSortedAndStable(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })
	for _, name := range []string{"users", "cpu-usage", "mount-info"} {
		hb.Beat(name)
	}
	now = time.Unix(1000+300, 0)

	stale := hb.Stale(60 * time.Second)
	want := []string{"cpu-usage", "mount-info", "users"}
	if len(stale) != len(want) {
		t.Fatalf("want %v, got %v", want, stale)
	}
	for i := range want {
		if stale[i] != want[i] {
			t.Fatalf("want %v, got %v", want, stale)
		}
	}
}

func TestHeartbeat_UnregisteredSourceIsNotStale(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := NewHeartbeat(func() time.Time { return now })

	// A plugin that has never beaten has not yet started; the watchdog must not
	// fire during startup.
	if stale := hb.Stale(time.Second); len(stale) != 0 {
		t.Errorf("want no stale sources before any beat, got %v", stale)
	}
}
