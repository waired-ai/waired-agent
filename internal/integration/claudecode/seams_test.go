package claudecode

import (
	"os"
	"testing"
)

// TestMain seals CLAUDE_CONFIG_DIR for the whole test binary.
//
// Since waired-agent#1457 SettingsPath honours it, so a developer who has it
// set in their shell would run these tests against a different file than CI
// does — and that file is a directory they use for real work. The tests that
// are ABOUT the variable set it themselves with t.Setenv, which restores it
// per test.
func TestMain(m *testing.M) {
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Exit(m.Run())
}
