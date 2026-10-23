package monitor

import "time"

// accumulator buffers data points between sends. The plugins previously
// conflated sampling interval with send interval, producing one message per
// sample with a single point and a full bpickle envelope each. Python separates
// the two and sends one message per window.
//
// now is injectable so the window can be tested without sleeping.
type accumulator struct {
	window   time.Duration
	now      func() time.Time
	lastSend time.Time
	points   []any
}

func newAccumulator(window time.Duration, now func() time.Time) *accumulator {
	if now == nil {
		now = time.Now
	}
	return &accumulator{
		window:   window,
		now:      now,
		lastSend: now(),
	}
}

func (a *accumulator) add(point any) {
	a.points = append(a.points, point)
}

// drainIfDue returns the buffered points and resets the window, or nil if the
// window has not elapsed. A zero window sends every point immediately.
func (a *accumulator) drainIfDue() []any {
	if len(a.points) == 0 {
		return nil
	}
	if a.window > 0 && a.now().Sub(a.lastSend) < a.window {
		return nil
	}
	points := a.points
	a.points = nil
	a.lastSend = a.now()
	return points
}
