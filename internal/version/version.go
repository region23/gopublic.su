package version

// Version is the current version of the application.
// It can be overridden at build time via ldflags:
// -ldflags "-X gopublic/internal/version.Version=1.0.0"
var Version = "dev"
