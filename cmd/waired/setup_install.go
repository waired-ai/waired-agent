package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// setupInstallEngine is the install seam so the executor path is
// table-testable without downloading a ~GB engine. It is the same
// per-OS installOllama the interactive path uses (waired#835 §11.1
// requires reuse, not a second installer).
var setupInstallEngine = installOllama

// setupInstallVLLM is the vLLM install seam. The real one builds the venv
// with the wider vLLM budget (installVLLMForSetup); a test fake records the
// call without a ~9 GB build. It takes the progress sink for the same
// reason setupInstallEngine does — see the call site.
var setupInstallVLLM = installVLLMForSetup

// setupVLLMActive reports whether a verified vLLM venv already exists under
// the state dir, so the executor reports the step done without a needless
// rebuild. Seam so tests decide the answer.
var setupVLLMActive = func(stateDir string) bool {
	_, ok := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm")).Active()
	return ok
}

// setupDetectNVIDIA reports whether this host has an NVIDIA driver. It
// is a cheap fast-fail guard: a host the CP's broadcast summary called
// NVIDIA but which cannot actually serve vLLM (no driver, wrong OS) is
// refused before a ~45-minute doomed venv build, not after. The
// installer's own SM_80 verify stays the final authority.
//
// It asks the profiler's detector rather than $PATH: this runs from the
// elevated executor, whose PATH is not the desktop user's, and a
// PATH-only probe there refuses vLLM on a perfectly capable card — the
// same defect as #67, one gate along.
var setupDetectNVIDIA = hardware.NVIDIADriverPresent

// setupDetectEngine is the detection seam, for the same reason.
var setupDetectEngine = setup.DetectOllama

// setupDetectEngineNoExec is the exec-free detection seam used by the repair
// path, which must not run the engine binary — see setup.DetectOllamaPathOnly.
var setupDetectEngineNoExec = setup.DetectOllamaPathOnly

// setupRepairDarwinBundle is the repair seam, so the executor test can drive
// both outcomes without a real Ollama.app.
var setupRepairDarwinBundle = setup.RepairDarwinBundleMarker

// setupEngineSignatureBroken is the signature-probe seam. The real one shells
// out to codesign/spctl on darwin and is a constant false elsewhere; a test
// fake decides the answer without needing a signed app bundle.
var setupEngineSignatureBroken = engineSignatureBroken

// repairDarwinEngineBundle undoes #329 on a host that was installed before the
// fix: waired used to write its "this install is ours" marker at the root of
// the signed Ollama.app bundle, which invalidates the bundle's resource seal.
// macOS then refuses to launch it ("Ollama is damaged") and SIGKILLs every
// headless exec, so setup wedges at the model step forever. Deleting the file
// restores the bundle; nothing has to be re-downloaded.
//
// Everything about it is safe to run unconditionally: it is a no-op off darwin,
// a no-op when the marker is absent, and it never execs the engine.
func repairDarwinEngineBundle(out io.Writer, goos, stateDir string) {
	if goos != "darwin" {
		return
	}
	det := setupDetectEngineNoExec(stateDir)
	if det.LegacyBundleMarkerPath == "" {
		return
	}
	changed, err := setupRepairDarwinBundle(goos, stateDir, det)
	if changed {
		writePromptf(out, "%s Repaired the AI engine's macOS app signature (removed a stray waired file from %s).\n",
			emo("🔧", ">>"), filepath.Dir(det.LegacyBundleMarkerPath))
	}
	if err != nil {
		// Not fatal to setup: either the bundle was already repaired and only
		// the bookkeeping failed, or we lack the privileges to touch
		// /Applications and the operator needs to know why.
		writePromptf(out, "%s Could not fully repair the AI engine install: %v\n", emo("⚠️", "!"), err)
	}
}

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
// engineInstallDecision as interactive init, so opt-out, already-present
// and not-elevated all resolve identically (§11.1).
//
// A failure here must not fail login (like ensureBundledEngine), and the
// outcome is reported to the daemon either way — that is what NAVI
// renders. The error return exists for the CALLER, not for the wizard:
// it used to return nothing, so the terminal walked straight into a
// model wait for an engine that would never appear and sat there for up
// to the whole setup budget (#188). The contract is exactly "did we tell
// the daemon this install failed": non-nil ⇔ s.Failed was called.
func runSetupEngineInstall(ctx context.Context, s *executorSession, out io.Writer) error {
	return setupEngineInstall(ctx, s, out, runtime.GOOS, elevation.IsElevated())
}

// setupEngineInstall is runSetupEngineInstall with the two host facts
// that vary by OS passed in, so the whole decision tree is table-testable
// on every OS from an unprivileged CI runner (repo rule: route
// GOOS-varying decisions through a function taking runtime.GOOS).
func setupEngineInstall(ctx context.Context, s *executorSession, out io.Writer, goos string, elevated bool) error {
	if !s.Supported() {
		return nil
	}
	st := s.State()
	// A host installed before #329 carries a marker inside the Ollama.app
	// bundle that invalidates its signature, so macOS kills every exec of the
	// engine. Deleting that one file repairs it.
	//
	// This runs ABOVE the presence gate below, because a broken host satisfies
	// it: the engine IS installed and all of its files ARE present, they just
	// cannot be executed. A repair placed after that gate would never run on
	// the hosts that need it.
	//
	// It is scoped to an active ollama install with a known state dir. The
	// scoping is not cosmetic — the repair probes the host, and running it for
	// a vLLM request (or on a device that is not setting up at all) would put
	// filesystem work on a path that has no app bundle to repair.
	if st.Active && st.DesiredEngine == "ollama" && st.StateDir != "" {
		repairDarwinEngineBundle(out, goos, st.StateDir)
	}
	// EngineInstalled is file presence, so it is true on a host whose engine
	// can never start — which is how the executor used to return here and let
	// the wizard report OK forever. EngineNeedsRepair is the daemon saying
	// "installed, but I have given up starting it": come in and look (#330).
	if !st.Active || st.DesiredEngine == "" || (st.EngineInstalled && !st.EngineNeedsRepair) {
		return nil
	}
	// Only the two engines the executor knows how to install. An unknown
	// desired engine is left to the daemon's own reporting rather than
	// half-supported here.
	if st.DesiredEngine != "ollama" && st.DesiredEngine != "vllm" {
		return nil
	}
	// A live lease already claimed this install. The claim is bound to
	// the lease (§11.1), so a stale one cannot be here — whoever holds
	// it is alive and working. Not our failure, so no error: the caller
	// keeps waiting for the engine that other executor is installing.
	if st.InstallClaimed != "" {
		return nil
	}
	// The daemon could not tell us where to install. Guessing would risk
	// installing somewhere this daemon never looks, which presents to the
	// operator as an install that "worked" and a step that never turns
	// green.
	if st.StateDir == "" {
		const detail = "the background service did not report where to install the engine"
		s.Failed(st.DesiredEngine, signer.SetupErrorInternal, detail)
		return errors.New(detail)
	}

	// vLLM's installer has a different shape (a uv/pip venv, not a tarball)
	// and needs an NVIDIA GPU on Linux, so it takes its own path rather than
	// ollama's decision tree (waired#835 Phase 2).
	if st.DesiredEngine == "vllm" {
		return installVLLMAsExecutor(ctx, s, out, goos, elevated, st.StateDir)
	}

	return installEngineAsExecutor(ctx, s, out, goos, elevated,
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
// on one concrete host. vLLM has no bundled tarball (it is always a fresh
// uv/pip venv) and requires an NVIDIA GPU on Linux, so its decision is its
// own rather than engineInstallDecision's.
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
func installVLLMAsExecutor(ctx context.Context, s *executorSession, out io.Writer, goos string, elevated bool, stateDir string) error {
	action := vllmInstallDecision(goos, elevated,
		setupDetectNVIDIA(ctx),
		setupVLLMActive(stateDir),
		os.Getenv("WAIRED_NO_VLLM") != "")

	switch action {
	case vllmActionInstall:
		claimed := s.Installing("vllm")
		if claimed.InstallClaimed != "" && claimed.InstallClaimed != "vllm" {
			// Another executor got there first with a different engine.
			return nil
		}
		writePromptf(out, "%s %s\n", emo("📦", ">>"), engineInstallNarrationVLLM)
		// The sink is what turns the ~4 GB venv build into a live row in
		// the browser instead of 45 minutes of "Working on it…"
		// (waired-agent#255). Bound to THIS lease, so an inert session
		// yields nil and the installer behaves exactly as it did.
		if err := setupInstallVLLM(stateDir, newVLLMProgressSink(s, "vllm")); err != nil {
			writePromptf(out, "%s vLLM install failed: %v\n", emo("⚠️", "!"), err)
			// No declared code: the build failed somewhere inside uv/pip
			// and its text is all the evidence there is, so the daemon's
			// disk-full reading of it beats anything we could assert.
			s.Failed("vllm", "", err.Error())
			return err
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
		return failEngineInstall(s, "vllm", signer.SetupErrorPermissionDenied,
			"the setup command on this device is not running with administrator privileges; "+
				elevation.Hint("waired init"))

	case vllmActionSkipOptOut:
		writePrompt(out, "vLLM install skipped (WAIRED_NO_VLLM).")
		return failEngineInstall(s, "vllm", signer.SetupErrorPermissionDenied,
			"engine installs are turned off on this device (WAIRED_NO_VLLM)")

	case vllmActionFailUnsupportedOS:
		// Defense in depth: the CP only offers vLLM on Linux, so this is a
		// host that reached vllm some other way. Name the fix.
		//
		// `internal` for both this and the no-GPU arm below: none of the
		// eight §7 codes means "this computer cannot run this engine", and
		// the detail says which one it is. A dedicated code would be a
		// proto addition — a tagged release, a CP validator bump, and new
		// wizard copy — for two arms the CP already gates the offer on.
		return failEngineInstall(s, "vllm", signer.SetupErrorInternal,
			"vLLM setup is only supported on Linux; use the standard engine on this device")

	case vllmActionFailNoGPU:
		return failEngineInstall(s, "vllm", signer.SetupErrorInternal,
			"no NVIDIA GPU was detected on this device; vLLM needs an NVIDIA graphics card (CUDA)")
	}
	return nil
}

// failEngineInstall reports a failed install to the daemon and returns
// the same detail as an error, so the wizard and the terminal always say
// the same thing about the same failure (#188).
//
// Every caller is a DECISION this process made rather than an installer
// error it caught, so every caller has a code to declare — that is what
// this helper is for (waired-agent#135).
func failEngineInstall(s *executorSession, engine, code, detail string) error {
	s.Failed(engine, code, detail)
	return errors.New(detail)
}

// engineInstallErrorCode picks the code the daemon paints on the wizard's
// engine row for a failed install.
//
// Most failures stay undeclared: the installer's text is the evidence, and the
// daemon's disk-full/network reading of it is the best available. A signature
// rejection is different — it is a verdict THIS process reached by asking
// macOS, not a guess from prose. Left undeclared it falls through
// classifySetupFailure's catch-all and gets painted "network_error", which
// sends the user to check their internet about a bundle that downloaded fine
// (#330).
func engineInstallErrorCode(err error) string {
	if isBundleSignatureError(err) {
		return signer.SetupErrorEngineNotReady
	}
	return ""
}

// installEngineAsExecutor is the shared install core: claim the lease,
// run the same decision the interactive path runs, install, hand the
// state dir back, report the outcome. Both entry points reach it.
func installEngineAsExecutor(
	ctx context.Context, s *executorSession, out io.Writer,
	goos string, elevated bool, engine, stateDir, narration string,
) error {
	claimed := s.Installing(engine)
	if claimed.InstallClaimed != "" && claimed.InstallClaimed != engine {
		// Another executor got there first with a different engine.
		return nil
	}

	bundledPresent := false
	if p := bundledEnginePath(goos, stateDir); p != "" {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			bundledPresent = true
		}
	}
	det := setupDetectEngine(ctx, stateDir)
	action := engineInstallDecision(
		goos, elevated, det, bundledPresent,
		os.Getenv("WAIRED_NO_OLLAMA") != "",
		engineIncomplete(goos, det, os.Getenv("ProgramFiles")),
		setupEngineSignatureBroken(ctx, det))

	switch action {
	// Repair runs the same installer against bits an earlier attempt left
	// unconfigured (#190); it skips the base download, so it is cheap.
	case engineActionInstall, engineActionRepair:
		writePromptf(out, "%s %s\n", emo("📦", ">>"), narration)
		// The sink is what turns this install into two live rows in the
		// browser (waired-agent#197). It is bound to THIS lease, so a
		// session that is inert (no daemon routes) yields nil and the
		// installer behaves exactly as it did.
		if err := setupInstallEngine(true, stateDir, newExecutorProgressSink(s, engine)); err != nil {
			writePromptf(out, "%s Engine install failed: %v\n", emo("⚠️", "!"), err)
			s.Failed(engine, engineInstallErrorCode(err), err.Error())
			return err
		}
		// The tarball was extracted as root; hand the state dir back or
		// the unprivileged daemon cannot read what we just installed
		// (Linux only, no-op elsewhere).
		setupHandState(stateDir)
		writePromptf(out, "%s AI engine installed.\n", emo("✅", "*"))
		s.Done(engine)

	case engineActionSkipPresent:
		// Nothing to install. Report done so the wizard advances instead
		// of waiting on the daemon's next profile refresh.
		s.Done(engine)

	case engineActionSkipNotElevated:
		// The daemon reports permission_denied for an unelevated lease
		// only while that lease is LIVE, and this process is about to
		// exit — so declaring the code here is what keeps the answer from
		// decaying into executor_gone ("run it again", which would fail
		// the same way) the moment we go (waired-agent#135/#137).
		return failEngineInstall(s, engine, signer.SetupErrorPermissionDenied,
			"the setup command on this device is not running with administrator privileges; "+
				elevation.Hint("waired init"))

	case engineActionSkipOptOut:
		// Engine installs are turned off on this host, but someone just
		// asked for one in the browser. permission_denied is the closest
		// of the eight codes ("this device will not do it"); the detail
		// carries the real reason (waired#835 decisions 20260720 13:00).
		// That was the intent from the start — until now the daemon
		// re-derived network_error from this text and the intent was lost.
		writePrompt(out, "Engine install skipped (WAIRED_NO_OLLAMA).")
		return failEngineInstall(s, engine, signer.SetupErrorPermissionDenied,
			"engine installs are turned off on this device (WAIRED_NO_OLLAMA)")
	}
	return nil
}

// engineArrivalPending reports whether an engine can still plausibly
// appear on this host, which is the only condition under which the model
// wait should ignore its own no_engine grace (#188).
//
// The wait used to disable that grace for the whole of a browser setup,
// so a failed install parked the terminal on "Waiting for the AI engine
// to start…" until the setup budget ran out — an hour, on the exact
// hosts where the engine was never coming. Three states genuinely mean
// "keep waiting": the wizard has not picked an engine yet, a live lease
// holds the install claim (someone else is installing), or the desired
// engine is not in place. Anything else gets the ordinary grace back,
// and a failed install skips the wait entirely at the caller.
//
// A host awaiting repair counts as "not in place": the engine on disk is one
// macOS refuses to run, so the useful engine really has not arrived yet (#330).
func engineArrivalPending(st management.SetupStateResponse) bool {
	return setupDriving(st) &&
		(st.DesiredEngine == "" || st.InstallClaimed != "" ||
			!st.EngineInstalled || st.EngineNeedsRepair)
}

// setupDriving reports whether st describes a browser setup that is
// driving this host NOW, rather than an instruction left over from an
// earlier run (waired-agent#308).
//
// Every place that used to read st.Active on its own asks this instead.
// The control plane persists desired_engine / desired_model_id for the
// life of the device, so `Active` answers "has this host ever been told
// what to run" — which, on a second `waired init`, is the wrong question
// and used to print "Setup has started in your browser" at a terminal
// with no browser open. The daemon marks the difference; this is the
// single place the CLI reads it.
func setupDriving(st management.SetupStateResponse) bool {
	return st.Active && !st.DesiredStale
}

// setupEngineInstallWanted reports whether the daemon's state calls for
// an executor-driven engine install. Split out so the caller can decide
// without a second round trip's worth of duplicated conditions.
// A host whose engine is installed but unusable wants one too: that is the
// repair case, and it must agree with setupEngineInstall's own gate or the
// caller decides not to call the thing that would fix it (#330).
func setupEngineInstallWanted(st management.SetupStateResponse) bool {
	return setupDriving(st) && st.DesiredEngine != "" &&
		(!st.EngineInstalled || st.EngineNeedsRepair) && st.InstallClaimed == ""
}
