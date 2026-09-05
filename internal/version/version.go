// Package version holds the slack bridge version string.
//
// Priority order:
//  1. Value injected at build time via -ldflags (used by CI/Docker releases).
//  2. Module version embedded by the Go toolchain (used by go install @vX.Y.Z).
//  3. "dev" fallback for plain local builds.
//
// To embed a version in a local build:
//
//	go build -ldflags="-X github.com/mkende/screenshotter_slack_bridge/internal/version.Version=v1.2.3" ./cmd/screenshotter-slack-bridge
package version

import "runtime/debug"

// Version is the current bridge version string (e.g. "v1.2.3" or "dev").
var Version = "dev"

func init() {
	if Version != "dev" {
		// Explicitly set via -ldflags at build time; nothing to do.
		return
	}
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
}
