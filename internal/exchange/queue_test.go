package exchange

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

func TestSpool_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	s := newSpool(path)

	msgs := []Message{
		{"type": "cpu-usage"},
		{"type": "operation-result", "operation-id": int64(42)},
	}
	if err := s.save(msgs); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got))
	}
	if got[1]["type"] != "operation-result" {
		t.Errorf("message order or content lost: %v", got)
	}
}

func TestSpool_MissingFileIsEmptyNotAnError(t *testing.T) {
	s := newSpool(filepath.Join(t.TempDir(), "does-not-exist.json"))
	got, err := s.load()
	if err != nil {
		t.Fatalf("first run must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want an empty queue, got %d messages", len(got))
	}
}

func TestSpool_CorruptFileIsEmptyNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := newSpool(path).load()
	if err != nil {
		t.Fatalf("a corrupt spool must not block startup: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want an empty queue, got %d messages", len(got))
	}
}

func TestEnforceQueueBound_DropsOldestTelemetryFirst(t *testing.T) {
	msgs := make([]Message, 0, maxQueuedMessages+10)
	// Oldest telemetry first, then a result, then more telemetry.
	for i := 0; i < maxQueuedMessages+9; i++ {
		msgs = append(msgs, Message{"type": "cpu-usage", "n": int64(i)})
	}
	msgs = append(msgs, Message{"type": "operation-result", "operation-id": int64(7)})

	kept, dropped := enforceQueueBound(msgs)

	if len(kept) > maxQueuedMessages {
		t.Errorf("queue not bounded: %d messages kept", len(kept))
	}
	if dropped == 0 {
		t.Error("expected some messages to be dropped")
	}

	var foundResult bool
	for _, m := range kept {
		if m["type"] == "operation-result" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("operation-result was dropped; the server is blocking on it")
	}

	// The oldest telemetry must be the part that went.
	if n, ok := kept[0]["n"].(int64); ok && n == 0 {
		t.Error("dropped newest telemetry instead of oldest")
	}
}

func TestEnforceQueueBound_NeverDropsResultsEvenWhenFull(t *testing.T) {
	msgs := make([]Message, 0, maxQueuedMessages+5)
	for i := 0; i < maxQueuedMessages+5; i++ {
		msgs = append(msgs, Message{"type": "operation-result", "operation-id": int64(i)})
	}

	kept, _ := enforceQueueBound(msgs)
	for _, m := range kept {
		if m["type"] != "operation-result" {
			t.Fatal("unexpected message type in kept set")
		}
	}
	if len(kept) != len(msgs) {
		t.Errorf("results must never be dropped: kept %d of %d", len(kept), len(msgs))
	}
}

// TestSpool_NormalisesNumbersToInt64OnLoad guards the encoding boundary:
// json.Unmarshal decodes numbers as float64, but bpickle encodes float64 as a
// float on the wire. A restored operation-id must return as int64 or the next
// exchange would send it to the server as a float.
func TestSpool_NormalisesNumbersToInt64OnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	s := newSpool(path)

	if err := s.save([]Message{{"type": "operation-result", "operation-id": int64(42)}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d", len(got))
	}
	if _, ok := got[0]["operation-id"].(int64); !ok {
		t.Errorf("operation-id must be int64 after a round trip, got %T", got[0]["operation-id"])
	}
}

// TestExchange_QueueSurvivesRestart asserts unsent messages are not lost when
// the daemon restarts — including on every snap refresh.
func TestExchange_QueueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "queue.json")

	tc, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	store := persist.New(filepath.Join(dir, "state.json"))
	cfg := &config.Config{URL: "http://127.0.0.1:1", AccountName: "acc"}

	first := New(cfg, store, tc)
	first.SetSpool(spoolPath)
	if err := first.SendResult(context.Background(), 42, StatusSucceeded, "done"); err != nil {
		t.Fatalf("SendResult: %v", err)
	}
	first.persistQueue()

	// Simulate a restart.
	second := New(cfg, store, tc)
	second.SetSpool(spoolPath)
	restored, err := second.spool.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("want 1 restored message, got %d", len(restored))
	}
	if restored[0]["type"] != "operation-result" {
		t.Errorf("wrong message restored: %v", restored[0])
	}
}
