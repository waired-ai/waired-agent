package main

import (
	"strings"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// The installers used to pre-install Ollama BEFORE `waired init` ran, so
// init's DetectOllama re-detected waired's own install as a "foreign"
// Ollama — and the ~GB engine download happened before the operator had
// answered "run AI models here?" at all. Now the engine decision AND the
// install both live here, inside init, right after the inference answers.

// engineInstallAction is what ensureBundledEngine should do for one
// concrete host state. Factored out of ensureBundledEngine so the
// GOOS-varying decision is table-testable on every OS (repo rule).
type engineInstallAction int

const (
	engineActionInstall         engineInstallAction = iota
	engineActionSkipPresent                         // a usable engine is already there
	engineActionSkipOptOut                          // WAIRED_NO_OLLAMA / --skip-ollama opt-out
	engineActionSkipNotElevated                     // install needs admin/root and we have neither
	engineActionRepair                              // our own bits are there but unconfigured
)

// engineInstallDecision decides whether init should install the bundled
// engine. Callers gate on "inference enabled" before calling.
//
// Per-OS "present" semantics: Linux and macOS are STRICT — only the
// state-dir binary counts (bundledPresent), because that is the only engine
// the daemon will serve with (#488). Windows still counts any DetectOllama
// hit, because its bundled install still lives at a global well-known path
// until #493 relocates it.
//
// Elevation: all three write a directory an ordinary user does not own —
// the root-owned state dir on Linux and macOS, %ProgramFiles% on Windows.
// macOS needed no elevation while it installed into the admin-group-writable
// /Applications; #492 moved it under /Library/Application Support/waired, so
// it needs root like the others.
//
// incomplete is Windows' "installed but unusable" fact: no completion marker
// beside ollama.exe, so an earlier attempt unpacked and stopped. macOS used
// to have a second one — a bundle whose code signature macOS would not
// execute — which went with the app bundle itself in #492.
func engineInstallDecision(
	goos string, elevated bool, det setup.OllamaDetection,
	bundledPresent, optOut, incomplete bool,
) engineInstallAction {
	if optOut {
		return engineActionSkipOptOut
	}
	switch goos {
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
	default: // linux, darwin
		if bundledPresent {
			return engineActionSkipPresent
		}
		if !elevated {
			return engineActionSkipNotElevated
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
// never called incomplete.
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

// bundledEnginePath is where the strict bundled resolver expects the engine
// binary. It is the same join the daemon makes, because it IS the daemon's
// join — one exported helper rather than three copies that can drift.
//
// Windows is the last OS whose "bundled" install lives somewhere else
// (%ProgramFiles%, covered by DetectOllama); #493 relocates it and this
// stops being a per-OS answer at all.
func bundledEnginePath(goos, stateDir string) string {
	if goos == "windows" {
		return ""
	}
	return infruntime.BundledOllamaBinaryPath(goos, stateDir)
}
