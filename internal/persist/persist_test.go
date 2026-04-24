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
		AcceptedTypesHash:      []byte("hash-xyz"),
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
		AcceptedTypesHash:      []byte("hash-roundtrip"),
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
	if string(got.AcceptedTypesHash) != string(want.AcceptedTypesHash) {
		t.Errorf("AcceptedTypesHash: got %x, want %x", got.AcceptedTypesHash, want.AcceptedTypesHash)
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

	// SetPluginState now auto-saves via read-modify-write; no explicit
	// store.Save call is needed. Use nil initial state to exercise the
	// lazy-load path in the accessor.
	acc := store.Accessor("myplugin", nil)

	want := myPluginState{Count: 42, Message: "hello"}
	if err := acc.SetPluginState(want); err != nil {
		t.Fatalf("SetPluginState failed: %v", err)
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

// TestSetPluginStatePreservesExchangeFields verifies that SetPluginState uses a
// read-modify-write so it does not clobber exchange fields saved by the exchange
// runner concurrently.
func TestSetPluginStatePreservesExchangeFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	// Simulate exchange saving its state first.
	exchangeState := &persist.State{
		SecureID:         "sec-123",
		OutboundSequence: 42,
	}
	if err := store.Save(exchangeState); err != nil {
		t.Fatalf("initial Save failed: %v", err)
	}

	// Plugin accessor (nil initial state — lazy load path).
	acc := store.Accessor("myplugin", nil)
	if err := acc.SetPluginState(map[string]int{"count": 7}); err != nil {
		t.Fatalf("SetPluginState failed: %v", err)
	}

	// The exchange fields must still be intact after the plugin save.
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got.SecureID != "sec-123" {
		t.Errorf("SecureID clobbered: got %q, want %q", got.SecureID, "sec-123")
	}
	if got.OutboundSequence != 42 {
		t.Errorf("OutboundSequence clobbered: got %d, want 42", got.OutboundSequence)
	}

	// And the plugin state must be there.
	acc2 := store.Accessor("myplugin", got)
	var ps map[string]int
	if err := acc2.GetPluginState(&ps); err != nil {
		t.Fatalf("GetPluginState failed: %v", err)
	}
	if ps["count"] != 7 {
		t.Errorf("plugin state count: got %d, want 7", ps["count"])
	}
}

func TestUpdatePreservesConcurrentPluginAndExchangeState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))

	if err := store.Save(&persist.State{
		SecureID:         "sec-123",
		OutboundSequence: 1,
	}); err != nil {
		t.Fatalf("initial Save failed: %v", err)
	}

	pluginAcc := store.Accessor("plugin-a", nil)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := pluginAcc.SetPluginState(map[string]int{"count": 3}); err != nil {
			t.Errorf("SetPluginState failed: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := store.Update(func(state *persist.State) error {
			state.OutboundSequence = 99
			state.ExchangeToken = "tok-123"
			return nil
		}); err != nil {
			t.Errorf("Update failed: %v", err)
		}
	}()

	wg.Wait()

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got.OutboundSequence != 99 {
		t.Fatalf("OutboundSequence: got %d, want 99", got.OutboundSequence)
	}
	if got.ExchangeToken != "tok-123" {
		t.Fatalf("ExchangeToken: got %q, want tok-123", got.ExchangeToken)
	}

	acc := store.Accessor("plugin-a", got)
	var pluginState map[string]int
	if err := acc.GetPluginState(&pluginState); err != nil {
		t.Fatalf("GetPluginState failed: %v", err)
	}
	if pluginState["count"] != 3 {
		t.Fatalf("plugin count: got %d, want 3", pluginState["count"])
	}
}
