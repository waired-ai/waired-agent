//go:build linux || darwin

package singleinstance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// acquire takes an advisory, non-blocking flock on a per-user lock file
// named after the app. The lock file lives under the per-user
// interactive state dir's runtime/ subdir — deliberately NOT the caller's
// StateDir, which for waired-tray may resolve to the root-owned system
// dir and be unwritable by the desktop user.
func acquire(name string) (func(), bool, error) {
	dir := filepath.Join(paths.StateDir(paths.Interactive), "runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return noop, true, fmt.Errorf("singleinstance: create %s: %w", dir, err)
	}
	return acquireLock(filepath.Join(dir, name+".lock"))
}

// acquireLock holds an exclusive, non-blocking flock for the process
// lifetime. The lock is tied to the open file description, so the kernel
// drops it automatically when the fd closes or the process exits — a
// crashed previous holder therefore leaves no stale lock, and no
// PID-liveness probe is needed. release closes the fd.
func acquireLock(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return noop, true, fmt.Errorf("singleinstance: open %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			// Another live instance holds the lock.
			return noop, false, nil
		}
		return noop, true, fmt.Errorf("singleinstance: flock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, true, nil
}
