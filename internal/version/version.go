package version

// Version is the landscape-client version string expected by the server.
// Overridden at build time via -ldflags -X, with snapcraft.yaml as the single
// source of truth. Must be a var, not a const: -ldflags cannot set a const, and
// a plain "go build" reports 0.0.0-dev rather than claiming a release number.
var Version = "0.0.0-dev"

// UserAgent is the HTTP header value sent to the Landscape server for
// compatibility checks. It is a function rather than a package-level var so an
// injected Version is always reflected, without also having to -X UserAgent.
func UserAgent() string { return "landscape-client/" + Version }
