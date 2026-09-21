package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
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
// What it does NOT do is bounce a running engine, and it never has to:
// nothing waired started runs from what it replaces. For ollama that is
// because the engine's first start waits for this converge
// (engineStartGate). Replacing bin/ and lib/ under a running `ollama
// serve` was NOT safe, which is what this used to say: the server launches
// its runners from its own directory on every model load, so a load
// mid-swap found lib/ half gone and one after it ran new runners under an
// old server; on Windows the locked ollama.exe failed the swap after lib/
// was already deleted (waired-ai/waired-agent#1511). Nothing is serving at
// daemon start, so holding the first start costs one `--version` on a
// host already at the pin and the download on one that is not — an
// engine off the pin was not going to serve what this build claims
// anyway. vLLM builds its new venv in a directory of its own and swaps a
// symlink, so the venv a running engine was started from is neither
// edited nor removed; the daemon reclaims it once nothing uses it
// (vllmVenvKeeper, waired-agent#1431).
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
//
// It returns a channel closed once the ollama pass is over, however it
// ended; the ollama adapter's StartGate waits on it (engineStartGate).
// wantROCmOverlay is setup.OllamaROCmOverlayWanted for this host — the
// answer the CLI's install gives too (#1511).
//
// After the vLLM pass it reclaims the venvs nothing uses. That covers a
// host whose vLLM venv is not the engine it runs, which no engine start
// would otherwise reclaim for; a venv the engine is running from is kept.
func startEngineConverge(logger *slog.Logger, stateDir string, wantROCmOverlay bool, vllmVenvs *vllmVenvKeeper) <-chan struct{} {
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
		wantROCmOverlay,
	)
	ollamaDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), engineConvergeTimeout)
		defer cancel()
		convergeBundledEngine(ctx, logger, deps)
		close(ollamaDone)
		convergeVLLMVenv(ctx, logger, vllmDeps)
		vllmVenvs.reclaim(ctx, logger)
	}()
	return ollamaDone
}

// engineStartGate is the ollama adapter's StartGate: it returns once the
// start-up converge's ollama pass is over, or with the start's own context
// error when a Stop or Park ends the wait first. It says so once, the first
// time a start actually has to wait.
func engineStartGate(done <-chan struct{}, logger *slog.Logger) func(context.Context) error {
	var once sync.Once
	return func(ctx context.Context) error {
		select {
		case <-done:
			return nil
		default:
		}
		once.Do(func() {
			logger.Info("engine start is waiting for the bundled engine update to finish")
		})
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
