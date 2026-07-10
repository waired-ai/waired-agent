//go:build linux

package autostart

import (
	"testing"
)

// newTestManager redirects XDG_CONFIG_HOME at a t.TempDir() so the
// real ~/.config/autostart is never touched by the round-trip test.
func newTestManager(t *testing.T) Manager {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return newManager("waired-test-autostart")
}
