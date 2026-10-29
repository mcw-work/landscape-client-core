package persist_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/canonical/landscape-client-core/internal/persist"
)

// syncWriter serialises writes so slog output can be read while other
// goroutines log.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// captureLogs redirects the default slog logger for the duration of the test.
// Callers must not use t.Parallel: the default logger is process-global.
func captureLogs(t *testing.T) *syncWriter {
	t.Helper()
	w := &syncWriter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return w
}

// assertOnlyEntries fails if dir holds anything other than the named entries,
// e.g. a leaked temp file.
func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directory entries = %v, want %v", got, want)
	}
}

func TestLoad_UnreadableStateIsError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_, err := persist.New(path).Load()
	if err == nil || !strings.Contains(err.Error(), "cannot read state file") {
		t.Errorf("Load err = %v, want a read error", err)
	}
}

func TestSave_UnencodableStateIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := persist.New(filepath.Join(dir, "state.json"))
	state := &persist.State{PluginState: map[string]json.RawMessage{"cpu": json.RawMessage("{broken")}}
	err := store.Save(state)
	if err == nil || !strings.Contains(err.Error(), "cannot encode state") {
		t.Fatalf("Save err = %v, want an encode error", err)
	}
	assertOnlyEntries(t, dir)
}

func TestSave_UnwritableDirectoryIsError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	err := persist.New(filepath.Join(dir, "state.json")).Save(&persist.State{SecureID: "x"})
	if err == nil || !strings.Contains(err.Error(), "cannot create temp file") {
		t.Errorf("Save err = %v, want a temp file error", err)
	}
}

func TestSave_RenameFailureIsErrorAndRemovesTempFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// A non-empty directory at the target path cannot be renamed over.
	if err := os.MkdirAll(filepath.Join(path, "child"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err := persist.New(path).Save(&persist.State{SecureID: "x"})
	if err == nil || !strings.Contains(err.Error(), "cannot rename temp file") {
		t.Fatalf("Save err = %v, want a rename error", err)
	}
	assertOnlyEntries(t, dir, "state.json")
}

// A failed backup must not block saving the new state.
func TestSave_BackupFailureIsLoggedAndStateIsSaved(t *testing.T) {
	logs := captureLogs(t)
	path := filepath.Join(t.TempDir(), "state.json")
	store := persist.New(path)
	if err := store.Save(&persist.State{SecureID: "first"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Mkdir(path+".old", 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := store.Save(&persist.State{SecureID: "second"}); err != nil {
		t.Fatalf("Save with an unwritable backup: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SecureID != "second" {
		t.Errorf("SecureID = %q, want second", got.SecureID)
	}
	if !strings.Contains(logs.String(), "persist: cannot write backup") {
		t.Errorf("backup failure was not logged: %q", logs.String())
	}
}

func TestSetPluginState_UnencodableValueIsErrorAndDoesNotWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	store := persist.New(path)
	if err := store.Save(&persist.State{SecureID: "keep"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	err = store.Accessor("cpu", nil).SetPluginState(make(chan int))
	if err == nil || !strings.Contains(err.Error(), `cannot encode plugin state for "cpu"`) {
		t.Fatalf("SetPluginState err = %v, want an encode error", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("state file changed after a failed SetPluginState: %s", after)
	}
}
