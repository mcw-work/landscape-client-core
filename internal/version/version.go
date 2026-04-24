package version

// Version is the landscape-client version string expected by the server.
const Version = "26.08~beta.2"

// UserAgent is the HTTP header value sent to the Landscape server for compatibility checks.
const UserAgent = "landscape-client/" + Version
