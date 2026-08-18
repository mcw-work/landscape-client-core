package version_test

import (
	"strings"
	"testing"

	"github.com/canonical/landscape-client-core/internal/version"
)

func TestUserAgent_TracksVersion(t *testing.T) {
	if !strings.HasSuffix(version.UserAgent(), version.Version) {
		t.Errorf("UserAgent %q does not carry Version %q", version.UserAgent(), version.Version)
	}
	if !strings.HasPrefix(version.UserAgent(), "landscape-client/") {
		t.Errorf("UserAgent %q lost the expected prefix; the server parses this", version.UserAgent())
	}
}

func TestVersion_IsOverridable(t *testing.T) {
	// -ldflags -X requires a var, not a const. A const would make the snap build
	// silently ship the hard-coded default while snapcraft.yaml says otherwise.
	orig := version.Version
	defer func() { version.Version = orig }()

	version.Version = "99.99"
	if version.Version != "99.99" {
		t.Fatal("Version is not assignable; -ldflags -X cannot override a const")
	}
	// UserAgent is a function so an injected Version is reflected without -X'ing
	// UserAgent as well.
	if version.UserAgent() != "landscape-client/99.99" {
		t.Errorf("UserAgent() = %q, want it to reflect the overridden Version", version.UserAgent())
	}
}
