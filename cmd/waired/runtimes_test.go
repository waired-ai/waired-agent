package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestInstallVLLM_StateDirAndHandoff verifies installVLLM roots the venv
// at <state-dir>/runtimes/vllm (not a $HOME-relative path) and hands the
// root-written state dir back to the waired-agent service user afterward,
// mirroring the ollama bundle install (#525 / ollama parity #484).
func TestInstallVLLM_StateDirAndHandoff(t *testing.T) {
	origInstall := vllmInstall
	t.Cleanup(func() { vllmInstall = origInstall })
	var gotBaseDir string
	called := false
	vllmInstall = func(_ context.Context, baseDir string, _ func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		called = true
		gotBaseDir = baseDir
		return infruntime.InstallResult{Version: "0.11.0", VenvPath: filepath.Join(baseDir, "0.11.0", ".venv")}, nil
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

	if err := installVLLM("/var/lib/waired"); err != nil {
		t.Fatalf("installVLLM: %v", err)
	}
	if !called {
		t.Fatal("vllmInstall seam was not invoked")
	}
	if want := filepath.Join("/var/lib/waired", "runtimes", "vllm"); gotBaseDir != want {
		t.Errorf("install baseDir = %q, want %q", gotBaseDir, want)
	}
	// The whole state dir (not just runtimes/vllm) is handed back, so a
	// root-run install can't leave the daemon locked out of its identity.
	if fixCalls != 1 {
		t.Errorf("fixStateOwnership called %d times, want 1", fixCalls)
	}
	if gotOwnedDir != "/var/lib/waired" {
		t.Errorf("fixStateOwnership dir = %q, want the full state dir /var/lib/waired", gotOwnedDir)
	}
}

// TestInstallVLLM_Error surfaces an install failure and skips the
// ownership hand-off: nothing was successfully written, so there is
// nothing to chown back.
func TestInstallVLLM_Error(t *testing.T) {
	origInstall := vllmInstall
	t.Cleanup(func() { vllmInstall = origInstall })
	vllmInstall = func(context.Context, string, func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		return infruntime.InstallResult{}, errors.New("uv venv failed")
	}

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	fixCalled := false
	fixStateOwnership = func(string) error { fixCalled = true; return nil }

	if err := installVLLM(t.TempDir()); err == nil {
		t.Fatal("expected install error to propagate")
	}
	if fixCalled {
		t.Error("fixStateOwnership should not run when the install failed")
	}
}
