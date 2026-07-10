//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
)

// TestInstallOllamaLinux_Bundled verifies installOllama drives the
// bundled installer seam (and never shells out to install.sh /
// systemctl, which were removed in the bundle redesign) when -y skips
// the prompt, and hands the root-written state dir back to the service
// user afterwards (#484).
func TestInstallOllamaLinux_Bundled(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })

	var gotBaseDir string
	called := false
	installOllamaBundled = func(_ context.Context, baseDir string) error {
		called = true
		gotBaseDir = baseDir
		return nil
	}

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	var gotOwnedDir string
	fixCalls := 0
	fixStateOwnership = func(dir string) error {
		fixCalls++
		gotOwnedDir = dir
		return nil
	}

	if err := installOllama(true, "/var/lib/waired"); err != nil {
		t.Fatalf("installOllama(-y): %v", err)
	}
	if !called {
		t.Fatal("bundled installer seam was not invoked")
	}
	if gotBaseDir != "/var/lib/waired/runtimes/ollama" {
		t.Errorf("baseDir = %q, want <state-dir>/runtimes/ollama", gotBaseDir)
	}
	// The whole state dir (not just runtimes/ollama) is handed back, so a
	// root-run install can't leave the daemon locked out of its identity.
	if fixCalls != 1 {
		t.Errorf("fixStateOwnership called %d times, want 1", fixCalls)
	}
	if gotOwnedDir != "/var/lib/waired" {
		t.Errorf("fixStateOwnership dir = %q, want the state dir /var/lib/waired", gotOwnedDir)
	}
}

// TestInstallOllamaLinux_Error surfaces an installer failure and skips the
// ownership hand-off: nothing was successfully written, so there is nothing
// to chown back.
func TestInstallOllamaLinux_Error(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	installOllamaBundled = func(context.Context, string) error { return errors.New("download failed") }

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	fixCalled := false
	fixStateOwnership = func(string) error { fixCalled = true; return nil }

	if err := installOllama(true, t.TempDir()); err == nil {
		t.Fatal("expected installer error to propagate")
	}
	if fixCalled {
		t.Error("fixStateOwnership should not run when the install failed")
	}
}
