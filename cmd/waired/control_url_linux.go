//go:build linux

package main

import (
	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// platformDefaultControlURL returns the Control Plane URL the installer
// recorded in the systemd EnvironmentFile (/etc/waired/agent.env), or ""
// if none is set or the file is unreadable. This lets `sudo waired init`
// (no --control) pick up what `install.sh --control <URL>` wrote: sudo
// strips the caller's env and the EnvironmentFile only feeds the daemon,
// so the value is otherwise invisible to an interactive init. The parser
// is shared with macOS (control_url_shared.go).
func platformDefaultControlURL() string {
	return parseControlURLFromEnvFile(service.LinuxEnvFilePath)
}
