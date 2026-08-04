//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// installOllamaBundled is a seam so tests exercise installOllama's
// confirm/orchestration without downloading a real release.
var installOllamaBundled = installOllamaBundledImpl

// installOllama (Linux) installs waired's bundled Ollama: download the
// pinned official release tarball into <state-dir>/runtimes/ollama and
// let waired-agent supervise it as a foreground child. No system
// service, no systemctl — that is the whole point of the bundle model
// (#188). It is the only Ollama the agent will serve with (#489).
//
// sink, when non-nil, receives the same progress events the terminal
// renderer draws — that is how the browser wizard gets the download it
// used to have no view of (waired-agent#197). nil for every caller that
// is not the setup executor.
func installOllama(yes bool, stateDir string, sink func(infruntime.OllamaInstallProgress)) error {
	baseDir := filepath.Join(stateDir, "runtimes", "ollama")
	if !yes && !confirmTTY(fmt.Sprintf("Install waired's bundled Ollama %s into %s ?", infruntime.OllamaPinnedVersion, baseDir)) {
		return errors.New("aborted by user")
	}
	// A backstop, not the working bound: the download itself is bounded by
	// download.Fetch's no-progress watchdog (#189).
	budget := ollamaInstallTimeout(os.Getenv)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	fmt.Printf("Installing bundled Ollama %s (downloading the official release)...\n", infruntime.OllamaPinnedVersion)
	if err := installOllamaBundled(ctx, baseDir, sink); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf(
				"ollama install: timed out after %s (raise it with %s, e.g. %s=3h): %w",
				budget, ollamaInstallTimeoutEnv, ollamaInstallTimeoutEnv, err)
		}
		return fmt.Errorf("ollama install: %w", err)
	}
	// The engine was just extracted under sudo (root-owned); hand the state
	// dir back to the waired-agent service user so the daemon can exec and
	// manage it — otherwise the bundled ollama dies with exit status 1 (#484).
	handStateToServiceUser(stateDir)
	fmt.Println("Ollama installed. waired-agent will adopt it on the next engine start.")
	return nil
}

func installOllamaBundledImpl(ctx context.Context, baseDir string, sink func(infruntime.OllamaInstallProgress)) error {
	inst := infruntime.NewOllamaInstaller(baseDir)
	inst.GPUVendor = detectOllamaGPUVendor(ctx)
	// Renderer shared with the darwin flow: runtimes_install_render.go.
	// The terminal bar and the daemon sink are peers — teeOllamaProgress
	// keeps the former even when the latter is absent.
	return inst.Install(ctx, teeOllamaProgress(
		newOllamaInstallRenderer(os.Stdout, isTerminal(os.Stdout), "Ollama "+infruntime.OllamaPinnedVersion),
		sink,
	))
}

// detectOllamaGPUVendor returns "amd" when an AMD GPU is present so the
// installer overlays the ROCm runtime; "" otherwise (CUDA+CPU base).
func detectOllamaGPUVendor(ctx context.Context) string {
	prof := hardware.NewProfiler("").Profile(ctx)
	for _, g := range prof.GPUs {
		if strings.EqualFold(g.Vendor, "amd") {
			return "amd"
		}
	}
	return ""
}
