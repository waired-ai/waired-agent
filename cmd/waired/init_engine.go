package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// The installers used to pre-install Ollama BEFORE `waired init` ran, so
// init's DetectOllama re-detected waired's own install as a "foreign"
// Ollama and asked the confusing bundled-vs-reuse question about it — and
// the ~GB engine download happened before the operator had answered "run
// AI models here?" at all. Now the engine decision AND the install both
// live here, inside init, right after the inference answers.

// engineInstallAction is what ensureBundledEngine should do for one
// concrete host state. Factored out of ensureBundledEngine so the
// GOOS-varying decision is table-testable on every OS (repo rule).
type engineInstallAction int

const (
	engineActionInstall         engineInstallAction = iota
	engineActionSkipPresent                         // a usable engine is already there
	engineActionSkipReuse                           // operator chose to reuse their own Ollama
	engineActionSkipOptOut                          // WAIRED_NO_OLLAMA / --skip-ollama opt-out
	engineActionSkipNotElevated                     // install needs admin/root and we have neither
)

// engineInstallDecision decides whether init should install the bundled
// engine. Callers gate on "inference enabled" before calling.
//
// Per-OS "present" semantics: Linux's bundled resolver is STRICT (only the
// state-dir binary counts — bundledPresent), while Windows/macOS bundled
// installs live at global well-known paths, so any DetectOllama hit counts.
// Elevation: Windows writes %ProgramFiles% (needs an elevated token), Linux
// writes the root-owned state dir (needs root); macOS /Applications is
// admin-group-writable, so the install is attempted and fails with a clear
// message for non-admin users.
func engineInstallDecision(
	goos string, elevated bool, det setup.OllamaDetection,
	source string, bundledPresent, optOut bool,
) engineInstallAction {
	if optOut {
		return engineActionSkipOptOut
	}
	if source == agentconfig.OllamaSourceReuse {
		return engineActionSkipReuse
	}
	switch goos {
	case "linux":
		if bundledPresent {
			return engineActionSkipPresent
		}
		if !elevated {
			return engineActionSkipNotElevated
		}
		return engineActionInstall
	case "windows":
		if det.Installed {
			return engineActionSkipPresent
		}
		if !elevated {
			return engineActionSkipNotElevated
		}
		return engineActionInstall
	default: // darwin
		if det.Installed {
			return engineActionSkipPresent
		}
		return engineActionInstall
	}
}

// bundledEnginePath is where Linux's strict bundled resolver expects the
// engine binary (cmd/waired-agent inference.go mirrors this join). Empty on
// Windows/macOS, whose "bundled" installs live at global well-known paths
// covered by DetectOllama instead.
func bundledEnginePath(goos, stateDir string) string {
	if goos != "linux" {
		return ""
	}
	return filepath.Join(stateDir, "runtimes", "ollama", "bin", "ollama")
}

// ensureBundledEngine installs the bundled Ollama engine when the operator's
// answers call for one and none is usable yet. Never fails init: the agent
// tolerates a missing engine (deploy/pull retry once one appears), so every
// failure path degrades to a warning + a copyable manual command.
func ensureBundledEngine(
	ctx context.Context, out io.Writer,
	det setup.OllamaDetection, source, stateDir string,
) {
	_ = ctx // per-OS installOllama manages its own timeout today
	bundledPresent := false
	if p := bundledEnginePath(runtime.GOOS, stateDir); p != "" {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			bundledPresent = true
		}
	}
	action := engineInstallDecision(
		runtime.GOOS, elevation.IsElevated(), det, source,
		bundledPresent, os.Getenv("WAIRED_NO_OLLAMA") != "")
	switch action {
	case engineActionInstall:
		writePromptf(out, "%s Installing the Ollama engine (one-time download)...\n", emo("📦", ">>"))
		if err := installOllama(true, stateDir); err != nil {
			writePromptf(out,
				"%s Engine install failed: %v\n  The agent retries once an engine appears; install it by hand later: %s\n",
				emo("⚠️", "!"), err, elevation.Hint("waired runtimes install ollama --yes"))
		}
	case engineActionSkipNotElevated:
		writePromptf(out,
			"%s Skipping the engine install (needs admin rights). Install it later: %s\n",
			emo("💡", "note:"), elevation.Hint("waired runtimes install ollama --yes"))
	case engineActionSkipOptOut:
		writePrompt(out, "Engine install skipped (WAIRED_NO_OLLAMA).")
	case engineActionSkipPresent, engineActionSkipReuse:
		// Nothing to do; promptOllamaSource already narrated the state.
	}
}
