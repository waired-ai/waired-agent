package main

import (
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
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
)

// engineInstallDecision decides whether init should install the bundled
// engine. Callers gate on "inference enabled" before calling.
//
// It has no per-OS branch left, which is the whole point of #488's Phase 2.
// "Present" is the waired-managed binary under the state dir, everywhere;
// installing writes a directory an ordinary user does not own, everywhere,
// so it needs root or an elevated token. The asymmetry this replaces was
// load-bearing for as long as the Windows and macOS engines lived at global
// well-known paths: there, "any DetectOllama hit counts" was the only
// workable rule, and it is precisely what let a user's own unpinned Ollama
// satisfy setup and then be served through (#139).
//
// It still takes a goos so the caller's OS stays visible at the call site
// and so the table test reads as a cross-OS parity check rather than one
// case repeated.
func engineInstallDecision(goos string, elevated bool, bundledPresent, optOut bool) engineInstallAction {
	_ = goos
	switch {
	case optOut:
		return engineActionSkipOptOut
	case bundledPresent:
		return engineActionSkipPresent
	case !elevated:
		return engineActionSkipNotElevated
	default:
		return engineActionInstall
	}
}

// bundledEnginePath is where the strict bundled resolver expects the engine
// binary. It is the same join the daemon makes, because it IS the daemon's
// join — one exported helper rather than three copies that can drift (#179
// was two of them disagreeing).
func bundledEnginePath(goos, stateDir string) string {
	return infruntime.BundledOllamaBinaryPath(goos, stateDir)
}
