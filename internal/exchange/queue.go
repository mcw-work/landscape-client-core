package exchange

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// maxQueuedMessages bounds the in-memory and on-disk queue. With no cap, a long
// server outage grows the queue without limit, which on a 512 MB Core device is
// an OOM path.
const maxQueuedMessages = 1000

// spool persists unsent messages across restarts. Without it every restart —
// including every snap refresh — silently drops unsent messages, including
// operation results the server is waiting on.
//
// It is deliberately a separate file from state.json: a queue write must never
// be able to touch SecureID or the outbound sequence number.
type spool struct {
	path string
}

func newSpool(path string) *spool {
	return &spool{path: path}
}

// load returns the persisted queue. A missing or corrupt spool yields an empty
// queue rather than an error: losing queued telemetry is bad, but refusing to
// start is worse.
func (s *spool) load() ([]Message, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("exchange: cannot read message spool: %w", err)
	}

	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		slog.Warn("exchange: message spool is corrupt, starting with an empty queue", "path", s.path, "error", err)
		return nil, nil
	}
	// json.Unmarshal decodes every number as float64, but bpickle encodes
	// float64 as a float on the wire. A restored operation-id would then be sent
	// to the server as a float and break operation-result processing, so
	// restore whole-number values to int64 before they re-enter the queue.
	for _, m := range msgs {
		for k, v := range m {
			m[k] = normalizeNumbers(v)
		}
	}
	return msgs, nil
}

// normalizeNumbers converts whole-number float64 values produced by
// json.Unmarshal back to int64, recursing through nested maps and slices.
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normalizeNumbers(e)
		}
		return x
	default:
		return v
	}
}

// save writes the queue atomically, using the same temp-file + fsync + rename
// sequence as persist.Store.
func (s *spool) save(msgs []Message) error {
	data, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("exchange: cannot encode message spool: %w", err)
	}

	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".queue-*.tmp")
	if err != nil {
		return fmt.Errorf("exchange: cannot create spool temp file: %w", err)
	}
	tmpPath := f.Name()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot write spool temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot sync spool temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot close spool temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exchange: cannot rename spool temp file: %w", err)
	}
	return nil
}

// enforceQueueBound trims the queue to maxQueuedMessages, dropping the oldest
// non-urgent messages first. Operation results are never dropped: the server is
// blocking on them, and telemetry is resampled anyway.
//
// Returns the retained messages and the number dropped.
func enforceQueueBound(msgs []Message) ([]Message, int) {
	if len(msgs) <= maxQueuedMessages {
		return msgs, 0
	}

	excess := len(msgs) - maxQueuedMessages
	kept := make([]Message, 0, len(msgs))
	dropped := 0

	for _, m := range msgs {
		msgType, _ := m["type"].(string)
		if dropped < excess && !isUrgentType(msgType) {
			dropped++
			continue
		}
		kept = append(kept, m)
	}
	return kept, dropped
}
