package persist_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/canonical/landscape-client-core/internal/persist"
)

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	state, err := store.Load()
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if state.SecureID != "" || state.OutboundSequence != 0 {
		t.Errorf("expected zero-value state, got: %+v", state)
	}
	if state.PluginState == nil {
		t.Error("expected initialized PluginState map, got nil")
	}
}

func TestLoadExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	want := &persist.State{
		SecureID:               "sec-123",
		InsecureID:             "insec-456",
		ServerUUID:             "srv-uuid",
		OutboundSequence:       42,
		NextExpectedFromServer: 7,
		ExchangeToken:          "tok-abc",
		AcceptedTypes:          []string{"a", "b"},
		AcceptedTypesHash:      "hash-xyz",
	}
	data, _ := json.Marshal(want)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	store := persist.New(path)
	got, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SecureID != want.SecureID {
		t.Errorf("SecureID: got %q, want %q", got.SecureID, want.SecureID)
	}
	if got.InsecureID != want.InsecureID {
		t.Errorf("InsecureID: got %q, want %q", got.InsecureID, want.InsecureID)
	}
	if got.OutboundSequence != want.OutboundSequence {
		t.Errorf("OutboundSequence: got %d, want %d", got.OutboundSequence, want.OutboundSequence)
	}
	if got.NextExpectedFromServer != want.NextExpectedFromServer {
		t.Errorf("NextExpectedFromServer: got %d, want %d", got.NextExpectedFromServer, want.NextExpectedFromServer)
	}
	if len(got.AcceptedTypes) != len(want.AcceptedTypes) {
		t.Errorf("AcceptedTypes length: got %d, want %d", len(got.AcceptedTypes), len(want.AcceptedTypes))
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	want := &persist.State{
		SecureID:               "sec-roundtrip",
		InsecureID:             "insec-roundtrip",
		ServerUUID:             "uuid-roundtrip",
		OutboundSequence:       100,
		NextExpectedFromServer: 200,
		ExchangeToken:          "tok-roundtrip",
		AcceptedTypes:          []string{"type1", "type2", "type3"},
		AcceptedTypesHash:      "hash-roundtrip",
		PluginState: map[string]json.RawMessage{
			"myplugin": json.RawMessage(`{"count":5}`),
		},
	}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if got.SecureID != want.SecureID {
		t.Errorf("SecureID: got %q, want %q", got.SecureID, want.SecureID)
	}
	if got.OutboundSequence != want.OutboundSequence {
		t.Errorf("OutboundSequence: got %d, want %d", got.OutboundSequence, want.OutboundSequence)
	}
	if got.ExchangeToken != want.ExchangeToken {
		t.Errorf("ExchangeToken: got %q, want %q", got.ExchangeToken, want.ExchangeToken)
	}
	if got.AcceptedTypesHash != want.AcceptedTypesHash {
		t.Errorf("AcceptedTypesHash: got %q, want %q", got.AcceptedTypesHash, want.AcceptedTypesHash)
	}
	// Unmarshal the raw message to compare semantically (MarshalIndent may re-indent it).
	var pluginData map[string]int
	if err := json.Unmarshal(got.PluginState["myplugin"], &pluginData); err != nil {
		t.Fatalf("unmarshalling PluginState[myplugin]: %v", err)
	}
	if pluginData["count"] != 5 {
		t.Errorf("PluginState[myplugin].count: got %d, want 5", pluginData["count"])
	}
}

func TestAtomicWriteOverwritesCorruptTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Pre-create a corrupt file matching the temp-file naming pattern.
	// Save must succeed regardless (os.CreateTemp uses unique names).
	corruptTmp := path + ".tmpcorrupt"
	if err := os.WriteFile(corruptTmp, []byte("CORRUPT"), 0600); err != nil {
		t.Fatal(err)
	}

	store := persist.New(path)
	state := &persist.State{SecureID: "after-corrupt"}

	if err := store.Save(state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// No leftover temp files from this save should remain.
	matches, _ := filepath.Glob(path + ".tmp[0-9]*")
	if len(matches) > 0 {
		t.Errorf("leftover temp files after successful Save: %v", matches)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load after atomic save failed: %v", err)
	}
	if got.SecureID != "after-corrupt" {
		t.Errorf("SecureID: got %q, want %q", got.SecureID, "after-corrupt")
	}
}

func TestSaveMissingParentDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Use a subdirectory that does not exist yet.
	path := filepath.Join(dir, "subdir", "nested", "state.json")

	store := persist.New(path)
	state := &persist.State{SecureID: "needs-mkdir"}

	if err := store.Save(state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got.SecureID != "needs-mkdir" {
		t.Errorf("SecureID: got %q, want %q", got.SecureID, "needs-mkdir")
	}
}

func TestLoadCorruptJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	store := persist.New(path)
	_, err := store.Load()
	if err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}
}

func TestPluginStateAccessorRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	type myPluginState struct {
		Count   int    `json:"count"`
		Message string `json:"message"`
	}

	state := &persist.State{PluginState: make(map[string]json.RawMessage)}
	acc := store.Accessor("myplugin", state)

	want := myPluginState{Count: 42, Message: "hello"}
	if err := acc.SetPluginState(want); err != nil {
		t.Fatalf("SetPluginState failed: %v", err)
	}

	if err := store.Save(state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	acc2 := store.Accessor("myplugin", loaded)
	var got myPluginState
	if err := acc2.GetPluginState(&got); err != nil {
		t.Fatalf("GetPluginState failed: %v", err)
	}
	if got != want {
		t.Errorf("GetPluginState: got %+v, want %+v", got, want)
	}
}

func TestGetPluginStateNoExistingState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	state := &persist.State{PluginState: make(map[string]json.RawMessage)}
	acc := store.Accessor("nonexistent", state)

	type myState struct {
		Value int
	}
	var v myState
	v.Value = 99 // pre-set to verify it's unchanged

	if err := acc.GetPluginState(&v); err != nil {
		t.Fatalf("GetPluginState returned unexpected error: %v", err)
	}
	if v.Value != 99 {
		t.Errorf("v.Value should be unchanged (99), got %d", v.Value)
	}
}

func TestConcurrentSave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := int64(0); i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			state := &persist.State{OutboundSequence: i}
			if err := store.Save(state); err != nil {
				t.Errorf("goroutine %d: Save failed: %v", i, err)
			}
		}()
	}
	wg.Wait()

	// Final file must be valid JSON.
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("reading final state file: %v", err)
	}
	var final persist.State
	if err := json.Unmarshal(data, &final); err != nil {
		t.Fatalf("final state is not valid JSON: %v\ncontents: %s", err, data)
	}
}
