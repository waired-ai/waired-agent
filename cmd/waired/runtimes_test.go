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
	gotRecreate := false
	called := false
	vllmInstall = func(_ context.Context, baseDir string, recreate bool, _ func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		called = true
		gotBaseDir = baseDir
		gotRecreate = recreate
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
	// The explicit verb answers "put a clean environment here", so it
	// recreates. The converge answers "make what is here match" and does
	// not — it may be running while the host serves, and clearing the
	// environment out from under it is what destroyed a working venv on a
	// real host before #843 separated the two.
	if !gotRecreate {
		t.Error("`runtimes install vllm` did not ask for a clean environment")
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
	vllmInstall = func(context.Context, string, bool, func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
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

// Product contract (waired-agent#319): `waired runtimes install --auto` must
// not offer to install vLLM on a host that cannot serve it. The CLI used to
// carry its own copy of the auto-pick rule with no OS term, so a Windows host
// with a large NVIDIA card was told to install a Linux-only engine. The rule
// now lives once, in router.VLLMAutoEligible.
func TestRecommendEngineFor(t *testing.T) {
	big := []recommendGPU{{Vendor: "nvidia", VRAMTotalMB: 24467}}
	cases := []struct {
		name string
		goos string
		gpus []recommendGPU
		want string
	}{
		{"linux big nvidia", "linux", big, "vllm"},
		{"windows big nvidia", "windows", big, "ollama"},
		{"darwin big nvidia", "darwin", big, "ollama"},
		{"linux small nvidia", "linux", []recommendGPU{{Vendor: "nvidia", VRAMTotalMB: 4096}}, "ollama"},
		{"linux amd", "linux", []recommendGPU{{Vendor: "amd", VRAMTotalMB: 64000}}, "ollama"},
		{"linux no gpu", "linux", nil, "ollama"},
		{
			"linux second gpu qualifies",
			"linux",
			[]recommendGPU{{Vendor: "amd", VRAMTotalMB: 64000}, {Vendor: "nvidia", VRAMTotalMB: 24467}},
			"vllm",
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
