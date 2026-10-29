package persist_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/canonical/landscape-client-core/internal/persist"
)

// An accessor created without a cached state must lazily load from disk.
func TestAccessor_NilStateLoadsFromDisk(t *testing.T) {
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Accessor("cpu", nil).SetPluginState(map[string]int{"n": 3}); err != nil {
		t.Fatalf("SetPluginState: %v", err)
	}

	var got map[string]int
	if err := store.Accessor("cpu", nil).GetPluginState(&got); err != nil {
		t.Fatalf("GetPluginState: %v", err)
	}
	if got["n"] != 3 {
		t.Errorf("got %v, want n=3", got)
	}
}

func TestAccessor_NilStateCorruptFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var v map[string]any
	if err := persist.New(path).Accessor("cpu", nil).GetPluginState(&v); err == nil {
		t.Error("want error when state cannot be loaded")
	}
}

func TestAccessor_UndecodablePluginStateIsError(t *testing.T) {
	store := persist.New(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Accessor("cpu", nil).SetPluginState("a string"); err != nil {
		t.Fatalf("SetPluginState: %v", err)
	}
	var v map[string]int
	if err := store.Accessor("cpu", nil).GetPluginState(&v); err == nil {
		t.Error("want error decoding a string into a map")
	}
}

func TestSave_ParentIsAFileIsError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := persist.New(filepath.Join(blocker, "state.json"))
	if err := store.Save(&persist.State{SecureID: "x"}); err == nil {
		t.Error("want error when the state directory cannot be created")
	}
}

func TestSave_WritesBackupOfPreviousState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := persist.New(path)
	if err := store.Save(&persist.State{SecureID: "first"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save(&persist.State{SecureID: "second"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	backup, err := persist.New(path + ".old").Load()
	if err != nil {
		t.Fatalf("Load backup: %v", err)
	}
	if backup.SecureID != "first" {
		t.Errorf("backup SecureID = %q, want first", backup.SecureID)
	}
}

func TestUpdate_CallbackErrorDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := persist.New(path)
	if err := store.Save(&persist.State{SecureID: "keep"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantErr := os.ErrInvalid
	err := store.Update(func(s *persist.State) error {
		s.SecureID = "changed"
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("Update err = %v, want %v", err, wantErr)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SecureID != "keep" {
		t.Errorf("SecureID = %q, want keep", got.SecureID)
	}
}
