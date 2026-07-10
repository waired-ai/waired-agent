package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// systemStateNotice explains why "Not enrolled" would be the wrong
// verdict: the caller resolved resolvedDir (per-user, no identity.json
// there), but the platform SYSTEM state dir looks enrolled — or can't
// even be inspected without elevation. Returns "" when the plain
// "Not enrolled" message is correct. cmdline is the command to suggest
// re-running elevated ("waired status").
func systemStateNotice(resolvedDir, cmdline string) string {
	return systemStateNoticeAt(resolvedDir, paths.StateDir(paths.System),
		cmdline, runtime.GOOS, os.Geteuid())
}

// systemStateNoticeAt is the testable core of systemStateNotice.
func systemStateNoticeAt(resolvedDir, sysDir, cmdline, goos string, euid int) string {
	// Root already resolves to the System dir by default; elevation
	// advice would be useless. Windows has no euid (Geteuid() == -1),
	// so the stat results below decide there.
	if goos != "windows" && euid == 0 {
		return ""
	}
	// A WAIRED_STATE_DIR override collapses both paths to the same dir
	// (paths.StateDir returns it verbatim for every mode); the explicit
	// fs.ErrPermission branch in the caller reports that case instead.
	if filepath.Clean(sysDir) == filepath.Clean(resolvedDir) {
		return ""
	}
	_, err := os.Stat(filepath.Join(sysDir, "identity.json"))
	switch {
	case err == nil:
		return fmt.Sprintf(
			"This device is enrolled system-wide, but its state (%s) is not readable by this user.\n%s.",
			sysDir, capitalize(elevationHintFor(goos, cmdline)))
	case errors.Is(err, fs.ErrPermission):
		// Typical service install: the 0700 root-owned dir denies
		// traversal, so we can't tell whether identity.json exists.
		return fmt.Sprintf(
			"Cannot read the system state directory %s (permission denied).\nIf this device was enrolled with `sudo waired init`, %s; otherwise run `waired init`.",
			sysDir, elevationHintFor(goos, cmdline))
	default:
		// ENOENT: genuinely not enrolled. Anything else: fail open to
		// the existing message rather than invent a new failure mode.
		return ""
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
