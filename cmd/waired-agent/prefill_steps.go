package main

import (
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// The mesh speed measurement, as the setup-progress reporter sees it
// (waired-agent#1301).
//
// The owner's rc6 review asked for one thing: setup is not complete until
// the benchmarks that follow the download have run. What the product did
// instead was report complete and then saturate the engine for minutes.
// Measured on the reference Linux host: the engine reached ready at
// 17:54:57, the boot benchmark finished at 17:55:50, and the three
// prefill rungs landed at 17:56:16, 17:56:38 and 17:58:19 — about four
// minutes past the moment every surface said the computer was set up,
// with the GPU busy the whole time and peers getting
// `503 waired_inference_measuring`.
//
// The measurement itself already refuses peer traffic while it runs
// (inference_prefill_state.go). The gap was that nothing SAID so: no
// surface carried it, so `waired init` printed its completion box and
// NAVI ticked its last step while the work was still going. The row here
// is that missing sentence, and because the control plane derives
// completion from the rows, adding it is also what makes setup wait —
// the implementation half of the ruling recorded in
// docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md,
// "3 分（オーナー裁定。init はこの計測を待つ）".
//
// It is reported rather than acted on: nothing here changes what the
// measurement does, when it runs, or how long it takes.

// prefillSetupBudget bounds how long this row may hold onboarding open.
//
// The measurement's own budget is three minutes (the owner ruling above),
// but that clock only starts once it has the engine. Two paths give it
// back without recording anything — a claim it could not take, and a
// yield to serving traffic before the first rung completed — and on a
// host that is always busy those can repeat. This is the terminal
// backstop for that, not a second opinion on the measurement's own
// budget: generous enough that no honest run reaches it, finite so that
// a computer which installs, downloads and serves exactly as asked is
// never told it is unfinished forever.
const prefillSetupBudget = 15 * time.Minute

// prefillSetupStage is how far the measurement has got.
type prefillSetupStage uint8

const (
	// prefillSetupStageNone is "nothing to say about it on this host":
	// no model is committed, so nothing is owed. It emits NO row, which
	// is the completion-safe direction and what every build before this
	// one reported.
	prefillSetupStageNone prefillSetupStage = iota
	prefillSetupStageMeasuring
	prefillSetupStageMeasured
	// prefillSetupStageFailed is a measurement that ran and could not
	// produce a figure. The control plane tolerates a failed benchmark
	// row (setup_readiness.go's failedIsTolerable), which is the right
	// treatment here too: a host that cannot be measured still serves.
	prefillSetupStageFailed
	// prefillSetupStageGaveUp is the budget above running out. Reported
	// as `skipped` rather than `failed`, because nothing about this host
	// failed — the engine was busy every time the measurement came to
	// take it.
	prefillSetupStageGaveUp
)

// prefillSetupProgress is the stage plus what the row needs to say.
type prefillSetupProgress struct {
	Stage  prefillSetupStage
	Detail string
}

// setupPrefillProgress reads the stage off the measurement's own state.
//
// Derived rather than tracked: every term is already recorded for another
// reason, and a second copy would be a second thing to keep in step
// (waired-agent#1206's lesson about two readers of one selection).
func (p *agentInferenceProvider) setupPrefillProgress() prefillSetupProgress {
	if p == nil {
		return prefillSetupProgress{}
	}
	variant := p.activeVariantID()
	if variant == "" {
		// No committed model, so no measurement is owed. Not the same as
		// "done": the row simply does not exist yet.
		return prefillSetupProgress{}
	}
	p.benchMu.Lock()
	m := p.lastPrefill
	p.benchMu.Unlock()
	if m != nil && m.VariantID == variant {
		if m.Failed {
			return prefillSetupProgress{Stage: prefillSetupStageFailed, Detail: m.Err}
		}
		return prefillSetupProgress{Stage: prefillSetupStageMeasured}
	}
	// Owed. Whether the gate has been armed yet or not — an engine that
	// is a moment behind its daemon has not armed it, and the row still
	// has to exist, or setup completes in that window.
	if armed := p.speedMeasureArmedAt.Load(); armed != 0 &&
		time.Since(time.Unix(0, armed)) > prefillSetupBudget {
		return prefillSetupProgress{Stage: prefillSetupStageGaveUp}
	}
	return prefillSetupProgress{Stage: prefillSetupStageMeasuring}
}

// prefillMeasurementSteps projects the stage onto the row.
//
// One row, not one per rung. The ladder is three fixed depths today and
// the wire carries the rungs themselves on /healthz, so a row per rung
// would put the ladder's shape into the setup vocabulary — where it
// would then have to be versioned against a constant that is allowed to
// change. What onboarding needs from this is one answer: is the computer
// still working this out.
func prefillMeasurementSteps(pr prefillSetupProgress) []signer.SetupStep {
	step := signer.SetupStep{ID: setupStepPrefillMeasurement}
	switch pr.Stage {
	case prefillSetupStageNone:
		return nil
	case prefillSetupStageMeasuring:
		step.Status = signer.SetupStatusRunning
	case prefillSetupStageMeasured:
		step.Status = signer.SetupStatusDone
	case prefillSetupStageFailed:
		step.Status = signer.SetupStatusFailed
		// Not classified into an error code. Every way this arm is
		// reached — the engine did not answer, it prefilled less than the
		// depth asked for, the readings never settled — is the engine
		// declining to be measured rather than a transfer that failed,
		// and the download-shaped codes would each name a cause that is
		// not there. The same reasoning host_speed_steps.go records for
		// its own measure-failed arm.
		step.ErrorDetail = clampSetupDetail(pr.Detail)
	case prefillSetupStageGaveUp:
		step.Status = signer.SetupStatusSkipped
	}
	return []signer.SetupStep{step}
}

// String is the stage as the local management API reports it, so
// `waired init` can tell a measurement that is still going from one that
// finished. Stable strings, and prefillSetupStageNone deliberately has
// none — a host with nothing to say says nothing, the same absence
// prefillMeasurementSteps returns no row for.
func (s prefillSetupStage) String() string {
	switch s {
	case prefillSetupStageMeasuring:
		return "measuring"
	case prefillSetupStageMeasured:
		return "measured"
	case prefillSetupStageFailed:
		return "failed"
	case prefillSetupStageGaveUp:
		return "gave_up"
	}
	return ""
}
