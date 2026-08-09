// The install-time host measurement's WIRING half (#496): getting the
// probe model onto the host, taking the measurement, publishing it, and
// acting on it. The policy is proto/hostfit/host_cutoff.go; the
// measurement is host_cutoff_probe.go.
//
// MEASURING and DECIDING are two different questions here, and they have
// different triggers.
//
// The MEASUREMENT is a property of the host, taken once per install and
// kept in the state dir. It runs from the engine bootstrap's tail, on
// every path, as soon as the engine is serving and before anything has
// asked for a 20-45 GB download. It is published on the wire
// (signer.InferenceState.HostSpeed) so the control plane, the admin
// device page and waired#1065's public-share gate can each ask their own
// question of the same number, and so an operator can be told why local
// inference is off.
//
// "Once per install" is the rule, not "once per machine": the stored
// figure is reused only while BOTH the engine build and the agent build
// still match (state.HostSpeedRecord.AgentVersion). A daemon restart
// reuses it; an install, an upgrade or an engine bump measures again.
// The alternative — keeping a figure for as long as the hardware looks
// the same — is what waired#1099 ruled out, and it is also what would
// leave a machine that gained a graphics card describing itself with the
// number it had before (waired#668 is the same lesson one level down).
//
// It runs at the bootstrap tail rather than beside the model choice
// because that is the only point that every install path passes through.
// The browser wizard names a model, which stands the pre-pull down, so
// the pre-pull's own call reaches only the hosts nobody is setting up
// from a browser — i.e. not the majority path (waired#1099).
//
// The DECISION — start local inference off, with #465's opt-in — is taken
// only where the daemon has no one else's answer to defer to: the bundled
// pre-pull path, where this host chose its own model, and only while the
// local-inference toggle is still unset. A person who named a model, or
// who has already moved the toggle, has said what they want, and #465's
// default is not ours to override (waired-ai/waired#1056: refusal is
// reserved for certain OOM, and this refuses nothing — it sets a
// default).
//
// WHERE THIS RUNS, and why not where #496 said. The issue put the cutoff
// beside the install-time model selection, which is
// maybeSelectBundledModelForFreshInstall — and that runs at main.go:219,
// before the logger exists and long before any engine does. A probe needs
// a running engine and its ~1 GB model, so it cannot run there. The
// verdict still reaches the same place SelectInstallModel's ok=false
// reaches — local inference off with the #465 opt-in — which is what
// docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
// decision 6 requires of it.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

const (
	// hostCutoffPullPoll / hostCutoffPullTimeout pace the wait for the
	// probe model's own download. PullModel is asynchronous, and the
	// cutoff has nothing to measure until the weights land. ~1 GB, so the
	// ceiling is about a connection slow enough that the 20-45 GB model
	// behind it was never going to arrive either.
	hostCutoffPullPoll    = 2 * time.Second
	hostCutoffPullTimeout = 20 * time.Minute

	// hostSpeedSettlePoll / hostSpeedSettleWait pace awaitQuietEngine. The
	// wait is generous because what it is usually waiting for is the
	// operator's own model download, which is the whole reason this host
	// has an engine at all; the measurement is the least urgent thing
	// running and should say so by yielding. A var so tests do not wait.
	hostSpeedSettlePoll = 2 * time.Second
)

// hostSpeedSettleWait bounds awaitQuietEngine. A var, not a const, so a
// test can shrink it.
var hostSpeedSettleWait = 60 * time.Minute

// hostSpeedNow returns the measurement this host publishes, or nil when
// there is none. Safe to call from the probe loop on every tick.
func (p *agentInferenceProvider) hostSpeedNow() *signer.HostSpeed {
	if p == nil {
		return nil
	}
	p.hostSpeedMu.Lock()
	defer p.hostSpeedMu.Unlock()
	p.loadHostSpeedLocked()
	return p.hostSpeed
}

// hostSpeedTurnedInferenceOff reports whether the stored measurement is
// what set the local-inference default to off.
//
// Read from disk on every call rather than cached, because that file is
// where the answer changes: `waired inference off|on` clears the flag
// through WriteDesiredInferenceState, and an in-memory copy would go on
// claiming the cutoff's reason for a state a person had since chosen.
func (p *agentInferenceProvider) hostSpeedTurnedInferenceOff() bool {
	if p == nil || p.stateDir == "" {
		return false
	}
	rec, err := state.ReadHostSpeed(p.stateDir)
	return err == nil && rec.TurnedInferenceOff
}

// loadHostSpeedLocked brings the stored measurement into memory, once per
// process. Call with hostSpeedMu held.
//
// It deliberately does NOT check that the stored figure was taken on the
// engine running now. That check belongs to ensureHostSpeedMeasured,
// which decides whether to re-measure; here the question is what this
// host currently knows about itself, and a figure from an earlier engine
// build is still the best answer available. It travels with its own
// EngineVersion, so a consumer can see what produced it.
//
// Without this the daemon that turned local inference off would forget
// why the moment it restarted: nothing re-measures on a host whose
// install path already ran, so `waired inference status` would be back to
// reporting a bare "off".
func (p *agentInferenceProvider) loadHostSpeedLocked() {
	if p.hostSpeedLoaded || p.stateDir == "" {
		return
	}
	p.hostSpeedLoaded = true
	rec, err := state.ReadHostSpeed(p.stateDir)
	if err != nil {
		p.logger.Warn("host speed: could not read the stored measurement", "err", err)
		return
	}
	if rec.Measurement != nil && p.hostSpeed == nil {
		p.hostSpeed = rec.Measurement
		p.hostSpeedAgentVersion = rec.AgentVersion
	}
}

// startHostSpeedMeasurement takes the measurement in the background, if
// this install has not taken it yet. Called from the engine bootstrap's
// tail, which is the one point every install path reaches with a serving
// engine.
//
// ASYNCHRONOUS on purpose. The tail's remaining job is to dispatch the
// model the operator will actually use, and this can take three minutes
// plus a ~1 GB download on exactly the hosts that most need the model to
// start arriving. Registered on pullsWG like holdBundledPrePull, so the
// whole chain still has one join point.
//
// And it waits for the host to go quiet first. The boot tail's two engine
// restarts (#359) are not the only ones: endPull fires a serve reconcile
// when a model lands, and that reconcile restarts the engine too. Measured
// on a CI host, the measurement started 400 ms before the operator's model
// finished downloading and died 3 ms after the reconcile it triggered —
// `connect: connection refused`, three minutes of work discarded, and
// nothing published. A measurement taken while the host is installing is
// also a measurement of contention, which is the one thing the median of
// three samples cannot correct for.
//
// Same reasoning warmTarget already applies to loading a model (see
// inference_warm.go): a pull holds the disk and the memory, so defer.
func (p *agentInferenceProvider) startHostSpeedMeasurement(ctx context.Context) {
	if p == nil || p.ollama == nil {
		return
	}
	p.pullsWG.Add(1)
	go func() {
		defer p.pullsWG.Done()
		if !p.awaitQuietEngine(ctx) {
			p.logger.Info("host speed: the engine did not go quiet; not measuring this boot")
			return
		}
		p.ensureHostSpeedMeasured(ctx)
	}()
}

// awaitQuietEngine blocks until nothing else on this host is using the
// engine, and reports whether it got there. Bounded by
// hostSpeedSettleWait — a host still downloading a 45 GB model after that
// is one whose measurement can wait for the next boot.
func (p *agentInferenceProvider) awaitQuietEngine(ctx context.Context) bool {
	deadline := time.Now().Add(hostSpeedSettleWait)
	for {
		if p.engineIsQuiet(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(hostSpeedSettlePoll):
		}
	}
}

// engineIsQuiet reports whether the engine is ready and nothing else is
// about to take it away.
//
// A pending reconcile counts as busy, not just a running one: reconcile
// STOPS AND RESTARTS the engine, so starting a minutes-long measurement
// while one is queued behind a finishing pull is how the measurement gets
// its connection refused.
func (p *agentInferenceProvider) engineIsQuiet(ctx context.Context) bool {
	if p.ollama == nil {
		return false
	}
	p.pullMu.Lock()
	pulling := len(p.pullsInFlight) > 0
	p.pullMu.Unlock()
	if pulling || p.engineReconcileInFlight.Load() {
		return false
	}
	if p.ollama.IsParked() {
		return false
	}
	return p.ollama.Health(ctx).State == infruntime.StateReady
}

// engineQuietForBench is engineIsQuiet as the boot/explicit benchmark
// asks it (BenchDeps.EngineQuiet, #582/#601). Same question — is anything
// about to take the engine away — with one difference at the nil end.
//
// No ollama adapter, or a host serving something else, answers QUIET
// rather than busy: the pull registry and the serve-env reconcile this
// guards against are both ollama's, so on a vLLM host there is nothing
// here that can restart the engine and a `false` would gate that host's
// benchmark off forever. engineIsQuiet cannot answer that way for its own
// caller — the host-speed measurement reads ollama's counters and refuses
// non-ollama outright (hostCutoffProbeTag).
func (p *agentInferenceProvider) engineQuietForBench(ctx context.Context) bool {
	if p == nil || p.ollama == nil || p.servingEngine() != catalog.RuntimeOllama {
		return true
	}
	return p.engineIsQuiet(ctx)
}

// ensureHostSpeedMeasured returns this host's measurement, taking it if
// this install has not been measured yet. measured is false whenever
// no usable measurement could be reached — no engine, no probe model, no
// timing counters, a truncated prefill, a machine too busy to answer —
// and callers must carry on unchanged in that case. An unrun measurement
// is not evidence about the host.
//
// Once per INSTALL, not once per process and not once per host. Two
// halves decide it, and a mismatch in either re-measures:
//
//   - the ENGINE build. The counters come from the engine, and waired#668
//     is the standing lesson that a bundle bump silently keeps serving
//     pre-bump numbers.
//   - the AGENT build. Every install and every upgrade restarts the
//     daemon on a new version, so this is what makes the measurement an
//     install-time step (waired#1099) without needing anything to tell it
//     that an install happened.
//
// Single-flight through hostSpeedMeasureMu: the bootstrap's background
// call and a later pre-pull or setup call can overlap, and the second one
// waits and then reuses rather than measuring a host the first is still
// measuring.
//
// hostSpeedMu — the lock Status() waits on — is taken only at the two
// ends, never across the engine request. See the field comment: one mutex
// doing both jobs is what made a running measurement stall
// /waired/v1/inference/status for the length of the measurement.
func (p *agentInferenceProvider) ensureHostSpeedMeasured(ctx context.Context) (probe hostfit.HostProbe, measured bool) {
	if p == nil || p.ollama == nil {
		return hostfit.HostProbe{}, false
	}
	p.hostSpeedMeasureMu.Lock()
	defer p.hostSpeedMeasureMu.Unlock()

	engine := p.servingEngine()
	engineVersion := p.engineVersionFor(ctx, engine)

	p.hostSpeedMu.Lock()
	p.loadHostSpeedLocked()
	stored, storedBy := p.hostSpeed, p.hostSpeedAgentVersion
	p.hostSpeedMu.Unlock()

	cached, ok := hostSpeedStillApplies(stored, string(engine), engineVersion)
	if ok && storedBy != buildinfo.Version {
		// Same engine, different agent build: this is an upgrade, and an
		// upgrade re-measures. Logged rather than silent because it is the
		// one re-measure whose cause is not visible in the record's own
		// fields — EngineKind and EngineVersion both still match.
		p.logger.Info("host speed: re-measuring, this agent build has not measured this host",
			"measured_by", storedBy, "agent_version", buildinfo.Version)
		ok = false
	}
	if ok {
		p.logger.Info("host speed: reusing the measurement already taken by this install",
			"turn_seconds", fmt.Sprintf("%.1f", cached.TurnSeconds()),
			"engine_version", engineVersion)
		return cached, true
	}

	tag, err := p.hostCutoffProbeTag(ctx)
	if err != nil {
		p.logger.Info("host speed: skipping the measurement", "err", err)
		return hostfit.HostProbe{}, false
	}
	if err := p.ensureHostCutoffProbeModel(ctx, tag); err != nil {
		p.logger.Info("host speed: probe model unavailable; skipping the measurement",
			"model", hostfit.HostCutoffProbeModelID, "err", err)
		return hostfit.HostProbe{}, false
	}

	m, err := measureHostCutoff(ctx, hostCutoffDeps{
		BaseURL:     p.ollama.BaseURL(),
		EngineModel: tag,
		Logger:      p.logger,
		// Unique per run: a repeat that shares a prefix is answered from
		// the engine's KV cache at a rate no host can achieve.
		Nonce: fmt.Sprintf("hostcutoff-%d", time.Now().UnixNano()),
	})
	if err != nil {
		p.logger.Info("host speed: measurement did not complete; leaving local inference as configured",
			"err", err)
		return hostfit.HostProbe{}, false
	}
	if !m.Probe.Measured() {
		// Reached when the engine answered but the prefill was not the
		// depth asked for — the silent-truncation case Measured() guards.
		// Nothing is published: a truncated prefill measures the
		// truncation, and a consumer cannot tell that from a fast host.
		p.logger.Warn("host speed: the engine did not prefill the depth asked for; no measurement",
			"prompt_tokens", m.Probe.PromptTokens, "want_tokens", hostfit.HostCutoffProbeDepthTokens)
		return hostfit.HostProbe{}, false
	}
	p.logger.Info("host speed: measured",
		"turn_seconds", fmt.Sprintf("%.1f", m.Probe.TurnSeconds()),
		"budget_seconds", fmt.Sprintf("%.0f", hostfit.HostCutoffTurnBudgetSeconds),
		"prefill_tok_s", fmt.Sprintf("%.0f", m.Probe.PrefillTokps),
		"decode_tok_s", fmt.Sprintf("%.1f", m.Probe.DecodeTokps),
		"prompt_tokens", m.Probe.PromptTokens,
		"samples", m.Samples, "spread_pct", fmt.Sprintf("%.1f", m.SpreadPct))

	p.hostSpeedMu.Lock()
	p.hostSpeed = &signer.HostSpeed{
		ProbeModelID:  hostfit.HostCutoffProbeModelID,
		DepthTokens:   hostfit.HostCutoffProbeDepthTokens,
		PromptTokens:  m.Probe.PromptTokens,
		PrefillTokps:  m.Probe.PrefillTokps,
		DecodeTokps:   m.Probe.DecodeTokps,
		TurnSeconds:   m.Probe.TurnSeconds(),
		Method:        signer.BenchmarkMethodOllamaEval,
		Samples:       m.Samples,
		SpreadPct:     m.SpreadPct,
		EngineKind:    string(engine),
		EngineVersion: engineVersion,
		MeasuredAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	p.hostSpeedAgentVersion = buildinfo.Version
	p.persistHostSpeedLocked(false)
	p.hostSpeedMu.Unlock()
	return m.Probe, true
}

// hostSpeedStillApplies reports whether a stored measurement describes
// the engine running now. An empty stored version is not a match: it
// means the version could not be read when the figure was taken, and a
// figure that cannot say what produced it cannot be trusted to survive an
// engine bump (waired#668).
func hostSpeedStillApplies(s *signer.HostSpeed, engineKind, engineVersion string) (hostfit.HostProbe, bool) {
	if s == nil || s.EngineKind != engineKind || s.EngineVersion == "" || s.EngineVersion != engineVersion {
		return hostfit.HostProbe{}, false
	}
	probe := hostfit.HostProbe{
		PromptTokens: s.PromptTokens,
		PrefillTokps: s.PrefillTokps,
		DecodeTokps:  s.DecodeTokps,
	}
	if !probe.Measured() {
		return hostfit.HostProbe{}, false
	}
	return probe, true
}

// persistHostSpeedLocked writes the in-memory measurement to the state
// dir. Call with hostSpeedMu held.
//
// turnedInferenceOff is a claim about causation, so it is written only by
// the caller that actually turned local inference off, and only after it
// did: WriteDesiredInferenceState clears the flag, so asserting it first
// would assert it into a file the disable then rewrites.
func (p *agentInferenceProvider) persistHostSpeedLocked(turnedInferenceOff bool) {
	if p.stateDir == "" || p.hostSpeed == nil {
		return
	}
	rec := state.HostSpeedRecord{
		Measurement:        p.hostSpeed,
		TurnedInferenceOff: turnedInferenceOff,
		AgentVersion:       p.hostSpeedAgentVersion,
	}
	if !turnedInferenceOff {
		// A re-measure does not un-say why inference was turned off; only
		// someone moving the toggle does, and that goes through
		// WriteDesiredInferenceState.
		if prev, err := state.ReadHostSpeed(p.stateDir); err == nil {
			rec.TurnedInferenceOff = prev.TurnedInferenceOff
		}
	}
	if err := state.WriteHostSpeed(p.stateDir, rec); err != nil {
		p.logger.Warn("host speed: could not store the measurement", "err", err)
	}
}

// hostCutoffProbeTag resolves the probe model to the engine-native tag to
// measure on. It fails rather than substituting another model: the
// threshold is calibrated against this one, and a number measured on
// something else is not comparable to it.
func (p *agentInferenceProvider) hostCutoffProbeTag(ctx context.Context) (string, error) {
	manifest, ok := catalog.LookupByAlias(hostfit.HostCutoffProbeModelID, p.manifests)
	if !ok {
		return "", fmt.Errorf("probe model %s is not in this build's catalog", hostfit.HostCutoffProbeModelID)
	}
	engine := p.servingEngine()
	if engine != catalog.RuntimeOllama {
		// The counters the measurement reads are ollama's. vLLM's
		// OpenAI-compat surface does not expose them, and a vLLM host has
		// a GPU by construction — it is not the host this is looking for.
		return "", fmt.Errorf("serving engine is %s; the cutoff probe reads ollama's counters", engine)
	}
	variant, pullable := router.FirstPullableVariant(manifest, engine, p.engineVersionFor(ctx, engine))
	if !pullable || variant.Source.Tag == "" {
		return "", fmt.Errorf("no %s variant of %s this engine can load", engine, manifest.ModelID)
	}
	return variant.Source.Tag, nil
}

// ensureHostCutoffProbeModel gets the probe model onto the host and waits
// for it, because a measurement cannot start before the weights do.
//
// The ~1 GB it may download is not wasted on the host that fails: that is
// the same model the below-floor opt-in offers (quality_tier 12, the
// smallest entry the catalog offers), so a host told local inference is
// off already has the one model it can actually run sitting on disk.
func (p *agentInferenceProvider) ensureHostCutoffProbeModel(ctx context.Context, tag string) error {
	if p.engineServesTag(ctx, tag) {
		return nil
	}
	if _, err := p.PullModel(ctx, hostfit.HostCutoffProbeModelID); err != nil {
		return fmt.Errorf("pull probe model: %w", err)
	}
	deadline := time.Now().Add(hostCutoffPullTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hostCutoffPullPoll):
		}
		st, _ := p.store.Load()
		switch st.Models[hostfit.HostCutoffProbeModelID].State {
		case catalog.ModelStateReady:
			return nil
		case catalog.ModelStateFailed:
			return fmt.Errorf("probe model download failed")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("probe model did not finish downloading within %s", hostCutoffPullTimeout)
		}
	}
}

// hostCutoffIsStillOurs reports whether the local-inference toggle is
// unset — nobody, and nothing, has expressed a choice on this axis yet.
// Only then may the cutoff decide it.
//
// Without this the opt-in does not survive a restart. The cutoff turns
// local inference off by writing exactly the file `waired inference on`
// writes to; an operator opts back in, the daemon restarts, the bundled
// model still is not on disk, the pre-pull path runs again — and the
// cutoff measures the same slow host and takes their choice away. #465
// calls the off state "a default with a working opt-in", and an opt-in
// that is silently reverted on the next boot is neither.
//
// An unreadable state dir reads as "unset", which costs at most one
// re-measure; refusing to measure at all on a read error would be the
// worse failure, since a fresh install's file is legitimately absent.
func (p *agentInferenceProvider) hostCutoffIsStillOurs() bool {
	desired, err := state.ReadDesiredInferenceState(p.stateDir)
	if err != nil {
		p.logger.Warn("host cutoff: could not read the local-inference choice; measuring anyway", "err", err)
		return true
	}
	if desired != "" {
		p.logger.Info("host cutoff: skipped — the local-inference choice was already made here",
			"desired", string(desired))
		return false
	}
	return true
}

// applyHostCutoff is the whole cutoff as the pre-pull path sees it:
// measure, and on a host that does not clear the budget turn local
// inference off and report why. It returns whether the caller should go
// on to download the bundled model.
//
// Turning it OFF is a default, not a refusal: #465's opt-in
// (`waired inference on`, the tray, the browser wizard) works immediately
// afterwards and starts the engine, and the node enrols and relays
// either way. Which is exactly why the message has to name the opt-in.
func (p *agentInferenceProvider) applyHostCutoff(ctx context.Context) bool {
	// Measured first, and unconditionally. The figure is a host fact
	// worth publishing however the toggle happens to be set — the control
	// plane and waired#1065 want it from every host, not only from the
	// ones nobody has answered for — and it is what lets `waired inference
	// status` explain an off state at all. Only the DECISION below is
	// withheld when someone has already made it.
	probe, measured := p.ensureHostSpeedMeasured(ctx)
	if !p.hostCutoffIsStillOurs() {
		return true
	}
	if !measured {
		return true
	}
	if ok, decided := probe.MeetsRecommendedSpec(); !decided || ok {
		return true
	}
	p.logger.Warn("this host is below the recommended spec for local inference: one coding-agent turn "+
		"would take longer than the budget, on the smallest model there is. Local inference starts off "+
		"and this node runs as a gateway/relay — it can still route inference to mesh peers. "+
		"Turn it on anyway with `waired inference on`.",
		"budget_seconds", fmt.Sprintf("%.0f", hostfit.HostCutoffTurnBudgetSeconds))
	if p.disableInference == nil {
		// --disable-inference already took the subsystem out; there is no
		// controller to persist through and nothing to turn off.
		return false
	}
	if err := p.disableInference(); err != nil {
		// The download is still the wrong thing to start. Saying so
		// matters: without the persisted state the next boot will measure
		// again and reach the same answer, which is correct but silent.
		p.logger.Warn("host cutoff: could not persist local inference off; skipping the download anyway", "err", err)
		return false
	}
	// After the disable, never before: WriteDesiredInferenceState clears
	// this flag on the way past, because everything else that writes the
	// toggle is a different reason for it to read the way it does.
	p.hostSpeedMu.Lock()
	p.persistHostSpeedLocked(true)
	p.hostSpeedMu.Unlock()
	return false
}
