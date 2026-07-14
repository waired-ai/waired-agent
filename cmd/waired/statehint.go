package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// resolveSystemFallback answers a status query from the platform SYSTEM
// state dir when the caller's resolved (per-user) dir held no identity.
// It is the honest replacement for the old os.Stat guess: instead of
// inferring readability from a stat of identity.json, it actually loads the
// System dir and reports what happened. cmdline is the command to suggest
// re-running elevated (e.g. "waired status").
func resolveSystemFallback(resolvedDir, cmdline string) (string, *identity.Identity, string) {
	return resolveSystemFallbackAt(resolvedDir, paths.StateDir(paths.System), cmdline, runtime.GOOS)
}

// resolveSystemFallbackAt is the testable core of resolveSystemFallback. Its
// three outcomes map to the caller's three exit-0 branches:
//
//   - (sysDir, id, "")   the System dir is enrolled AND readable → the caller
//     renders status from sysDir. This is the elevated / admin case: on a
//     Windows service install the whole %ProgramData%\waired tree is locked to
//     SYSTEM+Administrators, and on Windows even an elevated `waired status`
//     resolves to the admin's empty %AppData% first, so this fallback is the
//     only way it sees the system-wide enrollment.
//   - ("", nil, notice)  the System dir looks enrolled but can't be read
//     without elevation → the caller prints notice. Standard / basic-token
//     users land here (Windows DACL, or a 0700 root-owned dir on Unix).
//   - ("", nil, "")      no System-dir fallback applies (genuinely not
//     enrolled, or resolvedDir already IS the System dir) → the caller prints
//     its plain "Not enrolled" message.
func resolveSystemFallbackAt(resolvedDir, sysDir, cmdline, goos string) (string, *identity.Identity, string) {
	// A WAIRED_STATE_DIR override collapses every mode to the same dir, and
	// Unix root already defaults to the System dir — either way there is no
	// distinct System dir to fall back to. (The caller's explicit permission
	// short-circuit on resolvedDir covers an unreadable override.)
	if filepath.Clean(sysDir) == filepath.Clean(resolvedDir) {
		return "", nil, ""
	}
	id, err := identity.Load(sysDir)
	switch {
	case err == nil && id != nil:
		return sysDir, id, ""
	case errors.Is(err, fs.ErrPermission):
		// Enrolled machine whose System dir is locked down (Windows service
		// DACL → SYSTEM+Administrators, or a 0700 root-owned dir on Unix). Go
		// maps Windows ERROR_ACCESS_DENIED to fs.ErrPermission, so this branch
		// matches on all three OSes. Report it; don't fail.
		return "", nil, systemEnrolledElevationNotice(sysDir, cmdline, goos)
	default:
		// identity.Load returns (nil, nil) when identity.json is absent —
		// genuinely not enrolled. Any other error fails open to the plain
		// "Not enrolled" message rather than inventing a new failure mode.
		return "", nil, ""
	}
}

// systemEnrolledElevationNotice is the OS-aware wording for "the system state
// exists but you need elevation to read it". Kept pure so it can be
// table-tested across goos values without a real locked directory.
func systemEnrolledElevationNotice(sysDir, cmdline, goos string) string {
	return fmt.Sprintf(
		"This device is enrolled system-wide, but its state (%s) needs elevation to read.\n%s.",
		sysDir, capitalize(elevationHintFor(goos, cmdline)))
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
