package bpickle

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnmarshal_RejectsExcessiveNesting(t *testing.T) {
	depth := 10000
	data := append(bytes.Repeat([]byte{'l'}, depth), bytes.Repeat([]byte{';'}, depth)...)

	_, err := Unmarshal(data)
	if err == nil {
		t.Fatal("expected an error for deeply nested input, got nil")
	}
	if !strings.Contains(err.Error(), "nesting") {
		t.Errorf("error should mention nesting depth, got: %v", err)
	}
}

func TestUnmarshal_AcceptsReasonableNesting(t *testing.T) {
	depth := 10
	data := append(bytes.Repeat([]byte{'l'}, depth), bytes.Repeat([]byte{';'}, depth)...)

	if _, err := Unmarshal(data); err != nil {
		t.Fatalf("depth %d should decode, got: %v", depth, err)
	}
}

func TestUnmarshal_DepthAppliesToDictsAndTuples(t *testing.T) {
	tests := []struct {
		name   string
		marker byte
	}{
		{"dict", 'd'},
		{"tuple", 't'},
		{"list", 'l'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			depth := 10000
			data := append(bytes.Repeat([]byte{tt.marker}, depth), bytes.Repeat([]byte{';'}, depth)...)
			if _, err := Unmarshal(data); err == nil {
				t.Fatalf("%s: expected an error for %d levels of nesting", tt.name, depth)
			}
		})
	}
}
