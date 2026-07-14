//go:build darwin

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// TestDarwinControlURLEnvFile pins the macOS agent.env location to
// <system state dir>/agent.env — the path install.sh's
// darwin_write_control_url writes and platformDefaultControlURL reads.
// A drift between the two would silently break --dev/--control enrollment
// on macOS (the #22-adjacent parity gap this closes).
func TestDarwinControlURLEnvFile(t *testing.T) {
	got := darwinControlURLEnvFile()
	want := filepath.Join(paths.StateDir(paths.System), "agent.env")
	if got != want {
		t.Fatalf("darwinControlURLEnvFile() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "/agent.env") {
		t.Errorf("env file %q must end in /agent.env", got)
	}
	// The System state dir on macOS is /Library/Application Support/waired;
	// guard against a regression that points init at a per-user dir the root
	// daemon never reads.
	if !strings.Contains(got, "/Library/Application Support/waired") {
		t.Errorf("env file %q must live under the system state dir", got)
	}
}
