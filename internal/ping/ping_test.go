package ping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// pingResponse encodes a bpickle response body: {"messages": messages}.
func pingResponse(t *testing.T, messages bool) []byte {
	t.Helper()
	b, err := bpickle.Marshal(map[string]any{"messages": messages})
	if err != nil {
		t.Fatalf("marshal ping response: %v", err)
	}
	return b
}

// newTransport returns a transport.Client with default settings (no TLS, no proxy).
func newTransport(t *testing.T) *transport.Client {
	t.Helper()
	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	return tc
}

// TestPing_TriggersExchangeWhenMessagesTrue verifies that doPing returns true
// and TriggerExchange is called when the server says messages=True.
func TestPing_TriggersExchangeWhenMessagesTrue(t *testing.T) {
	var triggered atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify method and content-type.
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.FormValue("insecure_id"); got != "42" {
			t.Errorf("insecure_id: want 42, got %q", got)
		}
		w.Write(pingResponse(t, true))
	}))
	defer srv.Close()

	p := New(srv.URL, func() string { return "42" }, func() { triggered.Store(true) }, 10*time.Millisecond, newTransport(t))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go p.Run(ctx) //nolint:errcheck

	// Wait for triggered to become true.
	deadline := time.Now().Add(400 * time.Millisecond)
	for !triggered.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !triggered.Load() {
		t.Error("expected TriggerExchange to be called, but it was not")
	}
}

// TestPing_NoTriggerWhenMessagesFalse verifies TriggerExchange is NOT called
// when the server says messages=False.
func TestPing_NoTriggerWhenMessagesFalse(t *testing.T) {
	var triggered atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(pingResponse(t, false))
	}))
	defer srv.Close()

	p := New(srv.URL, func() string { return "99" }, func() { triggered.Store(true) }, 10*time.Millisecond, newTransport(t))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	p.Run(ctx) //nolint:errcheck

	if triggered.Load() {
		t.Error("expected TriggerExchange NOT to be called, but it was")
	}
}

// TestPing_SkipsWhenNotRegistered verifies that pings are skipped when
// getInsecureID returns empty string.
func TestPing_SkipsWhenNotRegistered(t *testing.T) {
	var pinged atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pinged.Store(true)
		w.Write(pingResponse(t, false))
	}))
	defer srv.Close()

	// Always return empty insecure-id (not yet registered).
	p := New(srv.URL, func() string { return "" }, func() {}, 10*time.Millisecond, newTransport(t))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	p.Run(ctx) //nolint:errcheck

	if pinged.Load() {
		t.Error("expected no ping requests when not registered, but server was contacted")
	}
}

// TestPing_HandlesServerError verifies that HTTP errors are logged and the
// loop continues without crashing.
func TestPing_HandlesServerError(t *testing.T) {
	var triggered atomic.Bool
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		// First call: server error. Second call: messages waiting.
		if callCount.Load() == 1 {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Write(pingResponse(t, true))
	}))
	defer srv.Close()

	p := New(srv.URL, func() string { return "7" }, func() { triggered.Store(true) }, 10*time.Millisecond, newTransport(t))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go p.Run(ctx) //nolint:errcheck

	deadline := time.Now().Add(400 * time.Millisecond)
	for !triggered.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !triggered.Load() {
		t.Error("expected TriggerExchange to be called after server recovered")
	}
}

// TestPing_SetInterval verifies that SetInterval changes the ping frequency.
func TestPing_SetInterval(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Write(pingResponse(t, false))
	}))
	defer srv.Close()

	// Start with a 1-second interval — should not ping in 200ms.
	p := New(srv.URL, func() string { return "1" }, func() {}, time.Second, newTransport(t))
	p.SetInterval(10 * time.Millisecond) // now fast

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	p.Run(ctx) //nolint:errcheck

	if callCount.Load() == 0 {
		t.Error("expected at least one ping after SetInterval, got none")
	}
}

// TestPinger_DoPing_FormValues verifies insecure_id form field encoding.
func TestPinger_DoPing_FormValues(t *testing.T) {
	var gotValues url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotValues = r.Form
		w.Write(pingResponse(t, false))
	}))
	defer srv.Close()

	p := &Pinger{
		pingURL: srv.URL,
		tc:      newTransport(t),
	}

	p.doPing(context.Background(), "special-id-123")

	if got := gotValues.Get("insecure_id"); got != "special-id-123" {
		t.Errorf("insecure_id: want %q, got %q", "special-id-123", got)
	}
}

func TestPinger_IntervalConcurrentAccess(t *testing.T) {
	p := New("", func() string { return "" }, func() {}, 10*time.Millisecond, newTransport(t))

	durations := []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
	}

	const readers = 8
	const writers = 4
	const iterations = 500

	var wg sync.WaitGroup
	start := make(chan struct{})
	var sawValue atomic.Bool

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				p.SetInterval(durations[(j+offset)%len(durations)])
			}
		}(i)
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				got := p.GetInterval()
				for _, want := range durations {
					if got == want {
						sawValue.Store(true)
						break
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	if !sawValue.Load() {
		t.Fatal("expected concurrent readers to observe a valid configured interval")
	}
}
