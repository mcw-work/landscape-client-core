// Package main implements the landscape-client-core-config wizard.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/term"
)

// SnapctlRunner abstracts snapctl calls for testability.
type SnapctlRunner interface {
	Get(key string) (string, error)
	// Set writes one or more "key=value" pairs in a single snapctl set call.
	// Passing all pairs at once is important: snapctl triggers the configure
	// hook after each call, so batching avoids N hook invocations and ensures
	// the hook sees the complete, valid configuration on the first run.
	Set(pairs ...string) error
	Restart(service string) error
}

// RealSnapctlRunner implements SnapctlRunner using the snapctl binary.
type RealSnapctlRunner struct{}

func (r *RealSnapctlRunner) Get(key string) (string, error) {
	out, err := exec.Command("snapctl", "get", key).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("snapctl get %s: %w: %s", key, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("snapctl get %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *RealSnapctlRunner) Set(pairs ...string) error {
	if len(pairs) == 0 {
		return nil
	}
	args := append([]string{"set"}, pairs...)
	cmd := exec.Command("snapctl", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("snapctl set: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("snapctl set: %w", err)
	}
	return nil
}

func (r *RealSnapctlRunner) Restart(service string) error {
	cmd := exec.Command("snapctl", "restart", service)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("snapctl restart %s: %w: %s", service, err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("snapctl restart %s: %w", service, err)
	}
	return nil
}

// wizardIO holds all terminal I/O — injectable for testing.
type wizardIO struct {
	inRaw io.Reader     // original reader, used only for TTY detection
	in    *bufio.Reader // buffered reader shared by all prompts
	out   io.Writer
	err   io.Writer
}

// wizardState collects values during the wizard session.
type wizardState struct {
	selfHosted      bool
	domain          string
	url             string
	computerTitle   string
	accountName     string
	registrationKey string
	httpProxy       string
	httpsProxy      string
	accessGroup     string
	tags            string
}

// wizard is the top-level struct.
type wizard struct {
	io      wizardIO
	snapctl SnapctlRunner
	state   wizardState
}

// tagRE matches valid Landscape tag names.
var tagRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

// validateTag returns true if t is a valid Landscape tag.
func validateTag(t string) bool {
	return tagRE.MatchString(t)
}

// readLine reads one line from r, trimming the trailing newline.
// A final line without a trailing newline is returned as success (not io.EOF).
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if errors.Is(err, io.EOF) && len(line) > 0 {
		return line, nil
	}
	return line, err
}

// promptLine prints prompt (with optional default hint), reads a line,
// and returns defaultVal when the user enters blank.
func (w *wizard) promptLine(prompt, defaultVal string) (string, error) {
	if defaultVal != "" {
		if _, err := fmt.Fprintf(w.io.out, "%s [%s]: ", prompt, defaultVal); err != nil {
			return "", err
		}
	} else {
		if _, err := fmt.Fprintf(w.io.out, "%s: ", prompt); err != nil {
			return "", err
		}
	}
	line, err := readLine(w.io.in)
	if err != nil {
		return "", err
	}
	if line == "" {
		return defaultVal, nil
	}
	return line, nil
}

// readPassword reads a masked password.
// Uses term.ReadPassword on a real TTY; falls back to plain readline otherwise
// (pipes, test *strings.Reader, etc.).
func (w *wizard) readPassword(prompt string) (string, error) {
	if f, ok := w.io.inRaw.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if _, err := fmt.Fprint(w.io.out, prompt); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(int(f.Fd()))
		if _, printErr := fmt.Fprintln(w.io.out); printErr != nil {
			return "", printErr
		}
		return string(b), err
	}
	// Non-TTY fallback: use the shared buffered reader.
	if _, err := fmt.Fprint(w.io.out, prompt); err != nil {
		return "", err
	}
	return readLine(w.io.in)
}

// Run executes the interactive configuration wizard.
func (w *wizard) Run() error {
	// Step 1: Landscape Edition
	if _, err := fmt.Fprintln(w.io.out, "Manage this machine with Landscape (https://ubuntu.com/landscape)"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w.io.out, "Will you be using your own Self-Hosted Landscape installation? [y/N]: "); err != nil {
		return err
	}
	edition, err := readLine(w.io.in)
	if err != nil {
		return err
	}
	edition = strings.ToLower(strings.TrimSpace(edition))
	if edition == "y" || edition == "yes" {
		w.state.selfHosted = true
		if _, err := fmt.Fprintln(w.io.out, "Provide the fully qualified domain name of your Landscape Server"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w.io.out, "e.g. landscape.example.com"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w.io.out, "Landscape Domain: "); err != nil {
			return err
		}
		domain, err := readLine(w.io.in)
		if err != nil {
			return err
		}
		domain = strings.TrimPrefix(domain, "https://")
		domain = strings.TrimPrefix(domain, "http://")
		domain = strings.TrimRight(domain, "/")
		w.state.domain = domain
		w.state.url = "https://" + domain + "/message-system"
	} else {
		w.state.selfHosted = false
		w.state.domain = "landscape.canonical.com"
		w.state.url = "https://landscape.canonical.com/message-system"
	}

	// Step 2: Computer Title
	hostname, _ := os.Hostname()
	defaultTitle, _ := w.snapctl.Get("computer-title")
	if defaultTitle == "" {
		defaultTitle = hostname
	}
	if _, err := fmt.Fprintln(w.io.out, "The computer title you provide will be used to represent this"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, "computer in the Landscape dashboard."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
		return err
	}
	title, err := w.promptLine("This computer's title", defaultTitle)
	if err != nil {
		return err
	}
	w.state.computerTitle = title

	// Step 3: Account Name.
	// Always ask when using a canonical.com server (SaaS or self-hosted on
	// canonical.com infrastructure). For other self-hosted servers, default
	// to "standalone" but still allow the user to override it.
	isSaaSDomain := strings.HasSuffix(w.state.domain, ".canonical.com") || w.state.domain == "canonical.com"
	defaultAccount, _ := w.snapctl.Get("account-name")
	if w.state.selfHosted && !isSaaSDomain {
		// Non-canonical self-hosted: default to standalone, still prompt.
		if defaultAccount == "" {
			defaultAccount = "standalone"
		}
	} else {
		// SaaS or canonical.com self-hosted: no default — must be provided.
		if _, err := fmt.Fprintln(w.io.out, "You must now specify the name of the Landscape account you"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w.io.out, "want to register this computer with. Your account name is shown"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w.io.out, "under 'Account name' at https://landscape.canonical.com ."); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
			return err
		}
	}
	for {
		account, err := w.promptLine("Account name", defaultAccount)
		if err != nil {
			return err
		}
		if account != "" {
			w.state.accountName = account
			break
		}
		if _, err := fmt.Fprintln(w.io.out, "Account name is required."); err != nil {
			return err
		}
	}

	// Step 4: Registration Key (optional; both entries must match)
	if _, err := fmt.Fprintln(w.io.out, "A Registration Key prevents unauthorized registration attempts."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "Provide the Registration Key found at:\nhttps://%s/account/%s\n\n",
		w.state.domain, w.state.accountName); err != nil {
		return err
	}
	for {
		key1, err := w.readPassword("(Optional) Registration Key: ")
		if err != nil {
			return err
		}
		key2, err := w.readPassword("Please confirm: ")
		if err != nil {
			return err
		}
		if key1 == key2 {
			w.state.registrationKey = key1
			break
		}
		if _, err := fmt.Fprintln(w.io.out, "Keys must match."); err != nil {
			return err
		}
	}

	// Step 5: Proxies (both optional)
	if _, err := fmt.Fprintln(w.io.out, "If your network requires you to use a proxy, provide the address"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, "of these proxies now."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
		return err
	}
	defaultHTTP, _ := w.snapctl.Get("http-proxy")
	defaultHTTPS, _ := w.snapctl.Get("https-proxy")
	httpProxy, err := w.promptLine("HTTP proxy URL", defaultHTTP)
	if err != nil {
		return err
	}
	w.state.httpProxy = httpProxy
	httpsProxy, err := w.promptLine("HTTPS proxy URL", defaultHTTPS)
	if err != nil {
		return err
	}
	w.state.httpsProxy = httpsProxy

	// Step 6: Access Group (optional)
	if _, err := fmt.Fprintln(w.io.out, "You may provide an access group for this computer e.g. webservers."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
		return err
	}
	defaultAccessGroup, _ := w.snapctl.Get("access-group")
	accessGroup, err := w.promptLine("Access group", defaultAccessGroup)
	if err != nil {
		return err
	}
	w.state.accessGroup = accessGroup

	// Step 7: Tags (optional; comma-separated; each validated against tagRE)
	if _, err := fmt.Fprintln(w.io.out, ""); err != nil {
		return err
	}
	defaultTags, _ := w.snapctl.Get("tags")
	for {
		tags, err := w.promptLine("Tags (comma-separated)", defaultTags)
		if err != nil {
			return err
		}
		if tags == "" {
			w.state.tags = ""
			break
		}
		valid := true
		for part := range strings.SplitSeq(tags, ",") {
			t := strings.TrimSpace(part)
			if t != "" && !validateTag(t) {
				if _, err := fmt.Fprintf(w.io.out,
					"Invalid tag %q: tags must start with alphanumeric and contain only alphanumeric characters and hyphens.\n", t); err != nil {
					return err
				}
				valid = false
				break
			}
		}
		if valid {
			w.state.tags = tags
			break
		}
	}

	// Step 8: Summary
	regKeyDisplay := "(not set)"
	if w.state.registrationKey != "" {
		regKeyDisplay = "(set)"
	}
	if _, err := fmt.Fprintln(w.io.out, "\nA summary of the provided information:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  Computer's Title: %s\n", w.state.computerTitle); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  Account Name:     %s\n", w.state.accountName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  Landscape FQDN:   %s\n", w.state.domain); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  Registration Key: %s\n", regKeyDisplay); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  HTTP Proxy:       %s\n", w.state.httpProxy); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  HTTPS Proxy:      %s\n", w.state.httpsProxy); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  Access Group:     %s\n", w.state.accessGroup); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.io.out, "  Tags:             %s\n", w.state.tags); err != nil {
		return err
	}

	// Step 9: Confirm
	if _, err := fmt.Fprint(w.io.out, "\nApply this configuration? [Y/n]: "); err != nil {
		return err
	}
	confirm, err := readLine(w.io.in)
	if err != nil {
		return err
	}
	confirm = strings.ToLower(strings.TrimSpace(confirm))
	if confirm == "n" || confirm == "no" {
		if _, err := fmt.Fprintln(w.io.out, "Configuration not saved."); err != nil {
			return err
		}
		return nil
	}

	// Step 10: Write via a single snapctl set call with all non-empty key=value
	// pairs. Batching into one call is critical: snapctl triggers the configure
	// hook once per invocation; calling set per-key would fire the hook in
	// incomplete intermediate states, causing it to fail validation.
	settings := []struct{ key, val string }{
		{"url", w.state.url},
		{"computer-title", w.state.computerTitle},
		{"account-name", w.state.accountName},
		{"registration-key", w.state.registrationKey},
		{"http-proxy", w.state.httpProxy},
		{"https-proxy", w.state.httpsProxy},
		{"access-group", w.state.accessGroup},
		{"tags", w.state.tags},
	}
	var pairs []string
	for _, s := range settings {
		if s.val == "" {
			continue
		}
		pairs = append(pairs, s.key+"="+s.val)
	}
	if err := w.snapctl.Set(pairs...); err != nil {
		return err
	}

	// Restart the daemon; failure is a warning only (config already written)
	if err := w.snapctl.Restart("landscape-client-core"); err != nil {
		_, _ = fmt.Fprintf(w.io.err, "Warning: failed to restart landscape-client-core: %v\n", err)
		_, _ = fmt.Fprintln(w.io.err, "Configuration has been saved. You can restart manually with:")
		_, _ = fmt.Fprintln(w.io.err, "  snap restart landscape-client-core")
	}

	return nil
}

func main() {
	w := &wizard{
		io: wizardIO{
			inRaw: os.Stdin,
			in:    bufio.NewReader(os.Stdin),
			out:   os.Stdout,
			err:   os.Stderr,
		},
		snapctl: &RealSnapctlRunner{},
	}
	if err := w.Run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
