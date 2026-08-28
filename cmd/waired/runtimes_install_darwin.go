//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// installOllamaBundled is a seam so tests exercise installOllama's
// confirm/orchestration without downloading a real release.
var installOllamaBundled = installOllamaBundledImpl

// installOllama (macOS) installs waired's bundled Ollama: download the
// pinned official release into <state-dir>/runtimes/ollama and let
// waired-agent supervise it as a foreground child. It is the only Ollama
// the agent will serve with (#489, #492).
//
// Until #492 this installed the official Ollama.app into /Applications
// instead. That location was never really waired's: it collides with the
// user's own install, it is a Gatekeeper-sealed bundle (writing waired's
// ownership marker into it is what made macOS refuse to run the engine at
// all — #329/#330), it self-updates away from whatever version we tested
// against, and it was not pinned in the first place — the download URL
// said `releases/latest`. Under the state dir there is no bundle to seal,
// no shared location to collide over, and one pinned version.
//
// An Ollama.app a previous waired left in /Applications is deliberately
// NOT touched: it cannot be reliably attributed to us (the in-bundle
// marker had to go with #329), so removing it is the user's call. The
// uninstall page says how.
//
// sink, when non-nil, receives the same progress events the terminal
// renderer draws — that is how the browser wizard gets the download it
// used to have no view of (waired-agent#197). nil for every caller that
// is not the setup executor.
func installOllama(yes bool, stateDir string, sink func(infruntime.OllamaInstallProgress)) error {
	baseDir := infruntime.BundledOllamaDir(stateDir)
	if !yes && !confirmTTY(fmt.Sprintf("Install waired's bundled Ollama %s into %s ?", infruntime.OllamaPinnedVersion, baseDir)) {
		return errors.New("aborted by user")
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
	fmt.Fprintln(stdout, "Ollama installed. waired-agent will adopt it on the next engine start.")
	return nil
}

func installOllamaBundledImpl(ctx context.Context, baseDir string, sink func(infruntime.OllamaInstallProgress)) error {
	// No GPUVendor: macOS has no ROCm overlay to fetch. Metal and the MLX
	// backend ship inside the release binary.
	inst := infruntime.NewOllamaInstaller(baseDir)
	// Renderer shared with the Linux flow: runtimes_install_render.go.
	// The terminal bar and the daemon sink are peers — teeOllamaProgress
	// keeps the former even when the latter is absent.
	if err := inst.Install(ctx, teeOllamaProgress(
		newOllamaInstallRenderer(stdout, isTerminal(os.Stdout), "Ollama "+infruntime.OllamaPinnedVersion),
		sink,
	)); err != nil {
		return err
	}
	clearQuarantine(ctx, filepath.Join(baseDir, "bin"))
	return nil
}

// clearQuarantine strips com.apple.quarantine from the freshly extracted
// engine. Belt and braces: the attribute is applied by LaunchServices, not
// by the kernel, so neither a Go HTTP download nor tar sets one and this is
// a no-op on the current path — it exists so a future change of download
// route cannot reintroduce the "Ollama is damaged" class through a
// different door. Best-effort by design: failing to remove an xattr that is
// probably not there must never fail an otherwise good install.
func clearQuarantine(ctx context.Context, dir string) {
	if err := runDarwinCmd(ctx, "xattr", "-dr", "com.apple.quarantine", dir); err != nil {
		fmt.Fprintf(stderr, "warn: could not clear the quarantine xattr on %s: %v\n", dir, err)
	}
}

func runDarwinCmd(ctx context.Context, name string, args ...string) error {
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}
