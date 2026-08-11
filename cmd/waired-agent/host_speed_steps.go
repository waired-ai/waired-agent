package main

import (
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The install-time measurement, as the setup-progress reporter sees it
// (waired#1143).
//
// The measurement is two pieces of work: the small model this host is timed
// on has to arrive (~1 GB on the reference catalog), and then the timing
// itself runs. Neither reached the setup-progress channel at all. The only
// observable was the ABSENCE of a figure, which is what NAVI's wizard was
// deriving "Timing this computer…" from — one line of text standing in for
// minutes of work, with no bytes, no state and no way to tell a slow link
// from a stall.
//
// It is reported rather than acted on: nothing here changes what the
// measurement does or when it runs.

// hostSpeedStage is how far the measurement has got.
type hostSpeedStage uint8

const (
	// hostSpeedStageNone is "nothing to say about it on this host": the
	// measurement has not started and none is stored. It emits NO rows, and
	// that is deliberate — see hostSpeedSteps.
	hostSpeedStageNone hostSpeedStage = iota
	hostSpeedStagePullingProbe
	hostSpeedStageMeasuring
	hostSpeedStageMeasured
	hostSpeedStageProbeFailed
	hostSpeedStageMeasureFailed
)

// hostSpeedProgress is the whole of what the reporter reads.
type hostSpeedProgress struct {
	Stage hostSpeedStage
	// Detail is the failure in the measurement's own words, for the failed
	// stages. Empty otherwise.
	Detail string
}

// hostSpeedSteps projects the stage onto the two rows.
//
// `probeBytes` reports the probe model's live download the way
// setupModelState does for the operator's own model — the probe goes through
// the same PullModel path, so the same accounting already covers it.
//
// NOTHING is emitted for a host with no measurement under way and none
// stored. That is the completion-safe direction and the only one: the
// control plane's setupComplete requires every reported step to be done or
// skipped (a failed measurement is tolerated, waired#1143), so a row that
// could sit at `pending` forever — a host whose engine cannot be driven by
// this probe, one where the measurement never starts — would deny
// completion to a computer that installs, downloads and serves exactly as
// asked. An absent row denies nothing, and is what every build before this
// one reported.
func hostSpeedSteps(
	pr hostSpeedProgress,
	probeBytes func(modelID string) (state string, completed, total int64, errText string),
) []signer.SetupStep {
	probe := signer.SetupStep{ID: setupStepProbeModelPull}
	measure := signer.SetupStep{ID: setupStepHostSpeed}

	switch pr.Stage {
	case hostSpeedStageNone:
		return nil

	case hostSpeedStagePullingProbe:
		probe.Status = signer.SetupStatusRunning
		if probeBytes != nil {
			_, completed, total, _ := probeBytes(hostfit.HostCutoffProbeModelID)
			probe.CompletedBytes = completed
			probe.TotalBytes = total
		}
		measure.Status = signer.SetupStatusPending

	case hostSpeedStageMeasuring:
		probe.Status = signer.SetupStatusDone
		measure.Status = signer.SetupStatusRunning

	case hostSpeedStageMeasured:
		probe.Status = signer.SetupStatusDone
		measure.Status = signer.SetupStatusDone

	case hostSpeedStageProbeFailed:
		probe.Status = signer.SetupStatusFailed
		probe.ErrorCode = classifyModelPullFailure(pr.Detail)
		probe.ErrorDetail = clampSetupDetail(pr.Detail)
		// The timing never ran, and `pending` would hold the setup open on
		// work that will not be attempted again this boot. §7's `skipped`
		// is the honest terminal state for a step this host did not reach;
		// the red row above it carries the reason.
		measure.Status = signer.SetupStatusSkipped

	case hostSpeedStageMeasureFailed:
		probe.Status = signer.SetupStatusDone
		measure.Status = signer.SetupStatusFailed
		// Not classified from the text the way the download is. Every way
		// this arm is reached — the engine did not answer, it prefilled
		// less than the depth asked for, the reading did not support a
		// verdict — is the engine declining to be measured rather than a
		// transfer that failed, and the download-shaped codes (disk_full,
		// network_error) would each name a cause that is not there.
		measure.ErrorCode = signer.SetupErrorInternal
		measure.ErrorDetail = clampSetupDetail(pr.Detail)
	}
	return []signer.SetupStep{probe, measure}
}
