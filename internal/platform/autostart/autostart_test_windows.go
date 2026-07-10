//go:build windows

package autostart

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

// newTestManager points at HKCU\Software\Waired\TestAutostart instead
// of the canonical Run key, so unit tests cannot insert a stray
// startup entry that survives the test process. The sub-key is
// removed at test end.
func newTestManager(t *testing.T) Manager {
	t.Helper()
	const path = `Software\Waired\TestAutostart`
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
	})
	return NewForTest("waired-test-autostart", path)
}
