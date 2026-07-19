//go:build windows

package singleinstance

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// newTestGuard binds a unique per-session mutex name (never the real
// Local\waired-tray) so parallel test binaries and a running tray cannot
// collide. All calls from one newTestGuard share the name, which is the
// point — the test acquires the same name twice.
func newTestGuard(t *testing.T) func() (func(), bool, error) {
	t.Helper()
	name := fmt.Sprintf(`Local\waired-test-si-%s-%d`, sanitizeMutexName(t.Name()), os.Getpid())
	return func() (func(), bool, error) { return acquireMutex(name) }
}

// sanitizeMutexName strips the backslashes a subtest name ("Parent/Sub")
// would otherwise inject into the object name after the Local\ prefix.
func sanitizeMutexName(s string) string {
	return strings.NewReplacer(`\`, "_", "/", "_").Replace(s)
}
