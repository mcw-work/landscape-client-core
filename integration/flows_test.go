package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/manager"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/ping"
	"github.com/canonical/landscape-client-core/internal/transport"
)

func registeredState() *persist.State {
	return &persist.State{SecureID: "sec-123", InsecureID: "ins-456"}
}

// slowSchedule makes scheduled exchanges effectively never fire, so any
// further exchange in a test must come from a wake-up (urgent send, ping,
// backlog) rather than the timer.
func slowSchedule(cfg *config.Config) {
	cfg.ExchangeInterval = time.Hour
}

func waitUntil(t *testing.T, desc string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", desc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func messageTypes(payload map[string]any) []string {
	var types []string
	for _, m := range payloadMessages(payload) {
		if s, ok := m["type"].(string); ok {
			types = append(types, s)
		}
	}
	return types
}

func newStoreWith(t *testing.T, dir string, state *persist.State) *persist.Store {
	t.Helper()
	store := persist.New(filepath.Join(dir, "state.json"))
	if state != nil {
		if err := store.Save(state); err != nil {
			t.Fatalf("store.Save: %v", err)
		}
	}
	return store
}

func TestRegistrationRequestContents(t *testing.T) {
	fs := newFakeServer(t)
	ex, _ := newTestExchange(t, fs.URL(), nil)
	startExchange(t, ex)
	fs.waitForCount(t, 1, 3*time.Second)

	h := fs.header(0)
	if got := h.Get("X-Computer-ID"); got != "" {
		t.Errorf("unregistered client sent X-Computer-ID %q", got)
	}
	if got := h.Get("X-Message-API"); got != "3.3" {
		t.Errorf("X-Message-API = %q, want 3.3", got)
	}

	payload := fs.get(0)
	if _, ok := payload["client-accepted-types"]; !ok {
		t.Error("first exchange must advertise client-accepted-types")
	}
	reg := payloadMessages(payload)[0]
	want := map[string]any{
		"type":                  "register",
		"api":                   "3.3",
		"account_name":          "test-account",
		"registration_password": "test-key",
		"computer_title":        "test-computer",
	}
	for k, v := range want {
		if reg[k] != v {
			t.Errorf("register[%q] = %#v, want %#v", k, reg[k], v)
		}
	}
}

func TestUnknownIDTriggersReRegistration(t *testing.T) {
	fs := newFakeServer(t)
	fs.queueResponse(map[string]any{
		"messages":               []any{map[string]any{"type": "unknown-id"}},
		"next-expected-sequence": int64(0),
	})

	ex, store := newTestExchange(t, fs.URL(), registeredState())
	startExchange(t, ex)
	fs.waitForCount(t, 2, 3*time.Second)

	if got := fs.header(0).Get("X-Computer-ID"); got != "sec-123" {
		t.Errorf("first exchange X-Computer-ID = %q, want sec-123", got)
	}
	if got := fs.header(1).Get("X-Computer-ID"); got != "" {
		t.Errorf("after unknown-id the client must drop its identity, sent %q", got)
	}
	if types := messageTypes(fs.get(1)); len(types) == 0 || types[0] != "register" {
		t.Errorf("second exchange messages = %v, want register first", types)
	}

	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.SecureID != "" || state.InsecureID != "" {
		t.Errorf("identity not cleared: %+v", state)
	}
}

func TestExchangeTokenIsEchoed(t *testing.T) {
	fs := newFakeServer(t)
	fs.queueResponse(map[string]any{
		"messages":            []any{},
		"next-exchange-token": "tok-1",
	})

	ex, store := newTestExchange(t, fs.URL(), registeredState())
	startExchange(t, ex)
	fs.waitForCount(t, 2, 3*time.Second)

	if got := fs.header(0).Get("X-Exchange-Token"); got != "" {
		t.Errorf("first exchange sent token %q before receiving one", got)
	}
	if got := fs.header(1).Get("X-Exchange-Token"); got != "tok-1" {
		t.Errorf("second exchange X-Exchange-Token = %q, want tok-1", got)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.ExchangeToken != "tok-1" {
		t.Errorf("persisted token = %q, want tok-1", state.ExchangeToken)
	}
}

// A partial ACK must retransmit exactly the un-ACK'd tail at the ACK'd sequence.
func TestPartialAckRetransmitsUnackedMessages(t *testing.T) {
	fs := newFakeServer(t)
	fs.queueResponse(map[string]any{
		"messages":               []any{},
		"next-expected-sequence": int64(1),
	})

	ex, _ := newTestExchange(t, fs.URL(), registeredState())
	for i := range 3 {
		_ = ex.Send(context.Background(), exchange.Message{"type": "test-msg", "n": int64(i)})
	}
	startExchange(t, ex)
	fs.waitForCount(t, 2, 3*time.Second)

	if got := len(payloadMessages(fs.get(0))); got != 3 {
		t.Fatalf("first exchange sent %d messages, want 3", got)
	}
	second := fs.get(1)
	if seq := toInt64(second["sequence"]); seq != 1 {
		t.Errorf("retransmit sequence = %d, want 1", seq)
	}
	var ns []int64
	for _, m := range payloadMessages(second) {
		ns = append(ns, toInt64(m["n"]))
	}
	if !slices.Equal(ns, []int64{1, 2}) {
		t.Errorf("retransmitted n = %v, want [1 2]", ns)
	}
}

func TestAcceptedTypesFiltersOutboundMessages(t *testing.T) {
	fs := newFakeServer(t)
	state := registeredState()
	state.AcceptedTypes = []string{"cpu-usage"}
	state.AcceptedTypesHash = hashTypes(state.AcceptedTypes)

	ex, _ := newTestExchange(t, fs.URL(), state)
	ctx := context.Background()
	_ = ex.Send(ctx, exchange.Message{"type": "cpu-usage"})
	_ = ex.Send(ctx, exchange.Message{"type": "memory-info"})
	_ = ex.SendResult(ctx, 5, exchange.StatusSucceeded, "ok")
	startExchange(t, ex)
	fs.waitForCount(t, 2, 3*time.Second)

	got := messageTypes(fs.get(0))
	slices.Sort(got)
	if want := []string{"cpu-usage", "operation-result"}; !slices.Equal(got, want) {
		t.Errorf("sent types = %v, want %v", got, want)
	}
	if _, ok := fs.get(0)["client-accepted-types"]; ok {
		t.Error("client-accepted-types must not be sent once the hash is known")
	}
	for i := 1; i < fs.count(); i++ {
		if slices.Contains(messageTypes(fs.get(i)), "memory-info") {
			t.Errorf("exchange %d sent a type the server does not accept", i)
		}
	}
}

// A large backlog must drain in protocol-sized batches without waiting for the
// scheduled interval between them.
func TestBacklogDrainsInBatches(t *testing.T) {
	fs := newFakeServer(t)
	store := newStoreWith(t, t.TempDir(), registeredState())
	ex := newExchange(t, fs.URL(), store, slowSchedule)
	for i := range 250 {
		_ = ex.Send(context.Background(), exchange.Message{"type": "test-msg", "n": int64(i)})
	}
	startExchange(t, ex)
	fs.waitForCount(t, 3, 3*time.Second)

	wantSizes := []int{100, 100, 50}
	wantSeqs := []int64{0, 100, 200}
	for i := range 3 {
		p := fs.get(i)
		if got := len(payloadMessages(p)); got != wantSizes[i] {
			t.Errorf("exchange %d: %d messages, want %d", i, got, wantSizes[i])
		}
		if got := toInt64(p["sequence"]); got != wantSeqs[i] {
			t.Errorf("exchange %d: sequence %d, want %d", i, got, wantSeqs[i])
		}
	}
}

func TestShutdownDrainsPendingMessages(t *testing.T) {
	fs := newFakeServer(t)
	store := newStoreWith(t, t.TempDir(), registeredState())
	ex := newExchange(t, fs.URL(), store, slowSchedule)
	stop := startExchange(t, ex)
	fs.waitForCount(t, 1, 3*time.Second)

	_ = ex.Send(context.Background(), exchange.Message{"type": "late-msg"})
	stop()

	if fs.count() != 2 {
		t.Fatalf("want a final drain exchange, got %d exchanges", fs.count())
	}
	if types := messageTypes(fs.get(1)); !slices.Equal(types, []string{"late-msg"}) {
		t.Errorf("drain exchange types = %v, want [late-msg]", types)
	}
}

// An operation result queued during a server outage must survive a daemon
// restart (e.g. a snap refresh) and be delivered once the server recovers.
func TestQueueSurvivesServerOutageAndRestart(t *testing.T) {
	fs := newFakeServer(t)
	fs.setDown(true)

	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "queue.json")
	store := newStoreWith(t, dir, registeredState())

	ex1 := newExchange(t, fs.URL(), store, nil)
	ex1.SetSpool(spoolPath)
	_ = ex1.SendResult(context.Background(), 99, exchange.StatusSucceeded, "done")
	stop1 := startExchange(t, ex1)
	waitUntil(t, "a failed exchange", 3*time.Second, func() bool { return fs.failedCount() >= 1 })
	stop1()

	if fs.count() != 0 {
		t.Fatalf("server accepted %d exchanges during the outage", fs.count())
	}
	if _, err := os.Stat(spoolPath); err != nil {
		t.Fatalf("spool not written on shutdown: %v", err)
	}
	fs.setDown(false)

	ex2 := newExchange(t, fs.URL(), store, nil)
	ex2.SetSpool(spoolPath)
	startExchange(t, ex2)

	msg := fs.waitForMessageType(t, "operation-result", 3*time.Second)
	if id, ok := msg["operation-id"].(int64); !ok || id != 99 {
		t.Errorf("operation-id = %#v, want int64(99)", msg["operation-id"])
	}
	if msg["result-text"] != "done" {
		t.Errorf("result-text = %#v, want done", msg["result-text"])
	}
}

func TestPingTriggersExchangeWhenServerHasMessages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hasMessages bool
		wantWake    bool
	}{
		{"messages waiting", true, true},
		{"nothing waiting", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t)
			fs.pingHasMessages = tc.hasMessages
			store := newStoreWith(t, t.TempDir(), registeredState())
			ex := newExchange(t, fs.URL(), store, slowSchedule)
			startExchange(t, ex)
			fs.waitForCount(t, 1, 3*time.Second)

			tc2, err := transport.New(transport.Config{})
			if err != nil {
				t.Fatalf("transport.New: %v", err)
			}
			pinger := ping.New(fs.URL()+"/ping", ex.InsecureID, ex.TriggerExchange, 20*time.Millisecond, tc2)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = pinger.Run(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})

			waitUntil(t, "pings", 3*time.Second, func() bool { return len(fs.pings()) >= 3 })
			if id := fs.pings()[0]; id != "ins-456" {
				t.Errorf("ping insecure_id = %q, want ins-456", id)
			}
			if tc.wantWake {
				fs.waitForCount(t, 2, 3*time.Second)
			} else if fs.count() != 1 {
				t.Errorf("ping with no messages triggered %d extra exchanges", fs.count()-1)
			}
		})
	}
}

// echoHandler answers with the text it was sent; explode panics.
type echoHandler struct{}

func (echoHandler) MessageType() string { return "echo-test" }

func (echoHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, _ := msg["operation-id"].(int64)
	text, _ := msg["text"].(string)
	return result.SendResult(ctx, opID, exchange.StatusSucceeded, "echo:"+text)
}

type explodeHandler struct{}

func (explodeHandler) MessageType() string { return "explode-test" }

func (explodeHandler) Handle(context.Context, exchange.Message, exchange.ResultSink) error {
	panic(errors.New("boom"))
}

func TestOperationRoundTripThroughManager(t *testing.T) {
	fs := newFakeServer(t)
	fs.queueResponse(map[string]any{
		"messages": []any{
			map[string]any{"type": "echo-test", "operation-id": int64(77), "text": "hi"},
			map[string]any{"type": "explode-test", "operation-id": int64(78)},
		},
		"next-expected-sequence": int64(0),
	})

	store := newStoreWith(t, t.TempDir(), registeredState())
	ex := newExchange(t, fs.URL(), store, slowSchedule)
	runner := manager.NewRunner([]manager.Handler{echoHandler{}, explodeHandler{}}, ex, ex)
	runner.Register()
	t.Cleanup(func() {
		if err := runner.WaitWithTimeout(3 * time.Second); err != nil {
			t.Error(err)
		}
	})
	startExchange(t, ex)

	fs.waitForCount(t, 1, 3*time.Second)
	advertised, _ := fs.get(0)["client-accepted-types"].([]any)
	for _, want := range []string{"echo-test", "explode-test", "cancel-operation"} {
		if !slices.Contains(advertised, any(want)) {
			t.Errorf("client-accepted-types %v missing %q", advertised, want)
		}
	}

	results := make(map[int64]map[string]any)
	waitUntil(t, "two operation results", 3*time.Second, func() bool {
		for i := range fs.count() {
			for _, m := range payloadMessages(fs.get(i)) {
				if m["type"] == "operation-result" {
					results[toInt64(m["operation-id"])] = m
				}
			}
		}
		return len(results) == 2
	})

	echo := results[77]
	if toInt64(echo["status"]) != exchange.StatusSucceeded || echo["result-text"] != "echo:hi" {
		t.Errorf("echo result = %v", echo)
	}
	failed := results[78]
	if toInt64(failed["status"]) != exchange.StatusFailed {
		t.Errorf("panicking handler status = %v, want %d", failed["status"], exchange.StatusFailed)
	}
	if text, _ := failed["result-text"].(string); !strings.Contains(text, "boom") {
		t.Errorf("panicking handler result-text = %q, want it to mention the panic", text)
	}
}
