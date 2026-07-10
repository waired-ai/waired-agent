//go:build linux

package main

import (
	"bufio"
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// platformDefaultControlURL returns the Control Plane URL the installer
// recorded in the systemd EnvironmentFile (/etc/waired/agent.env), or ""
// if none is set or the file is unreadable. This lets `sudo waired init`
// (no --control) pick up what `install.sh --control <URL>` wrote: sudo
// strips the caller's env and the EnvironmentFile only feeds the daemon,
// so the value is otherwise invisible to an interactive init.
func platformDefaultControlURL() string {
	return parseControlURLFromEnvFile(service.LinuxEnvFilePath)
}

// parseControlURLFromEnvFile reads WAIRED_CONTROL_URL from a systemd-style
// KEY=VALUE env file. Any read error is treated as "not configured" and
// returns "" — never fatal (the file is typically 0640 root:waired, which
// is readable under a sudo init but not by an unprivileged one).
func parseControlURLFromEnvFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "WAIRED_CONTROL_URL" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v != "" {
			return v
		}
	}
	return ""
}
