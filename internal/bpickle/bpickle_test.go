package bpickle

import (
	"math"
	"reflect"
	"testing"
)

// roundTrip marshals then unmarshals v and returns the decoded value.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	data, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%#v) error: %v", v, err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal(%q) error: %v", data, err)
	}
	return got
}

// TestRoundTrip covers the full round-trip for every supported type.
func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want any
	}{
		// nil
		{"nil", nil, nil},

		// bool
		{"bool true", true, true},
		{"bool false", false, false},

		// integers
		{"int 0", int64(0), int64(0)},
		{"int 1", int64(1), int64(1)},
		{"int -1", int64(-1), int64(-1)},
		{"int MaxInt64", int64(math.MaxInt64), int64(math.MaxInt64)},
		{"int MinInt64", int64(math.MinInt64), int64(math.MinInt64)},

		// all Go integer widths
		{"int", int(42), int64(42)},
		{"int8", int8(42), int64(42)},
		{"int16", int16(42), int64(42)},
		{"int32", int32(42), int64(42)},
		{"uint", uint(42), int64(42)},
		{"uint8", uint8(42), int64(42)},
		{"uint16", uint16(42), int64(42)},
		{"uint32", uint32(42), int64(42)},

		// floats
		{"float 0.0", float64(0.0), float64(0.0)},
		{"float 1.5", float64(1.5), float64(1.5)},
		{"float -1.5", float64(-1.5), float64(-1.5)},
		{"float pi", math.Pi, math.Pi},
		{"float MaxFloat64", math.MaxFloat64, math.MaxFloat64},

		// bytes
		{"bytes empty", []byte{}, []byte{}},
		{"bytes single", []byte{0x41}, []byte{0x41}},
		{"bytes multi", []byte("hello"), []byte("hello")},
		{"bytes with colon", []byte("a:b:c"), []byte("a:b:c")},
		{"bytes binary", []byte{0x00, 0x01, 0xFF}, []byte{0x00, 0x01, 0xFF}},

		// strings
		{"string empty", "", ""},
		{"string ASCII", "hello", "hello"},
		{"string unicode", "héllo", "héllo"},
		{"string emoji", "🎉", "🎉"},

		// lists
		{"list empty", []any{}, []any{}},
		{"list ints", []any{int64(1), int64(2), int64(3)}, []any{int64(1), int64(2), int64(3)}},
		{"list mixed", []any{nil, true, int64(42), "hi"}, []any{nil, true, int64(42), "hi"}},
		{"list nested", []any{[]any{int64(1), int64(2)}, []any{int64(3)}}, []any{[]any{int64(1), int64(2)}, []any{int64(3)}}},

		// dicts
		{"dict empty", map[string]any{}, map[string]any{}},
		{"dict single", map[string]any{"key": "val"}, map[string]any{"key": "val"}},
		{"dict multi", map[string]any{"a": int64(1), "b": int64(2)}, map[string]any{"a": int64(1), "b": int64(2)}},

		// deeply nested
		{"deep nest", map[string]any{
			"list": []any{map[string]any{"x": int64(1)}},
		}, map[string]any{
			"list": []any{map[string]any{"x": int64(1)}},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("round-trip(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestMarshalWireFormat verifies the exact bytes produced by Marshal.
func TestMarshalWireFormat(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []byte
	}{
		{"nil", nil, []byte("n")},
		{"bool true", true, []byte("b1")},
		{"bool false", false, []byte("b0")},
		{"int 0", int64(0), []byte("i0;")},
		{"int -1", int64(-1), []byte("i-1;")},
		{"int 42", int64(42), []byte("i42;")},
		{"float 1.5", float64(1.5), []byte("f1.5;")},
		{"float 0.0", float64(0.0), []byte("f0.0;")},
		{"float 1.0", float64(1.0), []byte("f1.0;")},
		{"float -1.0", float64(-1.0), []byte("f-1.0;")},

		{"bytes empty", []byte{}, []byte("s0:")},
		{"bytes hello", []byte("hello"), []byte("s5:hello")},
		{"string empty", "", []byte("u0:")},
		{"string hello", "hello", []byte("u5:hello")},
		// UTF-8: "é" is 2 bytes (0xC3 0xA9), so length prefix is 2 not 1
		{"string utf8 byte count", "é", []byte("u2:\xc3\xa9")},
		// list
		{"list empty", []any{}, []byte("l;")},
		{"list one int", []any{int64(1)}, []byte("li1;;")},
		// dict key sort order: "a" < "b"
		{"dict sort order", map[string]any{"b": int64(2), "a": int64(1)}, []byte("du1:ai1;u1:bi2;;")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal(%#v) error: %v", tc.in, err)
			}
			if string(got) != string(tc.want) {
				t.Errorf("Marshal(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestUnmarshalDecodeErrors verifies that truncated or corrupted input returns errors.
func TestUnmarshalDecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty input", []byte{}},
		{"bool truncated", []byte("b")},
		{"int no semicolon", []byte("i42")},
		{"float no semicolon", []byte("f1.5")},
		{"bytes no colon", []byte("s5")},
		{"bytes truncated body", []byte("s5:hi")},
		{"string no colon", []byte("u5")},
		{"string truncated body", []byte("u5:hi")},
		{"list unterminated", []byte("li1;")},
		{"dict unterminated", []byte("du1:ai1;")},
		{"unknown marker", []byte("z")},
		{"unknown marker in list", []byte("lz;")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Unmarshal(tc.data)
			if err == nil {
				t.Errorf("Unmarshal(%q) expected error, got nil", tc.data)
			}
		})
	}
}

// TestMarshalUnsupportedType verifies that unsupported types return an error.
func TestMarshalUnsupportedType(t *testing.T) {
	tests := []struct {
		name string
		in   any
	}{
		{"struct", struct{}{}},
		{"chan", make(chan int)},
		{"func", func() {}},
		{"map[int]any", map[int]any{1: "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Marshal(tc.in)
			if err == nil {
				t.Errorf("Marshal(%T) expected error, got nil", tc.in)
			}
		})
	}
}

// TestUnmarshalUTF8ByteCount verifies that the length prefix is byte count, not rune count.
func TestUnmarshalUTF8ByteCount(t *testing.T) {
	// "🎉" is 4 bytes in UTF-8
	s := "🎉"
	data, err := Marshal(s)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	// Length prefix must be 4 (bytes), not 1 (runes).
	if string(data[:3]) != "u4:" {
		t.Errorf("expected prefix u4:, got %q", data[:3])
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if got.(string) != s {
		t.Errorf("got %q, want %q", got, s)
	}
}

// TestUnmarshalDictBytesKey verifies that 's'-keyed dicts decode to string keys.
func TestUnmarshalDictBytesKey(t *testing.T) {
	// Hand-craft a dict with a bytes key: d s5:hello i42; ;
	data := []byte("ds5:helloi42;;")
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	v, ok := m["hello"]
	if !ok {
		t.Errorf("key 'hello' not found in %v", m)
	}
	if v.(int64) != 42 {
		t.Errorf("value = %v, want 42", v)
	}
}

// TestUnmarshalTupleAsSlice verifies that Python tuples ('t') decode as []any.
func TestUnmarshalTupleAsSlice(t *testing.T) {
	data := []byte("ti1;i2;;")
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	want := []any{int64(1), int64(2)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestMarshalUint64Overflow verifies that uint64 values exceeding math.MaxInt64 return an error.
func TestMarshalUint64Overflow(t *testing.T) {
	_, err := Marshal(uint64(math.MaxInt64 + 1))
	if err == nil {
		t.Error("Marshal(uint64(math.MaxInt64+1)) expected error, got nil")
	}
}

// TestUnmarshalLargeLength verifies that a crafted large length does not panic.
func TestUnmarshalLargeLength(t *testing.T) {
	_, err := Unmarshal([]byte("s99999999999999999999:x"))
	if err == nil {
		t.Error("Unmarshal(crafted large length) expected error, got nil")
	}
}

// TestUnmarshalInvalidBool verifies that a non-0/1 bool byte returns an error.
func TestUnmarshalInvalidBool(t *testing.T) {
	_, err := Unmarshal([]byte("b2"))
	if err == nil {
		t.Error("Unmarshal(\"b2\") expected error, got nil")
	}
}

// TestDeepNested verifies a deeply nested dict → list → dict structure.
func TestDeepNested(t *testing.T) {
	inner := map[string]any{"z": int64(99)}
	middle := []any{inner, nil, true}
	outer := map[string]any{"items": middle, "count": int64(3)}

	got := roundTrip(t, outer)
	if !reflect.DeepEqual(got, outer) {
		t.Errorf("deep-nested round-trip mismatch:\ngot  %#v\nwant %#v", got, outer)
	}
}

// TestMarshalConcreteSliceAndMapTypes verifies that concrete slice/map types
// (e.g. []map[string]any, map[string][]string) marshal successfully via the
// reflection fallback. These types are produced by monitor plugins and must
// round-trip through bpickle without error.
func TestMarshalConcreteSliceAndMapTypes(t *testing.T) {
	t.Run("slice_of_map", func(t *testing.T) {
		// Simulates []map[string]any used by network-device, processor-info.
		devices := []map[string]any{
			{"name": "eth0", "mac": "aa:bb:cc:dd:ee:ff"},
			{"name": "lo", "mac": "00:00:00:00:00:00"},
		}
		data, err := Marshal(devices)
		if err != nil {
			t.Fatalf("Marshal([]map[string]any) failed: %v", err)
		}
		// Must decode back as a list of dicts.
		got, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		list, ok := got.([]any)
		if !ok || len(list) != 2 {
			t.Fatalf("expected []any of length 2, got %T %v", got, got)
		}
		first, ok := list[0].(map[string]any)
		if !ok {
			t.Fatalf("expected map[string]any element, got %T", list[0])
		}
		if first["name"] != "eth0" {
			t.Errorf("name: got %v, want eth0", first["name"])
		}
	})

	t.Run("map_of_string_slices", func(t *testing.T) {
		// Simulates map[string][]string used by users plugin (group members).
		members := map[string][]string{
			"admins": {"alice", "bob"},
			"users":  {"carol"},
		}
		data, err := Marshal(members)
		if err != nil {
			t.Fatalf("Marshal(map[string][]string) failed: %v", err)
		}
		got, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		d, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("expected map[string]any, got %T", got)
		}
		admins, ok := d["admins"].([]any)
		if !ok || len(admins) != 2 {
			t.Fatalf("admins: expected []any of len 2, got %T %v", d["admins"], d["admins"])
		}
	})

	t.Run("nested_message_with_concrete_slice", func(t *testing.T) {
		// Simulates an exchange.Message where "devices" is []map[string]any.
		processors := []map[string]any{
			{"processor-id": 0, "model": "Intel"},
		}
		msg := map[string]any{
			"type":       "processor-info",
			"processors": processors,
		}
		data, err := Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal(exchange message with concrete slice) failed: %v", err)
		}
		got, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		d := got.(map[string]any)
		if d["type"] != "processor-info" {
			t.Errorf("type: got %v", d["type"])
		}
	})
}
