//go:build windows

package controlurl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// TestWindowsEnvFilePath pins the Windows agent.env location to <system
// state dir>\agent.env — the path install.ps1's Write-ControlUrlEnvFile
// writes and PlatformDefault reads. A drift between the two would
// silently reopen #42: enrollment would fall back to the baked production
// CP on any host where `waired init` did not run at install time.
// Product contract.
//
// envFileDir's GOOS table test (controlurl_test.go) covers the branch on
// every runner; this pins what the "system state dir" resolves to on a
// real Windows host, which only Windows can answer.
func TestWindowsEnvFilePath(t *testing.T) {
	got := EnvFilePath(runtime.GOOS, paths.StateDir(paths.System))
	want := filepath.Join(paths.StateDir(paths.System), "agent.env")
	if got != want {
		t.Fatalf("EnvFilePath(windows, ...) = %q, want %q", got, want)
	}
	if filepath.Base(got) != "agent.env" {
		t.Errorf("env file %q must be named agent.env", got)
	}
	// The System state dir on Windows is %ProgramData%\waired; guard
	// against a regression that points init at the per-user %AppData%
	// dir, which the LocalSystem service never reads.
	if userDir := paths.StateDir(paths.Interactive); userDir != paths.StateDir(paths.System) &&
		strings.HasPrefix(got, userDir) {
		t.Errorf("env file %q must live under the system state dir, not the per-user one (%q)", got, userDir)
	}
}

// TestWindowsPlatformDefault_ReadsEnvFile exercises the full read path
// against a redirected state dir: paths.StateDir honours
// $WAIRED_STATE_DIR verbatim, so the test can place a real agent.env
// where PlatformDefault will look for it.
func TestWindowsPlatformDefault_ReadsEnvFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(paths.EnvOverride, dir)

	if got := PlatformDefault(); got != "" {
		t.Fatalf("no agent.env should yield %q, got %q", "", got)
	}

	envFile := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(envFile, []byte("WAIRED_CONTROL_URL=https://cp.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := PlatformDefault(), "https://cp.example.test"; got != want {
		t.Errorf("PlatformDefault() = %q, want %q", got, want)
	}
}
