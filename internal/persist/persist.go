// Package persist manages durable JSON state files stored under $SNAP_COMMON,
// providing load and save operations for client identity and sequence numbers.
package persist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// State holds all persistent client state.
type State struct {
	SecureID               string                     `json:"secure_id"`
	InsecureID             string                     `json:"insecure_id"`
	ServerUUID             string                     `json:"server_uuid"`
	OutboundSequence       int64                      `json:"outbound_sequence"`
	NextExpectedFromServer int64                      `json:"next_expected_from_server"`
	ExchangeToken          string                     `json:"exchange_token"`
	AcceptedTypes          []string                   `json:"accepted_types"`
	AcceptedTypesHash      []byte                     `json:"accepted_types_hash"`
	PluginState            map[string]json.RawMessage `json:"plugin_state,omitempty"`
}

// Store manages atomic persistence of State to a JSON file.
type Store struct {
	path string
	mu   sync.Mutex
}

// New returns a Store that persists state to the given file path.
// The parent directory need not exist yet.
func New(path string) *Store {
	return &Store{path: path}
}

// Load reads State from disk. If the file does not exist, returns a
// zero-value State (not an error) — this is the normal case for a new client.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{PluginState: make(map[string]json.RawMessage)}, nil
		}
		return nil, fmt.Errorf("persist: reading state file: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("persist: decoding state file: %w", err)
	}
	if state.PluginState == nil {
		state.PluginState = make(map[string]json.RawMessage)
	}
	return &state, nil
}

// Save writes state to disk atomically: it writes to a temp file in the
// same directory and then renames over the target path.
// Creates the parent directory if it does not exist (mode 0700).
func (s *Store) Save(state *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(state)
}

func (s *Store) saveLocked(state *State) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("persist: creating state directory: %w", err)
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("persist: encoding state: %w", err)
	}

	// Use os.CreateTemp so each Save call gets a unique temp file, making
	// concurrent saves safe: each goroutine writes to its own inode and the
	// final os.Rename (atomic) leaves one complete, valid state on disk.
	f, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp")
	if err != nil {
		return fmt.Errorf("persist: creating temp file: %w", err)
	}
	tmpPath := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("persist: writing temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("persist: syncing temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("persist: closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("persist: renaming temp file: %w", err)
	}
	return nil
}

// Update performs a serialized read-modify-write of the persisted state.
// The callback receives the latest state loaded from disk and may mutate it
// before it is written back atomically.
func (s *Store) Update(update func(state *State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	if err := update(state); err != nil {
		return err
	}
	return s.saveLocked(state)
}

// PluginStateAccessor provides typed get/set access to a single plugin's
// slice of State.PluginState. Used by monitor plugins.
type PluginStateAccessor struct {
	store  *Store
	key    string
	cached *State
}

// Accessor returns a PluginStateAccessor for the named plugin, backed by
// the given state (which must have been loaded from disk).
func (s *Store) Accessor(name string, state *State) *PluginStateAccessor {
	return &PluginStateAccessor{
		store:  s,
		key:    name,
		cached: state,
	}
}

// ensureLoaded loads state from disk into p.cached if it is nil.
func (p *PluginStateAccessor) ensureLoaded() error {
	if p.cached != nil {
		return nil
	}
	state, err := p.store.Load()
	if err != nil {
		return fmt.Errorf("persist: loading state for plugin %q: %w", p.key, err)
	}
	p.cached = state
	return nil
}

// GetPluginState unmarshals the plugin's persisted state into v.
// Returns nil if no state is persisted yet for this plugin.
func (p *PluginStateAccessor) GetPluginState(v any) error {
	if err := p.ensureLoaded(); err != nil {
		return err
	}
	raw, ok := p.cached.PluginState[p.key]
	if !ok || raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("persist: decoding plugin state for %q: %w", p.key, err)
	}
	return nil
}

// SetPluginState marshals v, then performs a read-modify-write against the
// on-disk state file so that only this plugin's key is updated. This avoids
// clobbering exchange fields (SecureID, sequence numbers, etc.) that the
// exchange runner saves independently.
func (p *PluginStateAccessor) SetPluginState(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("persist: encoding plugin state for %q: %w", p.key, err)
	}
	var updated *State
	err = p.store.Update(func(latest *State) error {
		if latest.PluginState == nil {
			latest.PluginState = make(map[string]json.RawMessage)
		}
		latest.PluginState[p.key] = json.RawMessage(data)
		updated = latest
		return nil
	})
	if err != nil {
		// Fall back to whatever we have cached rather than failing entirely.
		if err2 := p.ensureLoaded(); err2 != nil {
			return fmt.Errorf("persist: saving plugin state for %q: update failed: %w", p.key, err)
		}
		if p.cached.PluginState == nil {
			p.cached.PluginState = make(map[string]json.RawMessage)
		}
		p.cached.PluginState[p.key] = json.RawMessage(data)
		return p.store.Save(p.cached)
	}
	p.cached = updated
	return nil
}
