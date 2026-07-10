//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// (#188). Reuse of an existing/user-run Ollama is selected at
// `waired init` instead, not here.
func installOllama(yes bool, stateDir string) error {
	baseDir := filepath.Join(stateDir, "runtimes", "ollama")
	if !yes && !confirmTTY(fmt.Sprintf("Install waired's bundled Ollama %s into %s ?", infruntime.OllamaPinnedVersion, baseDir)) {
		return errors.New("aborted by user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	fmt.Printf("Installing bundled Ollama %s (downloading the official release)...\n", infruntime.OllamaPinnedVersion)
	if err := installOllamaBundled(ctx, baseDir); err != nil {
		return fmt.Errorf("ollama install: %w", err)
	}
	// The engine was just extracted under sudo (root-owned); hand the state
	// dir back to the waired-agent service user so the daemon can exec and
	// manage it — otherwise the bundled ollama dies with exit status 1 (#484).
	handStateToServiceUser(stateDir)
	fmt.Println("Ollama installed. waired-agent will adopt it on the next engine start.")
	return nil
}

func installOllamaBundledImpl(ctx context.Context, baseDir string) error {
	inst := infruntime.NewOllamaInstaller(baseDir)
	inst.GPUVendor = detectOllamaGPUVendor(ctx)
	// Renderer shared with the darwin flow: runtimes_install_render.go.
	return inst.Install(ctx, newOllamaInstallRenderer(os.Stdout, isTerminal(os.Stdout), "Ollama "+infruntime.OllamaPinnedVersion))
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
