package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// engineDoctor is the engine half of one doctor run: the finding to print,
// what the fix flow can do about it, and the detection both were derived from.
// One probe, like trayDoctor, because the repair needs the same answer the
// finding was built from.
type engineDoctor struct {
	Finding   integration.AuditFinding
	Repair    engineRepairAction
	Detection setup.OllamaDetection
}

// engineRepairAction is what `waired doctor --fix` can do to the engine
// install. Deliberately narrow: doctor repairs what is cheap and local, and
// leaves re-installing to `waired init`.
type engineRepairAction int

const (
	// engineRepairNone: nothing doctor can fix here.
	engineRepairNone engineRepairAction = iota
	// engineRepairBundleMarker: delete the waired file that a pre-#329
	// install left at the root of the signed Ollama.app bundle, which
	// invalidated its code-signature seal.
	engineRepairBundleMarker
	// engineRepairReinstallNeeded: the bundle is not valid, but not because
	// of anything waired wrote. Reported, not fixed here — doctor only
	// removes files it put there.
	engineRepairReinstallNeeded
)

// Fixable reports whether `waired doctor --fix` can act. Reinstalling is
// deliberately not doctor's job: it is a 560 MB download that belongs to
// `waired init`, and doctor's contract is cheap local repairs.
func (a engineRepairAction) Fixable() bool { return a == engineRepairBundleMarker }

// engineDoctorFacts are the observations the engine check judges. Separated
// from the judging so the judging is an untagged (GOOS, facts) function that
// table-tests all three OSes from any runner.
type engineDoctorFacts struct {
	Installed bool
	// LegacyBundleMarkerPath is non-empty when a waired file sits at the
	// Ollama.app bundle root — see setup.OllamaDetection.
	LegacyBundleMarkerPath string
	// SignatureProblem is codesign/spctl's complaint about the engine's app
	// bundle, or "" when it is valid (or when there was nothing to assess).
	// It catches the general case: a bundle macOS will not run for any
	// reason, not only the marker waired itself wrote (#330).
	SignatureProblem string
}

// planEngineRepair decides what doctor can repair.
//
// Only macOS has a bundle to break, and only a waired-written file inside it
// is ours to delete: we never remove something we did not put there.
func planEngineRepair(goos string, f engineDoctorFacts) engineRepairAction {
	if goos != "darwin" || !f.Installed {
		return engineRepairNone
	}
	if f.LegacyBundleMarkerPath != "" {
		return engineRepairBundleMarker
	}
	if f.SignatureProblem != "" {
		// Broken, but not by a file we put there — deleting something is not
		// the answer, and guessing which file to remove from a signed bundle
		// would be worse than the disease. Reinstalling is, and that is
		// `waired init`'s job, not doctor's.
		return engineRepairReinstallNeeded
	}
	return engineRepairNone
}

// engineFindingFrom maps the facts into a doctor finding. Pure, so the mapping
// is unit-tested without a host.
//
// It reports only a problem, never an "all good" line: every other OS and
// every healthy Mac would otherwise gain a row that says nothing. Severity is
// Fail, not Warn — unlike the tray, this is not a convenience. A bundle in this
// state cannot be launched at all, so setup wedges forever and `waired doctor`
// should exit non-zero.
func engineFindingFrom(goos string, f engineDoctorFacts) integration.AuditFinding {
	const subject = "AI engine app signature"
	switch planEngineRepair(goos, f) {
	case engineRepairBundleMarker:
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: subject,
			Detail: fmt.Sprintf(
				"%s breaks the app's code signature, so macOS refuses to run the engine "+
					"(\"Ollama is damaged\"); run `sudo waired doctor --fix` to remove it",
				f.LegacyBundleMarkerPath),
		}
	case engineRepairReinstallNeeded:
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: subject,
			Detail: fmt.Sprintf(
				"macOS will not run the installed AI engine (%s); reinstall it with "+
					"`sudo waired runtimes install ollama`", f.SignatureProblem),
		}
	default:
		return integration.AuditFinding{}
	}
}

// checkEngine probes the installed engine once, by path only.
//
// It must never exec the engine binary: on the very hosts this check exists to
// find, exec is SIGKILLed by Gatekeeper and pops the "damaged" dialog again —
// so a check that ran `ollama --version` would re-inflict the symptom it is
// diagnosing. setup.DetectOllamaPathOnly is the exec-free detection.
//
// Callers that must not touch the host (tests) pass the zero engineDoctor,
// which prints no finding and offers no repair.
func checkEngine(stateDir string) engineDoctor {
	det := setup.DetectOllamaPathOnly(stateDir)
	facts := engineDoctorFacts{
		Installed:              det.Installed,
		LegacyBundleMarkerPath: det.LegacyBundleMarkerPath,
	}
	// codesign/spctl are static too — they read the bundle, they never run it.
	ctx, cancel := context.WithTimeout(context.Background(), doctorSignatureProbeTimeout)
	defer cancel()
	if err := doctorEngineSignatureProblem(ctx, det); err != nil {
		facts.SignatureProblem = err.Error()
	}
	return engineDoctor{
		Finding:   engineFindingFrom(runtime.GOOS, facts),
		Repair:    planEngineRepair(runtime.GOOS, facts),
		Detection: det,
	}
}

// repairEngineBundle carries out an engine repair plan.
func repairEngineBundle(e engineDoctor, stateDir string, out io.Writer) error {
	if !e.Repair.Fixable() {
		return nil
	}
	_, _ = fmt.Fprintln(out, "Repairing the AI engine's app signature...")
	changed, err := doctorRepairDarwinBundle(runtime.GOOS, stateDir, e.Detection)
	if err != nil {
		return err
	}
	if changed {
		_, _ = fmt.Fprintln(out, "  Removed the stray waired file from the app bundle; the signature is valid again.")
	}
	return nil
}

// doctorRepairDarwinBundle is the repair seam, so the fix flow is testable
// without a real /Applications/Ollama.app.
var doctorRepairDarwinBundle = setup.RepairDarwinBundleMarker

// doctorSignatureProbeTimeout bounds the codesign/spctl pair. `--deep` walks
// every nested binary, and doctor is an interactive command: a hung Gatekeeper
// query must degrade to "not assessed" rather than hang the whole report.
const doctorSignatureProbeTimeout = 20 * time.Second

// doctorEngineSignatureProblem is the signature-probe seam. It reports the
// bundle's problem, or nil when it is valid or could not be assessed.
var doctorEngineSignatureProblem = func(ctx context.Context, det setup.OllamaDetection) error {
	if !det.Installed {
		return nil
	}
	return engineBundleSignatureProblem(ctx, det)
}
