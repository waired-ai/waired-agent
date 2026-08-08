package signer

// InferenceState captures the operator-visible state of one device's
// local inference engine. It travels three places:
//
//   - agent → CP push body (POST /v1/devices/self/inference-status)
//   - Spanner Device.inference_state JSON column
//   - NetworkMapPeer.InferenceState (long-poll distribution to peers)
//
// Defining it in signer (a leaf package) keeps the wire format the
// single source of truth: store imports it for persistence, api uses
// it to receive pushes, controlclient uses it to send them.
//
// Timestamps are RFC3339Nano strings rather than time.Time so the
// canonical-JSON form is byte-deterministic across marshalling
// libraries (NetworkMap is signed; non-deterministic time formatting
// would break verification).
type InferenceState struct {
	// Reachable is the agent's last probe verdict for its local
	// engine. The wrapper's Phase-3 gate uses self only via the
	// runtime/state file, but peers consume this field to compute
	// the mesh-wide aggregate.
	Reachable bool `json:"reachable"`

	// Type is the engine kind. One of InferenceTypeOllama,
	// InferenceTypeVLLM, or InferenceTypeNone. Other values are
	// rejected by the API validator.
	Type string `json:"type"`

	// Endpoint is the local HTTP base URL the agent's gateway
	// proxies to (e.g., "http://127.0.0.1:11434"). Loopback by
	// design for v0; broadcast to the mesh as informational only —
	// peers cannot dial another peer's loopback. Future
	// peer-engine routing (Phase 4) will replace this with an
	// overlay-IP listener.
	Endpoint string `json:"endpoint"`

	// Models is the list of engine-reported model names (from
	// e.g. /api/tags). Bounded by the API validator; the field is
	// `omitempty` so zero-state pushes don't bloat network maps.
	Models []string `json:"models,omitempty"`

	// LastError is the last probe error message when Reachable is
	// false. Empty when Reachable is true. Bounded by the API
	// validator; `omitempty` keeps healthy peers' map entries small.
	LastError string `json:"last_error,omitempty"`

	// LastCheck is the agent's wall-clock time at the most recent
	// probe, formatted as RFC3339Nano. Acts as the K8s NodeStatus-
	// style heartbeat: peers ignore states older than the staleness
	// threshold (Phase 3 default: 15 s) when computing mesh
	// aggregation, so an agent that crashes silently ages out of
	// the aggregate naturally.
	LastCheck string `json:"last_check"`

	// Hardware summarises the GPU/RAM the agent has available. Phase 7
	// uses this for display only (e.g. tray UI showing "peer X:
	// RTX 4090, 24 GB"); the router does NOT score on raw hardware
	// because the same data is already encoded in Capacity. nil for
	// agents that predate the field, in which case peers fall back to
	// hardware-blind display.
	Hardware *HardwareSummary `json:"hardware,omitempty"`

	// Capacity is the number of concurrent inference requests this
	// agent will accept on its peer-overlay listener before returning
	// 503 waired_inference_overloaded. Derived at agent boot from a
	// token/s benchmark of the local engine (see Phase 7 plan §11).
	// 0 means "unlimited" — both the explicit semantics for external
	// (openai-compat) endpoints, and the zero-value backward-compat
	// fallback for agents that predate the field.
	Capacity int `json:"capacity,omitempty"`

	// Priority is the admin routing preference the CP folds in for this
	// device: High(1) / Middle(0) / Low(-1). The requesting agent's router
	// uses it as the dominant peer-selection key, preferring higher-priority
	// peers among those that can serve a request. Unlike the agent-pushed
	// fields above, this is CP-injected at map-assembly time (the agent never
	// sets it on its own push).
	//
	// `omitempty` keeps Middle (the default, 0) off the wire so the common
	// case is byte-identical and older agents verify the signed map unchanged.
	// Setting High/Low emits a non-zero field; agents that predate this field
	// would then reject the whole map (canonical re-marshal drops the unknown
	// field) — so non-default priority must only be set after the fleet is
	// upgraded. Low is negative so it stays distinct from the omitted default.
	Priority int `json:"priority,omitempty"`

	// ExcludeMain / ExcludeSub are the CP-injected Claude Code serving-eligibility
	// flags: when true, this device is NOT eligible to serve that traffic class
	// (main / sub) for the mesh, and the requesting router's buildMeshCandidates
	// drops the peer for the matching request Class. Negative sense + `omitempty`
	// so the default (eligible for both) stays off the wire and the common-case
	// signed map is byte-identical for older agents — the same fleet-upgrade
	// ordering caveat as Priority applies (only set an exclusion after the fleet
	// knows the field). CP-injected at map-assembly time (effectiveInferenceState);
	// the agent never sets these on its own push.
	ExcludeMain bool `json:"exclude_main,omitempty"`
	ExcludeSub  bool `json:"exclude_sub,omitempty"`

	// DesiredParallel is the operator's max-concurrent-requests target that the CP
	// injects at map-assembly time ONLY when an admin inference_max_clients
	// override is set (`effectiveInferenceState`) — it equals that override. The
	// serving agent drives OLLAMA_NUM_PARALLEL from it.
	//
	// CRITICAL: this is DISTINCT from Capacity. Capacity is the admission ceiling
	// and, absent an admin override, carries the agent's benchmark-derived value
	// — which must NOT be read as a parallelism target (doing so would restart the
	// engine on every fresh host). DesiredParallel is 0/omitted unless an admin
	// explicitly set the cap, so a default host never re-tunes parallelism.
	// `omitempty` + only-set-on-override keeps the common case byte-identical.
	DesiredParallel int `json:"desired_parallel,omitempty"`

	// PublicShare / PublicCapacity are the CP-injected Public Share state for
	// the device's OWN Self entry only (public share spec §7): PublicShare
	// mirrors Device.public_share_enabled so Tray/CLI render the toggle, and
	// PublicCapacity is the effective public client budget. On injected
	// provider PEER entries the CP folds the budget into Capacity instead —
	// these two fields are never set on peers. omitempty keeps the signed map
	// byte-identical for the (default OFF) common case, with the same
	// fleet-upgrade caveat as Priority: only emitted to pollers that declared
	// CapabilityPublicShareV1 (§8.4 gate).
	PublicShare    bool `json:"public_share,omitempty"`
	PublicCapacity int  `json:"public_capacity,omitempty"`

	// DesiredEngine / DesiredModelID / DesiredBenchmarkGen are the
	// CP-injected declarative onboarding targets (waired#835 §6) the
	// NAVI setup flow drives: which engine the agent should install and
	// run (InferenceTypeOllama / InferenceTypeVLLM), which catalog model
	// it should pull and activate, and a generation counter whose bump
	// requests a (re-)benchmark. They ride the signed map on the
	// device's OWN Self entry only, are injected at map-assembly time
	// (effectiveInferenceState), and the agent never sets them on its
	// own push.
	//
	// DesiredModelID is a catalog ID only — URLs, filesystem paths, and
	// commands are unrepresentable by contract, which is what keeps the
	// desired-state channel free of RCE-shaped payloads. Empty string /
	// 0 mean "no instruction"; `omitempty` keeps the common case off
	// the wire so signed maps stay byte-identical for older agents.
	// The CP only emits non-zero values to pollers that declared
	// CapabilityOnboardingV1 — the same fleet-upgrade caveat as
	// Priority applies.
	//
	// DesiredBenchmarkGen is declarative and idempotent: the agent
	// persists the last generation it completed and re-runs the
	// benchmark whenever the map's value is greater; re-bumping is
	// always safe.
	DesiredEngine       string `json:"desired_engine,omitempty"`
	DesiredModelID      string `json:"desired_model_id,omitempty"`
	DesiredBenchmarkGen int    `json:"desired_benchmark_gen,omitempty"`

	// DesiredIntegrations is the fourth CP-injected onboarding target:
	// which coding agents the wizard asked this device to configure. It
	// follows the three fields above exactly — Self entry only, injected
	// at map-assembly time, never set by the agent's own push, nil in
	// the common case so the signed map stays byte-identical — and
	// carries enum target IDs only, for the same reason DesiredModelID
	// is a catalog ID.
	//
	// It is gated on CapabilityOnboardingV2, NOT V1. The distinction
	// matters beyond byte-identity: an onboarding-v1 agent would drop
	// this field on canonical re-marshal and fail verification, and even
	// if it did not, it would silently never report an integration step
	// and leave the wizard waiting forever.
	//
	// See DesiredIntegrations for the nil / empty / populated semantics.
	DesiredIntegrations *DesiredIntegrations `json:"desired_integrations,omitempty"`

	// DesiredModelGen is the fifth CP-injected onboarding target: a
	// generation counter whose bump re-admits the desired model's
	// download. Same shape as DesiredBenchmarkGen — declarative,
	// idempotent, re-bumping always safe, 0 meaning "never asked" — and
	// the same Self-entry-only injection as the four fields above.
	//
	// It exists because admitting the pull once per desired model VALUE
	// leaves a failed download with no way back (waired-agent#136). The
	// only re-admission the agent had was the engine going absent→present,
	// and that transition cannot fire on a host whose engine was already
	// installed when the daemon started — which is every host after its
	// first setup. So a download that failed for a reason the operator
	// then FIXED (a full disk, an unplugged cable) stayed red until the
	// daemon restarted. Bumping this is the operator saying "try it
	// again", and it is deliberately the only thing that says so: an
	// agent-side timer would re-download tens of gigabytes on a metered
	// link with nobody asking.
	//
	// Gated on CapabilityOnboardingV3, not V2. The distinction is the
	// same one V2 draws against V1 and it is not cosmetic: this rides
	// the SIGNED map, so an agent that does not know the field drops it
	// on canonical re-marshal and fails verification outright.
	DesiredModelGen int `json:"desired_model_gen,omitempty"`

	// RecommendedMaxParallel is the agent-computed VRAM-safe engine parallelism
	// ceiling (floor(maxCtx/ctx) in the no-spill regime; 1 when spilling or when
	// the host is unsizable). It is ADVISORY telemetry for the Device detail page
	// (the operator may exceed it via an informed override) — NOT a routing input.
	//
	// It travels only agent → CP push → Spanner inference_state JSON → the
	// management API's inference_detail. It MUST be stripped from the served
	// NetworkMap (effectiveInferenceState zeros it) because, unlike the fields
	// above, it is non-zero in the common case (any host with an engine) and so
	// would break the byte-identical signed-map contract older agents rely on.
	// `omitempty` keeps 0 (unknown/unsizable) off the push.
	RecommendedMaxParallel int `json:"recommended_max_parallel,omitempty"`

	// NotShared is the agent's report that the operator has taken this device
	// out of MESH SERVING — `waired inference share off`, the tray toggle, or
	// a tray-Quit suspension. The engine keeps running for the machine's own
	// keyboard; it just stops answering for anyone else.
	//
	// Push-only, exactly like RecommendedMaxParallel: it travels agent → CP
	// push → Spanner inference_state JSON → the management API, and
	// effectiveInferenceState MUST zero it out of the served NetworkMap.
	// What the CP does with it is withhold this device's WHOLE InferenceState
	// from other peers' maps — the device's own Self entry keeps its state, so
	// the NAVI desired-state channel that rides Self stays alive while sharing
	// is off (waired#1030). Before this field existed the agent expressed the
	// same intent by not pushing at all, which also froze the admin's view of
	// the device at whatever it last said.
	//
	// Negative sense + `omitempty` so the default (sharing ON —
	// agentconfig's ShareWithMesh defaults to true) never reaches the wire and
	// the common case stays byte-identical for older readers. Same shape, and
	// the same reason, as ExcludeMain / ExcludeSub.
	//
	// A reader that predates the field sees false, i.e. "sharing" — the answer
	// it gave before, so a legacy agent is never wrongly withheld. The reverse
	// direction is the one that needs care: a CP that predates the field drops
	// it silently on intake, so an agent carrying it must not reach a fleet
	// whose control plane has not been updated first.
	NotShared bool `json:"not_shared,omitempty"`

	// ContextWindow is the input-token window this device's engine is
	// actually loaded with for the model in Models — not the model's
	// native window, and not what the host could theoretically hold. The
	// agent publishes it only when the serve tuning reached the window it
	// meant to serve; a trimmed window publishes 0 rather than a smaller
	// number, because "I serve less than I said" and "I say nothing" are
	// different claims and only the second one is safe to route on.
	//
	// Models is exactly one tag by the time it reaches the wire
	// (narrowPublishedModels: "design is 1 agent = 1 model"), so a single
	// value is unambiguous.
	//
	// It exists because a requesting node had no way to ask. Its gateway
	// sized the #623 overflow guard from its OWN manifests and its OWN
	// applied tuning even when dispatching to a peer, and the receiving
	// peer's overlay HandlerSet applies no window guard at all — so a
	// prompt that overran the SERVING engine reached it and was truncated
	// at the head instead of being compacted (waired-agent#436).
	//
	// 0 means the device declares no window: either an agent that
	// predates the field, or one whose engine is not serving a window it
	// is willing to stand behind. Consumers must treat 0 as "unknown" and
	// fail open to whatever they did before, so the fleet can upgrade in
	// any order.
	//
	// Unlike RecommendedMaxParallel / NotShared this DOES ride the served
	// NetworkMap — routing is the whole point — so it is gated on
	// CapabilityContextWindowV1. An agent that does not know the field
	// drops it on canonical re-marshal and fails verification, and unlike
	// the CP-injected fields this one appears on PEER entries, not only
	// on Self: the gate has to cover the whole map, not one entry.
	// `omitempty` keeps the undeclared case byte-identical.
	ContextWindow int `json:"context_window,omitempty"`

	// ActiveModel is the catalog model_id of the selection this device is
	// committed to serving — `qwen3-8b-instruct`, never the engine-side tag
	// (`qwen3:8b-q4_K_M`, `hf.co/unsloth/Qwen3-Coder-Next-GGUF:Q4_K_M`,
	// `Qwen/Qwen3-Next-80B-A3B-Instruct`). It is the same identifier
	// DesiredModelID carries in the other direction, so a reader can compare
	// what the CP asked for against what the device settled on.
	//
	// Models already names a model, so the reason for a second field is the
	// namespace, not the fact: an engine tag says the same model differently
	// depending on which engine loaded it, and vLLM is a Linux-only path, so
	// that difference tracks the PEER'S OS. A picker listing candidates from
	// a mixed fleet renders one model under three spellings and the operator
	// cannot tell they are the same weights (waired#1064). model_id is the
	// string every host agrees on, and it is the one the tray already shows
	// for the local machine, so local and peer rows finally read alike.
	//
	// Display only. Peer selection matches on the engine tags in Models
	// (buildMeshCandidates) — routing on this field would change which peers
	// answer, and that is not what it was added for.
	//
	// "" means the device names no model: an agent that predates the field,
	// or one with no committed selection at all. Consumers must fail open to
	// Models as before rather than reading it as "runs nothing".
	//
	// This field and SubsystemState below ride the served NetworkMap on PEER
	// entries — the tray's peer picker is the surface they exist for — but,
	// unlike ContextWindow, they carry NO capability gate. That is a product
	// decision recorded on waired#1064: nothing has shipped yet, so there is
	// no older fleet to keep verifying. The consequence is the one the
	// Priority comment above describes — an agent built before these fields
	// drops them on canonical re-marshal and rejects the whole map — so every
	// agent on a network has to be upgraded together until a gate exists. A
	// gate can be added later; CapabilityContextWindowV1 is the shape.
	ActiveModel string `json:"active_model,omitempty"`

	// SubsystemState is why this device is or is not serving right now, on
	// the axis the local management API already publishes under the same
	// name (`GET /waired/v1/inference/status`). Values are the SubsystemState
	// constants below; IsValidSubsystemState is the accepted set.
	//
	// It exists because the fields above cannot say it. Models empties the
	// moment the engine stops confirming the advertised tag — mid-pull, a
	// failed pull, a model switch, an engine restart all look the same — and
	// LastError is contractually empty while Reachable is true, so it cannot
	// carry the explanation either. Everything interesting therefore
	// collapsed into one wire state, and every peer-facing surface had no
	// choice but to render it as a bare "unavailable" (waired#1064).
	//
	// Deliberately NOT gated on Models being non-empty, which is the
	// opposite of ContextWindow above: describing the case where Models is
	// empty is the entire point. The two claims do not conflict — Models
	// says what may be routed to, this says what is going on.
	//
	// Byte progress deliberately does not ride here. It advances every few
	// seconds, and any content change re-fans the signed map to every peer
	// in the mesh; a state word changes a handful of times per model.
	//
	// Display only, same as ActiveModel. "" means the device declares
	// nothing — an agent that predates the field — and consumers must fail
	// open to whatever they decided before.
	SubsystemState string `json:"subsystem_state,omitempty"`

	// HostSpeed is what one coding-agent turn costs on this machine,
	// measured once per engine build on a fixed ~1 GB probe model
	// (waired-ai/waired-agent#496). See the type for what the numbers are.
	//
	// It is a RAW MEASUREMENT and carries no verdict. The threshold that
	// turns it into one is hostfit.HostCutoffTurnBudgetSeconds, and a
	// consumer applies whatever threshold its own question needs —
	// waired#1065's public-share gate is a different question from the
	// install-time default and may well settle on a different number. That
	// is deliberate: publishing the figure rather than the answer is what
	// makes this shape safe to freeze while its consumers are still being
	// designed, and proto is additive-only so the shape cannot be
	// corrected afterwards.
	//
	// It is NOT the reserved memory_bandwidth_measured_gbs (#252). That
	// field is a bandwidth figure bounding decode from below; this is a
	// turn time measured end to end on one model, and neither substitutes
	// for the other.
	//
	// nil means NO CLAIM — an agent that predates the field, a host whose
	// engine could not be measured, a truncated prefill — and a consumer
	// must fail open to whatever it did before. It never means "measured
	// as zero"; that is why it is a pointer.
	//
	// It MUST be stripped from the served NetworkMap, exactly as
	// RecommendedMaxParallel and NotShared are (effectiveInferenceState
	// zeroes them). Peers route on Capacity and Models and have no use for
	// it, and a field that rides the signed map is dropped on canonical
	// re-marshal by any agent that predates it — which fails verification
	// of the WHOLE map, not just that entry. The path this travels is
	// agent → CP push → Spanner inference_state JSON → the management API.
	HostSpeed *HostSpeed `json:"host_speed,omitempty"`
}

// HostSpeed is one coding-agent turn's cost on a host, measured at
// install time on a fixed probe model (waired-ai/waired-agent#496).
//
// The point of a FIXED probe is comparability: every host publishes a
// number measured the same way, on the same weights, at the same context
// depth, so two hosts' figures can be compared and one threshold can mean
// the same thing everywhere. A figure measured on whatever model the host
// happens to serve can do neither.
//
// Every field is `omitempty` — the additive guard requires it of a field
// added to a published struct — and a zero means "not reported" rather
// than a measured zero. The struct as a whole is absent when there is
// nothing to say.
type HostSpeed struct {
	// ProbeModelID is the catalog model_id the measurement was taken on
	// (hostfit.HostCutoffProbeModelID). It travels because the threshold
	// is calibrated against this model: a figure measured on anything
	// else is not comparable to it, and a consumer that finds an
	// unexpected id here must decline to judge rather than judge anyway.
	ProbeModelID string `json:"probe_model_id,omitempty"`

	// DepthTokens is the context depth the figures are normalised to
	// (hostfit.HostCutoffProbeDepthTokens); PromptTokens is what the
	// engine reported actually prefilling. They differ by ordinary
	// tokenizer drift. A large gap means the engine truncated the prompt,
	// and a truncated prefill measures the truncation rather than the
	// host — the agent publishes nothing in that case.
	DepthTokens  int `json:"depth_tokens,omitempty"`
	PromptTokens int `json:"prompt_tokens,omitempty"`

	// PrefillTokps and DecodeTokps are the engine's own counters
	// (prompt_eval_* and eval_* from ollama's /api/generate), never wall
	// clock: wall clock on a 1 GB model is dominated by model load and
	// request overhead, which is how the pre-#764 benchmark under-measured
	// fast hosts by ~35 %.
	PrefillTokps float64 `json:"prefill_tokps,omitempty"`
	DecodeTokps  float64 `json:"decode_tokps,omitempty"`

	// TurnSeconds is one turn at DepthTokens — a DepthTokens prefill plus
	// a DepthTokens/21 decode — computed from the two rates above.
	//
	// Derived, and deliberately on the wire anyway: it is the quantity a
	// threshold is compared against and the one an operator reads, and a
	// consumer that recomputes it has to know the 21:1 ratio to get the
	// same answer. All three come from the SAME sample, so they cannot
	// disagree with each other.
	TurnSeconds float64 `json:"turn_seconds,omitempty"`

	// Method is how the rates were obtained — one of the BenchmarkMethod*
	// constants, the same vocabulary SetupBenchmark.Method uses. It
	// travels for the same reason it does there: it changes what the
	// number may be used for downstream.
	Method string `json:"method,omitempty"`

	// Samples is how many measurements the published one is the median of;
	// SpreadPct is (max−min)/median across their turn times.
	//
	// They are not optional colour. A published measurement is the median
	// of N samples with its spread rather than a single reading, because
	// one reading on a machine that briefly got busy is off by enough to
	// cross a threshold: the runs that fixed this threshold sat within
	// ±2 % of each other while the one contended run landed +21 %. A
	// consumer that finds Samples <= 1 knows it holds a reading that was
	// never checked against another, and can weigh it accordingly.
	Samples   int     `json:"samples,omitempty"`
	SpreadPct float64 `json:"spread_pct,omitempty"`

	// EngineKind and EngineVersion identify the engine that produced the
	// counters. A measurement is only comparable within an engine build —
	// waired#668 is the same lesson from the boot benchmark's cache, where
	// an Ollama bundle bump left it serving pre-bump numbers.
	EngineKind    string `json:"engine_kind,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`

	// MeasuredAt is when the median sample was taken, RFC3339Nano — a
	// string rather than time.Time for the reason given at the top of this
	// file: the canonical JSON form has to be byte-deterministic.
	MeasuredAt string `json:"measured_at,omitempty"`
}

// HardwareSummary is the subset of the agent's hardware profile that
// the inference mesh broadcasts via NetworkMap. Kept minimal — the
// full profile lives in management/inference status responses and
// doesn't need to ride on every peer update.
type HardwareSummary struct {
	// GPUs lists each detected accelerator. Empty / nil for CPU-only
	// hosts. Multi-GPU agents list one entry per device.
	GPUs []HardwareGPUSummary `json:"gpus,omitempty"`

	// RAMTotalGB is the total system RAM in GB (rounded). Used for
	// display when a peer is serving an ollama (CPU-bound) variant.
	RAMTotalGB int `json:"ram_total_gb,omitempty"`

	// UnifiedMemory / UsableVRAMMB describe hosts where the GPU and CPU
	// share physical RAM (Apple Silicon, AMD Strix Halo). They mirror
	// hardware.Profile's fields of the same name and exist so a consumer
	// that is not the agent — today the control plane's onboarding
	// host-fit, which decides which catalog models it may offer for a
	// device — can reproduce the agent's own budget instead of comparing
	// against a raw VRAMTotalMB that overstates what the GPU can wire
	// down. UsableVRAMMB is the GPU-addressable upper bound after the OS
	// reserve; 0 means "unknown", and a consumer must then fall back to
	// GPUs[0].VRAMTotalMB (this is also what a pre-addition agent sends).
	//
	// Unlike RecommendedMaxParallel these DO ride the served NetworkMap:
	// HardwareSummary is a broadcast type by construction, and both
	// fields are fixed for the life of the host (the summary is sampled
	// once at boot), so they add no map churn.
	UnifiedMemory bool `json:"unified_memory,omitempty"`
	UsableVRAMMB  int  `json:"usable_vram_mb,omitempty"`

	// MemoryBandwidthSpecGBs is the PUBLISHED PEAK read bandwidth of the
	// pool the weights are read from — on a unified-memory host, the
	// shared SoC pool. 0 means "unknown", which a consumer must read as
	// "no bandwidth claim", never as "no bandwidth".
	//
	// It is the part's SPEC, not a measurement, and that is what makes it
	// useful: a peak is an UPPER BOUND on decode speed, and "too slow
	// even at peak" is the only claim that licenses excluding a model for
	// being slow. A figure that is not an upper bound may annotate but
	// must not exclude — see hostfit.Estimate.UpperBound, which this
	// field is what flips on unified-memory hosts
	// (waired-ai/waired-agent#251).
	//
	// The MEASURED counterpart is a SEPARATE field
	// (memory_bandwidth_measured_gbs, owned by #252) and must never be
	// published here, because the two bound from opposite directions: on
	// a unified host a CPU-side benchmark cannot reach what the GPU pulls
	// from the same pool, so a measurement is a LOWER bound. Carrying
	// both in one field — with or without a provenance discriminator —
	// would let a consumer silently invert the bound, and proto is
	// additive-only, so the shape cannot be corrected afterwards. When
	// #252 lands, it publishes the median of N samples together with
	// their spread, never a single reading, on every OS.
	//
	// Like UnifiedMemory / UsableVRAMMB this rides the served NetworkMap:
	// it is fixed for the life of the host (sampled once at boot), so it
	// adds no map churn, and omitempty keeps it off the wire entirely for
	// a host whose part is not in the table and for a pre-addition agent.
	MemoryBandwidthSpecGBs float64 `json:"memory_bandwidth_spec_gbs,omitempty"`

	// CarveOutVRAMMB is GPU memory reserved at the firmware level that
	// RAMTotalGB above EXCLUDES. It exists so a consumer can add the two
	// memory figures without double-counting on a host where both are
	// reads of one physical pool — the capacity gate is now that sum
	// (hostfit.TotalMemoryMB / hostfit.OllamaCapacityFit).
	//
	// Only a real reading sets it: sysfs mem_info_vram_total on Linux,
	// the AMD driver's qwMemorySize on Windows. It is 0 wherever
	// UsableVRAMMB is SYNTHESIZED from RAM instead of read — Apple
	// Silicon always (iogpu.wired_limit_mb, or 75 % of RAM), and the
	// Windows Strix Halo path when the registry value is unreadable and
	// the same 75 % heuristic stands in. That is why this is a published
	// quantity rather than something a consumer infers from
	// UnifiedMemory: the synthesized case is a provenance, not a
	// platform, and one of the two platforms producing it is not Apple.
	//
	// 0 therefore means "no separate pool", never "unknown, so guess" —
	// a consumer must not fall back to adding UsableVRAMMB. A
	// pre-addition agent sends 0 and is treated as having no carve-out,
	// which under-counts a Strix Halo's pool and over-counts nothing.
	//
	// Like UnifiedMemory / UsableVRAMMB this rides the served NetworkMap:
	// it is fixed for the life of the host (sampled once at boot), so it
	// adds no map churn, and omitempty keeps it off the wire for every
	// host that has no carve-out.
	CarveOutVRAMMB int `json:"carve_out_vram_mb,omitempty"`

	// RAMAvailableGB is how much of RAMTotalGB the operating system
	// reported as available at install time, in whole GB, rounded once
	// at measure time so every consumer computes on the same integer.
	// Linux reads /proc/meminfo MemAvailable, macOS sums vm_stat's
	// reclaimable page classes, Windows reads
	// GlobalMemoryStatusEx.AvailPhys — all three count reclaimable
	// cache as available, so RAMTotalGB − RAMAvailableGB never charges
	// the operating system for cache it would give back.
	//
	// It is measured ONCE per install/upgrade, while no engine or model
	// is resident, and persisted (waired-agent#568) — never a live
	// reading. That is what lets it ride the served NetworkMap under
	// the same claim the fields above make: fixed for the life of the
	// install, so it adds no map churn. A live figure would move with
	// every resample and would count a resident model against the very
	// host that serves it.
	//
	// 0 means "measurement unavailable", never "the OS holds
	// everything": a consumer computes the OS deduction as
	// max(hostfit.OSMemoryAllowanceGB, RAMTotalGB − RAMAvailableGB),
	// so 0 lands on the constant. A pre-addition agent sends 0 and
	// keeps today's arithmetic.
	//
	// Unlike the fields above this one is gated: it is agent-reported
	// and rides the signed map on every PEER entry (the
	// InferenceState.ContextWindow situation), so the CP strips it
	// across the whole map for a poller that has not declared
	// CapabilityRAMAvailableV1, keeping verification byte-identical
	// for older agents.
	RAMAvailableGB int `json:"ram_available_gb,omitempty"`
}

// HardwareGPUSummary identifies one GPU. Fields mirror
// hardware.GPU but stripped to what other peers can act on.
type HardwareGPUSummary struct {
	// Model is the vendor-reported model name, e.g. "NVIDIA GeForce
	// RTX 4090". Free-form; do not parse for routing decisions — read
	// Vendor below, which carries the same answer as a fixed token.
	Model string `json:"model"`

	// VRAMTotalMB is the device's total VRAM in megabytes.
	VRAMTotalMB int `json:"vram_total_mb,omitempty"`

	// ComputeCap is the CUDA compute capability formatted as a
	// string (e.g. "8.9" for Ada Lovelace). Empty for non-CUDA.
	ComputeCap string `json:"compute_cap,omitempty"`

	// Vendor is the lowercase GPU vendor token, mirroring
	// hardware.GPU.Vendor. The shipped detectors emit exactly
	// "nvidia", "amd" and "apple" today; the set grows as detectors are
	// added (Intel Arc is the named next one), so treat an unrecognised
	// token as "some GPU we have no rule for" rather than as invalid.
	// It was
	// deliberately absent while the summary served only peer display;
	// the control plane's onboarding host-fit now needs it, because
	// which engines a host can run is vendor-dependent (vLLM is an
	// NVIDIA path; AMD is served through Ollama's ROCm/Vulkan
	// backends, waired#290). Publishing the token is what lets a
	// consumer honour the "do not parse Model" rule above. Empty means
	// "unknown" — a pre-addition agent, or a detector that could not
	// identify the adapter — and a consumer must not read that as "no
	// GPU".
	Vendor string `json:"vendor,omitempty"`
}

// Engine type constants — accepted values for InferenceState.Type.
const (
	InferenceTypeOllama = "ollama"
	InferenceTypeVLLM   = "vllm"
	InferenceTypeNone   = "none"
)

// IsValidInferenceType reports whether t is one of the accepted
// engine type values. Used by the CP API validator and by the
// agent push client's pre-flight check.
func IsValidInferenceType(t string) bool {
	switch t {
	case InferenceTypeOllama, InferenceTypeVLLM, InferenceTypeNone:
		return true
	}
	return false
}

// Accepted values for InferenceState.SubsystemState.
//
// The vocabulary is not new here: these are the strings the agent's local
// management API has published under `subsystem_state` since Step 2, named
// as constants now that they cross the wire and a validator has to check
// them. The axis says WHAT is wrong, never whether it will fix itself — a
// crash loop alternates between starting and engine_failed for as long as
// its recovery budget lasts.
const (
	// SubsystemStateInitializing is the brief boot sequence.
	SubsystemStateInitializing = "initializing"
	// SubsystemStateReady is an active engine serving the active model.
	SubsystemStateReady = "ready"
	// SubsystemStateAwaitingModel means a model is chosen but not on disk.
	SubsystemStateAwaitingModel = "awaiting_model"
	// SubsystemStateLoading means the model is arriving or the engine is
	// still bringing it up — a pull in flight, or a restart around it.
	SubsystemStateLoading = "loading"
	// SubsystemStatePullFailed means the last download errored and nothing
	// retries it on its own.
	SubsystemStatePullFailed = "pull_failed"
	// SubsystemStateDegraded means a fallback engine is in use.
	SubsystemStateDegraded = "degraded"
	// SubsystemStateNoEngine means no engine is alive to serve anything.
	SubsystemStateNoEngine = "no_engine"
	// SubsystemStateStopped means the operator hard-stopped the engine to
	// free memory — a usable engine exists and is intentionally down.
	SubsystemStateStopped = "stopped"
	// SubsystemStateStarting means an engine restart is in flight.
	SubsystemStateStarting = "starting"
	// SubsystemStateEngineFailed means the engine is down: a crashed model
	// runner, an exhausted recovery budget, or a boot that never came up.
	SubsystemStateEngineFailed = "engine_failed"
	// SubsystemStateDisabled means the operator paused inference.
	SubsystemStateDisabled = "disabled"
)

// IsValidSubsystemState reports whether s is one of the accepted
// InferenceState.SubsystemState values. The empty string is NOT accepted
// here: it means "declares nothing", which every consumer must already
// handle, so a validator checks it separately rather than folding the two
// answers together.
func IsValidSubsystemState(s string) bool {
	switch s {
	case SubsystemStateInitializing,
		SubsystemStateReady,
		SubsystemStateAwaitingModel,
		SubsystemStateLoading,
		SubsystemStatePullFailed,
		SubsystemStateDegraded,
		SubsystemStateNoEngine,
		SubsystemStateStopped,
		SubsystemStateStarting,
		SubsystemStateEngineFailed,
		SubsystemStateDisabled:
		return true
	}
	return false
}
