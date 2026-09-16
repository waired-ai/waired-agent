//go:build linux

package main

// Linux-only because it needs a venv to exist for the decision to reach
// the installer at all: the Windows and macOS installer stubs always
// answer "no active install", and the symlink this seeds is not
// creatable unprivileged on Windows.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// seedActiveVLLMVenv lays down the shape ActiveErr reads: a version
// directory holding a .venv with an interpreter, and `current` pointing
// at it. Enough for the converge to decide; the install itself is faked.
func seedActiveVLLMVenv(t *testing.T, stateDir, version string) {
	t.Helper()
	base := filepath.Join(stateDir, "runtimes", "vllm")
	bin := filepath.Join(base, version, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "python"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(version, filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
}

// The converge must never ask for a clean environment. It runs
// unattended, possibly while vLLM is serving, and `uv venv` over an
// existing environment either refuses outright or clears it — on a real
// host the refusal path took the working venv with it, because the
// rollback then removed a directory this call had not created (#843).
func TestRuntimesUpgrade_VLLMNeverRecreatesTheEnvironment(t *testing.T) {
	prev := vllmInstall
	t.Cleanup(func() { vllmInstall = prev })

	asked := false
	sawRecreate := false
	vllmInstall = func(_ context.Context, _ string, recreate bool, _ func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		asked = true
		sawRecreate = recreate
		return infruntime.InstallResult{}, errors.New("stop here; what this pins is what the installer was asked for")
	}

	dir := t.TempDir()
	seedActiveVLLMVenv(t, dir, "0.20.0")
	// The error is expected — the fake refuses — so the assertion is on
	// the request, not the outcome.
	_ = runVLLMUpgrade(dir, true)

	if !asked {
		t.Fatal("a venv one release behind the pin never reached the installer")
	}
	if sawRecreate {
		t.Error("the converge asked for a clean environment; that clears the venv the host may be serving from")
	}
}

// A converge whose build fails still hands the state dir back to the
// service user: the uv, its cache and the managed Python the failed build
// left are root-owned otherwise (waired-ai/waired#1435).
func TestRuntimesUpgrade_VLLMFailedBuildStillHandsStateBack(t *testing.T) {
	prev := vllmInstall
	t.Cleanup(func() { vllmInstall = prev })
	vllmInstall = func(context.Context, string, bool, func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		return infruntime.InstallResult{}, errors.New("uv pip install failed")
	}
	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	var handed []string
	fixStateOwnership = func(dir string) error { handed = append(handed, dir); return nil }

	dir := t.TempDir()
	seedActiveVLLMVenv(t, dir, "0.20.0")
	_ = runVLLMUpgrade(dir, true)

	if len(handed) != 1 || handed[0] != dir {
		t.Errorf("hand-off calls = %v, want exactly [%s]", handed, dir)
	}
}

// `runtimes uninstall vllm` removes the managed uv and its cache with the
// last venv, and leaves them while another venv remains
// (waired-ai/waired#1435).
func TestRuntimesUninstall_VLLMRemovesUVWithTheLastVenv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		otherVenv  bool
		wantUVGone bool
	}{
		{"last venv", false, true},
		{"another venv remains", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seedActiveVLLMVenv(t, dir, "0.29.0")
			if tc.otherVenv {
				if err := os.MkdirAll(filepath.Join(dir, "runtimes", "vllm", "0.28.0", ".venv", "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			uvRoot := filepath.Join(dir, "runtimes", "uv")
			for _, p := range []string{filepath.Join(uvRoot, infruntime.UVPinnedVersion), filepath.Join(uvRoot, "cache", "wheels-v5")} {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			cmd := newRuntimesUninstallCmd()
			cmd.SetArgs([]string{"vllm", "--yes", "--state-dir", dir})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			_, err := os.Stat(uvRoot)
			if gone := os.IsNotExist(err); gone != tc.wantUVGone {
				t.Errorf("runtimes/uv gone = %v, want %v (stat err %v)", gone, tc.wantUVGone, err)
			}
		})
	}
}
