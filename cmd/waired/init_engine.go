package main

import (
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
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
	engineActionRepair                              // our own bits are there but unconfigured
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
//
// incomplete and signatureBroken are the two "installed but unusable" facts,
// and they are deliberately separate because their polarity is opposite.
// Windows' incomplete means "no completion marker beside ollama.exe, so an
// earlier attempt unpacked and stopped". macOS' signatureBroken means "macOS
// will not execute this bundle" — and a broken bundle is usually one waired
// itself installed, so it is marked as ours. Folding them into one predicate
// would make each wrong on the other OS.
func engineInstallDecision(
	goos string, elevated bool, det setup.OllamaDetection,
	source string, bundledPresent, optOut, incomplete, signatureBroken bool,
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
		if det.Installed && !incomplete {
			return engineActionSkipPresent
		}
		if !elevated {
			return engineActionSkipNotElevated
		}
		if incomplete {
			return engineActionRepair
		}
		return engineActionInstall
	default: // darwin
		// A bundle macOS refuses to run is not "present" in any useful sense.
		// Repair first: the cheap fix (drop the seal-breaking marker #329 left)
		// usually makes it valid again without downloading 560 MB.
		if det.Installed && signatureBroken {
			return engineActionRepair
		}
		if det.Installed {
			return engineActionSkipPresent
		}
		return engineActionInstall
	}
}

// engineIncomplete reports whether det points at an install this installer
// started and never finished.
//
// scripts/install/ollama-windows.ps1 writes the waired-managed marker as its
// LAST step, only once the binary, the machine PATH entry and the GPU env
// vars are all in place — so bits sitting in waired's own install directory
// with no marker mean "extracted, never configured". That is exactly the
// state a failed signature check used to leave behind, and because the
// Windows branch above keyed on det.Installed alone, every later run skipped
// the install and the host stayed broken forever (#190).
//
// Only waired's own directory counts. A user's own Ollama — the per-user
// OllamaSetup.exe layout under %LOCALAPPDATA%, or anything on PATH — is
// never called incomplete, and an operator who explicitly chose to reuse
// their own engine short-circuits above this anyway.
//
// Windows-only, and pure so the table test runs on every OS: it compares
// Windows paths textually rather than through filepath, whose separator
// would differ on the Linux CI runner.
func engineIncomplete(goos string, det setup.OllamaDetection, programFiles string) bool {
	if goos != "windows" || !det.Installed || det.WairedManaged || programFiles == "" {
		return false
	}
	return normalizeWindowsPath(det.Path) ==
		normalizeWindowsPath(programFiles+`\Ollama\ollama.exe`)
}

// normalizeWindowsPath lower-cases and unifies separators so two spellings
// of the same Windows path compare equal. Windows paths are
// case-insensitive and accept either separator.
func normalizeWindowsPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, `/`, `\`))
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
