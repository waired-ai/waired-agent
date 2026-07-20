package main

import (
	"context"
	"io"
	"os"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// setupInstallEngine is the install seam so the executor path is
// table-testable without downloading a ~GB engine. It is the same
// per-OS installOllama the interactive path uses (waired#835 §11.1
// requires reuse, not a second installer).
var setupInstallEngine = installOllama

// setupDetectEngine is the detection seam, for the same reason.
var setupDetectEngine = setup.DetectOllama

// setupHandState is the ownership-handoff seam. The real one shells out
// to chown and self-guards on euid 0 + an installed service, which a
// test running as root on a developer box would actually satisfy.
var setupHandState = handStateToServiceUser

// runSetupEngineInstall performs the engine install the browser wizard
// asked for, as the elevated executor holding the lease.
//
// This is the daemon-path counterpart of ensureBundledEngine
// (init_engine.go): on the daemon path `waired init` returns early at
// main.go's runInitViaDaemon branch and never reaches the standalone
// engine block, so without this the wizard's first step could only ever
// report permission_denied. The decision itself goes through the SAME
// engineInstallDecision as interactive init, so opt-out, already-present,
// reuse and not-elevated all resolve identically (§11.1).
//
// It never returns an error: the outcome is reported to the daemon,
// which is what NAVI renders. Like ensureBundledEngine, a failure here
// must not fail login.
func runSetupEngineInstall(ctx context.Context, s *executorSession, out io.Writer) {
	setupEngineInstall(ctx, s, out, runtime.GOOS, elevation.IsElevated())
}

// setupEngineInstall is runSetupEngineInstall with the two host facts
// that vary by OS passed in, so the whole decision tree is table-testable
// on every OS from an unprivileged CI runner (repo rule: route
// GOOS-varying decisions through a function taking runtime.GOOS).
func setupEngineInstall(ctx context.Context, s *executorSession, out io.Writer, goos string, elevated bool) {
	if !s.Supported() {
		return
	}
	st := s.State()
	if !st.Active || st.DesiredEngine == "" || st.EngineInstalled {
		return
	}
	// vllm has its own installer with a different shape (a venv, not a
	// tarball) and no wizard offers it yet; leave it to the daemon's
	// existing reporting rather than half-supporting it here.
	if st.DesiredEngine != "ollama" {
		return
	}
	// A live lease already claimed this install. The claim is bound to
	// the lease (§11.1), so a stale one cannot be here — whoever holds
	// it is alive and working.
	if st.InstallClaimed != "" {
		return
	}
	// The daemon could not tell us where to install. Guessing would risk
	// installing somewhere this daemon never looks, which presents to the
	// operator as an install that "worked" and a step that never turns
	// green.
	if st.StateDir == "" {
		s.Failed(st.DesiredEngine, "the background service did not report where to install the engine")
		return
	}

	installEngineAsExecutor(ctx, s, out, goos, elevated,
		st.DesiredEngine, st.StateDir, engineInstallNarrationWizard)
}

// Narration for the two entry points. The install itself is identical;
// only the reason we are doing it differs, and saying the wrong reason
// is confusing on a terminal-only install where no browser is involved.
const (
	engineInstallNarrationWizard = "Installing the AI engine for the setup in your browser (one-time download)..."
	engineInstallNarrationLocal  = "Installing the AI engine (one-time download)..."
)

// installEngineAsExecutor is the shared install core: claim the lease,
// run the same decision the interactive path runs, install, hand the
// state dir back, report the outcome. Both entry points reach it.
func installEngineAsExecutor(
	ctx context.Context, s *executorSession, out io.Writer,
	goos string, elevated bool, engine, stateDir, narration string,
) {
	claimed := s.Installing(engine)
	if claimed.InstallClaimed != "" && claimed.InstallClaimed != engine {
		// Another executor got there first with a different engine.
		return
	}

	bundledPresent := false
	if p := bundledEnginePath(goos, stateDir); p != "" {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			bundledPresent = true
		}
	}
	// OllamaSourceBundled, not the interactive prompt's answer: there is
	// no terminal question on this path, and we only get here when the
	// daemon reports no engine installed at all — so there is nothing to
	// reuse. A host that already has one never reaches this line.
	action := engineInstallDecision(
		goos, elevated, setupDetectEngine(ctx),
		agentconfig.OllamaSourceBundled, bundledPresent,
		os.Getenv("WAIRED_NO_OLLAMA") != "")

	switch action {
	case engineActionInstall:
		writePromptf(out, "%s %s\n", emo("📦", ">>"), narration)
		if err := setupInstallEngine(true, stateDir); err != nil {
			writePromptf(out, "%s Engine install failed: %v\n", emo("⚠️", "!"), err)
			s.Failed(engine, err.Error())
			return
		}
		// The tarball was extracted as root; hand the state dir back or
		// the unprivileged daemon cannot read what we just installed
		// (Linux only, no-op elsewhere).
		setupHandState(stateDir)
		writePromptf(out, "%s AI engine installed.\n", emo("✅", "*"))
		s.Done(engine)

	case engineActionSkipPresent, engineActionSkipReuse:
		// Nothing to install. Report done so the wizard advances instead
		// of waiting on the daemon's next profile refresh.
		s.Done(engine)

	case engineActionSkipNotElevated:
		// The daemon already reports permission_denied for an unelevated
		// lease; say it in the executor's own words so error_detail names
		// the command that fixes it.
		s.Failed(engine,
			"the setup command on this device is not running with administrator privileges; "+
				elevation.Hint("waired init"))

	case engineActionSkipOptOut:
		// Engine installs are turned off on this host, but someone just
		// asked for one in the browser. permission_denied is the closest
		// of the eight codes ("this device will not do it"); the detail
		// carries the real reason (waired#835 decisions 20260720 13:00).
		writePrompt(out, "Engine install skipped (WAIRED_NO_OLLAMA).")
		s.Failed(engine,
			"engine installs are turned off on this device (WAIRED_NO_OLLAMA)")
	}
}

// setupEngineInstallWanted reports whether the daemon's state calls for
// an executor-driven engine install. Split out so the caller can decide
// without a second round trip's worth of duplicated conditions.
func setupEngineInstallWanted(st management.SetupStateResponse) bool {
	return st.Active && st.DesiredEngine != "" && !st.EngineInstalled && st.InstallClaimed == ""
}
