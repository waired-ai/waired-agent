//go:build darwin

package autostart

import (
	"testing"
)

// newTestManager redirects $HOME to a temp dir so the test's plist
// writes land outside the user's real ~/Library/LaunchAgents, and
// stubs runLaunchctlFn so the test does NOT mutate the user's actual
// launchd state. The shared TestRoundTrip in autostart_test.go then
// exercises Enable / IsEnabled / Disable against this redirected
// view.
func newTestManager(t *testing.T) Manager {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origLaunchctl := runLaunchctlFn
	runLaunchctlFn = func(args []string) ([]byte, []byte, error) {
		return nil, nil, nil // accept every call as success
	}
	t.Cleanup(func() { runLaunchctlFn = origLaunchctl })

	return newManager("waired-test-autostart")
}
