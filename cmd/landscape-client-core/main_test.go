package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSnapctlLoader_Get_Success creates a fake snapctl script that echoes a
// fixed value and verifies that snapctlLoader.Get returns it.
func TestSnapctlLoader_Get_Success(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "snapctl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho testvalue"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+":"+origPath)

	loader := &snapctlLoader{}
	val, err := loader.Get("some-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "testvalue" {
		t.Errorf("Get() = %q, want %q", val, "testvalue")
	}
}

// TestSnapctlLoader_Get_Error verifies that snapctlLoader.Get returns an error
// when snapctl exits non-zero.
func TestSnapctlLoader_Get_Error(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "snapctl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+":"+origPath)

	loader := &snapctlLoader{}
	_, err := loader.Get("some-key")
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// TestSnapctlLoader_Get_Whitespace verifies that leading/trailing whitespace is
// trimmed from snapctl output.
func TestSnapctlLoader_Get_Whitespace(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "snapctl")
	// printf avoids a trailing newline being added by echo, but we test trimming.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '  trimmed  \\n'"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+":"+origPath)

	loader := &snapctlLoader{}
	val, err := loader.Get("key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "trimmed" {
		t.Errorf("Get() = %q, want %q", val, "trimmed")
	}
}
