package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// resolveOwnBinaryPath returns the absolute path of the sibling
// `waired` CLI binary alongside this `waired-agent`. Used as the
// "expected wrapper command prefix" for the management API's
// integration-status endpoint, which compares it against the values
// detected in the user's shell rc files and IDE settings.
//
// Resolution order:
//  1. Sibling of os.Executable() named "waired" (typical install layout
//     where both binaries live in the same directory).
//  2. exec.LookPath("waired") — for installs where the two binaries are
//     on PATH but in different directories.
//  3. Plain "waired" as a last-resort literal — the detect comparison
//     will surface this as "stale" against any absolute install,
//     which is the safer failure mode (false positive over false
//     negative).
func resolveOwnBinaryPath() string {
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		sibling := filepath.Join(filepath.Dir(exe), "waired")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if path, err := exec.LookPath("waired"); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	return "waired"
}
