package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type mapLoader map[string]string

func (m mapLoader) Get(key string) (string, error) { return m[key], nil }

type failingLoader struct{}

func (failingLoader) Get(string) (string, error) { return "", errors.New("snapctl down") }

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

func completeConfig() mapLoader {
	return mapLoader{
		"url":            "https://landscape.example.com/message-system",
		"account-name":   "acct",
		"computer-title": "host-1",
	}
}

func TestPublishConfDB_WritesViewWithConfig(t *testing.T) {
	logPath := installFakeSnapctl(t, "", 0)
	if err := publishConfDB(completeConfig()); err != nil {
		t.Fatalf("publishConfDB: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("snapctl was not invoked: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(args) != 4 || args[0] != "set" || args[1] != ":landscape-client-admin" || args[2] != "--view" {
		t.Fatalf("args = %v", args)
	}
	payload, ok := strings.CutPrefix(args[3], "config=")
	if !ok {
		t.Fatalf("view arg %q missing config= prefix", args[3])
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(payload), &view); err != nil {
		t.Fatalf("view payload is not JSON: %v", err)
	}
	if view["account-name"] != "acct" || view["computer-title"] != "host-1" {
		t.Errorf("view = %v", view)
	}
}

func TestPublishConfDB_IncompleteConfigIsSkipped(t *testing.T) {
	logPath := installFakeSnapctl(t, "", 1)
	cfg := completeConfig()
	delete(cfg, "account-name")
	if err := publishConfDB(cfg); err != nil {
		t.Fatalf("publishConfDB: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Error("snapctl must not be invoked for incomplete config")
	}
}

func TestPublishConfDB_LoaderErrorIsReturned(t *testing.T) {
	installFakeSnapctl(t, "", 0)
	if err := publishConfDB(failingLoader{}); err == nil {
		t.Error("want loader error")
	}
}

func TestPublishConfDB_SnapctlFailureIncludesOutput(t *testing.T) {
	installFakeSnapctl(t, "no matching rule", 1)
	err := publishConfDB(completeConfig())
	if err == nil || !strings.Contains(err.Error(), "no matching rule") {
		t.Errorf("err = %v, want it to include snapctl output", err)
	}
}
