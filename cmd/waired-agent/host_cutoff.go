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
	"net/http"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hostspeed"
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
	//
	// 4 minutes, down from 20 (waired-agent#579). 1 GB in 4 minutes is
	// ~34 Mbit/s; at the old ceiling it was ~6.8 Mbit/s, at which a 45 GB
	// model takes fifteen hours — so 20 minutes never met the bar its own
	// sentence above sets. What made the old figure look free is that this
	// is a WAIT, not the transfer: PullModel puts the download on the
	// daemon's own context (inference.go, backgroundCtx), so cutting the
	// wait does not cancel it. The weights keep landing, and the next
	// daemon start finds engineServesTag already true and skips this phase.
	hostCutoffPullPoll    = 2 * time.Second
	hostCutoffPullTimeout = 4 * time.Minute

	// hostSpeedMeasureDeadline bounds ONE ensureHostSpeedMeasured call end
	// to end — the probe download wait plus the measurement — and is the
	// whole of waired-agent#579 in one number.
	//
	// Before it, nothing bounded that call. hostCutoffMeasureBudget was
	// consulted only between samples, so the calibration and sample 1 could
	// each run for hostCutoffProbeTimeout, and measureHostCutoffSample
	// retries: roughly 50 minutes of requests behind a 20-minute download
	// wait. The bundled model's own download waits behind all of it
	// (inference_prepull_hold.go: applyHostCutoff, then dispatch), and on
	// the macOS runner that meant the pull was dispatched one second after
	// a 9m29s measurement ended and completed in 11.5 s. It was never
	// slow; it was never started.
	//
	// 16 minutes = hostCutoffPullTimeout + hostCutoffMeasureBudget, pinned
	// by TestHostSpeedBudgets_PartitionTheInstallWindow. It is 1.7x the
	// slowest measurement anyone has recorded.
	//
	// It is the BACKGROUND window — startHostSpeedMeasurement, which has
	// nothing waiting behind it. A caller with a download behind it gets
	// hostSpeedInstallWindow instead, which is smaller than this on
	// purpose: 16 minutes was first written against hostSpeedAskWait
	// (20 min, cmd/waired/init_host_speed.go), and that wait is not the
	// binding one. confirmHostSpeedBudget returns immediately when the
	// operator passed --inference-enabled (init_host_speed.go), so what
	// actually stands in front of the operator is hostspeed.ModelWait —
	// ten minutes, which 16 does not fit inside.
	hostSpeedMeasureDeadline = hostCutoffPullTimeout + hostCutoffMeasureBudget

	// hostSpeedSettlePoll / hostSpeedSettleWait pace awaitQuietEngine. The
	// wait is generous because what it is usually waiting for is the
	// operator's own model download, which is the whole reason this host
	// has an engine at all; the measurement is the least urgent thing
	// running and should say so by yielding. A var so tests do not wait.
	hostSpeedSettlePoll = 2 * time.Second
)

// hostSpeedSettleWait bounds awaitQuietEngine. A var, not a const, so a
// test can shrink it.
//
// Deliberately NOT bounded by hostSpeedMeasureDeadline: awaitQuietEngine
// sits in front of nothing. It runs only on the boot goroutine, and on the
// branch that reaches it the bundled pre-pull is not behind it — the
// pre-pull path calls applyHostCutoff directly. Waiting an hour for the
// operator's own download to finish is the measurement yielding, which is
// what that wait is for.
var hostSpeedSettleWait = 60 * time.Minute

// remeasureTiming paces the retry loop behind remeasureForActiveModel
// (waired-agent#821): how long it keeps trying, how often it re-asks
// whether the engine is quiet, and how long it waits after a run the
// engine gates declined anyway.
//
// A FIELD on the provider rather than package vars, and that is not a
// style choice. The loop outlives the call that started it, so a test
// writing a package var in its Cleanup writes it under a goroutine another
// test is still running — a real data race, and one the CI race detector
// caught before this shipped. hostSpeedWindow states the same rule for the
// same reason ("a field, not a package var, so the tests that shrink it
// stay parallel-safe"). Zero fields mean the defaults below.
type remeasureTiming struct {
	window time.Duration
	poll   time.Duration
	retry  time.Duration
}

// The defaults. Deliberately NOT hostSpeedSettleWait's knobs, for the
// reason awaitScreenQuiet gives for not sharing awaitQuietEngine's: the two
// waits are the same shape for different reasons, and one knob could only
// be right for one of them. Ten minutes because what this one has to
// outlast is a download that has ALREADY finished plus the serve reconcile
// that finishing fired — minutes, not the hour a boot-time host-speed
// measurement may spend yielding to a 45 GB pull it sits in front of
// nothing for.
//
// The retry pause is longer than the poll because the two guard different
// things. The poll is "ask again whether the host is busy"; the pause is
// what keeps a run the job declines for a reason this wait cannot see —
// EngineReady, which engineQuietForBench does not test — from spinning
// through the whole window in a tight loop.
const (
	remeasureSettleWait = 10 * time.Minute
	remeasureSettlePoll = 2 * time.Second
	remeasureRetryPause = 5 * time.Second
)

// remeasureTimers is this provider's timing with the defaults filled in.
func (p *agentInferenceProvider) remeasureTimers() remeasureTiming {
	t := p.remeasure
	if t.window <= 0 {
		t.window = remeasureSettleWait
	}
	if t.poll <= 0 {
		t.poll = remeasureSettlePoll
	}
	if t.retry <= 0 {
		t.retry = remeasureRetryPause
	}
	return t
}

// benchQuietNow reports whether the benchmark's own gates would admit a run
// right now.
//
// Two conditions, because the job has two gates and clearing one of them
// alone would still be declined by the other:
//
//   - engineQuietForBench is BenchDeps.EngineQuiet — the pull registry, a
//     pending or running serve reconcile, a parked engine, serving traffic,
//     health. Nil-safe at the non-ollama end, so a vLLM host answers quiet
//     immediately rather than waiting out the whole bound for conditions
//     that cannot apply to it.
//   - engineExclusiveHeld is what BenchDeps.EngineClaim refuses on. The
//     other measurement on this host holds it for its whole run.
//
// The claim is only READ here, never taken: taking it would race the job's
// own EngineClaim (claimEngineForBench takes the same flag) and make the
// benchmark stand down on its own caller. That is the same split
// engineIsQuiet documents, and the reason engineIsQuietAndUnclaimed exists
// separately on the host-speed side.
//
// A predicate and not a wait loop, so the one loop that owns this —
// remeasureWhenQuiet — re-asks whether the measurement is still WANTED on
// every pass rather than only on either side of a wait that can run for
// minutes.
func (p *agentInferenceProvider) benchQuietNow(ctx context.Context) bool {
	return p.engineQuietForBench(ctx) && !p.engineExclusiveHeld()
}

// hostSpeedMeasureWindow is hostSpeedMeasureDeadline, or the provider's
// override when a test set one. Matches the prePullHoldMax idiom (a field,
// not a package var) so the tests that shrink it stay parallel-safe.
func (p *agentInferenceProvider) hostSpeedMeasureWindow() time.Duration {
	if p != nil && p.hostSpeedWindow > 0 {
		return p.hostSpeedWindow
	}
	return hostSpeedMeasureDeadline
}

// hostSpeedInstallWindow is the window for a caller that has a model
// DOWNLOAD waiting behind it — the pre-pull hold and the browser wizard's
// apply path. See hostspeed.InstallWindow for the size and why.
//
// The distinction is the whole of the second half of waired-agent#579.
// Stage 2 bounded the measurement and the bound held: on run 31316731884
// the linux pre-pull released at 14:28:49 and the model was dispatched at
// 14:45:11 — 16 minutes, exactly hostSpeedMeasureDeadline. The download
// then took 21.9 seconds, and init had stopped waiting at minute ten. One
// window cannot serve both callers, because they are not waiting for the
// same thing: the background call wants the published median of three
// samples, and this one wants a verdict before 20-45 GB starts arriving.
//
// The minimum, not a replacement, so a test that shrinks hostSpeedWindow
// still shrinks this one — otherwise the install path would keep the full
// five minutes in a unit test that asked for milliseconds.
func (p *agentInferenceProvider) hostSpeedInstallWindow() time.Duration {
	if w := p.hostSpeedMeasureWindow(); w < hostspeed.InstallWindow {
		return w
	}
	return hostspeed.InstallWindow
}

// hostSpeedVerdict is this host's answer to "can this machine serve local
// inference usefully", together with what the answer rests on.
//
// It exists because there are now two shapes an answer can arrive in, and
// only one of them is a hostfit.HostProbe the policy package can judge
// (waired-agent#579 Stage 3): a full-depth measurement, and a
// prefill-only lower bound taken at ~2.8k tokens that is deliberately NOT
// Measured(). Callers used to be handed the probe and re-derive the
// verdict; a screen verdict would have read as "nothing measured" at
// every one of them.
type hostSpeedVerdict struct {
	// Decided is whether anything was learned about this host at all.
	// False means carry on unchanged — no engine, no probe model, no
	// timing counters, a truncated prefill, a machine too busy to answer.
	// An unrun measurement is not evidence about the host.
	Decided bool

	// MeetsBudget is whether the host clears
	// hostfit.HostCutoffTurnBudgetSeconds. Meaningless unless Decided.
	MeetsBudget bool

	// TurnSeconds is the measured turn and is zero under Method
	// BenchmarkMethodOllamaPrefillFloor, where nothing measured one;
	// TurnFloorSeconds is the lower bound and is set under both. For log
	// lines and the management API — MeetsBudget is the decision.
	TurnSeconds      float64
	TurnFloorSeconds float64
	Method           string
}

// hostSpeedVerdictOf reads a published measurement back as the verdict it
// stands for.
//
// One function for both directions — the figure just taken and the figure
// read off disk a boot later — so a measurement cannot mean one thing
// when it is written and another when it is loaded.
//
// ok=false means the record cannot support a verdict and the host should
// be measured again.
func hostSpeedVerdictOf(s *signer.HostSpeed) (hostSpeedVerdict, bool) {
	if s == nil {
		return hostSpeedVerdict{}, false
	}
	if s.Method == signer.BenchmarkMethodOllamaPrefillFloor {
		// A bound, not a measurement. The agent emits this shape only once
		// the bound is already past the budget (screenHostCutoffOnce), so a
		// record claiming it while sitting at or under the budget is
		// self-contradictory and gets re-measured rather than believed.
		if s.TurnFloorSeconds <= hostfit.HostCutoffTurnBudgetSeconds {
			return hostSpeedVerdict{}, false
		}
		return hostSpeedVerdict{
			Decided:          true,
			MeetsBudget:      false,
			TurnFloorSeconds: s.TurnFloorSeconds,
			Method:           s.Method,
		}, true
	}
	// Every other method — including the empty one written by builds
	// before the screen existed — is a full-depth measurement, and the
	// policy in proto/hostfit judges it.
	probe := hostfit.HostProbe{
		PromptTokens: s.PromptTokens,
		PrefillTokps: s.PrefillTokps,
		DecodeTokps:  s.DecodeTokps,
	}
	meets, decided := probe.MeetsRecommendedSpec()
	if !decided {
		return hostSpeedVerdict{}, false
	}
	method := s.Method
	if method == "" {
		method = signer.BenchmarkMethodOllamaEval
	}
	return hostSpeedVerdict{
		Decided:          true,
		MeetsBudget:      meets,
		TurnSeconds:      probe.TurnSeconds(),
		TurnFloorSeconds: hostfit.TurnFloorSeconds(probe.PrefillTokps),
		Method:           method,
	}, true
}

// logArgs renders the verdict for a daemon log line. Both figures travel,
// because which one is present is the difference between "this host takes
// 68 s" and "this host takes at least 210 s and nobody waited to find out
// the rest".
func (v hostSpeedVerdict) logArgs() []any {
	return []any{
		"turn_seconds", fmt.Sprintf("%.1f", v.TurnSeconds),
		"turn_floor_seconds", fmt.Sprintf("%.1f", v.TurnFloorSeconds),
		"budget_seconds", fmt.Sprintf("%.0f", hostfit.HostCutoffTurnBudgetSeconds),
		"method", v.Method,
	}
}

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

// noteHostSpeedStage records how far the measurement has got, for the
// setup-progress reporter (waired#1143). Report only.
func (p *agentInferenceProvider) noteHostSpeedStage(stage hostSpeedStage, detail string) {
	if p == nil {
		return
	}
	p.hostSpeedMu.Lock()
	defer p.hostSpeedMu.Unlock()
	p.hostSpeedStage, p.hostSpeedStageDetail = stage, detail
}

// setupHostSpeedProgress reports the measurement's stage to the
// setup-progress reporter (waired#1143).
//
// A stored figure counts as measured whatever this process has done. That
// is the whole reason the two are read under one lock: the measurement runs
// from the engine bootstrap behind awaitQuietEngine, which is bounded by
// hostSpeedSettleWait, so a daemon restart on a host that was set up weeks
// ago would otherwise report an unstarted measurement for up to an hour —
// and `pending` rows deny setup_complete on a computer that is finished.
//
// A LIVE stage always wins over that substitution, and the condition below
// is what makes it: the figure only stands in when nothing is under way.
// Do not simplify this to "a stored figure means measured".
//
// `waired init` step 6 depends on the distinction (waired-agent#703). It
// asks for a fresh measurement and waits for one, and it tells "still
// going" from "stopped without producing a figure" by this stage — none of
// which is visible in the stored figure, because a re-measurement leaves it
// untouched until it publishes. Collapsing the live stage would make a
// running re-measurement and a failed one both report `measured` on a host
// that has an old figure, and step 6 would lose its only signal to stop
// waiting: it would spend the whole hostSpeedAskWait budget on a
// measurement that had already given up.
func (p *agentInferenceProvider) setupHostSpeedProgress() hostSpeedProgress {
	if p == nil {
		return hostSpeedProgress{}
	}
	p.hostSpeedMu.Lock()
	defer p.hostSpeedMu.Unlock()
	p.loadHostSpeedLocked()
	if p.hostSpeedStage == hostSpeedStageNone && p.hostSpeed != nil {
		return hostSpeedProgress{Stage: hostSpeedStageMeasured}
	}
	return hostSpeedProgress{Stage: p.hostSpeedStage, Detail: p.hostSpeedStageDetail}
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
		p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow())
	}()
}

// awaitQuietEngine blocks until nothing else on this host is using the
// engine, and reports whether it got there. Bounded by
// hostSpeedSettleWait — a host still downloading a 45 GB model after that
// is one whose measurement can wait for the next boot.
func (p *agentInferenceProvider) awaitQuietEngine(ctx context.Context) bool {
	deadline := time.Now().Add(hostSpeedSettleWait)
	for {
		if p.engineIsQuietAndUnclaimed(ctx) {
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

// claimEngineExclusive takes the engine for a measurement that
// monopolises it, and reports whether it got it. The release is always
// non-nil, so a caller can defer it without checking ok.
//
// Never blocks. See the engineExclusive field comment: the two callers
// yield in different ways and both already have theirs.
func (p *agentInferenceProvider) claimEngineExclusive() (release func(), ok bool) {
	if p == nil || !p.engineExclusive.CompareAndSwap(false, true) {
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { p.engineExclusive.Store(false) }) }, true
}

// engineExclusiveHeld reports whether the other measurement has the
// engine right now.
func (p *agentInferenceProvider) engineExclusiveHeld() bool {
	return p != nil && p.engineExclusive.Load()
}

// servingInFlight reports how many inference requests this machine is
// serving. Zero when nothing is wired (unit tests, a host with no
// inference server), which is the truth for those hosts.
func (p *agentInferenceProvider) servingInFlight() int {
	if p == nil || p.servingInflight == nil {
		return 0
	}
	return p.servingInflight()
}

// servingAdmittedCount is servingInFlight's cumulative twin, read on
// both sides of the probe. See inference.Server.AdmittedCount.
func (p *agentInferenceProvider) servingAdmittedCount() uint64 {
	if p == nil || p.servingAdmitted == nil {
		return 0
	}
	return p.servingAdmitted()
}

// engineIsQuiet reports whether the engine is ready and nothing else is
// about to take it away.
//
// A pending reconcile counts as busy, not just a running one: reconcile
// STOPS AND RESTARTS the engine, so starting a minutes-long measurement
// while one is queued behind a finishing pull is how the measurement gets
// its connection refused.
//
// Serving traffic counts as busy too, and that is waired-agent#703. This
// predicate used to read four things — a pull, a reconcile, a parked
// engine, health — and not one of them was a REQUEST. Since
// infruntime.MaxResidentModels the engine holds one model at a time, so a
// request arriving mid-measurement no longer merely competes with a
// measurement: the two EVICT each other, at a measured ~8 s to reload the
// probe and ~13 s to reload a 4B serving model. A host measured at
// 12.017 s published 39.473 s that way, the same contended-host signature
// waired#1140 documents.
//
// The exclusive claim is deliberately NOT read here. Both measurements
// hold it for their whole run and both re-ask this question while they
// do — the benchmark once per bounce-grace retry, the screen from inside
// measureHostCutoff — so a predicate that read the claim would make each
// of them stand down on itself. engineIsQuietAndUnclaimed is the variant
// for the one caller that has not taken it yet.
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
	if p.servingInFlight() > 0 {
		return false
	}
	return p.ollama.Health(ctx).State == infruntime.StateReady
}

// engineIsQuietAndUnclaimed is engineIsQuiet plus "and the other
// measurement does not have the engine", for awaitQuietEngine.
//
// It saves a wait, it does not make one safe: the claim is what keeps the
// two measurements apart, and the caller takes it after this returns.
// What this buys is that a host-speed measurement arriving during a
// benchmark spends its settle window waiting instead of failing to claim
// and giving up on the boot.
func (p *agentInferenceProvider) engineIsQuietAndUnclaimed(ctx context.Context) bool {
	if p.engineExclusiveHeld() {
		return false
	}
	return p.engineIsQuiet(ctx)
}

// residentOnAnAdoptedEngine names a model already loaded in an engine this
// agent did not spawn, and reports whether there is one.
//
// It answers only for adopted engines because that is the only case the
// question changes anything. infruntime.MaxResidentModels rides the serve
// environment, so on an engine this agent started the probe's own request
// evicts what is loaded and the reading is clean whatever /api/ps says here.
// An adopted engine keeps the environment of the run that spawned it.
//
// Residency is deliberately NOT part of engineIsQuiet: that is a wait loop,
// and a serving model held at OLLAMA_KEEP_ALIVE=60m would make every host
// with a model wait out hostSpeedSettleWait and never measure. This is a
// check, like host_memory.go's engineListening.
func (p *agentInferenceProvider) residentOnAnAdoptedEngine(ctx context.Context) (string, bool) {
	if p == nil || p.ollama == nil {
		return "", false
	}
	mode := p.ollama.Mode()
	if mode != infruntime.EngineModeAdopted {
		// Nothing /api/ps could say would change the answer, so do not ask.
		return "", false
	}
	var ps psResponse
	if err := getJSON(ctx, &http.Client{}, p.ollama.BaseURL()+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		// Unreadable /api/ps is not evidence of residency, and refusing to
		// measure on it would turn a transient engine hiccup into a host
		// that never publishes a figure at all.
		return "", false
	}
	names := make([]string, 0, len(ps.Models))
	for _, m := range ps.Models {
		names = append(names, m.Name)
	}
	return residentBlocksMeasurement(mode, names)
}

// residentBlocksMeasurement is the decision inside residentOnAnAdoptedEngine,
// separated from the /api/ps read so both engine ownerships are testable
// without one — an adapter only learns it adopted an engine by finding a real
// orphan at EnsureRunning time.
func residentBlocksMeasurement(mode infruntime.EngineMode, resident []string) (string, bool) {
	if mode != infruntime.EngineModeAdopted {
		return "", false
	}
	for _, name := range resident {
		if name != "" {
			return name, true
		}
	}
	return "", false
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
//
// Delegating to engineIsQuiet is what gives the benchmark the
// serving-traffic condition (waired-agent#703) without a second copy of
// the rule. What keeps it away from the OTHER measurement is EngineClaim,
// not this — see engineIsQuiet's note on why the claim is not read here.
func (p *agentInferenceProvider) engineQuietForBench(ctx context.Context) bool {
	if p == nil || p.ollama == nil || p.servingEngine() != catalog.RuntimeOllama {
		return true
	}
	return p.engineIsQuiet(ctx)
}

// claimEngineForBench is BenchDeps.EngineClaim. Same nil-end reasoning as
// engineQuietForBench: a host this provider does not drive with ollama
// has nothing here to collide with, so it is handed the engine rather
// than gated off its own benchmark forever.
func (p *agentInferenceProvider) claimEngineForBench() (release func(), ok bool) {
	if p == nil || p.ollama == nil || p.servingEngine() != catalog.RuntimeOllama {
		return func() {}, true
	}
	return p.claimEngineExclusive()
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
// window is what THIS caller can spare, not a property of the
// measurement: hostSpeedMeasureWindow from the boot goroutine, which has
// nothing behind it, and hostSpeedInstallWindow from the two paths with a
// model download behind them (waired-agent#579).
func (p *agentInferenceProvider) ensureHostSpeedMeasured(ctx context.Context, window time.Duration) hostSpeedVerdict {
	if p == nil || p.ollama == nil {
		return hostSpeedVerdict{}
	}
	// Anchored BEFORE the single-flight lock, on purpose (waired-agent#579).
	//
	// Both the boot goroutine and the pre-pull path call this, and on the
	// ordinary install path they overlap: the second one blocks on the
	// mutex for the whole of the first one's measurement and only then
	// checks the cache. A deadline started after the lock would give that
	// caller a fresh window on top of the one it just waited out, so the
	// bound would not be a bound — the thing the pull is waiting behind
	// could still take twice as long as this says.
	notAfter := time.Now().Add(window)

	p.hostSpeedMeasureMu.Lock()
	defer p.hostSpeedMeasureMu.Unlock()

	// On the PARENT ctx, not the deadline below, and that is load-bearing:
	// engineVersionFor returns "" on a cancelled ctx, hostSpeedStillApplies
	// rejects an empty version, and a caller that waited out the window
	// would then discard the measurement the first caller had just
	// published and re-measure it. Pinned by
	// TestEnsureHostSpeedMeasured_AQueuedCallerStillReadsThePublishedMeasurement.
	engine := p.servingEngine()
	engineVersion := p.engineVersionFor(ctx, engine)

	p.hostSpeedMu.Lock()
	p.loadHostSpeedLocked()
	stored, storedBy := p.hostSpeed, p.hostSpeedAgentVersion
	p.hostSpeedMu.Unlock()

	cached, ok := hostSpeedStillApplies(stored, string(engine), engineVersion)
	if !ok && stored != nil {
		// A stored measurement that no longer applies. Logged because the
		// silence here was total: the two arms that DO log are the cache
		// hit just below and the agent-upgrade re-measure just after, so a
		// host that threw away a good measurement and paid for another one
		// looked exactly like a host measuring for the first time.
		//
		// That is how waired-agent#637 was found from the outside rather
		// than from a log: an install measured twice, eight seconds apart,
		// on the same engine build, and the only evidence was which lines
		// were ABSENT. Every field the decision reads goes on the line, so
		// the next occurrence names its own cause.
		p.logger.Info("host speed: the stored measurement does not apply; measuring again",
			"stored_engine_kind", stored.EngineKind, "engine_kind", string(engine),
			"stored_engine_version", stored.EngineVersion, "engine_version", engineVersion,
			"stored_method", stored.Method,
			"stored_prompt_tokens", stored.PromptTokens,
			"stored_turn_seconds", fmt.Sprintf("%.1f", stored.TurnSeconds),
			"stored_turn_floor_seconds", fmt.Sprintf("%.1f", stored.TurnFloorSeconds))
	}
	// An install-flow re-run asked for a fresh figure (Remeasure). Consumed
	// unconditionally — before the arms below can short-circuit past it — so
	// one request cannot latch this host into re-measuring on every boot: a
	// call that finds nothing stored measures anyway, and would otherwise
	// leave the flag standing for the next one.
	forced := p.hostSpeedForce.CompareAndSwap(true, false)
	if ok && forced {
		p.logger.Info("host speed: re-measuring, the install flow asked for a fresh figure",
			"stored_turn_seconds", fmt.Sprintf("%.1f", stored.TurnSeconds),
			"agent_version", buildinfo.Version)
		ok = false
	}
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
			append(cached.logArgs(), "engine_version", engineVersion)...)
		return cached
	}

	// A resident model this probe cannot displace makes the reading describe
	// the residency instead of the host, so keep what we have and try on a
	// later boot (waired#1139). Same answer host_memory.go gives to the same
	// question, and for the same reason: "a resident model is never charged
	// against the very host that serves it".
	//
	// Only ADOPTED engines reach this. On an engine this agent spawned,
	// infruntime.MaxResidentModels is in its environment and the probe's own
	// request evicts whatever is loaded — that is what makes the reading
	// clean, measured at 4.4376 s capped against 40.9954 s uncapped on the
	// same host (waired-agent#644). An adopted engine was started by a
	// previous run and its environment is not ours to set, which is the same
	// limit waired-agent#320 records for OLLAMA_KEEP_ALIVE.
	if resident, busy := p.residentOnAnAdoptedEngine(ctx); busy {
		p.logger.Info("host speed: an adopted engine is holding a model this measurement cannot displace; "+
			"keeping the previous measurement and trying on a later start",
			"resident_model", resident, "engine_version", engineVersion)
		return cached
	}

	// Everything from here down is the work the install waits behind, so
	// everything from here down is bounded. Applied after the cache read
	// for the reason stated above it.
	ctx, cancel := context.WithDeadline(ctx, notAfter)
	defer cancel()

	tag, err := p.hostCutoffProbeTag(ctx)
	if err != nil {
		p.logger.Info("host speed: skipping the measurement", "err", err)
		return hostSpeedVerdict{}
	}
	// From here the work is visible to the wizard (waired#1143). Above this
	// line nothing is reported: a host with no engine this probe can drive
	// has no measurement to describe, and a row it would leave at `pending`
	// forever would deny setup_complete to a computer that is otherwise
	// finished.
	p.noteHostSpeedStage(hostSpeedStagePullingProbe, "")
	if err := p.ensureHostCutoffProbeModel(ctx, tag); err != nil {
		p.logger.Info("host speed: probe model unavailable; skipping the measurement",
			"model", hostfit.HostCutoffProbeModelID, "err", err)
		p.noteHostSpeedStage(hostSpeedStageProbeFailed, err.Error())
		return hostSpeedVerdict{}
	}
	p.noteHostSpeedStage(hostSpeedStageMeasuring, "")

	// Put the serving model back afterwards. infruntime.MaxResidentModels is
	// what makes this measurement honest, and the way it does that is by
	// having the probe EVICT whatever was loaded; the probe then unloads
	// itself with keep_alive:0, so a host that was warm before this call is
	// cold after it and the next real request pays a multi-GB load — the
	// exact cost waired-agent#320 exists to remove.
	//
	// Deferred, so it also covers the arms below that return on an error
	// after the probe has already run. Cheap where nothing was evicted:
	// warmServingModel is single-flight and reads /api/ps before deciding to
	// load anything.
	defer p.warmServingModel()

	// Take the engine. Registered AFTER the re-warm above so LIFO releases
	// the claim first and the warm-up can take it in turn — a warm-up that
	// ran under this claim would be skipped, and the model would stay cold
	// until the next request paid for it.
	//
	// Declining rather than waiting, and the gap this covers is not small:
	// awaitQuietEngine asked its question before the probe model's own
	// download, which is minutes on a cold host. A benchmark that started
	// inside that is a benchmark most of the way through, and queueing
	// behind it would spend the install window waiting.
	//
	// The stage goes terminal because it has to: PullingProbe/Measuring are
	// already reported, and a setup row left at `running` on a boot that
	// will not come back to it denies setup_complete to a computer that is
	// otherwise finished (waired#1143).
	//
	// The force flag is PUT BACK. It was consumed above, and losing it here
	// would mean an install-flow re-run asked for a fresh figure, lost the
	// engine to a benchmark, and then quietly went on reusing the stored
	// one for the life of the install (waired-agent#599). Restoring it does
	// not latch the host into re-measuring forever — the next start
	// consumes it, measures, and clears it.
	releaseEngine, gotEngine := p.claimEngineExclusive()
	if !gotEngine {
		p.logger.Info("host speed: another measurement has the engine; "+
			"keeping the previous measurement and trying on a later start",
			"asked_for_a_fresh_figure", forced)
		p.noteHostSpeedStage(hostSpeedStageMeasureFailed,
			"another measurement had the engine")
		if forced {
			p.hostSpeedForce.Store(true)
		}
		return cached
	}
	defer releaseEngine()

	// Read on both sides of the probe. engineIsQuiet answered for the
	// instant awaitQuietEngine asked; this answers for the whole window,
	// which is the only question that covers a request arriving after the
	// gate passed (waired-agent#703).
	admittedBefore := p.servingAdmittedCount()

	m, err := measureHostCutoff(ctx, hostCutoffDeps{
		BaseURL:     p.ollama.BaseURL(),
		EngineModel: tag,
		Logger:      p.logger,
		// Nil in production, which leaves postOllamaGenerate on
		// http.DefaultClient exactly as before. A fixture sets it so the
		// engine it stands up can tell this provider's measurement from
		// other traffic that reaches the same loopback port
		// (waired-agent#932).
		HTTPClient: p.hostCutoffClient,
		// Unique per run: a repeat that shares a prefix is answered from
		// the engine's KV cache at a rate no host can achieve.
		Nonce: fmt.Sprintf("hostcutoff-%d", time.Now().UnixNano()),
		// Only the screen consults these, and only as a precondition for
		// concluding without a full-depth sample (waired-agent#579).
		EngineQuiet: p.engineQuietForBench,
		EngineGen:   p.engineProcessGen,
	})
	if err != nil {
		p.logger.Info("host speed: measurement did not complete; leaving local inference as configured",
			"err", err)
		p.noteHostSpeedStage(hostSpeedStageMeasureFailed, err.Error())
		return hostSpeedVerdict{}
	}
	// This machine served something while it was being measured, so what
	// came back describes the two of them sharing an engine that holds one
	// model at a time. Not published, and the reason is the one the
	// truncated-prefill arm below gives: a consumer cannot tell a contended
	// reading from a slow host — on real hardware the difference was
	// 12.017 s against 39.473 s, with the SPREAD across samples unchanged
	// at 1.78% and 2.70%, so nothing in the numbers themselves says which
	// is which (waired-agent#703).
	//
	// The stored figure survives and the next start tries again, the same
	// answer the adopted-engine arm above gives to the same question.
	if admittedAfter := p.servingAdmittedCount(); admittedAfter != admittedBefore {
		p.logger.Info("host speed: this host served inference while it was being measured; "+
			"discarding the reading and trying on a later start",
			"requests_served", admittedAfter-admittedBefore,
			"discarded_turn_seconds", fmt.Sprintf("%.1f", m.Probe.TurnSeconds()))
		p.noteHostSpeedStage(hostSpeedStageMeasureFailed,
			"this host served inference while it was being measured")
		return hostSpeedVerdict{}
	}
	if m.Method != signer.BenchmarkMethodOllamaPrefillFloor && !m.Probe.Measured() {
		// Reached when the engine answered but the prefill was not the
		// depth asked for — the silent-truncation case Measured() guards.
		// Nothing is published: a truncated prefill measures the
		// truncation, and a consumer cannot tell that from a fast host.
		//
		// The screen arm is exempt because it is never at that depth by
		// construction; its own guards ran in screenHostCutoffOnce, and
		// they are stricter — two readings, one engine process, an idle
		// host, and a bound already past the budget with margin.
		p.logger.Warn("host speed: the engine did not prefill the depth asked for; no measurement",
			"prompt_tokens", m.Probe.PromptTokens, "want_tokens", hostfit.HostCutoffProbeDepthTokens)
		p.noteHostSpeedStage(hostSpeedStageMeasureFailed,
			fmt.Sprintf("the engine prefilled %d tokens, not the %d asked for",
				m.Probe.PromptTokens, hostfit.HostCutoffProbeDepthTokens))
		return hostSpeedVerdict{}
	}

	// Read the engine version AGAIN, now that the measurement is done, and
	// keep whichever read produced one.
	//
	// The version is provenance for the record, and the moment it is most
	// likely to be readable is after a serving engine has just answered
	// requests for a minute or more — not at the top of this call, which
	// on the boot path can land while the engine is still coming up. All
	// three sources can miss there: the adapter has not recorded a version
	// yet, the profiler's snapshot is cold, and probedOllamaVersion
	// MEMOISES a failed exec for engineVersionMemoTTL.
	//
	// A record published with no version is not merely incomplete: it can
	// never be reused, because hostSpeedStillApplies rejects an empty
	// version by design (waired#668 — a figure that cannot say what
	// produced it must not survive an engine bump). So it re-measures on
	// every daemon start, forever, until something happens to overwrite it
	// with a better one. Measured on real hardware at ~82 s per start
	// (waired-agent#637), and nothing in the code guarantees the
	// overwrite ever comes.
	if engineVersion == "" {
		if v := p.engineVersionFor(ctx, engine); v != "" {
			p.logger.Info("host speed: the engine version was unreadable when the measurement "+
				"started and readable when it finished; recording the later one",
				"engine_version", v)
			engineVersion = v
		}
	}
	if engineVersion == "" {
		// Publishing it anyway: the figure is still the best thing this
		// host knows about itself, and withholding it would leave the
		// admin page and `waired inference status` blank without stopping
		// the re-measure — the next start would take the same reading and
		// fail to record it the same way. Said out loud instead, because
		// the cost is real and recurring.
		p.logger.Warn("host speed: measured, but this engine will not say what version it is; "+
			"the measurement cannot be reused and this host will measure again on every start",
			"engine_kind", string(engine))
	}

	published := &signer.HostSpeed{
		ProbeModelID: hostfit.HostCutoffProbeModelID,
		DepthTokens:  hostfit.HostCutoffProbeDepthTokens,
		PromptTokens: m.Probe.PromptTokens,
		PrefillTokps: m.Probe.PrefillTokps,
		DecodeTokps:  m.Probe.DecodeTokps,
		// Zero on the screen arm, and not by a branch here: HostProbe's own
		// TurnSeconds returns 0 unless Measured(), and a screen probe is
		// never Measured(). That is the owner ruling on #620 holding at the
		// producer — TurnSeconds stays a measurement wherever it appears,
		// and the bound travels in its own field.
		TurnSeconds:      m.Probe.TurnSeconds(),
		TurnFloorSeconds: m.TurnFloorSeconds,
		Method:           m.Method,
		Samples:          m.Samples,
		SpreadPct:        m.SpreadPct,
		EngineKind:       string(engine),
		EngineVersion:    engineVersion,
		MeasuredAt:       time.Now().UTC().Format(time.RFC3339Nano),
	}
	// Read back through the same function that reads it off disk a boot
	// later, so what this call returns and what the next boot concludes
	// cannot drift apart.
	verdict, ok := hostSpeedVerdictOf(published)
	if !ok {
		p.logger.Warn("host speed: the measurement does not support a verdict; not publishing it",
			"method", m.Method, "prompt_tokens", m.Probe.PromptTokens,
			"turn_floor_seconds", fmt.Sprintf("%.1f", m.TurnFloorSeconds))
		p.noteHostSpeedStage(hostSpeedStageMeasureFailed,
			fmt.Sprintf("the reading does not support a verdict (method %s)", m.Method))
		return hostSpeedVerdict{}
	}
	p.logger.Info("host speed: measured",
		append(verdict.logArgs(),
			"prefill_tok_s", fmt.Sprintf("%.0f", m.Probe.PrefillTokps),
			"decode_tok_s", fmt.Sprintf("%.1f", m.Probe.DecodeTokps),
			"prompt_tokens", m.Probe.PromptTokens,
			"samples", m.Samples, "spread_pct", fmt.Sprintf("%.1f", m.SpreadPct),
			// Only the LAST measurement survives in the state dir, so when a
			// host measures more than once the log is the only place the two
			// can be compared — and the engine version is the field the
			// reuse decision turns on (waired-agent#637).
			"engine_version", engineVersion)...)

	p.hostSpeedMu.Lock()
	p.hostSpeed = published
	p.hostSpeedAgentVersion = buildinfo.Version
	p.hostSpeedStage, p.hostSpeedStageDetail = hostSpeedStageMeasured, ""
	p.persistHostSpeedLocked(false)
	p.hostSpeedMu.Unlock()
	p.hostSpeedTakenHere.Store(true)
	return verdict
}

// Remeasure re-takes the install-time measurement for a re-run of the
// install flow (management.HostSpeedController, waired-agent#599). It
// returns as soon as the work is admitted — the measurement is minutes of
// engine time, and the caller polls the status route for the figure.
//
// A process that has already measured declines, and that is the whole of
// the "same host, twice, in one install" guard: on a fresh install the
// engine bootstrap measures seconds before `waired init` reaches step 6, so
// the ask arrives against a figure this daemon just took. A re-run days
// later reaches a daemon whose boot found a usable stored figure and
// measured nothing, so the ask goes through.
func (p *agentInferenceProvider) Remeasure(ctx context.Context) bool {
	if p == nil || p.ollama == nil {
		return false
	}
	if p.hostSpeedTakenHere.Load() {
		p.logger.Info("host speed: the install flow asked for a fresh figure; " +
			"reusing the one this daemon already took")
		return false
	}
	p.hostSpeedForce.Store(true)
	// Not the caller's context: net/http cancels a request context the
	// moment the response is written, and this outlives the response by
	// minutes (the same rule PullModel states for downloads).
	p.startHostSpeedMeasurement(p.backgroundCtx())
	return true
}

// hostSpeedStillApplies reports whether a stored measurement describes
// the engine running now. An empty stored version is not a match: it
// means the version could not be read when the figure was taken, and a
// figure that cannot say what produced it cannot be trusted to survive an
// engine bump (waired#668).
func hostSpeedStillApplies(s *signer.HostSpeed, engineKind, engineVersion string) (hostSpeedVerdict, bool) {
	if s == nil || s.EngineKind != engineKind || s.EngineVersion == "" || s.EngineVersion != engineVersion {
		return hostSpeedVerdict{}, false
	}
	// What the record MEANS is hostSpeedVerdictOf's question, not this
	// one's. Before the screen existed the two were the same test, because
	// a stored figure supported a verdict exactly when its probe was
	// Measured(); a stored bound is not Measured() and supports one
	// anyway, so a Measured() check here would have re-measured every slow
	// host on every boot (waired-agent#579 Stage 3).
	return hostSpeedVerdictOf(s)
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
		if err := ctx.Err(); err != nil {
			// Named rather than returned bare: this is the arm a slow link
			// now takes, and `context deadline exceeded` in a daemon log
			// says nothing about which deadline or why it exists.
			return fmt.Errorf("probe model did not arrive inside the install window (%s): %w",
				p.hostSpeedMeasureWindow(), err)
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
// desiredInferenceStateSet reports whether the local-inference toggle has
// been written on this host at all — the same read hostCutoffIsStillOurs
// makes below, without the log line, for the callers that only need the fact.
//
// An unreadable file answers "not set", which is the conservative direction
// here: it makes a caller treat the state as a default rather than as
// somebody's answer, and the only caller that acts on it (install-flow step 6,
// waired#1142) then asks rather than assuming.
func (p *agentInferenceProvider) desiredInferenceStateSet() bool {
	if p == nil {
		return false
	}
	written, err := state.ReadDesiredInferenceState(p.stateDir)
	return err == nil && written != ""
}

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
	//
	// On the INSTALL window, not the background one: a 20-45 GB download
	// is waiting on this return, and a verdict that arrives after init has
	// stopped waiting has cost the operator their first run to decide
	// nothing (waired-agent#579). Handing the window back is the right
	// answer when the host cannot be measured in it — the background
	// goroutine keeps measuring on the full budget, and publishes.
	verdict := p.ensureHostSpeedMeasured(ctx, p.hostSpeedInstallWindow())
	if !p.hostCutoffIsStillOurs() {
		return true
	}
	if !verdict.Decided {
		// Say so. This is the arm a host takes when the measurement could
		// not finish — and after #579 bounded that measurement, it is the
		// arm the SLOWEST hosts take, which are the ones the cutoff exists
		// for. Silence here is what made "the cutoff was wrong" and "the
		// cutoff never ran" indistinguishable in a daemon log.
		p.logger.Info("host cutoff: no measurement, so no verdict; downloading the model as configured",
			"window", p.hostSpeedInstallWindow())
		return true
	}
	if verdict.MeetsBudget {
		p.logger.Info("host cutoff: this host clears the budget", verdict.logArgs()...)
		return true
	}
	p.logger.Warn("this host is below the recommended spec for local inference: one coding-agent turn "+
		"would take longer than the budget, on the smallest model there is. Local inference starts off "+
		"and this node runs as a gateway/relay — it can still route inference to mesh peers. "+
		"Turn it on anyway with `waired inference on`.",
		verdict.logArgs()...)
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
