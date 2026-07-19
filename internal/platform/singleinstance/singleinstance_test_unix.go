//go:build linux || darwin

package singleinstance

import (
	"path/filepath"
	"testing"
)

// newTestGuard binds a lock path inside a per-test temp dir so the test
// never touches the real per-user runtime/waired-tray.lock. All calls
// from one newTestGuard share the path, which is the point — the test
// acquires the same lock file twice.
func newTestGuard(t *testing.T) func() (func(), bool, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guard.lock")
	return func() (func(), bool, error) { return acquireLock(path) }
}
