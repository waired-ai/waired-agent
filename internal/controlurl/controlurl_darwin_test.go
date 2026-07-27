//go:build darwin

package controlurl

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// TestDarwinEnvFilePath pins the macOS agent.env location to <system
// state dir>/agent.env — the path install.sh's darwin_write_control_url
// writes and PlatformDefault reads. A drift between the two would
// silently break --dev/--control enrollment and app sign-in on macOS.
// Product contract.
//
// envFileDir's GOOS table test (controlurl_test.go) covers the branch on
// every runner; this pins what the "system state dir" resolves to on a
// real macOS host, which only macOS can answer.
func TestDarwinEnvFilePath(t *testing.T) {
	got := EnvFilePath(runtime.GOOS, paths.StateDir(paths.System))
	want := filepath.Join(paths.StateDir(paths.System), "agent.env")
	if got != want {
		t.Fatalf("EnvFilePath(darwin, ...) = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "/agent.env") {
		t.Errorf("env file %q must end in /agent.env", got)
	}
	// The System state dir on macOS is /Library/Application Support/waired;
	// guard against a regression that points init at a per-user dir the
	// root daemon never reads.
	if !strings.Contains(got, "/Library/Application Support/waired") {
		t.Errorf("env file %q must live under the system state dir", got)
	}
}
