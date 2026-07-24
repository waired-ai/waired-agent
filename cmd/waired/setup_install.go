package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// setupInstallEngine is the install seam so the executor path is
// table-testable without downloading a ~GB engine. It is the same
// per-OS installOllama the interactive path uses (waired#835 §11.1
// requires reuse, not a second installer).
var setupInstallEngine = installOllama

// setupInstallVLLM is the vLLM install seam. The real one builds the venv
// with the wider vLLM budget (installVLLMForSetup); a test fake records the
// call without a ~6 GB build.
var setupInstallVLLM = installVLLMForSetup

// setupVLLMActive reports whether a verified vLLM venv already exists under
// the state dir, so the executor reports the step done without a needless
// rebuild. Seam so tests decide the answer.
var setupVLLMActive = func(stateDir string) bool {
	_, ok := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm")).Active()
	return ok
}

// setupDetectNVIDIA reports whether this host has an NVIDIA driver
// (nvidia-smi on PATH). It is a cheap fast-fail guard: a host the CP's
// broadcast summary called NVIDIA but which cannot actually serve vLLM
// (no driver, wrong OS) is refused before a ~45-minute doomed venv build,
// not after. The installer's own SM_80 verify stays the final authority.
var setupDetectNVIDIA = func(context.Context) bool {
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

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
	// Only the two engines the executor knows how to install. An unknown
	// desired engine is left to the daemon's own reporting rather than
	// half-supported here.
	if st.DesiredEngine != "ollama" && st.DesiredEngine != "vllm" {
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

	// vLLM's installer has a different shape (a uv/pip venv, not a tarball)
	// and needs an NVIDIA GPU on Linux, so it takes its own path rather than
	// ollama's decision tree (waired#835 Phase 2).
	if st.DesiredEngine == "vllm" {
		installVLLMAsExecutor(ctx, s, out, goos, elevated, st.StateDir)
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
	engineInstallNarrationVLLM   = "Installing the vLLM engine for the setup in your browser (a larger one-time download)..."
)

// vllmInstallAction is what the executor should do for a vLLM setup request
// on one concrete host. vLLM has no "reuse your own" or bundled tarball (it
// is always a fresh uv/pip venv) and requires an NVIDIA GPU on Linux, so its
// decision is its own rather than engineInstallDecision's.
type vllmInstallAction int

const (
	vllmActionInstall           vllmInstallAction = iota
	vllmActionSkipPresent                         // a verified venv is already here
	vllmActionSkipNotElevated                     // needs root and we have none
	vllmActionSkipOptOut                          // WAIRED_NO_VLLM
	vllmActionFailUnsupportedOS                   // vLLM setup is Linux-only
	vllmActionFailNoGPU                           // no NVIDIA GPU / driver on this host
)

// vllmInstallDecision decides what the executor does for a vLLM request.
// Pure so the whole tree is table-testable on every OS from an unprivileged
// runner (repo rule: route GOOS-varying decisions through runtime.GOOS).
// A host that already has a verified venv reports present regardless of the
// other conditions — the engine is genuinely there.
func vllmInstallDecision(goos string, elevated, nvidiaPresent, alreadyActive, optOut bool) vllmInstallAction {
	switch {
	case alreadyActive:
		return vllmActionSkipPresent
	case optOut:
		return vllmActionSkipOptOut
	case goos != "linux":
		return vllmActionFailUnsupportedOS
	case !nvidiaPresent:
		return vllmActionFailNoGPU
	case !elevated:
		return vllmActionSkipNotElevated
	default:
		return vllmActionInstall
	}
}

// installVLLMAsExecutor installs vLLM as the elevated executor holding the
// lease. Unlike ollama it fast-fails on the two conditions that would
// otherwise waste a ~45-minute venv build — a non-Linux host and a host with
// no NVIDIA GPU — before claiming and building. The CP already gates the
// wizard's vLLM offer on those, so reaching a fail here means the offer and
// the host disagree; the executor is the final authority (waired#835 §11).
func installVLLMAsExecutor(ctx context.Context, s *executorSession, out io.Writer, goos string, elevated bool, stateDir string) {
	action := vllmInstallDecision(goos, elevated,
		setupDetectNVIDIA(ctx),
		setupVLLMActive(stateDir),
		os.Getenv("WAIRED_NO_VLLM") != "")

	switch action {
	case vllmActionInstall:
		claimed := s.Installing("vllm")
		if claimed.InstallClaimed != "" && claimed.InstallClaimed != "vllm" {
			// Another executor got there first with a different engine.
			return
		}
		writePromptf(out, "%s %s\n", emo("📦", ">>"), engineInstallNarrationVLLM)
		if err := setupInstallVLLM(stateDir); err != nil {
			writePromptf(out, "%s vLLM install failed: %v\n", emo("⚠️", "!"), err)
			s.Failed("vllm", err.Error())
			return
		}
		// Built as root; hand the state dir back or the unprivileged daemon
		// cannot read the venv we just created (Linux only, no-op elsewhere).
		setupHandState(stateDir)
		writePromptf(out, "%s vLLM installed.\n", emo("✅", "*"))
		s.Done("vllm")

	case vllmActionSkipPresent:
		// A verified venv is already here; report done so the wizard advances
		// instead of waiting on the daemon's next profile refresh.
		s.Done("vllm")

	case vllmActionSkipNotElevated:
		s.Failed("vllm",
			"the setup command on this device is not running with administrator privileges; "+
				elevation.Hint("waired init"))

	case vllmActionSkipOptOut:
		writePrompt(out, "vLLM install skipped (WAIRED_NO_VLLM).")
		s.Failed("vllm",
			"engine installs are turned off on this device (WAIRED_NO_VLLM)")

	case vllmActionFailUnsupportedOS:
		// Defense in depth: the CP only offers vLLM on Linux, so this is a
		// host that reached vllm some other way. Name the fix.
		s.Failed("vllm",
			"vLLM setup is only supported on Linux; use the standard engine on this device")

	case vllmActionFailNoGPU:
		s.Failed("vllm",
			"no NVIDIA GPU was detected on this device; vLLM needs an NVIDIA graphics card (CUDA)")
	}
}

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
