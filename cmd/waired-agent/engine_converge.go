package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// Daemon-side backstop for #826 (bundled Ollama) and #843 (the vLLM
// venv): bring an already-installed engine onto this build's pin at
// start.
//
// The installer scripts do the same thing on the path a person takes
// (`waired update`, and the tray, which runs `waired update --yes` on all
// three OSes). This exists for the paths that never reach them — on Linux
// `apt upgrade waired` is the ordinary one, and it is the reason the APT
// repo exists: the .deb postinst restarts the agent and knows nothing
// about the engine.
//
// What it does NOT do is bounce a running engine. Replacing the binary on
// disk is safe while the old one runs — it holds its own inode — and the
// adapter's BinaryResolver is consulted on each EnsureRunning, so the new
// one is picked up at the next engine start (ollama.go's own comment says
// so). The path where the user is waiting restarts the service anyway, so
// it converges immediately there; here the choice is between a lagging
// converge and an unannounced restart of an engine that may be mid-answer,
// and the lag is the smaller harm. vLLM reaches the same place by a
// different route: its install builds a new versioned venv and swaps a
// symlink at the end, so the venv in use is never edited at all.
//
// engineConvergeTimeout is a backstop, not the working bound: the download
// itself is bounded by download.Fetch's no-progress watchdog (#189), the
// same as every other engine install.
const engineConvergeTimeout = 2 * time.Hour

// convergeBundledEngine runs the converge and logs what it decided.
// Never returns an error: this is a background repair, and a host that
// cannot reach GitHub right now must still finish starting.
func convergeBundledEngine(ctx context.Context, logger *slog.Logger, deps infruntime.OllamaConvergeDeps) {
	decision, err := infruntime.ConvergeOllama(ctx, deps)
	switch {
	case err != nil:
		// Warn, not error: the product already prints the mismatch in
		// `waired status` and `waired models ls --detail`, so the user is
		// not left without a signal — they are left with the one they had
		// before this existed.
		logger.Warn("bundled engine converge failed; the pinned version is still not installed",
			"reason", decision.Reason, "pin", infruntime.OllamaPinnedVersion, "err", err)
	case decision.Install:
		logger.Info("bundled engine converged to the pin; it takes effect at the next engine start",
			"reason", decision.Reason, "pin", infruntime.OllamaPinnedVersion)
	default:
		logger.Debug("bundled engine needs no converge", "reason", decision.Reason)
	}
}

// convergeVLLMVenv runs the vLLM converge and logs what it decided
// (#843). Never returns an error, for the same reason as the Ollama one.
//
// The three outcomes are logged apart on purpose. "Blocked" is a host
// that needs the rebuild and cannot have it — today only for want of
// disk — and it must not be filed under the Debug line that means
// "nothing to do", because nothing will change until somebody frees
// space.
func convergeVLLMVenv(ctx context.Context, logger *slog.Logger, deps infruntime.VLLMConvergeDeps) {
	decision, err := infruntime.ConvergeVLLM(ctx, deps)
	switch {
	case err != nil:
		logger.Warn("vLLM converge failed; the venv is still not at the pinned set",
			"reason", decision.Reason, "pin", infruntime.VLLMPinnedVersion, "err", err)
	case decision.Blocked:
		logger.Warn("vLLM venv needs a rebuild but it cannot run now",
			"reason", decision.Reason, "pin", infruntime.VLLMPinnedVersion)
	case decision.Install:
		logger.Info("vLLM venv converged to the pin; it takes effect at the next engine start",
			"reason", decision.Reason, "pin", infruntime.VLLMPinnedVersion)
	default:
		logger.Debug("vLLM venv needs no converge", "reason", decision.Reason)
	}
	if len(decision.Pruned) > 0 {
		logger.Info("removed the superseded vLLM venv(s)", "versions", decision.Pruned)
	}
	if decision.PruneErr != nil {
		logger.Warn("could not remove a superseded vLLM venv; it is unused but still on disk",
			"err", decision.PruneErr)
	}
}

// startEngineConverge kicks the converge off in the background, once per
// process. Off the startup path on purpose: a converge is a ~1.4 GB
// download (~6 GB for vLLM), and local inference must not wait on it — a
// host whose engine already matches the pin is serving in the meantime,
// and one whose engine does not was not going to serve anyway.
//
// The two engines converge in sequence inside the one goroutine rather
// than concurrently: both are multi-GB fetches, and a host that has both
// installed should not have them compete for its uplink while it serves.
// vLLM second because it is the larger and the rarer — off Linux, and on
// any host without a venv, its whole pass is one symlink read.
func startEngineConverge(logger *slog.Logger, stateDir string) {
	vllmBase := filepath.Join(stateDir, "runtimes", "vllm")
	vllmDeps := infruntime.NewVLLMConvergeDeps(vllmBase, func() int64 {
		free, err := hardware.FreeDiskBytes(vllmBase)
		if err != nil {
			// Unknown, not zero: DecideVLLMConverge treats 0 as "no
			// reading" and proceeds, which is right — a statfs that
			// failed is not evidence of a full disk.
			return 0
		}
		return free
	})
	deps := infruntime.NewOllamaConvergeDeps(
		infruntime.BundledOllamaDir(stateDir),
		// The same resolution the daemon uses for the engine it spawns
		// and for the version it warns about, so all three agree on which
		// binary "the engine" means (#238).
		func(ctx context.Context, _ string) (bool, string) {
			return engineVersionOnHost(runtime.GOOS, stateDir, hardware.EngineVersionAt)(ctx, "ollama")
		},
	)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), engineConvergeTimeout)
		defer cancel()
		convergeBundledEngine(ctx, logger, deps)
		convergeVLLMVenv(ctx, logger, vllmDeps)
	}()
}
