package exchange

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestToInt64(t *testing.T) {
	tests := []struct {
		in   any
		want int64
	}{
		{int64(5), 5},
		{int(6), 6},
		{int32(7), 7},
		{float64(8.9), 8},
		{"9", 0},
		{nil, 0},
	}
	for _, tc := range tests {
		if got := toInt64(tc.in); got != tc.want {
			t.Errorf("toInt64(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMsgBytes(t *testing.T) {
	tests := []struct {
		in     any
		want   string
		wantOK bool
	}{
		{"unknown-account", "unknown-account", true},
		{[]byte("max-pending-computers"), "max-pending-computers", true},
		{int64(1), "", false},
		{nil, "", false},
	}
	for _, tc := range tests {
		got, ok := msgBytes(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("msgBytes(%#v) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestNormalizeNumbers_Nested(t *testing.T) {
	in := map[string]any{
		"id":    float64(42),
		"ratio": 0.25,
		"list":  []any{float64(1), 1.5, "s", map[string]any{"n": float64(3)}},
	}
	want := map[string]any{
		"id":    int64(42),
		"ratio": 0.25,
		"list":  []any{int64(1), 1.5, "s", map[string]any{"n": int64(3)}},
	}
	if got := normalizeNumbers(in); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestPreviousAPIVersion_HasNoDowngrade(t *testing.T) {
	if got := previousAPIVersion(apiVersion); got != "" {
		t.Errorf("previousAPIVersion(%q) = %q, want empty", apiVersion, got)
	}
}

func TestSpool_SaveFailsWhenDirectoryMissing(t *testing.T) {
	s := newSpool(filepath.Join(t.TempDir(), "missing", "queue.json"))
	if err := s.save([]Message{{"type": "x"}}); err == nil {
		t.Fatal("want error when spool directory does not exist")
	}
}

func TestSpool_SaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := newSpool(filepath.Join(dir, "queue.json"))
	for range 3 {
		if err := s.save([]Message{{"type": "x"}}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "queue.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("want only queue.json, got %v", names)
	}
}

func TestSpool_LoadUnreadableIsError(t *testing.T) {
	// A directory at the spool path is an I/O error, not a corrupt file.
	path := filepath.Join(t.TempDir(), "queue.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := newSpool(path).load(); err == nil {
		t.Error("want error when spool path cannot be read")
	}
}

func TestInsecureID_ReflectsSetIDUnderLock(t *testing.T) {
	e := New(nil, nil, nil)
	if got := e.InsecureID(); got != "" {
		t.Fatalf("fresh exchange InsecureID = %q, want empty", got)
	}
	e.mu.Lock()
	e.insecureID = "ins-1"
	e.mu.Unlock()
	if got := e.InsecureID(); got != "ins-1" {
		t.Errorf("InsecureID = %q, want ins-1", got)
	}
}
