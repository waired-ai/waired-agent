//go:build darwin

package main

import (
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// darwinControlURLEnvFile is the macOS analog of Linux's
// /etc/waired/agent.env: <system state dir>/agent.env, written by
// install.sh's darwin_write_control_url. It is a fixed path (the System
// state dir) because platformDefaultControlURL is consulted as the
// --control flag default before --state-dir is parsed — matching Linux's
// fixed EnvironmentFile path.
func darwinControlURLEnvFile() string {
	return filepath.Join(paths.StateDir(paths.System), "agent.env")
}

// platformDefaultControlURL returns the Control Plane URL install.sh
// persisted to the macOS state-dir agent.env, or "" if none. sudo strips
// the caller's env and the launchd plist cannot feed one to the daemon, so
// this file is how a bare `sudo waired init` recovers what `install.sh
// --dev/--control` configured — the darwin parity for control_url_linux.go.
// The parser is shared with Linux (control_url_shared.go).
func platformDefaultControlURL() string {
	return parseControlURLFromEnvFile(darwinControlURLEnvFile())
}
