//go:build darwin

// This file provides stubs for the vLLM-related types and entry points
// that are excluded from the macOS build (vllm.go / vllm_install.go /
// uv.go are `//go:build linux` because the pinned vLLM wheel is
// Linux/CUDA only and there is no working Apple Silicon path in
// upstream). cmd/waired and cmd/waired-agent still reference these
// symbols at compile time; the stubs let those binaries compile on
// darwin and surface "vLLM is not supported on macOS" errors at
// runtime. Apple Silicon hosts run inference through the Ollama Metal
// backend via the engine_picker apple branch; the MLX-LM runtime
// adapter is tracked separately in docs/todo.md.
//
// The struct / method shapes here match vllm_stub_windows.go and the
// real Linux build byte-for-byte so the renderer code in
// cmd/waired/runtimes.go is identical across platforms.
//
// Decision: waired/docs/decisions/ "Mac 版 waired-agent の推論エンジン方針"
// (20260517).

package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrVLLMUnsupportedOnDarwin is returned by every vLLM operation that
// callers may invoke from cmd/waired on macOS. Use errors.Is for
// comparison; do not check the message text.
var ErrVLLMUnsupportedOnDarwin = errors.New("runtime: vLLM is not supported on macOS; use Ollama (Metal) instead")

// The pin set (VLLMPinnedVersion and friends) used to be mirrored here
// so CLI help text and confirmation prompts could render the version on
// macOS too. It lives untagged in vllm_pins.go now — one copy that
// cannot disagree with itself, which the converge needs anyway (#843).

// InstallStage / InstallProgress / InstallResult / InstallOpts are the
// type signatures cmd/waired's installVLLM driver expects. Fields
// match the Linux definitions byte-for-byte so the renderer code is
// identical across platforms.

type InstallStage string

const (
	StageResolveUV  InstallStage = "resolve-uv"
	StageCreateVenv InstallStage = "create-venv"
	StagePipInstall InstallStage = "pip-install"
	StageToolchain  InstallStage = "host-toolchain"
	StageVerify     InstallStage = "verify"
	StageActivate   InstallStage = "activate"
)

type InstallProgress struct {
	Stage   InstallStage
	Step    int
	Total   int
	Percent int
	Message string

	CompletedBytes int64
	TotalBytes     int64
	BytesPerSec    int64
}

type InstallResult struct {
	Version     string
	VenvPath    string
	BinDir      string
	InstalledAt time.Time
	// Always empty here: nothing installs a vLLM venv off Linux, so
	// there is no host toolchain to have an opinion about
	// (waired-agent#898). Declared so cmd/waired compiles on every OS.
	Advisories []string
}

type InstallOpts struct {
	Version           string
	HFTransferVersion string
	PythonVersion     string
	KeepFailed        bool
	ExtraPipPackages  []string
	// Recreate mirrors the Linux field so cmd/waired compiles here; the
	// stub installer never builds anything either way.
	Recreate bool
}

// VLLMInstaller is a no-op stub on macOS.
type VLLMInstaller struct{}

// NewVLLMInstallerAt mirrors the Linux constructor's signature so the
// cross-platform CLI / agent code compiles on macOS; baseDir is ignored
// because every method is a no-op stub here.
func NewVLLMInstallerAt(string) *VLLMInstaller { return &VLLMInstaller{} }

// NewVLLMInstaller returns a stub installer whose every method returns
// ErrVLLMUnsupportedOnDarwin or its equivalent zero-value+false.
func NewVLLMInstaller() *VLLMInstaller { return &VLLMInstaller{} }

// Active always reports "no active install" so engineViable's vllm
// branch in cmd/waired-agent returns false and the auto-picker
// silently falls through to Ollama (Metal).
func (*VLLMInstaller) Active() (InstallResult, bool) { return InstallResult{}, false }

// Install refuses with ErrVLLMUnsupportedOnDarwin.
func (*VLLMInstaller) Install(_ context.Context, _ InstallOpts, _ func(InstallProgress)) (InstallResult, error) {
	return InstallResult{}, ErrVLLMUnsupportedOnDarwin
}

// Uninstall refuses with ErrVLLMUnsupportedOnDarwin.
func (*VLLMInstaller) Uninstall(_ context.Context, _ string) error {
	return ErrVLLMUnsupportedOnDarwin
}

// ActivePins reports no record, which — with Active saying "nothing
// installed" — is what makes DecideVLLMConverge answer "no venv here"
// on macOS without a build tag at the call site (#843).
func (*VLLMInstaller) ActivePins() (VLLMPinSet, bool) { return VLLMPinSet{}, false }

// PruneOtherVersions refuses with ErrVLLMUnsupportedOnDarwin: there is
// never an install to prune around.
func (*VLLMInstaller) PruneOtherVersions() ([]string, error) {
	return nil, ErrVLLMUnsupportedOnDarwin
}
