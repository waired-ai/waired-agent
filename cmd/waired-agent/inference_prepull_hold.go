package main

import (
	"context"
	"time"
)

// The boot pre-pull of the bundled model is the FALLBACK for a host with
// no operator model to serve (#306). On a host that is about to be set up
// from a browser it is also a race: the engine is already installed when
// the daemon boots (an ordinary restart, a re-auth reactivation, a
// reinstall, or an installer that puts the engine in place before starting
// the service), so bootstrapAfterEngineStart runs within about a second —
// before a preferred model exists and before the setup reconciler has
// folded a single control-plane frame. The fallback download starts, and
// the wizard's choice arrives minutes later as a SECOND multi-GB download,
// because the in-flight registry is keyed by model_id and two different
// ids never dedupe (#305).
//
// #379 proposed cancelling the fallback once the choice lands. This holds
// it back instead: the daemon has nothing to gain by starting a download
// it is about to be told is the wrong one, and prevention needs no
// provenance on the job, no cancellation predicate, and no dependency on
// whether `ollama serve` aborts a transfer when its client hangs up (it
// does, at the pinned version, by reference-counting each blob's waiters —
// but that is upstream's internal implementation, not a contract, and the
// pin is bumped automatically).
//
// What it deliberately does NOT cover, both of which stay with #379's
// cancellation idea: a choice that arrives after the hold has already
// released, and an operator switching models from the tray while a
// download is running.
const (
	// prePullFrameGrace bounds the wait for the FIRST control-plane frame.
	// A host that is not enrolled, is offline, or has no control plane at
	// all never gets one, and must still pre-pull exactly as it always
	// did — a few seconds later than before.
	prePullFrameGrace = 30 * time.Second
	// prePullHoldMax is the ceiling on holding for a wizard that is
	// driving but never names a model — a setup abandoned after the engine
	// step, say. It matches setupDesiredFreshWindow, which is how long the
	// reconciler itself keeps calling such an instruction fresh.
	prePullHoldMax = setupDesiredFreshWindow
)

// setupNoteDesired records what the control plane's latest frame said
// about this host and wakes anything waiting on it. See the setupProvider
// interface for the contract; the fields it writes are documented on
// agentInferenceProvider.
func (p *agentInferenceProvider) setupNoteDesired(modelID string, driving bool) {
	p.setupFrameMu.Lock()
	p.setupFrameSeen = true
	p.setupDriving = driving
	if modelID != "" && p.setupNamedModel == "" {
		p.setupNamedModel = modelID
	}
	// Close-and-replace, so a waiter that read the old channel wakes and
	// one that arrives afterwards parks on a channel that is still open.
	if p.setupFrameCh != nil {
		close(p.setupFrameCh)
	}
	p.setupFrameCh = make(chan struct{})
	p.setupFrameMu.Unlock()
}

// setupFrameSnapshot reads the four fields together with the channel to
// park on, so a waiter cannot miss a frame that lands between its read and
// its select.
func (p *agentInferenceProvider) setupFrameSnapshot() (named string, seen, driving bool, next <-chan struct{}) {
	p.setupFrameMu.Lock()
	defer p.setupFrameMu.Unlock()
	if p.setupFrameCh == nil {
		p.setupFrameCh = make(chan struct{})
	}
	return p.setupNamedModel, p.setupFrameSeen, p.setupDriving, p.setupFrameCh
}

// holdBundledPrePull waits until the fallback download is either wrong to
// start or safe to start, then starts it. It returns immediately; the
// waiter is registered on pullsWG, so waitForPulls() still joins the whole
// chain (hold, dispatch, download) and tests do not have to know the hold
// exists.
//
// Deliberately not spawnPull: that releases an in-flight slot this has not
// claimed, and endPull on an absent key fires the deferred reconciles a
// finished pull owns.
func (p *agentInferenceProvider) holdBundledPrePull(ctx context.Context, modelID string) {
	p.pullsWG.Add(1)
	go func() {
		defer p.pullsWG.Done()
		if !p.awaitPrePullRelease(ctx) {
			return
		}
		// Re-taken rather than trusted: minutes have passed, and the
		// operator's own switch may have published a preference (its pull
		// is already in flight) or the weights may have landed some other
		// way. bundledPrePullTarget also re-reads the config's model id,
		// so a retirement resolved since boot is honoured.
		if id, ok := p.prePullStillWanted(ctx, modelID); ok {
			// #496: the last point before 20-45 GB lands, and the first
			// point at which the question can be measured — an engine is
			// up and this host chose its own model, so nobody has said
			// they want to serve here. Undecided leaves the download
			// exactly as it was.
			if !p.applyHostCutoff(ctx) {
				return
			}
			p.dispatchBundledPrePull(ctx, id)
		}
	}()
}

// awaitPrePullRelease blocks until the bundled pre-pull should be
// dispatched, and reports false when it should never be dispatched at all.
//
// The four outcomes, in the order they are tested:
//
//   - setup named a model — stand down for good. The reconciler owns the
//     model path from here, and it is the one downloading.
//   - a frame arrived and nobody is driving — pre-pull now. This is the
//     ordinary restart, and the control plane has answered.
//   - no frame within prePullFrameGrace — pre-pull. Nothing is coming.
//   - a wizard is driving — keep waiting, up to prePullHoldMax.
func (p *agentInferenceProvider) awaitPrePullRelease(ctx context.Context) bool {
	frameGrace := p.prePullFrameGrace
	if frameGrace <= 0 {
		frameGrace = prePullFrameGrace
	}
	holdMax := p.prePullHoldMax
	if holdMax <= 0 {
		holdMax = prePullHoldMax
	}
	// Said on the way IN, not only on the way out. Until #540 the hold was
	// silent for as long as it held and logged only its release, so a host
	// sitting on an undispatched download for ten minutes had nothing in the
	// daemon log to read — the state had to be inferred from which release
	// line eventually appeared, and from when.
	p.logger.Info("boot pre-pull is holding until setup has had its say",
		"frame_grace", frameGrace, "hold_max", holdMax)
	firstFrame := time.NewTimer(frameGrace)
	defer firstFrame.Stop()
	ceiling := time.NewTimer(holdMax)
	defer ceiling.Stop()
	for {
		named, seen, driving, next := p.setupFrameSnapshot()
		switch {
		case named != "":
			p.logger.Info("boot pre-pull stands down: setup chose a model for this host",
				"model", named)
			return false
		case seen && !driving:
			// The ordinary release, and the only one that used to happen
			// silently — which is how #540 stayed unreadable: this is the arm
			// that fires, and the log said nothing about it either way.
			p.logger.Info("boot pre-pull proceeding: the control plane answered and nobody is driving")
			return true
		}
		select {
		case <-next:
			// A new frame; re-read it.
		case <-firstFrame.C:
			if seen {
				// Frames ARE arriving — a wizard is driving and has not
				// named a model yet. The grace was only ever about the
				// host nobody is going to answer for.
				continue
			}
			p.logger.Info("boot pre-pull proceeding: no control-plane frame arrived",
				"waited", frameGrace)
			return true
		case <-ceiling.C:
			p.logger.Info("boot pre-pull proceeding: setup is driving but named no model",
				"waited", holdMax)
			return true
		case <-ctx.Done():
			return false
		}
	}
}

// prePullStillWanted re-takes the boot decision at dispatch time. The
// preference checks are what keep the fallback from piling onto an
// operator switch that started while the hold was waiting: SwapPreferredModel
// publishes preferredOverride before dispatching, and records
// pendingSwapModel while the weights it needs are still downloading.
func (p *agentInferenceProvider) prePullStillWanted(ctx context.Context, modelID string) (string, bool) {
	if pref := p.preferredOverride.Load(); pref != nil && *pref != "" && *pref != modelID {
		p.logger.Info("boot pre-pull stands down: the operator switched models while it waited",
			"preferred", *pref, "bundled", modelID)
		return "", false
	}
	if pending := p.pendingSwapModel.Load(); pending != nil && *pending != "" && *pending != modelID {
		p.logger.Info("boot pre-pull stands down: a model switch is already downloading",
			"switching_to", *pending, "bundled", modelID)
		return "", false
	}
	return p.bundledPrePullTarget(ctx)
}
