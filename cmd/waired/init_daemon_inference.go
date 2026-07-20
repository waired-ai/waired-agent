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
			writePromptf(out, "warn: could not select the model %q (%v); pick one later with `waired models use`\n", inf.ModelID, err)
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
func ensureDaemonPathEngine(ctx context.Context, s *executorSession, mgmtURL string, out io.Writer) {
	daemonPathEngineInstall(ctx, s, mgmtURL, out, runtime.GOOS, elevation.IsElevated())
}

// daemonPathEngineInstall is ensureDaemonPathEngine with the OS-varying
// facts injected, so all three OSes are table-testable from an
// unprivileged runner (repo rule).
func daemonPathEngineInstall(
	ctx context.Context, s *executorSession, mgmtURL string, out io.Writer,
	goos string, elevated bool,
) {
	if !s.Supported() {
		// No executor routes means a daemon older than this feature. It
		// cannot report progress and we cannot claim an install, so stay
		// on the pre-#835 behaviour exactly.
		return
	}
	st := s.State()
	if st.StateDir == "" {
		// The daemon did not say where to install. Guessing risks
		// installing somewhere it never looks — an install that
		// "succeeds" and changes nothing.
		return
	}
	if !daemonWantsEngine(mgmtURL) {
		return
	}
	// A wizard-driven install may already hold the claim; do not race it.
	if st.InstallClaimed != "" {
		return
	}
	installEngineAsExecutor(ctx, s, out, goos, elevated,
		"ollama", st.StateDir, engineInstallNarrationLocal)
}

// daemonWantsEngine polls the inference subsystem until it says
// something decisive. Only "no_engine" means install: "disabled" and
// "stopped" are deliberate operator states, and anything else means an
// engine is already up.
func daemonWantsEngine(mgmtURL string) bool {
	deadline := time.Now().Add(engineWaitForStatus)
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
