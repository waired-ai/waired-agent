package tray

import "github.com/waired-ai/waired-agent/internal/platform/browser"

// OpenBrowser launches url with the user's default handler.
//
// The tray used to carry its own per-OS copy of this in each actions_*.go —
// which is how the Windows one (a bare "rundll32.exe" as lpApplicationName,
// #181) kept the "Sign in…" item broken after the CLI's copy was extracted into
// internal/platform/browser. One implementation, three OSes, one place to fix.
//
// The tray always runs as the desktop user, so browser.Open's root
// de-escalation is inert here; it costs a euid check.
func OpenBrowser(url string) error { return browser.Open(url) }
