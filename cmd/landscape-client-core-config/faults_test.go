package main

import (
	"errors"
	"strings"
	"testing"
)

// Self-hosted with every optional field and a key, so every prompt is reached.
const fullInput = "y\nhttps://ls.example.com/\ntitle\nacct\nk\nk\nhttp://p\nhttp://ps\ngrp\na,b\ny\n"

var errWrite = errors.New("write failed")

// failingWriter fails every write after the first ok writes.
type failingWriter struct {
	ok     int
	writes int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.ok {
		return 0, errWrite
	}
	return len(p), nil
}

func TestWizard_EveryOutputWriteErrorIsReturned(t *testing.T) {
	for ok := 0; ; ok++ {
		w, _, _ := newWizard(fullInput, &mockSnapctlRunner{gets: map[string]string{}})
		fw := &failingWriter{ok: ok}
		w.io.out = fw
		err := w.Run()
		if fw.writes <= ok {
			// Every write succeeded: the sweep has passed the last write.
			if err != nil {
				t.Fatalf("ok=%d: unexpected error %v", ok, err)
			}
			return
		}
		if !errors.Is(err, errWrite) {
			t.Fatalf("write %d failed but Run returned %v", ok+1, err)
		}
		if ok > 500 {
			t.Fatal("sweep did not terminate")
		}
	}
}

// Truncating input at every line must surface EOF, never write config.
func TestWizard_TruncatedInputNeverWritesConfig(t *testing.T) {
	lines := strings.SplitAfter(fullInput, "\n")
	for n := range len(lines) - 1 {
		m := &mockSnapctlRunner{gets: map[string]string{}}
		w, _, _ := newWizard(strings.Join(lines[:n], ""), m)
		if err := w.Run(); err == nil {
			t.Errorf("input cut after %d lines: want error", n)
		}
		if len(m.setCalls) != 0 {
			t.Errorf("input cut after %d lines: config written %v", n, m.setCalls)
		}
	}
}

func TestWizard_SelfHostedNormalisesDomain(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	w, _, _ := newWizard(fullInput, m)
	if err := w.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "url=https://ls.example.com/message-system"
	if len(m.setCalls) == 0 || m.setCalls[0] != want {
		t.Errorf("setCalls = %v, want first %q", m.setCalls, want)
	}
}

func TestRunWizard_ExitCodes(t *testing.T) {
	var out, errOut strings.Builder
	if code := runWizard(strings.NewReader(fullInput), &out, &errOut, &mockSnapctlRunner{gets: map[string]string{}}); code != 0 {
		t.Errorf("success exit code = %d, stderr %q", code, errOut.String())
	}

	errOut.Reset()
	m := &mockSnapctlRunner{gets: map[string]string{}, setErr: errors.New("denied")}
	if code := runWizard(strings.NewReader(fullInput), &out, &errOut, m); code != 1 {
		t.Errorf("failure exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "error: denied") {
		t.Errorf("stderr = %q, want the error", errOut.String())
	}
}
