package main

import (
	"context"
	"encoding/json"
	"io"
	"runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
)

// daemonInitInference carries the three inference answers `waired init`
// accepts on the command line into the daemon path.
//
// On the standalone path these are read by the configureInference hook
// (cmd/waired/main.go), which the daemon path returns before ever
// reaching. LoginStartRequest carries only a control URL and a device
// name, so until now they were accepted, ignored, and never mentioned —
// the installer passed --inference-enabled on Windows and it did
// nothing (waired#835 §11.2).
//
// Rather than widening the login wire, the CLI re-applies them through
// the management routes that already exist for exactly these three
// controls. Nil means "not passed": absence must not overwrite what the
// host already decided.
type daemonInitInference struct {
	Enabled *bool
	Share   *bool
	ModelID string
}

// empty reports whether the operator passed none of the three, in which
// case this whole step is skipped and the daemon's own defaults stand.
func (d daemonInitInference) empty() bool {
	return d.Enabled == nil && d.Share == nil && d.ModelID == ""
}

// applyDaemonInitInference re-applies the command-line inference answers
// after a daemon-path login.
//
// Ordering matters and is the reason this runs where it does: it must
// land BEFORE waitForBundledModel, or the terminal blocks waiting for a
// download the operator asked to skip, or downloads the auto-selected
// model and only then switches to the requested one.
//
// Every failure is a warning, never fatal: login succeeded, and a knob
// that did not apply is worth a line of text, not a failed install.
func applyDaemonInitInference(mgmtURL string, inf daemonInitInference, out io.Writer) {
	if inf.empty() {
		return
	}
	if inf.Enabled != nil {
		route := "/waired/v1/inference/disable"
		what := "off"
		if *inf.Enabled {
			route, what = "/waired/v1/inference/enable", "on"
		}
		if _, err := httpPost(mgmtURL+route, nil); err != nil {
			writePromptf(out, "warn: could not turn local AI %s (%v); change it later with `waired inference %s`\n", what, err, what)
		}
	}
	if inf.Share != nil {
		route := "/waired/v1/inference/share/disable"
		what := "off"
		if *inf.Share {
			route, what = "/waired/v1/inference/share/enable", "on"
		}
		if _, err := httpPost(mgmtURL+route, nil); err != nil {
			writePromptf(out, "warn: could not turn sharing %s (%v); change it later with `waired inference share %s`\n", what, err, what)
		}
	}
	// Only meaningful with inference on; asking for a model on a host
	// that just turned it off would download weights nobody can use.
	if inf.ModelID != "" && (inf.Enabled == nil || *inf.Enabled) {
		body, _ := json.Marshal(management.PreferredModelRequest{ModelID: inf.ModelID})
		if _, err := httpPost(mgmtURL+"/waired/v1/inference/preferred-model", body); err != nil {
			// `waired models use`, which now exists (waired-agent#753).
			// This line used to say `models pull` because `models use`
			// was one of two remediation lines naming a command that was
			// not there (#465) — but pull only fetches weights, and what
			// failed here is the SELECTION, so pull retried the wrong
			// half. The command that retries this one is the new one.
			writePromptf(out, "warn: could not select the model %q (%v); set it later with `waired models use %s`\n",
				inf.ModelID, err, inf.ModelID)
		}
	}
}

// engineWaitForStatus bounds how long we let the daemon settle before
// concluding it has no engine. The subsystem reports "no_engine" almost
// immediately on a fresh host, but right after login it may still be
// starting up, and installing an engine a host already has would be a
// pointless multi-GB download.
var engineWaitForStatus = 20 * time.Second

// ensureDaemonPathEngine installs the engine on the daemon path when the
// host wants local inference and has none — with or without a browser
// wizard driving.
//
// waired#835 §11 gave the wizard case an executor-driven install, but
// gated it on setupActive. That leaves every terminal-only daemon-path
// install with no engine at all: --non-interactive, --no-browser, no
// TTY, pressing Enter to take the terminal back, or simply not touching
// the browser. macOS reaches this today on its DEFAULT install, because
// its installer registers the LaunchDaemon (RunAtLoad) before running
// init, so init has always taken the daemon path there.
//
// The condition is therefore "does this host want inference", not "is a
// wizard driving" — read from the daemon's own subsystem state rather
// than from any flag, so it reflects what the agent actually decided.
//
// Like the wizard-driven entry point it returns an error only when it
// told the daemon an install failed, so the caller can skip a model wait
// that has nothing to wait for (#188).
func ensureDaemonPathEngine(ctx context.Context, s *executorSession, mgmtURL string, out io.Writer, inf daemonInitInference, nonInteractive bool, sc lineReader) error {
	return daemonPathEngineInstall(ctx, s, mgmtURL, out, runtime.GOOS, elevation.IsElevated(), inf, nonInteractive, sc)
}

// daemonPathEngineInstall is ensureDaemonPathEngine with the OS-varying
// facts injected, so all three OSes are table-testable from an
// unprivileged runner (repo rule).
func daemonPathEngineInstall(
	ctx context.Context, s *executorSession, mgmtURL string, out io.Writer,
	goos string, elevated bool,
	inf daemonInitInference, nonInteractive bool, sc lineReader,
) error {
	if !s.Supported() {
		// No executor routes means a daemon older than this feature. It
		// cannot report progress and we cannot claim an install, so stay
		// on the pre-#835 behaviour exactly.
		return nil
	}
	// One deadline covers both waits below. They are waiting on the same
	// thing — a daemon that login just started settling down — and giving
	// each its own engineWaitForStatus would double what a host with an
	// unreachable daemon spends here (waired-agent#746).
	deadline := time.Now().Add(engineWaitForStatus)
	st, err := awaitSetupStateDir(s, deadline)
	if st.StateDir == "" {
		// The daemon did not say where to install. Guessing risks
		// installing somewhere it never looks — an install that
		// "succeeds" and changes nothing. Say which of the two happened:
		// before #746 an unreachable daemon and a daemon reporting no
		// state dir both returned here in silence, and the wizard path
		// treats the second as a reportable failure (setup_install.go).
		if err != nil {
			writePromptf(out, "warn: could not ask the background service where to install the engine (%v); "+
				"skipping the engine install. Run \"waired doctor\" to see why.\n", err)
		} else {
			writePromptf(out, "warn: the background service did not report where to install the engine; "+
				"skipping the engine install.\n")
		}
		return nil
	}
	if !daemonWantsEngine(mgmtURL, deadline) {
		return nil
	}
	// A wizard-driven install may already hold the claim; do not race it.
	if st.InstallClaimed != "" {
		return nil
	}
	// Install-flow step 4 (waired-agent#584): the install is asked for,
	// not assumed. Asked here — after the daemon said it has no engine
	// and no wizard holds the claim — so the browser-driven journey
	// never sees a terminal question.
	if !confirmDaemonPathEngineInstall(mgmtURL, inf, nonInteractive, sc, out) {
		return nil
	}
	return installEngineAsExecutor(ctx, s, out, goos, elevated,
		"ollama", st.StateDir, engineInstallNarrationLocal)
}

// awaitSetupStateDir polls the daemon's setup view until it names a state
// dir, or the deadline passes. It reads through fetchState rather than
// State because State cannot fail: it maps every error to the zero value,
// which is the same shape as a daemon that answered and reported no state
// dir. The last error is returned so the caller can tell them apart.
//
// The retry matters for the same reason daemonWantsEngine's does, over
// the same daemon one line later: right after login it may still be
// starting up, and a single read gave that host a silently skipped
// install (waired-agent#746).
func awaitSetupStateDir(s *executorSession, deadline time.Time) (management.SetupStateResponse, error) {
	for {
		st, err := s.fetchState()
		if err == nil && st.StateDir != "" {
			return st, nil
		}
		// Sleep no further than the deadline: this budget is shared with
		// daemonWantsEngine, so overshooting it here would spend the
		// caller's remaining time on nothing.
		wait := time.Until(deadline)
		if wait <= 0 {
			return st, err
		}
		if wait > setupStatePollInterval {
			wait = setupStatePollInterval
		}
		time.Sleep(wait)
	}
}

// daemonWantsEngine polls the inference subsystem until it says
// something decisive. Only "no_engine" means install: "disabled" and
// "stopped" are deliberate operator states, and anything else means an
// engine is already up. The deadline is the caller's, so the two waits it
// runs in sequence share one budget rather than stacking.
func daemonWantsEngine(mgmtURL string, deadline time.Time) bool {
	for {
		st, ok := fetchInferenceStatus(mgmtURL)
		switch {
		case !ok:
			// Unreachable this tick; retry until the deadline.
		case st.SubsystemState == "no_engine":
			return true
		case st.SubsystemState == "disabled" || st.SubsystemState == "stopped":
			return false
		case st.SubsystemState != "":
			// starting / ready / downloading / … — an engine exists.
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(setupStatePollInterval)
	}
}
