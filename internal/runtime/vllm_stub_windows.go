//go:build windows

// This file provides stubs for the vLLM-related types and entry points
// that are excluded from the Windows build (vllm.go / vllm_install.go /
// uv.go are `//go:build linux` because vLLM is Linux/Python+CUDA only).
// cmd/waired and cmd/waired-agent still reference these symbols at
// compile time; the stubs let those binaries compile on Windows and
// surface "vLLM is not supported on Windows" errors at runtime.
//
// Decision: docs/decisions.md "Windows 版 waired-agent の方針" (20260514).

package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrVLLMUnsupportedOnWindows is returned by every vLLM operation that
// callers may invoke from cmd/waired on Windows. Use errors.Is for
// comparison; do not check the message text.
var ErrVLLMUnsupportedOnWindows = errors.New("runtime: vLLM is not supported on Windows; use Ollama instead")

// VLLMPinnedVersion mirrors the Unix constant so CLI help text and
// confirmation prompts in cmd/waired can render the version even on
// Windows (where Install will refuse to proceed).
// renovate: datasource=pypi depName=vllm
const VLLMPinnedVersion = "0.24.0"

// InstallStage / InstallProgress / InstallResult / InstallOpts are the
// type signatures cmd/waired's installVLLM driver expects. Fields
// match the Unix definitions byte-for-byte so the renderer code is
// identical across platforms.

type InstallStage string

const (
	StageResolveUV  InstallStage = "resolve-uv"
	StageCreateVenv InstallStage = "create-venv"
	StagePipInstall InstallStage = "pip-install"
	StageVerify     InstallStage = "verify"
	StageActivate   InstallStage = "activate"
)

type InstallProgress struct {
	Stage   InstallStage
	Step    int
	Total   int
	Percent int
	Message string
}

type InstallResult struct {
	Version     string
	VenvPath    string
	BinDir      string
	InstalledAt time.Time
}

type InstallOpts struct {
	Version           string
	HFTransferVersion string
	PythonVersion     string
	KeepFailed        bool
	ExtraPipPackages  []string
}

// VLLMInstaller is a no-op stub on Windows.
type VLLMInstaller struct{}

// NewVLLMInstallerAt mirrors the Linux constructor's signature so the
// cross-platform CLI / agent code compiles on Windows; baseDir is ignored
// because every method is a no-op stub here.
func NewVLLMInstallerAt(string) *VLLMInstaller { return &VLLMInstaller{} }

// NewVLLMInstaller returns a stub installer whose every method returns
// ErrVLLMUnsupportedOnWindows or its equivalent zero-value+false.
func NewVLLMInstaller() *VLLMInstaller { return &VLLMInstaller{} }

// Active always reports "no active install" so engineViable's vllm
// branch in cmd/waired-agent returns false and the auto-picker
// silently falls through to Ollama.
func (*VLLMInstaller) Active() (InstallResult, bool) { return InstallResult{}, false }

// Install refuses with ErrVLLMUnsupportedOnWindows.
func (*VLLMInstaller) Install(_ context.Context, _ InstallOpts, _ func(InstallProgress)) (InstallResult, error) {
	return InstallResult{}, ErrVLLMUnsupportedOnWindows
}

// Uninstall refuses with ErrVLLMUnsupportedOnWindows.
func (*VLLMInstaller) Uninstall(_ context.Context, _ string) error {
	return ErrVLLMUnsupportedOnWindows
}
