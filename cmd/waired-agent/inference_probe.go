package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// inferenceProbeDeps bundles the collaborators the local probe loop
// needs. Constructed once in main and shared by both the immediate
// initial probe and the ticker-driven loop.
type inferenceProbeDeps struct {
	StateWriter *state.Writer
	Aggregator  *inferencemesh.Aggregator // optional; nil = no diagnose snapshot
	PushClient  *controlclient.Client     // optional; nil = no CP push
	DeviceID    string
	MachineKey  ed25519.PrivateKey
	// CPCtx scopes the Control-Plane push only. It is the session context
	// minus "the CP still accepts us", so when auto-refresh gives up
	// (waired-agent#318) the push stops dead while the local half of this
	// loop — the state-file heartbeat and the mesh aggregator, which
	// nothing about a dead CP credential invalidates — keeps running on
	// the loop's own ctx. Nil falls back to that ctx.
	CPCtx context.Context
	// EngineDead, when non-nil and returning true, forces Reachable=false
	// regardless of what the HTTP probe saw. See the call site in tick for
	// why the probe alone is not enough, and why the predicate is
	// StateFailed-only.
	EngineDead func() bool

	// OnLocalReachable is called on the false->true edge of this node's
	// own reachability, and only on the edge (waired-agent#806).
	//
	// This loop is where that transition becomes observable: it is the
	// same s.Reachable the control plane's Public Share eligibility check
	// reads, so a consumer of this edge is hearing about the exact fact
	// that decides whether this node may consume public capacity.
	//
	// Must not block — it runs on the probe tick. The production
	// implementation is a non-blocking send onto a buffered-1 channel.
	// nil means nobody is listening, which is every path except the
	// public-grant acquirer.
	OnLocalReachable func()

	// Interval paces the loop after its first, immediate tick. Zero means
	// state.HeartbeatInterval, which is what production uses.
	//
	// A seam for the tests whose subject is what happens ACROSS ticks —
	// the reachability edge above is one — so they do not have to spend
	// five real seconds per tick to observe it. Matches publicGrantDeps'
	// own Tick field, and is the same shape.
	Interval time.Duration

	// EngineKind selects which probe runs each tick. Accepted values:
	// signer.InferenceTypeOllama, signer.InferenceTypeVLLM. Empty
	// string or signer.InferenceTypeNone short-circuits the loop
	// (same effect as Disabled=true) so no spurious "reachable=false
	// ollama" gets pushed when a vLLM-on-GPU host is up.
	EngineKind string
	// EnginePort is the loopback port the EngineKind subprocess
	// listens on. Mapped from cfg.Inference.ResolvedOllamaPort() or
	// cfg.Inference.ResolvedVLLMPort() at wiring time. 0 disables the probe.
	EnginePort int

	// IsShared, when non-nil and returning false, means the operator has
	// taken this host out of mesh serving. The push then carries
	// InferenceState.NotShared rather than being suppressed (waired#1030):
	// the control plane is what withholds the engine from other peers'
	// maps, and it can do that without also going blind to the device.
	//
	// Suppressing the push was the old implementation, and it did stop
	// peers — but the stored state does not clear, it freezes, so a device
	// whose operator ran `waired inference share off` showed the admin its
	// last report forever, then read as stale. A device that never shared
	// looked like it had no engine and the setup wizard scored its catalog
	// blind. Refusing peers' actual requests is unaffected and stays where
	// it always was: the overlay listener's shareGate, 503
	// waired_inference_not_shared.
	//
	// Nil means "sharing" (the Phase 4 default, and agentconfig's).
	IsShared func() bool

	// --- Phase 7 routing inputs --------------------------------------
	//
	// Hardware returns the host's GPU/RAM summary: what peers render as
	// "peer X: RTX 4090, 64 GB" in the tray, and what the control plane
	// scores the onboarding model catalog against. nil (or a nil return)
	// means "this host has nothing to say about itself" and keeps the
	// field off the wire — it is omitempty.
	//
	// A getter, read on every tick, rather than the boot-time pointer it
	// replaced (#387): what a host IS changes when a GPU or a driver is
	// installed, and the captured pointer kept reporting the pre-change
	// answer until the daemon restarted. Re-detection is throttled by the
	// profiler behind the getter (hardwareResampleInterval), so calling
	// it per tick costs a cache read almost every time.
	Hardware func() *signer.HardwareSummary

	// HostSpeed, when non-nil, returns what one coding-agent turn costs on
	// this machine (#496), or nil when nothing has been measured. It rides
	// both push paths for the same reason Hardware does: the browser setup
	// wizard scores its catalog in exactly the window where there is no
	// engine to probe, and a figure that only an engine-present host could
	// publish would be missing from the host most likely to need it.
	//
	// A getter rather than a captured value because the measurement lands
	// mid-process — the install path takes it just before the model
	// download — and a value captured at boot would be nil for the life of
	// the daemon that measured it.
	HostSpeed func() *signer.HostSpeed

	// Capacity is the concurrent-request admission cap the Phase 7
	// Selector enforces against. Derived from the local token/s
	// benchmark. 0 means "unlimited" (the backward-compat value, and
	// what a skipped benchmark reports), and the tick below only
	// overwrites the pushed field when non-zero.
	//
	// A getter, read on every tick, for the same reason as Hardware
	// above (#387): it was captured at boot, so a benchmark that failed
	// because the engine had not finished installing yet de-rated the
	// node to Capacity=1 for the life of the process — and #203's own
	// "the de-rating is indefinite" complaint was exactly that. Every
	// later measurement already lands in SetLastBench (the boot path in
	// main.go, and RunBenchmark for a CLI- or control-plane-triggered
	// run), so reading it live is all the lift needs.
	//
	// nil is allowed and reads as 0.
	Capacity func() int

	// RecommendedMaxParallel, when non-nil, returns the engine's current
	// VRAM-safe parallelism ceiling (from the applied ollama tuning). Reported
	// as advisory telemetry so the admin Device detail page can show it and warn
	// before an operator sets a higher concurrency. Read live each probe tick so
	// it tracks a re-tune; 0 (or nil) omits the field from the push.
	RecommendedMaxParallel func() int

	// DeclaredContextWindow, when non-nil, returns the input-token window
	// this node stands behind for its active model, or 0 for "declares
	// nothing" (waired#1031). Read live each tick so a re-tune or a model
	// switch moves it, exactly like RecommendedMaxParallel above.
	//
	// Unlike that one this is NOT advisory telemetry: the requesting side
	// of the mesh routes on it, which is why the getter reports the
	// APPLIED tuning only and reports 0 rather than a number below the
	// smallest declarable window (see agentInferenceProvider.
	// DeclaredContextWindow). Nil, or 0, keeps the field off the wire and
	// leaves every consumer on its pre-#1031 behaviour.
	DeclaredContextWindow func() int

	// ActiveModel and SubsystemState, when non-nil, answer the two
	// questions a peer's picker asks about this node (waired#1064):
	// which model it is committed to, in the catalog's namespace, and
	// why it is or is not serving it. Read live each tick, like the
	// getters above, so a model switch or a pull failure moves them.
	//
	// Unlike DeclaredContextWindow these are NOT gated on Models being
	// non-empty. narrowPublishedModels empties Models for a node mid-pull
	// or one whose engine has diverged, and describing exactly that case
	// is why these two exist — gating them would blank them precisely
	// when they carry the only information a peer has.
	//
	// Nil, or an empty return, keeps the field off the wire and leaves
	// consumers on their pre-#1064 behaviour (they fall back to the
	// engine tag in Models).
	ActiveModel    func() string
	SubsystemState func() string

	// RefreshResidency, when non-nil, is called once per probe tick to
	// record whether the engine currently holds weights in (V)RAM
	// (waired-agent#879). It rides this loop because the loop already
	// runs at the right cadence and already talks to the engine, whereas
	// the status surfaces that consume the answer are polled
	// independently by the tray, the CLI and the management API.
	//
	// Nil leaves residency unobserved, which the surfaces render as "no
	// claim" rather than as "not resident".
	RefreshResidency func(context.Context)

	// LocalModelChoiceAt, when non-nil, answers when a person at this
	// machine last chose a model — see the wire field of the same name.
	// Read live each tick, like the getters above, because the answer can
	// arrive at any time through the loopback management API.
	//
	// Nil, or an empty return, keeps the field off the wire, which is the
	// "no claim" case every consumer must already handle.
	LocalModelChoiceAt func() string

	// Residency and LocalResidencyChoiceAt are the upward half of model
	// residency (waired#1232): the setting this host actually has, and
	// when a person here last set one. The control plane orders the two
	// against its own instruction and moves the instruction when the local
	// choice is newer, which is what stops a device set locally being
	// described by a stale instruction forever.
	//
	// Read live each tick, like the getters above. Residency in particular
	// must resolve the SERVING engine at call time rather than at wiring
	// time: #339 lets a host adopt a different engine after boot, and a
	// decision frozen at wiring would keep publishing the old engine's
	// answer (waired-agent#948).
	//
	// Nil, or an empty return, keeps the field off the wire — "no claim",
	// which every consumer must already handle. Note that a vLLM host is
	// NOT such a case: it holds the model until the engine exits, so it
	// reports "0s" (waired-agent#943).
	Residency              func() string
	LocalResidencyChoiceAt func() string

	// ModelMeasurements is what this host has actually run and timed, one
	// entry per model (waired-agent#970). It is what lets the control
	// plane reach the same conclusion the device reached in #784: a model
	// measured below the floor stops being the one this machine
	// recommends to itself.
	//
	// Read live each tick, like the getters above, because a benchmark
	// that finishes between two ticks has to reach the control plane on
	// the next one — the whole point is that the browser page stops
	// recommending a model this machine has already rejected.
	//
	// Nil, or an empty return, keeps the field off the wire: "no claim",
	// which is every host that has not benchmarked and every fresh
	// install.
	ModelMeasurements func() []signer.ModelMeasurement

	// ServingEngineVersion is the version of the engine this host serves
	// with, reported whether or not it has ever benchmarked
	// (waired-agent#970).
	//
	// That "whether or not" is the point. The only engine version on the
	// wire before it rode HostSpeed, so it was absent for every device
	// that never ran the probe — which is why the control plane's
	// engine-floor check has to fail open where this agent's fails
	// closed. Reporting it directly is what shrinks that population.
	//
	// Resolved at call time for the reason Residency is: a host can adopt
	// a different engine after boot, and a version frozen at wiring time
	// would keep describing the old one.
	ServingEngineVersion func() string

	// EngineTags returns the two engine-side names for this node's Active
	// selection:
	//
	//   - advertise: the name peers may ask this node for (Ollama
	//     /api/tags name, or vLLM /v1/models id). When non-empty,
	//     runLocalInferenceProbe enforces the "1 agent = 1 model"
	//     invariant by narrowing the published Models list to just this
	//     tag; surplus tags pulled locally are stripped from the
	//     network-map advertisement (the engine itself still serves them —
	//     this only affects what peers see).
	//   - serving: the tag this node's own engine actually loaded. Equal to
	//     advertise except when a #642 derived batch model is in use, where
	//     the engine serves `<base>-wb<batch>` while peers are told the
	//     base tag (waired-agent#324). The probe needs it to recognise the
	//     engine's own report of the active model, and to keep the derived
	//     name out of the "surplus models" warning.
	//
	// Both empty when no Active selection is set (fresh agent,
	// pre-model-pull).
	//
	// A getter, read on every tick, rather than the boot-time pair it
	// replaced (#656) — the same correction #387 made to Hardware and
	// Capacity above. The Active selection it reads is committed
	// asynchronously, after the probe loop is wired, and since #812 a model
	// choice no longer restarts the agent; a pair captured at boot
	// therefore stayed empty for the life of a daemon that started before
	// its first model landed. Empty skips the narrowing entirely (see
	// narrowPublishedModels), so the host advertised every tag on disk —
	// host-speed probe model included — with no way to recover.
	//
	// One getter returning both rather than two, so a tick cannot pair an
	// advertise name from before a model switch with a serving name from
	// after it: narrowPublishedModels reads the two together and would take
	// a torn pair for a diverged engine, withdrawing the advertisement.
	//
	// nil, or an empty advertise name, keeps that skip: the probe result
	// passes through unmodified.
	EngineTags func() (advertise, serving string)

	Disabled bool
	Logger   *slog.Logger
}

// runLocalInferenceProbe is the agent-side feeder for the
// InferenceReachableLocal flag the `waired claude` wrapper consults,
// AND for Phase 3's mesh-aggregation push:
//
//   - probes the local engine (ollama on /api/tags or vLLM on /health
//   - /v1/models, selected by EngineKind) at HeartbeatInterval
//   - writes the boolean result into runtime/state (for the wrapper
//     hot path)
//   - feeds the full InferenceState into the in-memory aggregator
//     (for diagnose / tray / mgmt API consumers)
//   - pushes the same InferenceState to the Control Plane so peers
//     see it via their network map (Phase 3)
//
// Disabled=true OR EngineKind in {"", "none"} OR EnginePort==0 pin
// the runtime/state flag to false and skip both aggregator updates
// and CP pushes — the device is intentionally engine-less, so peers
// see no entry rather than a misleading reachable=false ping.
// cpCtx returns the context to use for Control-Plane pushes, falling
// back to the loop's own context when the caller did not scope them
// separately.
func (d inferenceProbeDeps) cpCtx(fallback context.Context) context.Context {
	if d.CPCtx != nil {
		return d.CPCtx
	}
	return fallback
}

// capacityFn is the inferenceProbeDeps.Capacity getter. It prefers the
// provider's live answer so a benchmark that runs after boot lifts the
// advertised cap; boot is the fallback for the paths that have no provider
// (--disable-inference, an unenrolled daemon), where it is 0 anyway.
//
// A host that HAS a provider and no measurement yet advertises
// unmeasuredCapacity rather than 0, so the figure peers read agrees with the
// one this host enforces (waired-agent#738). Both are the same claim — "one
// at a time until I know" — and publishing 0 while enforcing 1 would put the
// same quantity on the wire twice with two answers, which is the shape
// waired-agent#713 was about.
//
// The no-provider return stays 0 on purpose: that is a host with no engine
// at all, and RunBootBenchmark's skip paths document 0 as the right encoding
// for "no admission cap" there.
func capacityFn(boot int, sub *inferenceSubsystem) func() int {
	if sub != nil && sub.provider != nil {
		prov := sub.provider
		return func() int {
			if c := prov.AdvertisedCapacity(); c != 0 {
				return c
			}
			if boot != 0 {
				return boot
			}
			return unmeasuredCapacity
		}
	}
	return func() int { return boot }
}

func runLocalInferenceProbe(ctx context.Context, deps inferenceProbeDeps) {
	if deps.StateWriter == nil {
		return
	}
	if deps.Disabled || deps.EnginePort == 0 || !engineKindProbable(deps.EngineKind) {
		_ = deps.StateWriter.SetInferenceReachableLocal(false)
		_ = deps.StateWriter.SetInferenceReachableInMesh(false)
		if deps.Aggregator != nil {
			deps.Aggregator.UpdateLocal(nil)
		}
		// There is no engine to probe, but there is still a machine to
		// describe — and until #387 nothing described it. Blocks on ctx
		// like the probe loop below.
		runHardwareOnlyReport(ctx, deps)
		return
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", deps.EnginePort)
	probe := func() signer.InferenceState {
		switch deps.EngineKind {
		case signer.InferenceTypeVLLM:
			return probeLocalVLLM(ctx, baseURL, time.Second)
		default:
			// #879: record residency alongside reachability. Ollama only
			// — vLLM holds its pool from launch to process exit, so there
			// is no residency axis to report there.
			if deps.RefreshResidency != nil {
				deps.RefreshResidency(ctx)
			}
			return probeLocalOllama(ctx, baseURL, time.Second)
		}
	}

	// lastSurplusSig dedups the "surplus locally-pulled models" warning
	// across probe ticks (every state.HeartbeatInterval). Without dedup,
	// an operator with two ollama tags pulled would see the warning
	// every 15 s. The signature is the sorted union of {surplus tags,
	// mismatch indicator}; it changes only when the underlying state
	// changes.
	var lastSurplusSig string

	// wasReachable carries this node's own reachability across ticks so
	// the false->true EDGE can be reported (waired-agent#806). Starts
	// false, so a node that comes up reachable reports one edge on its
	// first tick — which is the fresh-install case the signal exists for.
	// A node that goes on being reachable reports nothing.
	wasReachable := false

	tick := func() {
		s := probe()
		// waired-agent#29: the HTTP probe cannot see a dead model runner —
		// ollama's parent keeps answering /api/tags with 200 — so without
		// this the node keeps advertising itself to mesh peers as a healthy
		// inference target that 500s every request. Fail-open: nil, or "not
		// sure", keeps the probe's own verdict, and the wired predicate is
		// StateFailed-ONLY. It must not fire during a normal boot, because
		// flipping InferenceReachableLocal false also degrades the `waired
		// claude` wrapper and the transparent proxy.
		if deps.EngineDead != nil && deps.EngineDead() {
			s.Reachable = false
			if s.LastError == "" {
				s.LastError = "engine model runner is not serving"
			}
		}
		// Re-read here rather than captured at boot — see the field's doc
		// comment (#656, and #387 for the same correction to the getters
		// below).
		var advertiseTag, servingTag string
		if deps.EngineTags != nil {
			advertiseTag, servingTag = deps.EngineTags()
		}
		narrowPublishedModels(&s, advertiseTag, servingTag, &lastSurplusSig, deps.Logger)
		// Phase 7: decorate the probe result with Hardware and Capacity.
		// Both are omitempty on the wire so a zero-value agent (no
		// hardware probe, no benchmark) still produces a compact push.
		// Hardware is re-read here rather than captured at boot — see the
		// field's doc comment (#387).
		if deps.Hardware != nil {
			if hw := deps.Hardware(); hw != nil {
				s.Hardware = hw
			}
		}
		if deps.HostSpeed != nil {
			s.HostSpeed = deps.HostSpeed()
		}
		if deps.Capacity != nil {
			if c := deps.Capacity(); c != 0 {
				s.Capacity = c
			}
		}
		if deps.RecommendedMaxParallel != nil {
			if n := deps.RecommendedMaxParallel(); n > 0 {
				s.RecommendedMaxParallel = n
			}
		}
		// waired#1031: the window this node stands behind. Set AFTER
		// narrowPublishedModels, because the two have to agree — a node
		// that just withdrew its model advertisement is not serving
		// anything, so it may not be carrying a window for it either.
		if deps.DeclaredContextWindow != nil && len(s.Models) > 0 {
			if w := deps.DeclaredContextWindow(); w > 0 {
				s.ContextWindow = w
			}
		}
		// waired#1064: what this node runs and why it is or is not
		// serving it. Deliberately NOT behind the len(s.Models) > 0 gate
		// above — narrowPublishedModels just emptied Models for the node
		// that is mid-pull, mid-switch or wedged, and explaining that node
		// is the entire reason these two are on the wire. Before them, a
		// peer's picker could only render every one of those as a bare
		// "unavailable".
		if deps.ActiveModel != nil {
			s.ActiveModel = deps.ActiveModel()
		}
		if deps.SubsystemState != nil {
			s.SubsystemState = deps.SubsystemState()
		}
		// waired-agent#647: when a person here last answered the model
		// question. Ungated for the same reason as the two above — a host
		// that just demoted away from the model it was told to run is
		// mid-switch, which is exactly when Models is empty and exactly
		// the case the control plane needs to hear about.
		if deps.LocalModelChoiceAt != nil {
			s.LocalModelChoiceAt = deps.LocalModelChoiceAt()
		}
		// waired#1232: what this host's residency actually is, and when a
		// person here last set one. Ungated for the same reason as the
		// fields above, and push-only — effectiveInferenceState strips
		// both from the served map, so no peer ever re-marshals them.
		if deps.Residency != nil {
			s.ResidencyIdleTimeout = deps.Residency()
		}
		if deps.LocalResidencyChoiceAt != nil {
			s.LocalResidencyChoiceAt = deps.LocalResidencyChoiceAt()
		}
		// waired-agent#970: what this host measured, and the engine it
		// serves with. Ungated for the reason the fields above are — a
		// host mid-switch has an empty Models and is exactly the host
		// whose measurements explain why — and push-only:
		// effectiveInferenceState strips both from the served map, so no
		// peer ever re-marshals them.
		if deps.ModelMeasurements != nil {
			s.ModelMeasurements = deps.ModelMeasurements()
		}
		if deps.ServingEngineVersion != nil {
			s.ServingEngineVersion = deps.ServingEngineVersion()
		}
		// Set before the aggregator sees it, so the on-host diagnose view
		// describes the same node the control plane is told about
		// (waired#1030). omitempty keeps a sharing host's push byte-identical.
		s.NotShared = deps.IsShared != nil && !deps.IsShared()
		if err := deps.StateWriter.SetInferenceReachableLocal(s.Reachable); err != nil && deps.Logger != nil {
			deps.Logger.Warn("inference reachability write failed", "err", err)
		}
		// Announced beside the write, not instead of it: the write is the
		// value, this is the transition, and only the transition tells a
		// listener that a condition it was waiting out has resolved.
		if s.Reachable && !wasReachable && deps.OnLocalReachable != nil {
			deps.OnLocalReachable()
		}
		wasReachable = s.Reachable
		if deps.Aggregator != nil {
			deps.Aggregator.UpdateLocal(&s)
			// Phase 4: also publish the peers-only mesh aggregate to
			// runtime/state so the wrapper's Stage 1 gate can OR in
			// the mesh axis without crossing a process boundary.
			snap := deps.Aggregator.Snapshot()
			if err := deps.StateWriter.SetInferenceReachableInMesh(snap.Reachable); err != nil && deps.Logger != nil {
				deps.Logger.Warn("inference mesh reachability write failed", "err", err)
			}
		}
		if deps.PushClient != nil && deps.DeviceID != "" && len(deps.MachineKey) == ed25519.PrivateKeySize {
			pushCtx, cancel := context.WithTimeout(deps.cpCtx(ctx), 5*time.Second)
			_, err := deps.PushClient.PushInferenceStatus(pushCtx, deps.DeviceID, s, deps.MachineKey)
			cancel()
			if err != nil && deps.Logger != nil && !errors.Is(err, context.Canceled) {
				deps.Logger.Warn("inference status push failed", "err", err)
			}
		}
	}

	tick()

	interval := deps.Interval
	if interval <= 0 {
		interval = state.HeartbeatInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// hardwareOnlyPushInterval paces runHardwareOnlyReport. Deliberately
// slower than state.HeartbeatInterval, which paces the probe: the content
// is a description of the machine, so re-pushing it is a retry (for a CP
// that was unreachable, or a device row that was recreated), not a
// heartbeat. A permanently engine-less host therefore does not double its
// push volume, and nothing downstream needs the freshness — the admin UI
// resolves a type=none state before it ever consults last_check.
const hardwareOnlyPushInterval = 60 * time.Second

// runHardwareOnlyReport publishes the host profile from a device that has
// no engine to probe (#387).
//
// Until this existed, HardwareSummary rode ONLY the probe result, and the
// loop above returns before pushing anything when there is no engine —
// so a host that had not decided on an engine, was still installing one,
// or whose engine failed to start had no path at all to tell the control
// plane what it is. That is exactly the window the browser setup wizard
// operates in: with no hardware stored, the per-device catalog reports
// every model runnable with unknown_hardware and recommends none.
//
// What is published is an honest description of that host — type=none,
// reachable=false, no endpoint, no models — carrying only the hardware.
// Peers read it as "no engine here", which is what the aggregator already
// does with any non-reachable entry, and the admin UI renders it exactly
// as it renders a device that has never pushed at all.
//
// Two cases stay silent:
//
//   - Disabled: --disable-inference means this host is not participating.
//   - No profile: a host that cannot profile itself has nothing to say,
//     the same rule hardwareSummaryFor applies by returning nil.
//
// Mesh sharing is NOT one of them any more (waired#1030): a host that is
// not sharing says so on the state and reports anyway. The summary no
// longer rides the served map for such a device — the control plane
// withholds its whole InferenceState from peers — so reporting widens
// nothing, and withholding it left exactly the hosts the setup wizard
// needs to score invisible to it.
func runHardwareOnlyReport(ctx context.Context, deps inferenceProbeDeps) {
	if deps.Disabled || deps.Hardware == nil || deps.PushClient == nil ||
		deps.DeviceID == "" || len(deps.MachineKey) != ed25519.PrivateKeySize {
		return
	}

	push := func() {
		hw := deps.Hardware()
		if hw == nil {
			return
		}
		st := signer.InferenceState{
			Type:      signer.InferenceTypeNone,
			Reachable: false,
			LastCheck: time.Now().UTC().Format(time.RFC3339Nano),
			Hardware:  hw,
			NotShared: deps.IsShared != nil && !deps.IsShared(),
		}
		// The engine-less host has an answer for this too, and it is the
		// one worth having: no_engine says why, where Type=none only says
		// what. It never reaches a peer picker (Type=none is filtered out
		// as a candidate), but the admin Device page reads the same field.
		if deps.SubsystemState != nil {
			st.SubsystemState = deps.SubsystemState()
		}
		if deps.HostSpeed != nil {
			st.HostSpeed = deps.HostSpeed()
		}
		// A host with no engine right now can still have measured models
		// before — a stopped engine, a failed converge — and the control
		// plane ranks its catalog page either way. Reported here for the
		// same reason HostSpeed is (waired-agent#970).
		//
		// ServingEngineVersion is NOT reported on this path: there is no
		// serving engine to name, and an empty string is the honest
		// answer rather than a stale one.
		if deps.ModelMeasurements != nil {
			st.ModelMeasurements = deps.ModelMeasurements()
		}
		pushCtx, cancel := context.WithTimeout(deps.cpCtx(ctx), 5*time.Second)
		_, err := deps.PushClient.PushInferenceStatus(pushCtx, deps.DeviceID, st, deps.MachineKey)
		cancel()
		if err != nil && deps.Logger != nil && !errors.Is(err, context.Canceled) {
			deps.Logger.Warn("hardware profile push failed", "err", err)
		}
	}

	push()

	t := time.NewTicker(hardwareOnlyPushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			push()
		}
	}
}

// engineKindProbable reports whether the EngineKind is one this
// package knows how to probe. signer.InferenceTypeNone is excluded
// deliberately so a "no engine" host short-circuits to the disabled
// branch rather than running an unused ollama probe against port 0.
func engineKindProbable(kind string) bool {
	switch kind {
	case signer.InferenceTypeOllama, signer.InferenceTypeVLLM:
		return true
	}
	return false
}

// ollamaTagsResponse is the relevant subset of /api/tags. We only
// pull names; sizes / modified timestamps would bloat the network
// map without serving any current consumer.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
		// Size is the on-disk blob size; the #621 tuning verification
		// uses it as the weight baseline for its KV-size heuristic.
		Size int64 `json:"size"`
	} `json:"models"`
}

// probeLocalOllama issues a single GET /api/tags against the local
// engine. On a 2xx response it parses the model list (best-effort —
// a non-JSON body still counts as reachable but yields nil Models).
// Any 2xx-4xx HTTP status counts as "reachable" — the precise model
// list does not matter for the wrapper gate, only the fact that
// something is listening and answering HTTP. Network errors, dials,
// and timeouts all map to reachable=false with the error captured
// in LastError.
func probeLocalOllama(parent context.Context, baseURL string, timeout time.Duration) signer.InferenceState {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := signer.InferenceState{
		Type:      signer.InferenceTypeOllama,
		Endpoint:  baseURL,
		LastCheck: now,
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		out.LastError = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return out
	}
	out.Reachable = true
	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err == nil {
			var tags ollamaTagsResponse
			if json.Unmarshal(body, &tags) == nil {
				for _, m := range tags.Models {
					if m.Name != "" {
						out.Models = append(out.Models, m.Name)
					}
				}
			}
		}
	}
	return out
}

// openAIModelsResponse is the relevant subset of vLLM's (and any
// OpenAI-compatible server's) /v1/models payload — only the ids
// are kept; vLLM populates `owned_by` and `object` but they bloat
// the network map without any consumer.
type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeLocalVLLM issues GET /health for the alive verdict and (on
// success) GET /v1/models to discover served names. Reachable
// follows the /health result alone — vLLM flips /health green once
// the model is loaded and ready, so a 2xx there is enough to call
// the engine usable. /v1/models is best-effort: a failure leaves
// Models nil but does not flip Reachable back to false. Failure of
// /health (dial error / 5xx) maps to Reachable=false with LastError
// populated, matching the ollama probe shape so the rest of the
// pipeline does not branch on engine kind.
func probeLocalVLLM(parent context.Context, baseURL string, timeout time.Duration) signer.InferenceState {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := signer.InferenceState{
		Type:      signer.InferenceTypeVLLM,
		Endpoint:  baseURL,
		LastCheck: now,
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Step 1: /health — vLLM's authoritative readiness signal.
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	hresp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode < 200 || hresp.StatusCode >= 300 {
		out.LastError = fmt.Sprintf("HTTP %d on /health", hresp.StatusCode)
		return out
	}
	out.Reachable = true

	// Step 2: /v1/models — best-effort enumeration. vLLM always returns
	// at least one entry (the --served-model-name) when /health is
	// green, but defensively tolerate missing / malformed bodies.
	mreq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return out
	}
	mresp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		return out
	}
	defer mresp.Body.Close()
	if mresp.StatusCode < 200 || mresp.StatusCode >= 300 {
		return out
	}
	body, err := io.ReadAll(io.LimitReader(mresp.Body, 64*1024))
	if err != nil {
		return out
	}
	var models openAIModelsResponse
	if err := json.Unmarshal(body, &models); err != nil {
		return out
	}
	for _, m := range models.Data {
		if m.ID != "" {
			out.Models = append(out.Models, m.ID)
		}
	}
	return out
}

// narrowPublishedModels enforces the "1 agent = 1 model" invariant on
// the InferenceState the agent broadcasts to peers. When advertise is
// non-empty the published Models list is forced to a single-element
// {advertise} — but ONLY once the engine is confirmed to be serving
// it. This prevents operator misconfiguration (extra `ollama pull`
// runs leaving surplus tags around) from leaking into Selector's
// candidate set as variants the agent isn't actually serving.
//
// advertise vs serving: they differ when a #642 derived batch model is
// in use, where the engine loads `<base>-wb<batch>` but peers must be
// told the base tag — no consumer want set can ever contain a derived
// name (waired-agent#324). Either name in the engine's report proves
// the model is loaded, and neither counts as surplus.
//
// The agent used to publish {advertise} unconditionally, "optimistic
// for the loading case". That advertised a model to the whole mesh
// while it was still downloading, and consumers catching that window
// routed there and got a 404 / model_not_ready back. Publishing
// nothing until the engine confirms is the honest answer: an agent
// with no models simply is not a candidate.
//
// When advertise is empty (fresh agent before any model pull) the
// probe result passes through unmodified — the Selector falls back
// to its pre-Phase-7 behaviour.
//
// Side-effect: emits a warn on the supplied logger when the engine's
// reported tags don't match the Active selection. The dedup key
// (*lastSurplusSig) suppresses the same warning across consecutive
// probe ticks (every state.HeartbeatInterval, otherwise noisy).
// Pass a fresh `var sig string` for tests; pass a closure-scoped one
// from runLocalInferenceProbe for production.
func narrowPublishedModels(s *signer.InferenceState, advertise, serving string, lastSurplusSig *string, logger *slog.Logger) {
	if s == nil {
		return
	}
	if advertise == "" {
		// Nothing to enforce — let the probe result through unmodified.
		// Reset the dedup so a subsequent transition back to "Active set"
		// re-emits the first warn.
		if lastSurplusSig != nil {
			*lastSurplusSig = ""
		}
		return
	}

	reported := s.Models
	matched := false
	surplus := make([]string, 0, len(reported))
	for _, m := range reported {
		switch m {
		case "":
			continue
		case advertise, serving:
			// The engine is serving the active model. It reports the
			// derived tag when one is in use and the base tag when the
			// base is also pulled; either proves the model is loaded.
			matched = true
		default:
			surplus = append(surplus, m)
		}
	}
	sort.Strings(surplus)

	// Compose the dedup signature: surplus tags + whether the active
	// model was served + whether the engine reported anything at all.
	// Three distinct misconfiguration shapes (surplus only,
	// active-missing, engine-empty) should each emit their own
	// warn/info, and the signature must differ from the zero value so
	// the first tick actually fires.
	sig := strings.Join(surplus, ",")
	switch {
	case !matched && len(reported) == 0:
		sig = "empty"
	case !matched && len(reported) > 0:
		sig = "missing|" + sig
	case matched && len(surplus) > 0:
		sig = "surplus|" + sig
	default:
		// matched + no surplus is the steady state; no warn to dedup.
		sig = ""
	}

	if logger != nil && lastSurplusSig != nil && sig != *lastSurplusSig {
		switch {
		case len(surplus) > 0 && matched:
			logger.Warn("agent has surplus locally-pulled models; design is 1 agent = 1 model",
				"advertised_tag", advertise,
				"surplus", surplus,
				"hint", "remove the extras (e.g. 'ollama rm <tag>') so peers see a consistent advertisement")
		case len(surplus) > 0 && !matched:
			logger.Warn("active model not served by engine; advertising nothing to peers",
				"advertised_tag", advertise,
				"serving_tag", serving,
				"engine_reports", surplus,
				"hint", "engine state has diverged from the agent's Active selection — restart or re-pull")
		case len(reported) == 0:
			logger.Info("engine has not reported the active model yet; advertising nothing to peers",
				"advertised_tag", advertise,
				"serving_tag", serving)
		}
	}
	if lastSurplusSig != nil {
		*lastSurplusSig = sig
	}

	if !matched {
		// Not loaded (still pulling, wedged setup, diverged engine
		// state). Say so rather than inviting requests that will 404.
		s.Models = nil
		return
	}
	// Final invariant: publish exactly the advertised name, regardless
	// of what else the engine returned. Defensive for the misconfigured
	// case; canonical for the derived-batch-model case.
	s.Models = []string{advertise}
}
