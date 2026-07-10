package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// TestRunStatusBodyNotEnrolledUnchanged pins the genuinely-not-enrolled
// UX: empty state dir → the exact historical message on stdout, nil
// error (exit 0). WAIRED_STATE_DIR makes systemStateNotice's sysDir
// equal the resolved dir, so the notice cannot fire regardless of the
// machine the test runs on.
func TestRunStatusBodyNotEnrolledUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(paths.EnvOverride, dir)
	var err error
	out := captureStdout(t, func() {
		err = runStatusBody(defaultMgmtURL, dir, false, "")
	})
	if err != nil {
		t.Fatalf("runStatusBody: %v", err)
	}
	if want := "Not enrolled. Run `waired init` to connect this device.\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestRunAuthStatusBodyNotEnrolledUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(paths.EnvOverride, dir)
	var err error
	out := captureStdout(t, func() {
		err = runAuthStatusBody(dir)
	})
	if err != nil {
		t.Fatalf("runAuthStatusBody: %v", err)
	}
	if !strings.Contains(out, "not enrolled") {
		t.Errorf("stdout = %q, want the not-enrolled message", out)
	}
}
