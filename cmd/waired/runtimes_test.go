package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
	var events []string
	vllmInstall = func(_ context.Context, baseDir string, _ func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		called = true
		gotBaseDir = baseDir
		events = append(events, "install")
		return infruntime.InstallResult{Version: "0.11.0", VenvPath: filepath.Join(baseDir, "0.11.0", ".venv")}, nil
	}
	origLock := vllmLock
	t.Cleanup(func() { vllmLock = origLock })
	var lockedDir string
	vllmLock = func(_ context.Context, baseDir string, _ func()) (func(), error) {
		lockedDir = baseDir
		events = append(events, "lock")
		return func() { events = append(events, "unlock") }, nil
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
	// The build runs under the vLLM base's lock, taken once: the daemon's
	// converge and its reclaim of unused venvs take the same one
	// (waired-agent#1431).
	if want := filepath.Join("/var/lib/waired", "runtimes", "vllm"); gotBaseDir != want || lockedDir != want {
		t.Errorf("install baseDir = %q, lock dir = %q; want both %q", gotBaseDir, lockedDir, want)
	}
	if got := strings.Join(events, ","); got != "lock,install,unlock" {
		t.Errorf("events = %s, want lock,install,unlock", got)
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

// TestInstallVLLM_Error surfaces an install failure AND still hands the
// state dir back.
//
// Inverted by waired-ai/waired#1435 (owner-approved plan). This used to
// assert the opposite ("nothing was successfully written, so there is
// nothing to chown back"), which stopped being true once uv and its cache
// moved under the state dir: a build that fails still leaves a root-owned
// uv, cache and managed Python there, and the service user's next
// converge stops on them.
func TestInstallVLLM_Error(t *testing.T) {
	origInstall := vllmInstall
	t.Cleanup(func() { vllmInstall = origInstall })
	vllmInstall = func(context.Context, string, func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		return infruntime.InstallResult{}, errors.New("uv venv failed")
	}

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	fixCalls := 0
	fixStateOwnership = func(string) error { fixCalls++; return nil }

	if err := installVLLM(t.TempDir()); err == nil {
		t.Fatal("expected install error to propagate")
	}
	if fixCalls != 1 {
		t.Errorf("fixStateOwnership called %d times after a failed install, want 1", fixCalls)
	}
}

// Product contract (waired-agent#319): `waired runtimes install --auto` must
// not offer to install vLLM on a host that cannot serve it. The CLI used to
// carry its own copy of the auto-pick rule with no OS term, so a Windows host
// with a large NVIDIA card was told to install a Linux-only engine. The rule
// now lives once, in router.VLLMAutoEligible.
//
// PRODUCT CONTRACT (waired-agent#1311, owner ruling 2026-09-12): --auto now
// answers ollama on every host, including the Linux/NVIDIA ones the ladder
// used to claim. "Auto" is the host that did not choose, and the engine is
// a choice; `waired runtimes install vllm` is how someone asks for the
// other one. The rows below keep the hardware shapes so the diff is legible
// if the ladder is ever put back.
func TestRecommendEngineFor(t *testing.T) {
	big := []recommendGPU{{Vendor: "nvidia", VRAMTotalMB: 24467}}
	cases := []struct {
		name string
		goos string
		gpus []recommendGPU
		want string
	}{
		{"linux big nvidia", "linux", big, "ollama"},
		{"windows big nvidia", "windows", big, "ollama"},
		{"darwin big nvidia", "darwin", big, "ollama"},
		{"linux small nvidia", "linux", []recommendGPU{{Vendor: "nvidia", VRAMTotalMB: 4096}}, "ollama"},
		{"linux amd", "linux", []recommendGPU{{Vendor: "amd", VRAMTotalMB: 64000}}, "ollama"},
		{"linux no gpu", "linux", nil, "ollama"},
		{
			"linux second gpu qualifies",
			"linux",
			[]recommendGPU{{Vendor: "amd", VRAMTotalMB: 64000}, {Vendor: "nvidia", VRAMTotalMB: 24467}},
			"ollama",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recommendEngineFor(tc.goos, tc.gpus); got != tc.want {
				t.Errorf("recommendEngineFor(%q, %+v) = %q, want %q", tc.goos, tc.gpus, got, tc.want)
			}
		})
	}
}
