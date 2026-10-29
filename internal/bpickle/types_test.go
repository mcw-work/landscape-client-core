package bpickle

import (
	"testing"
)

func TestMarshal_TupleWireFormat(t *testing.T) {
	got, err := Marshal(Tuple{int64(1700000000), 0.5})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := "ti1700000000;f0.5;;"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMarshal_TupleInsideList(t *testing.T) {
	got, err := Marshal([]any{Tuple{int64(1), int64(2)}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := "lti1;i2;;;"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMarshal_TupleRoundTripsAsSequence(t *testing.T) {
	data, err := Marshal(Tuple{int64(7), "x"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	v, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	seq, ok := v.([]any)
	if !ok || len(seq) != 2 || seq[0] != int64(7) || seq[1] != "x" {
		t.Errorf("got %#v, want []any{7, \"x\"}", v)
	}
}

func TestMarshal_TupleUnsupportedElement(t *testing.T) {
	if _, err := Marshal(Tuple{struct{}{}}); err == nil {
		t.Error("want error for unsupported tuple element")
	}
}

// The server schema for network-activity requires Python bytes keys, so keys
// must be encoded with 's', never 'u', and sorted for deterministic output.
func TestMarshal_BytesDictUsesBytesKeysSorted(t *testing.T) {
	got, err := Marshal(BytesDict{"lo": int64(2), "eth0": int64(1)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := "ds4:eth0i1;s2:loi2;;"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMarshal_BytesDictEmpty(t *testing.T) {
	got, err := Marshal(BytesDict{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != "d;" {
		t.Errorf("got %q, want %q", got, "d;")
	}
}

func TestMarshal_BytesDictUnsupportedValue(t *testing.T) {
	if _, err := Marshal(BytesDict{"a": struct{}{}}); err == nil {
		t.Error("want error for unsupported value")
	}
}
