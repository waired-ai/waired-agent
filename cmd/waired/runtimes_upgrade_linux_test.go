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
