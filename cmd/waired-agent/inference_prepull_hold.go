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

	// modelChoiceWaitMax is the ceiling on the terminal install flow's
	// claim that a model question is about to be asked (waired-agent#586,
	// the same 60 minutes as prePullHoldMax and for the same reason: an
	// abandoned setup — here a closed terminal — eventually gets the
	// fallback download it would have had without the question; owner
	// ruling 2026-08-09). The deadline is stamped server-side when the
	// claim is registered, so a killed `waired init` cannot renew it.
	modelChoiceWaitMax = prePullHoldMax
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
		id, ok := p.prePullStillWanted(ctx, modelID)
		if !ok {
			return
		}
		// #496: the last point before 20-45 GB lands, and the first
		// point at which the question can be measured — an engine is
		// up and this host chose its own model, so nobody has said
		// they want to serve here. Undecided leaves the download
		// exactly as it was.
		if !p.applyHostCutoff(ctx) {
			return
		}
		// waired-agent#586: `waired init` may have claimed that the model
		// question is about to be asked at this terminal. The claim sits
		// HERE — after the cutoff measurement, never before it — because
		// the terminal's own flow asks its questions only once the
		// measurement has been published, so a wait placed upstream would
		// deadlock the two against each other.
		if waited, proceed := p.awaitModelChoice(ctx); !proceed {
			return
		} else if waited {
			// The question was asked, and time has passed. A browser setup
			// may also have taken the terminal over and named a model
			// mid-wait — the arm awaitPrePullRelease owns, re-checked here
			// because that release has already happened.
			if named, _, _, _ := p.setupFrameSnapshot(); named != "" {
				p.logger.Info("boot pre-pull stands down: setup chose a model while the terminal was asked",
					"model", named)
				return
			}
			if id, ok = p.prePullStillWanted(ctx, id); !ok {
				return
			}
		}
		p.dispatchBundledPrePull(ctx, id)
	}()
}

// noteModelChoicePending registers (or withdraws) the terminal install
// flow's claim that a model question is about to be asked (#586). The
// deadline is stamped here, server-side: the CLI states intent, never a
// duration.
func (p *agentInferenceProvider) noteModelChoicePending(pending bool) {
	window := p.modelChoiceWait
	if window <= 0 {
		window = modelChoiceWaitMax
	}
	p.modelChoiceMu.Lock()
	if pending {
		p.modelChoicePendingUntil = time.Now().Add(window)
	} else {
		p.modelChoicePendingUntil = time.Time{}
	}
	p.wakeModelChoiceLocked()
	p.modelChoiceMu.Unlock()
	if pending {
		p.logger.Info("bundled fallback download deferred: the install flow is asking which model to download",
			"wait_max", window)
	}
}

// noteModelChoiceAnswered withdraws the claim because an answer landed —
// a model choice (SwapPreferredModel) or the none choice
// (applyNoModelSelected). Distinct from noteModelChoicePending(false)
// only in that it never logs: the answer's own path already says what
// happened.
func (p *agentInferenceProvider) noteModelChoiceAnswered() {
	p.modelChoiceMu.Lock()
	p.modelChoicePendingUntil = time.Time{}
	p.wakeModelChoiceLocked()
	p.modelChoiceMu.Unlock()
}

// applyNoModelSelected applies the operator's "don't download a model
// now" choice in process (#586): the management handler has already
// persisted it; this is what a held fallback dispatch reads.
func (p *agentInferenceProvider) applyNoModelSelected() {
	p.noModelSelected.Store(true)
	p.noteModelChoiceAnswered()
	p.logger.Info("the operator chose to run without a local model; the bundled fallback download stands down")
}

// wakeModelChoiceLocked is the setupFrameCh pattern: close-and-replace so
// a parked waiter wakes and a later one parks on a live channel. Callers
// hold modelChoiceMu.
func (p *agentInferenceProvider) wakeModelChoiceLocked() {
	if p.modelChoiceCh != nil {
		close(p.modelChoiceCh)
	}
	p.modelChoiceCh = make(chan struct{})
}

// modelChoiceSnapshot reads the claim's deadline together with the
// channel to park on, so a waiter cannot miss a change between its read
// and its select.
func (p *agentInferenceProvider) modelChoiceSnapshot() (until time.Time, next <-chan struct{}) {
	p.modelChoiceMu.Lock()
	defer p.modelChoiceMu.Unlock()
	if p.modelChoiceCh == nil {
		p.modelChoiceCh = make(chan struct{})
	}
	return p.modelChoicePendingUntil, p.modelChoiceCh
}

// awaitModelChoice blocks while the terminal install flow's claim is
// live. waited reports whether it blocked at all (so the caller knows to
// re-take its decision); proceed is false only when ctx ended. The claim
// expiring is a proceed: an abandoned terminal gets the fallback
// download, exactly like an abandoned browser wizard at prePullHoldMax.
func (p *agentInferenceProvider) awaitModelChoice(ctx context.Context) (waited, proceed bool) {
	for {
		until, next := p.modelChoiceSnapshot()
		if until.IsZero() {
			return waited, true
		}
		remaining := time.Until(until)
		if remaining <= 0 {
			p.logger.Info("boot pre-pull proceeding: the install flow asked which model to download and got no answer")
			return waited, true
		}
		waited = true
		expiry := time.NewTimer(remaining)
		select {
		case <-next:
			// The claim changed (answered or withdrawn); re-read it.
		case <-expiry.C:
			// Re-read: the loop's remaining<=0 arm logs and proceeds.
		case <-ctx.Done():
			expiry.Stop()
			return waited, false
		}
		expiry.Stop()
	}
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
