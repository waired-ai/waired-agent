package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// InferenceProvider is the seam between the loopback management API
// and the inference subsystem. waired-agent constructs a concrete
// implementation in main.go that wraps catalog.Store, download.Puller,
// runtime.Registry, hardware.Profiler, and router.Selector.
type InferenceProvider interface {
	Status(ctx context.Context) InferenceStatus
	Hardware(ctx context.Context) hardware.Profile
	Runtimes(ctx context.Context) []RuntimeStatus
	ListModels(ctx context.Context) []ModelEntry
	// ModelSizes reports the on-disk bytes of each downloaded model, keyed
	// by model id. Nil or a missing key means "not known right now" — the
	// engine holds the figure, so a stopped engine reports nothing rather
	// than zero.
	//
	// Separate from ListModels because it talks to the engine and
	// ListModels does not. ListModels is also read on a control path
	// (modelDownloaded, in the preferred-model flow), where an engine
	// round trip would buy nothing and could hang; only the listing
	// handler below asks for sizes.
	ModelSizes(ctx context.Context) map[string]int64
	// PullModel starts a model download and returns as soon as it is
	// admitted. ctx bounds the SYNCHRONOUS admission only — the
	// implementation MUST run the download itself on its own long-lived
	// context. Handlers pass r.Context(), which net/http cancels the
	// moment the response is written, so a job that inherited it would be
	// killed within milliseconds of the 202 (#305).
	//
	// A pull already in flight for the same model is joined rather than
	// duplicated; the returned PullJob then describes the running job.
	PullModel(ctx context.Context, modelOrAlias string) (PullJob, error)

	// CancelPull stops the download in flight for modelID and returns
	// once it has stopped. Unlike PullModel this is synchronous on
	// purpose: the operator's next `waired models ls` must show what
	// actually happened, so the answer waits for the job to unwind.
	//
	// A model with nothing downloading is NOT an error — it reports
	// PullCancel.Status "not_downloading". "stop this" and "there was
	// nothing to stop" leave the host in the same state, which is the
	// state the caller asked for.
	CancelPull(ctx context.Context, modelID string) (PullCancel, error)

	DeleteModel(ctx context.Context, modelID string) error
	Select(ctx context.Context, req router.Request) (router.Selection, error)

	// RunBenchmark forces a fresh on-device throughput benchmark of the
	// active model and returns the measured throughput plus any
	// resulting recommendation: lighter when below the interactive
	// floor (issue #133), upgrade when there is enough headroom for a
	// higher-quality model. ok is false when the engine/model is not
	// ready yet (the handler maps this to 425/409 so a caller can
	// poll), or the benchmark was skipped. err covers unexpected
	// failures.
	RunBenchmark(ctx context.Context) (out BenchmarkOutcome, ok bool, err error)

	// DismissRecommendation records that the user declined the
	// recommendation to switch from→to (variant IDs) so it is not
	// re-surfaced after a re-benchmark of the same pairing. Empty from/to
	// dismisses the current live recommendation.
	DismissRecommendation(from, to string) error

	// BenchmarkStatus reports the benchmark job's current state
	// (waired#835 §12): the benchmark runs as a single-flight job
	// detached from any request context, so callers that time out or
	// disconnect can poll this instead of losing the measurement.
	BenchmarkStatus() BenchmarkStatusResponse
}

// InferenceStatus is the body of GET /waired/v1/inference/status.
//
// SubsystemState is one of (Step 2):
//
//	"initializing"     boot sequence (brief)
//	"ready"            active engine + model serving requests
//	"awaiting_model"   active.model_id chosen but not on disk yet
//	"loading"          on disk, engine restart in progress
//	"pull_failed"      most recent download errored, no auto-retry
//	"degraded"         fallback engine in use (chosen != current)
//	"no_engine"        no engine alive — inference API returns 503
//	"stopped"          engine hard-stopped by operator (parked, #186)
//	"starting"         engine restart in flight after a start request
//	"engine_failed"    the engine is down: a crashed model runner, an
//	                   exhausted recovery budget, or a boot that never came
//	                   up. Previously any of these read as "ready" whenever
//	                   the active model happened to be on disk
//	                   (waired-agent#29).
//
// This axis says WHAT is wrong, never whether it will fix itself: a crash
// loop alternates between "starting" and "engine_failed" for as long as the
// budget lasts. Read runtimes[...].failure_latched for that (#310).
type InferenceStatus struct {
	SubsystemState  string                   `json:"subsystem_state"`
	Runtimes        map[string]RuntimeStatus `json:"runtimes"`
	Models          ModelsSnapshot           `json:"models"`
	ActiveEndpoints []ActiveEndpoint         `json:"active_endpoints"`

	// Active is the engine + model the agent is committed to serving
	// (mirrors state.json `active`). nil when no decision has been
	// recorded yet (= run `waired runtimes install --auto`).
	Active *ActiveSelection `json:"active,omitempty"`

	// Inflight is how many inference requests this machine's engine is
	// serving right now — the owner's own work and peer arrivals alike
	// (the counter Server.AdmitLocal and the capacity gate share).
	//
	// It answers the question someone asks while a coding agent sits
	// there saying nothing: is this computer busy at all? Zero is the
	// informative answer, not the boring one — a frozen agent and
	// `0 requests` together say the wait is somewhere else — so it is a
	// pointer: an agent that does not publish the field omits it, and
	// "not reported" must not render as "idle" (waired-agent#837).
	Inflight *int `json:"inflight,omitempty"`

	// AvailableUpdate is set when the auto-picker would choose a
	// strictly better candidate on the current hardware than what
	// Active records. Populated by the bootstrap's background
	// re-evaluation; used by `waired status` and refresh prompts.
	AvailableUpdate *AvailableUpdate `json:"available_update,omitempty"`

	// BenchmarkRecommendation is set when the most recent on-device
	// benchmark measured throughput below the interactive floor AND a
	// genuinely lighter fitting candidate exists (issue #133). nil when
	// none. Advisory only — never auto-applied; acceptance reuses POST
	// /waired/v1/inference/preferred-model (with ToModelID) and decline
	// POSTs /waired/v1/inference/recommendation/dismiss. Carries
	// Dismissed=true (rather than being nil) when the user has already
	// declined this exact pairing, so the CLI/tray can stay quiet without
	// re-deriving the decision.
	//
	// This field carries LIGHTER recommendations only. Upgrades go in
	// BenchmarkUpgrade: an old tray/CLI reading an upgrade out of this
	// field would render "local inference is slow — switch to the
	// lighter model X" for a host with headroom, and its default-Yes
	// prompt could auto-accept the multi-GB switch.
	BenchmarkRecommendation *BenchmarkRecommendation `json:"benchmark_recommendation,omitempty"`

	// BenchmarkUpgrade is the inverse suggestion: the most recent
	// benchmark cleared the interactive floor with enough headroom that
	// a higher-quality_tier model is predicted to still run above it
	// (Direction="upgrade", PredictedTokps set). Same acceptance /
	// dismissal endpoints as BenchmarkRecommendation; never set at the
	// same time as a lighter recommendation.
	BenchmarkUpgrade *BenchmarkRecommendation `json:"benchmark_upgrade,omitempty"`

	// LongContext is the most recent depth-aware benchmark (#624):
	// prefill/decode measured at 64k/128k/~200k of filled context
	// (clipped to the applied serve window). nil until the background
	// sweep completes its first run (or on agents without one).
	LongContext *LongContextBench `json:"long_context,omitempty"`

	// HostMemory is the install-time available-memory measurement every
	// fit decision on this host is based on (waired-agent#568), and when
	// it was taken. nil when nothing has been measured — an env-seam
	// override supplies a value and no date, and dating it from the
	// record would attribute the number to a measurement that did not
	// produce it.
	//
	// Surfaced because the figure is emphatically NOT live: it is fixed
	// for the life of the install, so a host measured during a busy
	// moment keeps that snapshot, and nothing showed an operator what
	// the verdicts rest on (waired-agent#589).
	HostMemory *HostMemoryMeasurement `json:"host_memory,omitempty"`

	// DesiredState surfaces the operator's persisted enable/disable
	// intent for the inference subsystem ("enabled" | "disabled").
	// Empty when the daemon has no InferenceController attached
	// (older builds, tests). The tray uses this to decide whether the
	// toggle button should be in the "Disable" or "Enable" position
	// independently of SubsystemState (which describes engine health).
	DesiredState string `json:"desired_state,omitempty"`

	// DesiredStateSet reports whether that intent was actually WRITTEN on
	// this host — by the CLI, the tray, the browser wizard, the management
	// API, --inference-enabled, or the host cutoff standing local inference
	// down. False means nobody and nothing has moved this toggle, and
	// DesiredState above is only reporting the live default.
	//
	// The two cannot be collapsed. A host above the recommended spec starts
	// enabled without writing anything, so "enabled" alone cannot tell a
	// person's answer from a default — and install-flow step 6 needs exactly
	// that distinction to know whose choice it would be overriding
	// (waired#1142). The daemon's own cutoff has always read the file
	// directly for the same reason (hostCutoffIsStillOurs).
	DesiredStateSet bool `json:"desired_state_set,omitempty"`

	// ShareWithMesh surfaces the operator's persisted choice for
	// whether the local inference engine is exposed to mesh peers
	// ("shared" | "not_shared"). Empty when the daemon has no
	// ShareController attached (older builds, tests, or agents with
	// inference disabled at install time). The tray uses this to
	// render the "Share engine to mesh" / "Stop sharing engine to
	// mesh" toggle independently of SubsystemState. Set by the
	// management Server.handleInferenceStatus after consulting the
	// ShareController so the InferenceProvider interface stays
	// orthogonal to the share concern.
	ShareWithMesh string `json:"share_with_mesh,omitempty"`

	// ShareSuspended reports the live-only session override (#316):
	// sharing is withheld right now even though ShareWithMesh still
	// records the operator's "shared" choice. The tray sets it on Quit
	// and lifts it on its next start, so a user who quits the tray stops
	// serving peers without losing the preference. Kept separate from
	// ShareWithMesh so older clients keep reading a two-valued field.
	ShareSuspended bool `json:"share_suspended,omitempty"`

	// Worker is the operator's manual inference routing choice
	// (Tailscale-exit-node-style). nil when the daemon has no
	// WorkerController attached. Embedding the resolved state here
	// (instead of forcing the tray into a separate GET /v1/worker
	// poll) lets the tray refresh "Inference routing" submenu state
	// in the same 5 s tick that already drives the rest of the
	// menu. Set by Server.handleInferenceStatus.
	Worker *WorkerResponse `json:"worker,omitempty"`

	// EnginePower surfaces the live hard engine power axis (#186):
	// "running" | "stopped" | "starting". Empty when the daemon has no
	// EngineController attached (older builds, tests). The tray/CLI use
	// it to render the Stop/Start engine control independently of the
	// soft DesiredState toggle. Set by Server.handleInferenceStatus.
	EnginePower string `json:"engine_power,omitempty"`

	// EngineManaged is false when the serving engine was ADOPTED (#336):
	// an exact-pin orphan of a previous waired run, held with no process
	// handle, so the power axis does not apply. Only meaningful alongside
	// a non-empty EnginePower. The tray renders the Stop/Start control
	// disabled ("Engine not managed") when false.
	EngineManaged bool `json:"engine_managed,omitempty"`

	// NoModelSelected reports the operator's standing "run without a
	// local model" choice (waired-agent#586): the install flow's model
	// picker offered "don't download a model now" and they took it. The
	// engine may still be installed and running; nothing is failed and
	// nothing is pending — the bundled fallback download stands down and
	// a model can be added later. Cleared the moment any model choice
	// lands (tray, CLI, or wizard).
	NoModelSelected bool `json:"no_model_selected,omitempty"`

	// HostSpeed is what one coding-agent turn costs on this machine
	// (waired-ai/waired-agent#496), and whether that measurement is what
	// set DesiredState to disabled. nil when nothing has been measured.
	//
	// It exists so an operator can be told WHY local inference is off.
	// Until this shipped the only trace of the decision was a line in the
	// daemon log, so `waired inference status` could say "off" and nothing
	// more — which reads as a setting someone forgot rather than an
	// answer the machine worked out.
	HostSpeed *HostSpeedStatus `json:"host_speed,omitempty"`

	// HostSpeedStage is how far the measurement has got — one of
	// "pulling_probe", "measuring", "measured", "probe_failed",
	// "measure_failed", or absent on a host with nothing to say about it.
	//
	// A SIBLING of HostSpeed rather than a field inside it, because the
	// states worth reporting are exactly the ones where there is no figure
	// to hang it on. `waired init` asks for a fresh measurement and then
	// waits for it (waired-agent#599); without this the only two outcomes
	// it could distinguish were "a figure arrived" and "twenty minutes
	// passed", so a measurement that ran and failed cost the install the
	// whole budget in silence.
	//
	// Report only. Nothing reads it to decide anything, and a client that
	// does not know a value renders it as an unknown stage rather than
	// failing — the same treatment the setup-progress rows it is derived
	// from get (waired#1143).
	HostSpeedStage string `json:"host_speed_stage,omitempty"`

	// Residency is the model-residency setting in force on this host
	// (waired-agent#861): how long the engine holds the weights after the
	// last request, and whether that means "never unload". nil when the
	// daemon has no ResidencyController attached (older builds, tests).
	//
	// Carried in the status body rather than behind its own read route so
	// the tray renders the current choice in the same 5 s tick that draws
	// the rest of the menu, which is the arrangement every other setting
	// here already uses.
	Residency *ResidencyResponse `json:"residency,omitempty"`

	// FirstToken is the last first-token wait this host served with the
	// model it is serving now, and the fastest comparable one it has seen
	// since the agent started (waired-agent#912).
	//
	// ModelResident above answers whether the weights are in memory, which
	// was the whole of waired-agent#879. It is not the whole question: a
	// model can be resident and still re-read the entire prompt, and on the
	// measured hosts that is the difference between 2.6 s and 35.4 s with
	// nothing else different — same model, same host, stock settings. The
	// pair of figures is what makes either of them readable.
	//
	// Beside Residency rather than behind the observability route because
	// this is the line under `model loaded:`, and splitting one thought
	// across two endpoints is what waired-agent#912 filed. nil whenever
	// there is nothing honest to say — see firstTokenReading.
	FirstToken *FirstTokenReading `json:"first_token,omitempty"`
}

// FirstTokenReading is one observed time-to-first-token with the yardstick
// to judge it against (waired-agent#912).
//
// There is deliberately no verdict here — no "cold", no "warm", no flag.
// A word needs a threshold, and a fixed one is wrong on at least one
// reference host: the 4 B model's WARM first token (1,960 ms) is 7.5x
// slower than the 35 B-A3B's warm first token (259 ms), and both are
// correct. The ratio survives the hardware differences where the constant
// does not, which is why the comparison travels and the judgement stays
// with the reader.
type FirstTokenReading struct {
	// Ms is the wait, and At is when the request it belongs to ended,
	// RFC3339Nano. The timestamp is not decoration: a reading from an hour
	// ago rendered under a line that says the model is loaded reads as a
	// promise about the next request, and it is not one.
	Ms uint32 `json:"ms"`
	At string `json:"at"`
	// Model is the catalog id this reading belongs to. It always equals the
	// active model — the derivation refuses to mix them — and travels so a
	// client can say so without inferring it.
	Model string `json:"model,omitempty"`
	// BestMs is the fastest comparable reading on this host, and
	// BestOfSamples how many readings it won against. 0 means there was no
	// comparable one, which is the ordinary state on a fresh daemon: the
	// figure then stands alone rather than being compared to something it
	// should not be.
	BestMs        uint32 `json:"best_ms,omitempty"`
	BestOfSamples int    `json:"best_of_samples,omitempty"`
}

// firstTokenSampleLimit bounds how far back the yardstick may look. The
// ring holds ~hours of traffic; a first-token time from the far end of it
// describes a machine that may since have changed model, engine version
// or tuning.
const firstTokenSampleLimit = 256

// firstTokenComparableNumer / firstTokenComparableDenom is the share of
// the latest prompt a reading must ALSO have processed to count as
// comparable — half.
//
// Some bound is unavoidable. Prefill is the dominant term in a cold first
// token, so a one-line question answers far faster than a coding turn on
// the same host with the same weights, and letting those into the pool
// would set a floor no real turn can reach — every ordinary answer would
// then read as slow. Half is a claim about which SAMPLES are alike, not
// about what counts as fast; it is not a threshold anything is judged
// against.
const (
	firstTokenComparableNumer = 1
	firstTokenComparableDenom = 2
)

// firstTokenReading derives the pair from the event ring.
//
// Three filters, and each one is the difference between a true line and a
// misleading one:
//
//   - Observed. TTFTMs == 0 means the serving leg could not see a first
//     token (the OpenAI leg forwards bytes without parsing them, and a
//     non-streamed answer has no first token distinct from its last), so
//     a zero here is not a fast request (waired-agent#874).
//   - Served HERE. A peer-answered request measures the peer's prefill.
//     Rendering it under this host's `model loaded:` line would report
//     another machine's speed as this one's.
//   - This model. The reading belongs to the weights that produced it, so
//     a reading taken before a model switch says nothing about the model
//     named on the line above.
//
// nil when nothing survives, which is the right answer on a fresh daemon,
// on a host that only ever routes to peers, and on one whose traffic is
// not Anthropic streaming. Saying nothing is not the same as saying zero.
func firstTokenReading(ring *observability.Ring, activeModelID string) *FirstTokenReading {
	if ring == nil || activeModelID == "" {
		return nil
	}
	events := ring.RecentRequests(firstTokenSampleLimit)
	if len(events) == 0 {
		return nil
	}
	qualifies := func(ev *observability.Event) bool {
		return ev.Request != nil && ev.Request.TTFTMs > 0 &&
			ev.Request.PeerID == "" && ev.Request.Model == activeModelID
	}

	var last *observability.Event
	lastIdx := -1
	for i := range events {
		if qualifies(&events[i]) {
			last, lastIdx = &events[i], i
			break
		}
	}
	if last == nil {
		return nil
	}

	out := &FirstTokenReading{
		Ms:    last.Request.TTFTMs,
		At:    last.TS.UTC().Format(time.RFC3339Nano),
		Model: activeModelID,
	}

	// Without a prompt size on the latest reading there is nothing to call
	// a comparable sample comparable TO, so the figure stands alone.
	if last.Request.InputTokens <= 0 {
		return out
	}
	floor := last.Request.InputTokens * firstTokenComparableNumer / firstTokenComparableDenom
	for i := lastIdx + 1; i < len(events); i++ {
		ev := &events[i]
		if !qualifies(ev) || ev.Request.InputTokens < floor {
			continue
		}
		out.BestOfSamples++
		if out.BestMs == 0 || ev.Request.TTFTMs < out.BestMs {
			out.BestMs = ev.Request.TTFTMs
		}
	}
	// A "fastest" that is the reading itself is not a comparison. Report
	// only a strictly better one; equal or slower leaves the figure alone.
	if out.BestMs >= out.Ms {
		out.BestMs, out.BestOfSamples = 0, 0
	}
	return out
}

// HostSpeedStatus is the install-time host measurement as the local
// management API reports it (waired-ai/waired-agent#496). The wire form
// the control plane sees is signer.HostSpeed; this is the subset a person
// standing at the machine is asking about.
type HostSpeedStatus struct {
	// TurnSeconds is one coding-agent turn at the measured depth, and
	// BudgetSeconds is what it is compared against
	// (hostfit.HostCutoffTurnBudgetSeconds). The budget travels so a
	// client can render "68 s against a 45 s budget" without carrying its
	// own copy of a threshold that is allowed to move.
	TurnSeconds   float64 `json:"turn_seconds"`
	BudgetSeconds float64 `json:"budget_seconds"`

	// TurnFloorSeconds is a LOWER BOUND on the same turn, and on a host
	// far below the cutoff it is the ONLY figure there is: TurnSeconds is
	// zero and this one is set (waired-ai/waired-agent#579). Measuring
	// such a host at full depth costs minutes standing in front of the
	// model download, so the agent concludes from the prefill rate alone
	// and says so in Method.
	//
	// A client that renders only TurnSeconds shows nothing at all for
	// exactly the hosts that most need telling. Both fields are present
	// so a client can render "210.4 s or more" without inferring the
	// distinction from a method string.
	TurnFloorSeconds float64 `json:"turn_floor_seconds,omitempty"`

	// Method is how the figure above was obtained — one of proto/signer's
	// BenchmarkMethod* values. BenchmarkMethodOllamaPrefillFloor is the
	// bound; anything else is a measurement at DepthTokens.
	//
	// DepthTokens is the depth the turn is normalised to and PromptTokens
	// is what the engine actually prefilled. They are equal within
	// tolerance for a measurement and far apart for a bound, which is what
	// makes the distinction checkable rather than merely asserted.
	Method       string `json:"method,omitempty"`
	DepthTokens  int    `json:"depth_tokens,omitempty"`
	PromptTokens int    `json:"prompt_tokens,omitempty"`

	// PrefillTokps / DecodeTokps / Samples / SpreadPct / ProbeModelID /
	// MeasuredAt are the same figures signer.HostSpeed carries, for
	// `waired status --observability` and for a bug report.
	PrefillTokps float64 `json:"prefill_tokps,omitempty"`
	DecodeTokps  float64 `json:"decode_tokps,omitempty"`
	Samples      int     `json:"samples,omitempty"`
	SpreadPct    float64 `json:"spread_pct,omitempty"`
	ProbeModelID string  `json:"probe_model_id,omitempty"`
	MeasuredAt   string  `json:"measured_at,omitempty"`

	// TurnedInferenceOff is true when this measurement is what set the
	// local-inference default to off. False on a host that measured above
	// the budget, and false again as soon as anyone moves the toggle for
	// any other reason — it is a claim about why the toggle reads the way
	// it does, so it stops being made the moment that stops being true.
	TurnedInferenceOff bool `json:"turned_inference_off,omitempty"`
}

// ActiveSelection mirrors catalog.ActiveSelection's wire shape so the
// management API can return it without forcing callers to import the
// catalog package. Kept structurally identical; if the catalog struct
// grows a field, surface it here too.
type ActiveSelection struct {
	Runtime        string   `json:"runtime"`
	RuntimeVersion string   `json:"runtime_version,omitempty"`
	ModelID        string   `json:"model_id"`
	VariantID      string   `json:"variant_id"`
	DecidedBy      string   `json:"decided_by,omitempty"`
	DecisionReason []string `json:"decision_reason,omitempty"`
}

// AvailableUpdate hints that a refresh would change the active
// selection. PreCached signals whether the candidate's weights are
// already on disk — when true the swap will be instant; when false
// the user faces another download.
type AvailableUpdate struct {
	Runtime             string   `json:"runtime"`
	ModelID             string   `json:"model_id"`
	VariantID           string   `json:"variant_id"`
	Reasons             []string `json:"reasons,omitempty"`
	PreCached           bool     `json:"precached"`
	ExpectedSwapSeconds int      `json:"expected_swap_seconds,omitempty"`
}

// Direction values for BenchmarkRecommendation. The zero value (legacy
// wire payloads from older daemons) means lighter.
const (
	RecommendationLighter = "lighter"
	RecommendationUpgrade = "upgrade"
)

// LongContextBench mirrors the agent's depth-aware benchmark result
// for the management surface (#624).
type LongContextBench struct {
	ContextLength int                `json:"context_length"`
	KVCacheType   string             `json:"kv_cache_type,omitempty"`
	Completed     bool               `json:"completed"`
	MeasuredAt    time.Time          `json:"measured_at"`
	Stages        []LongContextStage `json:"stages"`
}

// LongContextStage is one measured depth.
type LongContextStage struct {
	TargetTokens int     `json:"target_tokens"`
	PromptTokens int     `json:"prompt_tokens,omitempty"`
	PrefillTokps float64 `json:"prefill_tok_s"`
	DecodeTokps  float64 `json:"decode_tok_s"`
	Failed       bool    `json:"failed,omitempty"`
}

// BenchmarkRecommendation describes a benchmark-driven model-switch
// suggestion: step down to a lighter model when the measurement is
// below the interactive floor (issue #133), or step up to a higher
// quality tier when the host has throughput headroom
// (Direction="upgrade"). The switch is never applied automatically;
// the user accepts it via the preferred-model endpoint or declines it
// via the dismiss endpoint.
type BenchmarkRecommendation struct {
	// Direction is RecommendationLighter or RecommendationUpgrade.
	// Empty means lighter (payloads from pre-upgrade daemons).
	Direction     string  `json:"direction,omitempty"`
	FromModelID   string  `json:"from_model_id"`
	FromVariantID string  `json:"from_variant_id"`
	ToModelID     string  `json:"to_model_id"`
	ToVariantID   string  `json:"to_variant_id"`
	MeasuredTokps float64 `json:"measured_tokps"`
	FloorTokps    float64 `json:"floor_tokps"`
	// PredictedTokps is the bandwidth-scaled throughput estimate for
	// the suggested model on this host. Upgrade direction only.
	PredictedTokps float64 `json:"predicted_tokps,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	// Dismissed is true when the user already declined this exact
	// from→to pairing; surfaces so the CLI/tray can stay silent.
	Dismissed bool `json:"dismissed,omitempty"`
}

// BenchmarkOutcome is RunBenchmark's result: the raw measurement plus
// at most one of (Lighter, Upgrade) — mutually exclusive by
// construction (below floor → lighter; at/above floor → upgrade or
// nothing).
type BenchmarkOutcome struct {
	MeasuredTokps float64
	Lighter       *BenchmarkRecommendation
	Upgrade       *BenchmarkRecommendation
	// Failed reports that the benchmark RAN and did not complete — the
	// warm-up got an engine error, the measurement timed out, and so on.
	// It is distinct from RunBenchmark's ok=false, which means "not ready
	// yet, retry" (425). Without it a failed run is indistinguishable from
	// a host too slow to measure a rate: both yield MeasuredTokps 0, and
	// that ambiguity is how a dead engine printed a green "Local inference
	// works" (waired-agent#29).
	Failed bool
	// Error is the failure reason when Failed, for the caller to show.
	Error string
}

// ModelsSnapshot summarises model lifecycle states for display.
type ModelsSnapshot struct {
	Ready       []string `json:"ready"`
	Downloading []string `json:"downloading"`
	Failed      []string `json:"failed,omitempty"`
	// NotPresent names the catalog models this daemon has nothing under
	// way for: no weights on disk and no download running. It covers a
	// model nothing has ever touched, one that was deleted, and one that
	// was evicted — three histories, one answer to the question a caller
	// is actually asking.
	//
	// Before it, "absent from ready / downloading / failed" was the only
	// observation available, and it could not be told apart from an id
	// this build has never heard of (waired-agent#403). The daemon can
	// refuse a chosen model permanently, so those two want opposite
	// things said about them, and `waired init` had to bound the
	// difference with a blind five-minute grace.
	//
	// NOT omitempty, unlike the lists above: a reader has to be able to
	// tell "this daemon says nothing is pending on any model" from "this
	// daemon is too old to answer", and an omitted empty list reads as
	// both. Callers key off the list being non-empty before drawing any
	// conclusion from a model's absence from it.
	NotPresent []string `json:"not_present"`
	// Downloads carries byte-level progress for the in-flight downloads
	// named in Downloading. Optional: old clients read Downloading (names
	// only) and ignore this; new clients render a percentage + size from
	// it. A model can be in Downloading without a Downloads entry (queued
	// before the first progress line, or progress unknown).
	Downloads []ModelDownload `json:"downloads,omitempty"`

	// Failures carries WHY each model named in Failed stopped. Optional
	// in the same way Downloads is: old clients read Failed (names only)
	// and ignore this.
	//
	// The daemon has had the reason all along — runPullJob writes it into
	// the model's stored state — and this snapshot was the wall it did not
	// cross, so `waired models pull` could only print "failed" and the
	// speed check could only refuse with one fixed sentence
	// (waired-agent#328). A model can be in Failed without an entry here:
	// the failure predates the field, or nothing was recorded.
	Failures []ModelFailure `json:"failures,omitempty"`
}

// ModelFailure is one failed model's stored reason, verbatim. Not
// classified into an enum here: this is the local management API, whose
// readers are the CLI and the tray, and both want the text a human can
// act on. The §7 error-code mapping happens where the setup wire needs
// it (classifySetupFailure), from this same text.
type ModelFailure struct {
	Model string `json:"model"`
	Error string `json:"error"`
}

// ModelDownload is one in-flight model download's aggregate byte progress
// (summed across the layers ollama streams). TotalBytes is 0 until ollama
// reports a size.
type ModelDownload struct {
	Model          string `json:"model"`
	CompletedBytes int64  `json:"completed_bytes"`
	TotalBytes     int64  `json:"total_bytes"`
}

type RuntimeStatus struct {
	Name      string `json:"name,omitempty"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	State     string `json:"state"`
	// Backend is the GPU compute backend the engine settled on (#290),
	// e.g. "rocm" / "vulkan" / "metal" / "cuda" / "cpu". Surfaced so a
	// silent CPU fallback (GPU expected but not engaged) is visible in
	// the doctor / admin UI. Empty for engines that don't report one.
	Backend string `json:"backend,omitempty"`

	// Engine provenance. New fields (old CLIs/trays ignore them;
	// Version above keeps its binary-`--version` semantics for old
	// clients). Empty when the agent predates them.
	//
	// Mode is who owns the serving process: "spawned" (waired's own
	// child) / "adopted" (exact-pin orphan of a previous run; not
	// stoppable by waired). "borrowed" was reuse mode, removed in #489.
	Mode string `json:"mode,omitempty"`
	// LiveVersion is the serving engine's GET /api/version answer —
	// the version actually handling requests, which differs from
	// Version in adopted mode. "" until the engine has been ready once.
	LiveVersion string `json:"live_version,omitempty"`
	// PinnedVersion is the release waired bundles.
	PinnedVersion string `json:"pinned_version,omitempty"`
	// VersionWarning is the agent-computed mismatch warning: the live
	// engine version is not the pin. "" when versions agree (or are
	// unknown).
	VersionWarning string `json:"version_warning,omitempty"`
	// Serve tuning the agent exported to the engine (#621): the
	// effective context window, KV cache quantization, and request
	// parallelism. Zero/empty when the agent predates the tuning or
	// no sizing was possible (the engine then runs its own defaults).
	ContextLength int    `json:"context_length,omitempty"`
	KVCacheType   string `json:"kv_cache_type,omitempty"`
	NumParallel   int    `json:"num_parallel,omitempty"`
	// NumBatch is the forced generation ubatch (#642), delivered via a
	// derived model on spilled discrete-GPU hosts; 0 when left to Ollama's
	// automatic batch sizing.
	NumBatch int `json:"num_batch,omitempty"`
	// TuningWarning is the user-visible tuning outcome when something
	// is off: context floored below the manifest window, a silent f16
	// KV fallback, or a spill to system RAM. "" when the tuning applied
	// cleanly.
	TuningWarning string `json:"tuning_warning,omitempty"`
	// Model residency (waired-agent#879): whether the weights are in
	// (V)RAM right now, and until when. Every other readiness field
	// here answers "process alive + model file on disk", which is the
	// same on a host that answers in 0.5 s and one that will spend
	// 17-56 s reloading first (waired-agent#861).
	//
	// ModelResident is a pointer so "not observed" is distinguishable
	// from "observed, nothing loaded"; old agents omit it entirely.
	// ModelResidentUntil is RFC3339 and empty when nothing is resident
	// — note an indefinite keep-alive renders as a date centuries out,
	// so a far-future value is normal, not a bug.
	ModelResident      *bool  `json:"model_resident,omitempty"`
	ModelResidentModel string `json:"model_resident_model,omitempty"`
	ModelResidentUntil string `json:"model_resident_until,omitempty"`
	// ModelResidentIndefinitely reports a model the engine has no
	// intention of unloading. Separate from an empty ModelResidentUntil,
	// which means "resident, expiry unknown": the engine states an
	// indefinite hold by handing back a date centuries away, and passing
	// that on leaves every surface rendering "until 2318-11-30", which
	// reads as corruption rather than as the product default
	// (waired-agent#910).
	ModelResidentIndefinitely bool `json:"model_resident_indefinitely,omitempty"`
	// ModelResidentAt is when the residency reading above was taken
	// (RFC3339, waired-agent#837). Residency is a state someone has to go
	// and look at, on a probe cadence, so a reader has no way to tell a
	// fresh answer from one whose probe missed its tick — and the case
	// that matters is exactly the pathological one, where the machine is
	// so busy that the probe loop itself stretches. Empty on an agent
	// that does not publish it, and on one that has not looked.
	ModelResidentAt string `json:"model_resident_at,omitempty"`
	// ModelResidentIsActive reports whether what the engine holds is the
	// model THIS computer serves. Under one-model-resident
	// (docs/decisions/20260811/2340-…) a request for another model evicts
	// the one the router points at, so "something is loaded" and "the
	// right thing is loaded" are different answers and only the agent can
	// tell them apart: /api/ps returns an engine-native tag, and the
	// catalog id lives in state.
	//
	// A pointer, and nil — never false — whenever the serving tag cannot
	// be resolved. A wrong false would put "model not loaded" in front of
	// someone whose machine is perfectly warm.
	ModelResidentIsActive *bool `json:"model_resident_is_active,omitempty"`
	// LastError carries the engine's failure detail when State is
	// "failed" (e.g. the port-conflict refusal naming the foreign
	// engine's version and the remediation). Also set, whatever State
	// says, once FailureLatched is true: the latch outlives the Health
	// snapshot a Stop overwrites, and a give-up with no reason on it is
	// not worth publishing (#310).
	LastError string `json:"last_error,omitempty"`
	// FailureLatched reports that waired has STOPPED restarting this
	// engine automatically — the recovery budget is spent, and nothing
	// will change until an explicit `waired inference engine start` (or a
	// model switch) clears it.
	//
	// Distinct from State "failed", which is a reading of this instant: an
	// engine in a crash-restart cycle is failed on some ticks and starting
	// on others, and only this field separates "down, recovering" from
	// "down, and waiting will not help" (#310). False on agents that
	// predate it, which read as the recovering case — the safe default,
	// since it is the one where waiting is still the right advice.
	FailureLatched bool `json:"failure_latched,omitempty"`
}

type ActiveEndpoint struct {
	EndpointID string `json:"endpoint_id"`
	Runtime    string `json:"runtime"`
	ModelID    string `json:"model_id,omitempty"`
	State      string `json:"state"`
}

type ModelEntry struct {
	ModelID   string   `json:"model_id"`
	Aliases   []string `json:"aliases,omitempty"`
	State     string   `json:"state"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	VariantID string   `json:"variant_id,omitempty"`
	Source    string   `json:"source,omitempty"` // "ollama:qwen3:8b-q4_K_M" etc.
}

// PullJob is the 202-Accepted handle for an asynchronous pull.
type PullJob struct {
	JobID   string `json:"job_id"`
	ModelID string `json:"model_id"`
	Status  string `json:"status"`
}

// PullCancel is the answer to DELETE /waired/v1/models/{model_id}/pull.
//
// Status is "cancelled" when a download was stopped, or
// "not_downloading" when there was nothing in flight. Both are 200:
// the caller asked for this model not to be downloading, and in both
// cases it is not. JobID is set only for the former — there is no job to
// name in the latter.
type PullCancel struct {
	ModelID string `json:"model_id"`
	JobID   string `json:"job_id,omitempty"`
	Status  string `json:"status"`
}

// inferenceMux registers the inference handlers on mux. Called from
// Server.Handler when an InferenceProvider is wired.
func (s *Server) inferenceMux(mux *http.ServeMux) {
	if s.inference == nil {
		return
	}
	mux.HandleFunc("/waired/v1/inference/status", s.handleInferenceStatus)
	mux.HandleFunc("/waired/v1/inference/hardware", s.handleInferenceHardware)
	mux.HandleFunc("/waired/v1/inference/runtimes", s.handleInferenceRuntimes)
	mux.HandleFunc("/waired/v1/inference/select", s.handleInferenceSelect)

	mux.HandleFunc("/waired/v1/models", s.handleModelsCollection)
	// Subtree: handleModelsItem splits DELETE /waired/v1/models/{id}
	// (remove the model) from DELETE /waired/v1/models/{id}/pull (stop
	// its download) rather than registering a second pattern, so the
	// path-parsing rules for this subtree stay in one function.
	mux.HandleFunc("/waired/v1/models/", s.handleModelsItem)
	mux.HandleFunc("/waired/v1/models/pull", s.handleModelsPull)
}

// HostMemoryMeasurement is the install-time available-memory record,
// projected for display. AvailableGB is what the OS and everything
// resident at install time left of RAMTotalGB.
type HostMemoryMeasurement struct {
	AvailableGB int    `json:"available_gb"`
	TotalGB     int    `json:"total_gb,omitempty"`
	MeasuredAt  string `json:"measured_at,omitempty"`
}

func (s *Server) handleInferenceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	body := s.inference.Status(r.Context())
	// Read off the profile rather than added to the InferenceProvider
	// interface: the figure is already there, injected at construction
	// by WithRAMAvailableAtInstall, and every other consumer reads it
	// from exactly here.
	if hw := s.inference.Hardware(r.Context()); hw.RAMAvailableAtInstallGB > 0 {
		body.HostMemory = &HostMemoryMeasurement{
			AvailableGB: hw.RAMAvailableAtInstallGB,
			TotalGB:     hw.RAMTotalGB,
			MeasuredAt:  hw.RAMAvailableAtInstallMeasuredAt,
		}
	}
	if s.shareControl != nil {
		_, desired := s.shareControl.State()
		body.ShareWithMesh = string(desired)
		body.ShareSuspended = s.shareControl.IsSuspended()
	}
	if s.workerControl != nil {
		_, desired := s.workerControl.State()
		wr := &WorkerResponse{
			Mode:               desired.Mode,
			PinnedPeerDeviceID: desired.PinnedPeerDeviceID,
		}
		if desired.Mode == state.RoutingModePinned && desired.PinnedPeerDeviceID != "" {
			v := s.resolvePinStatus(r, desired.PinnedPeerDeviceID)
			wr.PinnedPeerName, wr.PinnedPeerStatus = v.Name, v.Status
			wr.PinnedPeerModel, wr.PinnedPeerCondition = v.Model, v.Condition
			// The tray reads the worker state from HERE, not from
			// GET /waired/v1/worker (Client.Worker has no caller), so the
			// display identifier has to be filled on both producers or
			// the CLI is fixed and the menu still leaks (#739).
			wr.PinnedPeerDisplayID = pinDisplayID(v, desired)
		}
		body.Worker = wr
	}
	if s.residencyControl != nil {
		if d, err := s.residencyControl.Residency(r.Context()); err == nil {
			res := residencyResponse(d)
			// The tray gates the residency presets AND the Unload item on
			// this block, so an engine with no residency axis has to say so
			// here or those controls are offered on a host where they
			// cannot work (waired-ai/waired-agent#943).
			supported := s.residencyControl.ResidencySupported()
			res.Supported = &supported
			body.Residency = &res
		}
	}
	if body.Active != nil {
		body.FirstToken = firstTokenReading(s.observability.Ring, body.Active.ModelID)
	}
	if s.engineControl != nil {
		power, managed := s.engineControl.EngineState()
		body.EnginePower = string(power)
		body.EngineManaged = managed
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleInferenceHardware(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	writeJSON(w, http.StatusOK, s.inference.Hardware(r.Context()))
}

func (s *Server) handleInferenceRuntimes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtimes": s.inference.Runtimes(r.Context())})
}

func (s *Server) handleInferenceSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	var req router.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "invalid JSON: "+err.Error()))
		return
	}
	sel, err := s.inference.Select(r.Context(), req)
	if err != nil {
		writeJSON(w, mapRouterStatus(err), errorBody("selection_failed", err.Error()))
		return
	}
	// Select commits, and committing acquires the peer's admission slot
	// (router.Selection.Release: "Always non-nil — callers MUST
	// defer-call it"). This endpoint is a dry run, so it has to give the
	// slot back: without this, one `waired infer --explain` held a slot
	// for the daemon's remaining lifetime, and on a host whose effective
	// admission capacity is 1 that was enough to make every later
	// request read "every matching mesh peer is at capacity"
	// (waired-agent#624). Nil guard and defer for the same reasons as
	// the gateway's two release sites.
	if sel.Release != nil {
		defer sel.Release()
	}
	writeJSON(w, http.StatusOK, scrubSelectionForDisplay(sel))
}

// scrubSelectionForDisplay replaces the functional peer key in Runtime
// with the Selector's display identifier before the Selection leaves the
// management API.
//
// This endpoint is a dry-run explain surface — `waired infer --explain`
// prints its fields verbatim and nothing dials from the response — while
// Selection.Runtime carries "remote:<DeviceID>" because the in-process
// gateway resolves a peer adapter from it. For a Public Share peer that
// DeviceID must never be shown; only the grant pseudonym may (public
// share spec §8.5). Own-network peers are unaffected: their display
// identifier IS their DeviceID.
func scrubSelectionForDisplay(sel router.Selection) router.Selection {
	const remotePrefix = "remote:"
	if sel.PeerDisplayID == "" || !strings.HasPrefix(sel.Runtime, remotePrefix) {
		return sel
	}
	sel.Runtime = remotePrefix + sel.PeerDisplayID
	return sel
}

// handleModelsCollection serves GET /waired/v1/models. The trailing
// "/pull" sub-path is handled separately via the dedicated route.
func (s *Server) handleModelsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": modelsWithSizes(r.Context(), s.inference)})
}

// modelsWithSizes is ListModels plus whatever on-disk sizes the engine can
// account for right now.
//
// The size is asked for here rather than inside ListModels because it
// costs an engine round trip and ListModels is also a control-path read
// (#661). A stopped or wedged engine reports nothing and the entries go
// out with whatever the state file holds, which is what this endpoint
// returned before sizes existed at all.
func modelsWithSizes(ctx context.Context, p InferenceProvider) []ModelEntry {
	entries := p.ListModels(ctx)
	sizes := p.ModelSizes(ctx)
	for i := range entries {
		if b := sizes[entries[i].ModelID]; b > 0 {
			entries[i].SizeBytes = b
		}
	}
	return entries
}

// handleModelsPull serves POST /waired/v1/models/pull.
func (s *Server) handleModelsPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", `body must be {"model":"..."}`))
		return
	}
	job, err := s.inference.PullModel(r.Context(), body.Model)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("pull_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleModelsItem serves DELETE /waired/v1/models/{model_id} (remove the
// model) and DELETE /waired/v1/models/{model_id}/pull (stop the download
// in flight for it, waired-agent#633).
func (s *Server) handleModelsItem(w http.ResponseWriter, r *http.Request) {
	const prefix = "/waired/v1/models/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	// Defend against the /models/pull subroute being routed here in
	// older mux setups.
	if rest == "" || rest == "pull" {
		writeJSON(w, http.StatusNotFound, errorBody("not_found", "no model id"))
		return
	}
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "DELETE only"))
		return
	}
	// The download, not the model. Checked before the model-id path so a
	// model whose id ends in "pull" cannot shadow it — ids come from the
	// catalog and none does, but the split must not depend on that.
	if modelID, isCancel := strings.CutSuffix(rest, "/pull"); isCancel {
		if modelID == "" {
			writeJSON(w, http.StatusNotFound, errorBody("not_found", "no model id"))
			return
		}
		res, err := s.inference.CancelPull(r.Context(), modelID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("cancel_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	if err := s.inference.DeleteModel(r.Context(), rest); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("delete_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"model_id": rest, "status": "deleted"})
}

func errorBody(code, msg string) map[string]string {
	return map[string]string{"error_code": code, "message": msg}
}

// mapRouterStatus maps a router selection error to the status
// /inference/select answers with.
//
// The serving surfaces map the same sentinels in
// gateway.respondSelectionError and its Anthropic twin, and this
// endpoint is the dry run OF those surfaces — `waired infer --explain`
// exists to explain what a real request would do. When the two
// disagree, explain is a worse signal than the thing it explains
// (waired-agent#710): a saturated mesh answered 500 here and 503 there,
// and 500 says the daemon is broken rather than busy.
//
// The responders are the comparison, NOT gateway.selectionStatus, which
// feeds the gateway's request record rather than its response. Reading
// the record as if it were the wire is how ErrHardwareInsufficient came
// to be written up below as a divergence it never was
// (waired-agent#740).
//
// Two divergences are deliberate and stay:
//
//   - ErrCapabilityNotMet is 422 here and 400 at the gateway. Both
//     describe a request this host cannot satisfy; 422 is the more precise
//     reading (the JSON parsed fine, its requirements were the problem)
//     and this endpoint has no OpenAI/Anthropic wire shape to stay
//     compatible with. ErrHardwareInsufficient is read that way on every
//     side — 422 here, 422 on both wires, and since waired-agent#740 422
//     in the gateway's record too.
//   - ErrNoEndpointForWindow is 500 on both sides today. Recorded here as
//     today's behaviour rather than asserted as intended — see
//     TestMapRouterStatus_AgreesWithServingSurfaces.
func mapRouterStatus(err error) int {
	switch {
	case errors.Is(err, router.ErrModelNotFound):
		return http.StatusNotFound
	case errors.Is(err, router.ErrCapabilityNotMet),
		errors.Is(err, router.ErrHardwareInsufficient):
		return http.StatusUnprocessableEntity
	case errors.Is(err, router.ErrModelNotReady) && !router.ModelIsArriving(err):
		// A model no host serves and none is fetching: both responders
		// answer 404 rather than a retryable 503, and explain has to
		// dry-run what they would really do (waired-agent#788).
		return http.StatusNotFound
	case errors.Is(err, router.ErrModelNotReady),
		// A mesh that is busy, silent, or whose pinned peer is
		// unreachable is not an internal error — the gateway has said so
		// with a dedicated code since #707, and this endpoint now agrees.
		errors.Is(err, router.ErrLocalInferenceOff),
		errors.Is(err, router.ErrAllPeersOverloaded),
		errors.Is(err, router.ErrPeersDidNotAnswer),
		errors.Is(err, router.ErrPinnedPeerUnreachable),
		errors.Is(err, router.ErrRuntimeNotInstalled):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
