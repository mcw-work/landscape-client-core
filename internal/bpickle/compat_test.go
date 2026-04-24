//go:build compat

package bpickle

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

// pythonRoundTrip marshals v with Go, sends the bpickle bytes through Python
// (loads then dumps), and returns the Go-decoded result. This exercises both
// Python's decoder and Python's encoder, as well as Go's decoder.
func pythonRoundTrip(t *testing.T, landscapePath string, v any) any {
	t.Helper()

	encoded, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%#v): %v", v, err)
	}

	// Python script: read bpickle from stdin, decode, re-encode, write to stdout.
	script := `
import sys
sys.path.insert(0, '` + landscapePath + `')
from landscape.lib.bpickle import loads, dumps
data = sys.stdin.buffer.read()
sys.stdout.buffer.write(dumps(loads(data)))
`
	cmd := exec.Command("python3", "-c", script)
	cmd.Stdin = bytes.NewReader(encoded)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python round-trip failed: %v", err)
	}

	result, err := Unmarshal(out)
	if err != nil {
		t.Fatalf("Unmarshal of Python output %q: %v", out, err)
	}
	return result
}

// pythonEncode runs Python to encode a literal value and returns the bpickle bytes.
func pythonEncode(t *testing.T, landscapePath string, pythonExpr string) []byte {
	t.Helper()

	script := `
import sys, math
sys.path.insert(0, '` + landscapePath + `')
from landscape.lib.bpickle import dumps
val = ` + pythonExpr + `
sys.stdout.buffer.write(dumps(val))
`
	cmd := exec.Command("python3", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pythonEncode(%q) failed: %v", pythonExpr, err)
	}
	return out
}

func TestCompat(t *testing.T) {
	landscapePath := os.Getenv("LANDSCAPE_CLIENT_PATH")
	if landscapePath == "" {
		t.Skip("LANDSCAPE_CLIENT_PATH not set; skipping compat tests")
	}

	// -------------------------------------------------------------------------
	// Part 1: Go → Python → Go round-trip.
	// For each test case: Marshal in Go, Python decodes + re-encodes, Go decodes,
	// compare with original (accounting for type normalisation: ints become int64).
	// -------------------------------------------------------------------------
	roundTripTests := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, nil},
		{"bool true", true, true},
		{"bool false", false, false},
		{"int 0", int64(0), int64(0)},
		{"int 1", int64(1), int64(1)},
		{"int -1", int64(-1), int64(-1)},
		{"int large", int64(1 << 40), int64(1 << 40)},
		{"float 1.5", float64(1.5), float64(1.5)},
		{"float pi", math.Pi, math.Pi},
		{"float 0.0", float64(0.0), float64(0.0)},
		{"string empty", "", ""},
		{"string ascii", "hello", "hello"},
		{"string utf8", "héllo", "héllo"},
		{"bytes empty", []byte{}, []byte{}},
		{"bytes hello", []byte("hello"), []byte("hello")},
		{"bytes binary", []byte{0x00, 0x7F, 0xFF}, []byte{0x00, 0x7F, 0xFF}},
		{"list empty", []any{}, []any{}},
		{"list mixed", []any{int64(1), "two", true, nil}, []any{int64(1), "two", true, nil}},
		{"dict empty", map[string]any{}, map[string]any{}},
		{"dict single", map[string]any{"k": "v"}, map[string]any{"k": "v"}},
		{"dict multi", map[string]any{"a": int64(1), "b": int64(2)}, map[string]any{"a": int64(1), "b": int64(2)}},
		{"nested", map[string]any{
			"list": []any{map[string]any{"x": int64(1)}, nil},
			"ok":   true,
		}, map[string]any{
			"list": []any{map[string]any{"x": int64(1)}, nil},
			"ok":   true,
		}},
	}

	for _, tc := range roundTripTests {
		t.Run("go_to_python/"+tc.name, func(t *testing.T) {
			got := pythonRoundTrip(t, landscapePath, tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Go→Python→Go(%#v):\n  got  %#v\n  want %#v", tc.in, got, tc.want)
			}
		})
	}

	// -------------------------------------------------------------------------
	// Part 2: Python → Go decode.
	// Python encodes known values; Go decodes them.
	// -------------------------------------------------------------------------
	pythonDecodeTests := []struct {
		name   string
		pyExpr string
		want   any
	}{
		{"nil", "None", nil},
		{"bool true", "True", true},
		{"bool false", "False", false},
		{"int 0", "0", int64(0)},
		{"int 42", "42", int64(42)},
		{"int -99", "-99", int64(-99)},
		{"float 1.5", "1.5", float64(1.5)},
		{"float pi", "math.pi", math.Pi},
		{"string", "'hello'", "hello"},
		{"bytes", "b'hello'", []byte("hello")},
		{"list", "[1, 2, 3]", []any{int64(1), int64(2), int64(3)}},
		{"dict", "{'a': 1, 'b': 2}", map[string]any{"a": int64(1), "b": int64(2)}},
		{"nested dict+list", "{'items': [1, None, True]}", map[string]any{
			"items": []any{int64(1), nil, true},
		}},
	}

	for _, tc := range pythonDecodeTests {
		t.Run("python_to_go/"+tc.name, func(t *testing.T) {
			pyBytes := pythonEncode(t, landscapePath, tc.pyExpr)
			got, err := Unmarshal(pyBytes)
			if err != nil {
				t.Fatalf("Unmarshal(Python output for %q): %v", tc.pyExpr, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Python→Go(%s):\n  got  %#v\n  want %#v", tc.pyExpr, got, tc.want)
			}
		})
	}
}
