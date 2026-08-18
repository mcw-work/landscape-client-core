package monitor

import (
	"slices"
	"sync"
	"time"
)

// Heartbeat records the last time each named source made progress. A source that
// stops beating is wedged: restart-condition only covers process exit, so a
// goroutine blocked in a syscall keeps the process looking healthy while
// reporting nothing.
//
// now is injectable so the watchdog can be tested without sleeping.
type Heartbeat struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func NewHeartbeat(now func() time.Time) *Heartbeat {
	if now == nil {
		now = time.Now
	}
	return &Heartbeat{
		last: make(map[string]time.Time),
		now:  now,
	}
}

// Beat records progress for source. Nil-safe.
func (h *Heartbeat) Beat(source string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[source] = h.now()
}

// Stale returns the sorted names of sources that have not beaten within
// threshold. Sources that have never beaten are excluded: they have not started
// yet, and the watchdog must not fire during startup. Nil-safe.
func (h *Heartbeat) Stale(threshold time.Duration) []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	cutoff := h.now().Add(-threshold)
	var stale []string
	for source, last := range h.last {
		if last.Before(cutoff) {
			stale = append(stale, source)
		}
	}
	slices.Sort(stale)
	return stale
}

// StaleSources returns beaten sources older than their per-source threshold.
// Sources absent from thresholds, or never beaten, are excluded. Nil-safe.
func (h *Heartbeat) StaleSources(thresholds map[string]time.Duration) []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	var stale []string
	for source, last := range h.last {
		threshold, ok := thresholds[source]
		if !ok {
			continue
		}
		cutoff := h.now().Add(-threshold)
		if last.Before(cutoff) {
			stale = append(stale, source)
		}
	}
	slices.Sort(stale)
	return stale
}

// heartbeatSource is embedded by every plugin so the runner can wire a
// heartbeat in without changing the Plugin.Run signature. A nil heartbeat
// (e.g. in unit tests that construct plugins directly) makes beat a no-op.
type heartbeatSource struct{ hb *Heartbeat }

func (s *heartbeatSource) setHeartbeat(h *Heartbeat) { s.hb = h }
func (s *heartbeatSource) beat(source string)        { s.hb.Beat(source) }
