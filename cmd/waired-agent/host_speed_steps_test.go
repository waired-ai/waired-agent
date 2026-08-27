package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The install-time measurement's two rows (waired#1143).
//
// Product contract, ratified by waired#1143 and the decision record
// docs/decisions/20260812/0200-install-time-measurement-steps.md in the CP
// repo: every stage this table covers must terminate the rows at done,
// skipped or failed, or leave them out. The control plane's setupComplete
// requires every reported step to be done or skipped, tolerating `failed`
// on these two alone — so a row that could sit at running or pending with
// nothing left to move it would deny completion to a host that installs,
// downloads and serves exactly as asked.
func TestHostSpeedSteps(t *testing.T) {
	probe := func(modelID string) (string, modelPullProgress, string) {
		if modelID != hostfit.HostCutoffProbeModelID {
			t.Fatalf("probe bytes looked up %q, want the probe model", modelID)
		}
		return catalog.ModelStateDownloading, modelPullProgress{Completed: 512, Total: 4096}, ""
	}

	tests := []struct {
		name string
		pr   hostSpeedProgress
		// want is the (status, status) pair for (probe_model_pull, host_speed),
		// or nothing at all when the stage emits no rows.
		wantNone    bool
		wantProbe   string
		wantMeasure string
	}{
		{
			// The completion-safe default, and the reason it is the zero
			// value: a host whose engine this probe cannot drive never
			// reaches the measurement, and a row left at pending forever
			// would be worse than no row at all.
			name:     "nothing started and nothing stored says nothing",
			pr:       hostSpeedProgress{Stage: hostSpeedStageNone},
			wantNone: true,
		},
		{
			name:        "the probe model coming down",
			pr:          hostSpeedProgress{Stage: hostSpeedStagePullingProbe},
			wantProbe:   signer.SetupStatusRunning,
			wantMeasure: signer.SetupStatusPending,
		},
		{
			name:        "the timing itself",
			pr:          hostSpeedProgress{Stage: hostSpeedStageMeasuring},
			wantProbe:   signer.SetupStatusDone,
			wantMeasure: signer.SetupStatusRunning,
		},
		{
			name:        "measured",
			pr:          hostSpeedProgress{Stage: hostSpeedStageMeasured},
			wantProbe:   signer.SetupStatusDone,
			wantMeasure: signer.SetupStatusDone,
		},
		{
			// The timing never ran. `skipped` rather than pending: §7's
			// "already true / not reached on this computer", and the red
			// row above it carries the reason.
			name:        "the probe model never arrived",
			pr:          hostSpeedProgress{Stage: hostSpeedStageProbeFailed, Detail: "no space left on device"},
			wantProbe:   signer.SetupStatusFailed,
			wantMeasure: signer.SetupStatusSkipped,
		},
		{
			name:        "the engine declined to be measured",
			pr:          hostSpeedProgress{Stage: hostSpeedStageMeasureFailed, Detail: "context deadline exceeded"},
			wantProbe:   signer.SetupStatusDone,
			wantMeasure: signer.SetupStatusFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			steps := hostSpeedSteps(tc.pr, probe)
			if tc.wantNone {
				if len(steps) != 0 {
					t.Fatalf("steps = %+v, want none", steps)
				}
				return
			}
			if len(steps) != 2 {
				t.Fatalf("steps = %+v, want 2", steps)
			}
			if steps[0].ID != setupStepProbeModelPull || steps[1].ID != setupStepHostSpeed {
				t.Fatalf("ids = %q/%q, want %q/%q",
					steps[0].ID, steps[1].ID, setupStepProbeModelPull, setupStepHostSpeed)
			}
			if steps[0].Status != tc.wantProbe || steps[1].Status != tc.wantMeasure {
				t.Fatalf("statuses = %q/%q, want %q/%q",
					steps[0].Status, steps[1].Status, tc.wantProbe, tc.wantMeasure)
			}
			for _, s := range steps {
				switch s.Status {
				case signer.SetupStatusDone, signer.SetupStatusSkipped,
					signer.SetupStatusFailed, signer.SetupStatusRunning, signer.SetupStatusPending:
				default:
					t.Fatalf("step %q has status %q, which is not in the enum", s.ID, s.Status)
				}
			}
		})
	}
}

// The bytes on the probe row are the PROBE's, not whatever the operator's
// own model happens to be doing. Both rows read setupModelState, and the
// two downloads overlap on a first run — the wizard commits the model while
// the measurement is still fetching its own.
func TestHostSpeedSteps_ProbeRowCarriesTheProbesBytes(t *testing.T) {
	steps := hostSpeedSteps(
		hostSpeedProgress{Stage: hostSpeedStagePullingProbe},
		func(modelID string) (string, modelPullProgress, string) {
			if modelID == hostfit.HostCutoffProbeModelID {
				return catalog.ModelStateDownloading, modelPullProgress{Completed: 512, Total: 1024, RateBps: 40_000_000}, ""
			}
			return catalog.ModelStateDownloading, modelPullProgress{Completed: 90_000, Total: 100_000, RateBps: 1}, ""
		},
	)
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want 2", steps)
	}
	if steps[0].CompletedBytes != 512 || steps[0].TotalBytes != 1024 {
		t.Fatalf("probe row bytes = %d/%d, want 512/1024",
			steps[0].CompletedBytes, steps[0].TotalBytes)
	}
	// And the rate belongs to the same download as the counters
	// (waired#1286).
	if steps[0].RateBps != 40_000_000 {
		t.Fatalf("probe row rate = %d, want 40000000", steps[0].RateBps)
	}
}

// A failed download is classified the way the model row's is — the two are
// the same transfer through the same code, so a full disk has to read as a
// full disk on either.
func TestHostSpeedSteps_ProbeFailureCarriesACode(t *testing.T) {
	steps := hostSpeedSteps(
		hostSpeedProgress{Stage: hostSpeedStageProbeFailed, Detail: "write /var: no space left on device"},
		nil,
	)
	if steps[0].ErrorCode != signer.SetupErrorDiskFull {
		t.Fatalf("probe error code = %q, want %q", steps[0].ErrorCode, signer.SetupErrorDiskFull)
	}
	if !strings.Contains(steps[0].ErrorDetail, "no space left") {
		t.Fatalf("probe error detail = %q, want the measurement's own words", steps[0].ErrorDetail)
	}
}

// The reporter's own read of the stage. A stored figure means measured
// however far THIS process got: the measurement runs from the engine
// bootstrap behind awaitQuietEngine, so a daemon restart on a host that was
// set up weeks ago would otherwise report an unstarted measurement — and
// pending rows deny setup_complete on a computer that is finished.
func TestSetupHostSpeedProgress_AStoredFigureCountsAsMeasured(t *testing.T) {
	p := &agentInferenceProvider{logger: quietLogger()}
	if got := p.setupHostSpeedProgress(); got.Stage != hostSpeedStageNone {
		t.Fatalf("stage = %v, want none on a host that has neither", got.Stage)
	}

	p.hostSpeedMu.Lock()
	p.hostSpeedLoaded = true // no state dir to read from in this test
	p.hostSpeed = &signer.HostSpeed{TurnSeconds: 4.5}
	p.hostSpeedMu.Unlock()

	if got := p.setupHostSpeedProgress(); got.Stage != hostSpeedStageMeasured {
		t.Fatalf("stage = %v, want measured", got.Stage)
	}
}

// ...but a live stage outranks the stored figure. A re-measure asked for by
// an install-flow re-run is real work, and the wizard is the surface that
// exists to show it.
func TestSetupHostSpeedProgress_ALiveStageOutranksTheStoredFigure(t *testing.T) {
	p := &agentInferenceProvider{logger: quietLogger()}
	p.hostSpeedMu.Lock()
	p.hostSpeedLoaded = true
	p.hostSpeed = &signer.HostSpeed{TurnSeconds: 4.5}
	p.hostSpeedMu.Unlock()
	p.noteHostSpeedStage(hostSpeedStageMeasuring, "")

	if got := p.setupHostSpeedProgress(); got.Stage != hostSpeedStageMeasuring {
		t.Fatalf("stage = %v, want measuring", got.Stage)
	}
}

// The rows ride the ENGINE, not the chosen model. On the browser path the
// measurement runs off the engine bootstrap, minutes before the operator
// picks anything (waired#1099) — gating them on a model would emit them
// only after the window they describe had closed. Product contract
// (waired#1143).
func TestSetupSnapshot_MeasurementRowsRideTheEngine(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{
		engineInstalled:   true,
		engineReady:       true,
		hostSpeedProgress: hostSpeedProgress{Stage: hostSpeedStagePullingProbe},
		modelStateFor: map[string]fakeModelState{
			hostfit.HostCutoffProbeModelID: {state: catalog.ModelStateDownloading, completed: 512, total: 1024},
		},
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	// Engine only — the halfway state the wizard's first step writes.
	r.Apply(ctx, desiredFrame("ollama", "", 0))

	snap := r.snapshot(ctx)
	probe := stepByID(t, snap, setupStepProbeModelPull)
	if probe.Status != signer.SetupStatusRunning || probe.CompletedBytes != 512 {
		t.Fatalf("probe row = %+v, want running with the probe's bytes", probe)
	}
	if got := stepByID(t, snap, setupStepHostSpeed); got.Status != signer.SetupStatusPending {
		t.Fatalf("timing row = %+v, want pending", got)
	}
	// Ahead of the model row, because that is the order the work happens
	// in and the wire order IS what NAVI renders.
	if snap.Steps[0].ID != setupStepEngineInstall ||
		snap.Steps[1].ID != setupStepProbeModelPull ||
		snap.Steps[2].ID != setupStepHostSpeed {
		t.Fatalf("step order = %+v, want engine → probe → timing", snap.Steps)
	}
}

// No engine asked for, no rows. The measurement cannot have started, and a
// row on a host the wizard has never touched would be a claim about work
// nobody requested.
func TestSetupSnapshot_NoEngineNoMeasurementRows(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{hostSpeedProgress: hostSpeedProgress{Stage: hostSpeedStageMeasured}}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("", "", 0))

	snap := r.snapshot(ctx)
	if snap == nil {
		return // nothing reported at all is the strongest form of the same thing
	}
	for _, s := range snap.Steps {
		if s.ID == setupStepProbeModelPull || s.ID == setupStepHostSpeed {
			t.Fatalf("step %q reported on a host with no engine asked for", s.ID)
		}
	}
}
