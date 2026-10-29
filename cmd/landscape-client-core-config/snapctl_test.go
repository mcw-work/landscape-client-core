package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// installFakeSnapctl puts a snapctl script first on PATH that records its
// arguments (one per line) to the returned log path, prints out, and exits
// with code.
func installFakeSnapctl(t *testing.T, out string, code int) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + logPath + "\nprintf '%s' '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "snapctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logPath
}

func readArgs(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("snapctl was not invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestRealSnapctlRunner_Get(t *testing.T) {
	logPath := installFakeSnapctl(t, "  value \n", 0)
	got, err := (&RealSnapctlRunner{}).Get("account-name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "value" {
		t.Errorf("Get = %q, want trimmed value", got)
	}
	if args := readArgs(t, logPath); strings.Join(args, " ") != "get account-name" {
		t.Errorf("args = %v", args)
	}
}

func TestRealSnapctlRunner_GetFailure(t *testing.T) {
	installFakeSnapctl(t, "", 1)
	if _, err := (&RealSnapctlRunner{}).Get("url"); err == nil || !strings.Contains(err.Error(), "snapctl get url") {
		t.Errorf("err = %v, want snapctl get url failure", err)
	}
}

// All pairs must go in one snapctl call so the configure hook runs once.
func TestRealSnapctlRunner_SetBatchesPairs(t *testing.T) {
	logPath := installFakeSnapctl(t, "", 0)
	if err := (&RealSnapctlRunner{}).Set("url=https://x", "tags=a,b"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	want := []string{"set", "url=https://x", "tags=a,b"}
	if args := readArgs(t, logPath); strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestRealSnapctlRunner_SetNoPairsIsNoop(t *testing.T) {
	logPath := installFakeSnapctl(t, "", 1)
	if err := (&RealSnapctlRunner{}).Set(); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Error("snapctl must not be invoked with no pairs")
	}
}

func TestRealSnapctlRunner_SetFailureIncludesOutput(t *testing.T) {
	installFakeSnapctl(t, "invalid key", 1)
	err := (&RealSnapctlRunner{}).Set("bad=1")
	if err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("err = %v, want it to include snapctl output", err)
	}
}

func TestRealSnapctlRunner_Restart(t *testing.T) {
	logPath := installFakeSnapctl(t, "", 0)
	if err := (&RealSnapctlRunner{}).Restart("landscape-client-core.daemon"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if args := readArgs(t, logPath); strings.Join(args, " ") != "restart landscape-client-core.daemon" {
		t.Errorf("args = %v", args)
	}
}

func TestRealSnapctlRunner_RestartFailure(t *testing.T) {
	installFakeSnapctl(t, "", 2)
	if err := (&RealSnapctlRunner{}).Restart("svc"); err == nil || !strings.Contains(err.Error(), "snapctl restart svc") {
		t.Errorf("err = %v, want snapctl restart failure", err)
	}
}
