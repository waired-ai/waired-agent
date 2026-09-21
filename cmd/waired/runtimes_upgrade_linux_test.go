//go:build linux

package main

// Linux-only because it needs a venv to exist for the decision to reach
// the installer at all: the Windows and macOS installer stubs always
// answer "no active install", and the symlink this seeds is not
// creatable unprivileged on Windows.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// The upgrade builds under the vLLM base's lock, takes it once (the install
// inside the converge must not take it again: flock is per open file, and a
// second take in this process would wait on the first), and removes
// nothing — the venv it replaces may be the one the engine is running from
// (waired-agent#1431).
func TestRuntimesUpgrade_VLLMBuildsUnderTheLockAndRemovesNothing(t *testing.T) {
	prev := vllmInstall
	t.Cleanup(func() { vllmInstall = prev })
	prevLock := vllmLock
	t.Cleanup(func() { vllmLock = prevLock })

	var events []string
	vllmInstall = func(_ context.Context, _ string, _ func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		events = append(events, "install")
		return infruntime.InstallResult{}, nil
	}
	vllmLock = func(context.Context, string, func()) (func(), error) {
		events = append(events, "lock")
		return func() { events = append(events, "unlock") }, nil
	}

	dir := t.TempDir()
	seedActiveVLLMVenv(t, dir, "0.20.0")
	if err := runVLLMUpgrade(dir, true); err != nil {
		t.Fatalf("runVLLMUpgrade: %v", err)
	}
	if got := strings.Join(events, ","); got != "lock,install,unlock" {
		t.Errorf("events = %s, want lock,install,unlock", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "runtimes", "vllm", "0.20.0", ".venv", "bin", "python")); err != nil {
		t.Errorf("the upgrade removed the venv it replaced: %v", err)
	}
}

// A converge whose build fails still hands the state dir back to the
// service user: the uv, its cache and the managed Python the failed build
// left are root-owned otherwise (waired-ai/waired#1435).
func TestRuntimesUpgrade_VLLMFailedBuildStillHandsStateBack(t *testing.T) {
	prev := vllmInstall
	t.Cleanup(func() { vllmInstall = prev })
	vllmInstall = func(context.Context, string, func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
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
			// A management address nothing answers on: no daemon, so no
			// engine to protect, whatever runs on the machine the test is on.
			cmd.SetArgs([]string{"vllm", "--yes", "--state-dir", dir, "--mgmt", "http://127.0.0.1:1"})
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

// Removing the venv under a running engine fails its next request
// (waired-agent#1431), so uninstall asks the daemon first and refuses while
// the vLLM engine has a process.
func TestRuntimesUninstall_VLLMRefusesWhileTheEngineRuns(t *testing.T) {
	for _, state := range []string{infruntime.StateReady, infruntime.StateStarting} {
		t.Run(state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/waired/v1/inference/runtimes" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"runtimes":[{"name":"vllm","state":"` + state + `"}]}`))
			}))
			defer srv.Close()
			dir := t.TempDir()
			seedActiveVLLMVenv(t, dir, "0.29.0")

			cmd := newRuntimesUninstallCmd()
			cmd.SetArgs([]string{"vllm", "--yes", "--state-dir", dir, "--mgmt", srv.URL})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "waired inference engine stop") {
				t.Fatalf("uninstall = %v, want a refusal naming the stop command", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "runtimes", "vllm", "0.29.0", ".venv")); err != nil {
				t.Errorf("the venv was removed anyway: %v", err)
			}
		})
	}
}
