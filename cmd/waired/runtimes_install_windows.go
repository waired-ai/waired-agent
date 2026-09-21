//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// installOllamaBundled is a seam so tests exercise installOllama's
// confirm/orchestration without downloading a real release.
var installOllamaBundled = installOllamaBundledImpl

// installOllama (Windows) installs waired's bundled Ollama: download the
// pinned official release into <state-dir>\runtimes\ollama and let
// waired-agent supervise it as a foreground child. It is the only Ollama
// the agent will serve with (#489, #493).
//
// It needs an elevated token: %ProgramData%\waired is locked down to
// administrators by the service install. The tray invokes the CLI elevated
// via UAC RunAs, and a bare invocation should be run from an elevated
// prompt.
//
// Until #493 this wrote the embedded ollama-windows.ps1 to a temp file and
// ran it under Windows PowerShell 5.1, installing into %ProgramFiles%\Ollama
// with a machine PATH entry, machine-scope GPU environment variables and a
// marker file to tell waired's install from the user's own. All of that
// existed because the install was global. Under the state dir the location
// IS the identity, the agent supplies the GPU environment at spawn, and the
// version pin stops being a literal kept in sync with a Go const by comment
// (#43).
//
// sink, when non-nil, receives the same progress events the terminal
// renderer draws — that is how the browser wizard gets the download it
// used to have no view of (waired-agent#197). nil for every caller that
// is not the setup executor.
func installOllama(yes bool, stateDir string, sink func(infruntime.OllamaInstallProgress)) error {
	baseDir := infruntime.BundledOllamaDir(stateDir)
	if !yes && !confirmTTY(fmt.Sprintf("Install Waired's bundled Ollama %s into %s?", infruntime.OllamaPinnedVersion, baseDir)) {
		return errors.New("aborted, no changes made")
	}
	// A backstop, not the working bound: the download itself is bounded by
	// download.Fetch's no-progress watchdog (#189).
	budget := ollamaInstallTimeout(os.Getenv)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	fmt.Fprintf(stdout, "Installing bundled Ollama %s (downloading the official release)...\n", infruntime.OllamaPinnedVersion)
	if err := installOllamaBundled(ctx, baseDir, sink); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf(
				"ollama install: timed out after %s (raise it with %s, e.g. %s=3h): %w",
				budget, ollamaInstallTimeoutEnv, ollamaInstallTimeoutEnv, err)
		}
		return fmt.Errorf("ollama install: %w", err)
	}
	sweepLegacyOllamaInstall(os.Getenv, stdout)
	fmt.Fprintln(stdout, "Ollama installed. The background service picks it up on the next engine start.")
	return nil
}

func installOllamaBundledImpl(ctx context.Context, baseDir string, sink func(infruntime.OllamaInstallProgress)) error {
	inst := infruntime.NewOllamaInstaller(baseDir)
	inst.WantROCmOverlay = wantROCmOverlay(ctx, os.Getenv)
	// Renderer shared with the Linux and macOS flows:
	// runtimes_install_render.go. The terminal bar and the daemon sink are
	// peers — teeOllamaProgress keeps the former even when the latter is
	// absent.
	return inst.Install(ctx, teeOllamaProgress(
		newOllamaInstallRenderer(stdout, isTerminal(os.Stdout), "Ollama "+infruntime.OllamaPinnedVersion),
		sink,
	))
}

// wantROCmOverlay decides whether to fetch the ~250 MB AMD ROCm overlay.
//
// The Windows base archive ships CUDA, Vulkan and CPU; ROCm is separate.
// The answer is infruntime.WantsROCmOverlay over the same inputs the
// daemon builds its plan from (setup.OllamaBackendInputs): fetch it for
// an AMD GPU in use and let the engine decide what the overlay serves,
// except on a Strix Halo, whose measured arm names Vulkan (#1492).
//
// WAIRED_OLLAMA_GPU_MODE (install.ps1's -OllamaGpuMode) still forces the
// answer, with the five values it has always taken. Only 'rocm' asks for
// the overlay; 'vulkan', 'cuda-only' and 'cpu-only' are all served by the
// base archive, which is why the old script downloaded nothing extra for
// them either.
func wantROCmOverlay(ctx context.Context, getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("WAIRED_OLLAMA_GPU_MODE"))) {
	case "rocm":
		return true
	case "vulkan", "cuda-only", "cpu-only":
		return false
	}
	return infruntime.WantsROCmOverlay(
		setup.OllamaBackendInputs("windows", hardware.NewProfiler("").Profile(ctx)))
}
