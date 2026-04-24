package main

import (
	"bufio"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// mockSnapctlRunner is a test double for SnapctlRunner.
type mockSnapctlRunner struct {
	gets          map[string]string
	setCalls      []string // each entry is "key=value"
	setErr        error
	restartCalled bool
	restartErr    error
}

func (m *mockSnapctlRunner) Get(key string) (string, error) {
	return m.gets[key], nil
}

func (m *mockSnapctlRunner) Set(key, value string) error {
	m.setCalls = append(m.setCalls, key+"="+value)
	return m.setErr
}

func (m *mockSnapctlRunner) Restart(service string) error {
	m.restartCalled = true
	return m.restartErr
}

// newWizard returns a wizard wired to scripted string input and string builder outputs.
// inRaw is a *strings.Reader — not *os.File — so TTY detection always falls back to
// plain readline. This means readPassword works without a real terminal in tests.
func newWizard(input string, m *mockSnapctlRunner) (*wizard, *strings.Builder, *strings.Builder) {
	r := strings.NewReader(input)
	out := &strings.Builder{}
	errOut := &strings.Builder{}
	return &wizard{
		io: wizardIO{
			inRaw: r,
			in:    bufio.NewReader(r),
			out:   out,
			err:   errOut,
		},
		snapctl: m,
	}, out, errOut
}

// TestWizard_SaaSFlow covers the full happy path for a SaaS installation.
// Input order: edition, title, account, regkey, regkey-confirm,
//              http-proxy, https-proxy, access-group, tags, confirm.
func TestWizard_SaaSFlow(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	w, _, _ := newWizard("n\nMy Machine\nmyaccount\nmykey\nmykey\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	wantSets := []string{
		"url=https://landscape.canonical.com/message-system",
		"computer-title=My Machine",
		"account-name=myaccount",
		"registration-key=mykey",
	}
	if !reflect.DeepEqual(m.setCalls, wantSets) {
		t.Errorf("Set calls:\n  got  %v\n  want %v", m.setCalls, wantSets)
	}
	if !m.restartCalled {
		t.Error("expected Restart to be called")
	}
}

// TestWizard_SelfHostedFlow covers the happy path for a self-hosted installation.
// Answering "y" skips the account-name prompt and auto-sets it to "standalone".
// URL is derived as https://<domain>/message-system.
func TestWizard_SelfHostedFlow(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	// edition=y, domain, title, regkey, confirm, proxies/group/tags blank, apply
	w, _, _ := newWizard("y\nlandscape.mycompany.com\nMy Machine\nmykey\nmykey\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	wantSets := []string{
		"url=https://landscape.mycompany.com/message-system",
		"computer-title=My Machine",
		"account-name=standalone",
		"registration-key=mykey",
	}
	if !reflect.DeepEqual(m.setCalls, wantSets) {
		t.Errorf("Set calls:\n  got  %v\n  want %v", m.setCalls, wantSets)
	}
}

// TestWizard_RequiredFieldReprompt verifies that a blank account name is re-prompted
// until a non-empty value is provided.
func TestWizard_RequiredFieldReprompt(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	// First account name blank → "Account name is required." → second attempt "myaccount"
	w, out, _ := newWizard("n\nMy Machine\n\nmyaccount\nmykey\nmykey\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !strings.Contains(out.String(), "Account name is required.") {
		t.Errorf("expected reprompt message, output was:\n%s", out.String())
	}
	found := false
	for _, c := range m.setCalls {
		if c == "account-name=myaccount" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected account-name=myaccount in Set calls: %v", m.setCalls)
	}
}

// TestWizard_RegistrationKeyMismatch verifies that mismatched key confirmation is
// re-prompted with the message "Keys must match."
func TestWizard_RegistrationKeyMismatch(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	// First pair key1/key2 mismatches → "Keys must match." → second pair key3/key3 matches
	w, out, _ := newWizard("n\nMy Machine\nmyaccount\nkey1\nkey2\nkey3\nkey3\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !strings.Contains(out.String(), "Keys must match.") {
		t.Errorf("expected mismatch message, output was:\n%s", out.String())
	}
	found := false
	for _, c := range m.setCalls {
		if c == "registration-key=key3" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected registration-key=key3 in Set calls: %v", m.setCalls)
	}
}

// TestWizard_PrefilledDefaults verifies that existing snapctl values appear as
// default hints in prompts, and are used when the user presses Enter.
func TestWizard_PrefilledDefaults(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{
		"computer-title": "Existing Title",
		"account-name":   "existingaccount",
	}}
	// Press Enter on title and account to accept their defaults
	w, out, _ := newWizard("n\n\n\nmykey\nmykey\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !strings.Contains(out.String(), "[Existing Title]") {
		t.Errorf("expected title default hint in output, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "[existingaccount]") {
		t.Errorf("expected account default hint in output, got:\n%s", out.String())
	}
	foundTitle, foundAccount := false, false
	for _, c := range m.setCalls {
		if c == "computer-title=Existing Title" {
			foundTitle = true
		}
		if c == "account-name=existingaccount" {
			foundAccount = true
		}
	}
	if !foundTitle {
		t.Errorf("expected computer-title=Existing Title in Set calls: %v", m.setCalls)
	}
	if !foundAccount {
		t.Errorf("expected account-name=existingaccount in Set calls: %v", m.setCalls)
	}
}

// TestWizard_UserAborts verifies that answering "n" at the confirm prompt
// exits cleanly without calling Set or Restart.
func TestWizard_UserAborts(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	w, out, _ := newWizard("n\nMy Machine\nmyaccount\nmykey\nmykey\n\n\n\n\nn\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !strings.Contains(out.String(), "Configuration not saved.") {
		t.Errorf("expected abort message, output was:\n%s", out.String())
	}
	if len(m.setCalls) != 0 {
		t.Errorf("expected no Set calls, got: %v", m.setCalls)
	}
	if m.restartCalled {
		t.Error("expected no Restart call")
	}
}

// TestWizard_SnapctlSetFailure verifies that a Set error propagates as a non-nil
// return from Run (causing the binary to exit 1).
func TestWizard_SnapctlSetFailure(t *testing.T) {
	m := &mockSnapctlRunner{
		gets:   map[string]string{},
		setErr: errors.New("permission denied"),
	}
	w, _, _ := newWizard("n\nMy Machine\nmyaccount\nmykey\nmykey\n\n\n\n\ny\n", m)

	err := w.Run()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied' in error, got: %v", err)
	}
}

// TestWizard_TagValidation verifies that tags with invalid characters are rejected
// with an error message, and the prompt loops until a valid value is provided.
func TestWizard_TagValidation(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	// "invalid tag" has a space → rejected; "webservers" is valid
	// After account + blank regkey/confirm + blank proxies/group, then tags
	w, out, _ := newWizard("n\nMy Machine\nmyaccount\n\n\n\n\n\ninvalid tag\nwebservers\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !strings.Contains(out.String(), "Invalid tag") {
		t.Errorf("expected invalid tag message, output was:\n%s", out.String())
	}
	found := false
	for _, c := range m.setCalls {
		if c == "tags=webservers" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected tags=webservers in Set calls: %v", m.setCalls)
	}
}

// TestWizard_RestartFailure verifies that a failed Restart is a warning on stderr
// and Run returns nil (config already written; exit 0 so the user knows to retry).
func TestWizard_RestartFailure(t *testing.T) {
	m := &mockSnapctlRunner{
		gets:       map[string]string{},
		restartErr: errors.New("service not found"),
	}
	w, _, errOut := newWizard("n\nMy Machine\nmyaccount\nmykey\nmykey\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("expected nil error on restart failure, got: %v", err)
	}
	if !strings.Contains(errOut.String(), "Warning:") {
		t.Errorf("expected warning in stderr, got:\n%s", errOut.String())
	}
}

// TestWizard_SkipsEmptyOptionals verifies that blank optional fields (registration-key,
// proxies, access-group, tags) are not passed to snapctl set.
func TestWizard_SkipsEmptyOptionals(t *testing.T) {
	m := &mockSnapctlRunner{gets: map[string]string{}}
	// All optionals left blank: 2 blank for regkey+confirm, 4 blank for proxies/group/tags
	w, _, _ := newWizard("n\nMy Machine\nmyaccount\n\n\n\n\n\n\ny\n", m)

	if err := w.Run(); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	wantSets := []string{
		"url=https://landscape.canonical.com/message-system",
		"computer-title=My Machine",
		"account-name=myaccount",
	}
	if !reflect.DeepEqual(m.setCalls, wantSets) {
		t.Errorf("Set calls:\n  got  %v\n  want %v", m.setCalls, wantSets)
	}
}
