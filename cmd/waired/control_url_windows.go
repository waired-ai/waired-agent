//go:build windows

package main

import (
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// windowsControlURLEnvFile is the Windows analog of Linux's
// /etc/waired/agent.env: <system state dir>\agent.env
// (%ProgramData%\waired\agent.env), written by install.ps1's
// Write-ControlUrlEnvFile. It is a fixed path (the System state dir)
// because platformDefaultControlURL is consulted as the --control flag
// DEFAULT, before --state-dir is parsed — matching Linux's fixed
// EnvironmentFile path and control_url_darwin.go.
func windowsControlURLEnvFile() string {
	return filepath.Join(paths.StateDir(paths.System), "agent.env")
}

// platformDefaultControlURL returns the Control Plane URL install.ps1
// persisted to %ProgramData%\waired\agent.env, or "" if none. Windows had
// no such file until #42: the previous stub returned "" and claimed the
// installer set a machine-level WAIRED_CONTROL_URL environment variable,
// which it never did (its only SetEnvironmentVariable call is for Path).
// The result was that any install where `waired init` did not enroll
// (-SkipInit, a cancelled sign-in, or the `iwr | iex` form where -Dev /
// -Control cannot bind) left a later bare `waired init` falling back to
// the baked production CP instead of the one the device was installed
// with — the recovery path Linux and macOS have always had.
//
// Unlike Linux there is no SCM EnvironmentFile, so this feeds `waired
// init` only; the daemon reads ControlURL from the agent.json init
// writes. A non-elevated read returns "" — secrets.SecureDir locks the
// state dir to SYSTEM + Administrators + the installing user with
// inheritance disabled — which is harmless: the parser treats any read
// error as "not configured", and `waired init` on Windows must run
// elevated anyway (it writes identity.json into that same dir).
//
// The parser is shared with Linux and macOS (control_url_shared.go).
func platformDefaultControlURL() string {
	return parseControlURLFromEnvFile(windowsControlURLEnvFile())
}
