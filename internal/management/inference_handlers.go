package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
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
	PullModel(ctx context.Context, modelOrAlias string) (PullJob, error)
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
type InferenceStatus struct {
	SubsystemState  string                   `json:"subsystem_state"`
	Runtimes        map[string]RuntimeStatus `json:"runtimes"`
	Models          ModelsSnapshot           `json:"models"`
	ActiveEndpoints []ActiveEndpoint         `json:"active_endpoints"`

	// Active is the engine + model the agent is committed to serving
	// (mirrors state.json `active`). nil when no decision has been
	// recorded yet (= run `waired runtimes install --auto`).
	Active *ActiveSelection `json:"active,omitempty"`

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

	// DesiredState surfaces the operator's persisted enable/disable
	// intent for the inference subsystem ("enabled" | "disabled").
	// Empty when the daemon has no InferenceController attached
	// (older builds, tests). The tray uses this to decide whether the
	// toggle button should be in the "Disable" or "Enable" position
	// independently of SubsystemState (which describes engine health).
	DesiredState string `json:"desired_state,omitempty"`

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

	// Worker is the operator's manual inference routing choice
	// (Tailscale-exit-node-style). nil when the daemon has no
	// WorkerController attached. Embedding the resolved state here
	// (instead of forcing the tray into a separate GET /v1/worker
	// poll) lets the tray refresh "Inference worker" submenu state
	// in the same 5 s tick that already drives the rest of the
	// menu. Set by Server.handleInferenceStatus.
	Worker *WorkerResponse `json:"worker,omitempty"`

	// EnginePower surfaces the live hard engine power axis (#186):
	// "running" | "stopped" | "starting". Empty when the daemon has no
	// EngineController attached (older builds, tests). The tray/CLI use
	// it to render the Stop/Start engine control independently of the
	// soft DesiredState toggle. Set by Server.handleInferenceStatus.
	EnginePower string `json:"engine_power,omitempty"`

	// EngineManaged is false in reuse mode (#188), where the engine is
	// the user's own `ollama serve` and the power axis does not apply.
	// Only meaningful alongside a non-empty EnginePower. The tray renders
	// the Stop/Start control disabled ("reused — not managed") when false.
	EngineManaged bool `json:"engine_managed,omitempty"`
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
}

// ModelsSnapshot summarises model lifecycle states for display.
type ModelsSnapshot struct {
	Ready       []string `json:"ready"`
	Downloading []string `json:"downloading"`
	Failed      []string `json:"failed,omitempty"`
	// Downloads carries byte-level progress for the in-flight downloads
	// named in Downloading. Optional: old clients read Downloading (names
	// only) and ignore this; new clients render a percentage + size from
	// it. A model can be in Downloading without a Downloads entry (queued
	// before the first progress line, or progress unknown).
	Downloads []ModelDownload `json:"downloads,omitempty"`
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
	// child) / "borrowed" (reuse mode, the user's engine) / "adopted"
	// (exact-pin orphan of a previous run; not stoppable by waired).
	Mode string `json:"mode,omitempty"`
	// LiveVersion is the serving engine's GET /api/version answer —
	// the version actually handling requests, which differs from
	// Version in borrowed/adopted modes. "" until the engine has been
	// ready once.
	LiveVersion string `json:"live_version,omitempty"`
	// PinnedVersion is the release waired bundles (bundled mode only).
	PinnedVersion string `json:"pinned_version,omitempty"`
	// VersionWarning is the agent-computed mismatch warning: bundled
	// live != pin, or a reuse engine below the supported floor. ""
	// when versions agree (or are unknown).
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
	// KV fallback, a spill to system RAM, or a reuse engine waired
	// cannot tune. "" when the tuning applied cleanly.
	TuningWarning string `json:"tuning_warning,omitempty"`
	// LastError carries the engine's failure detail when State is
	// "failed" (e.g. the port-conflict refusal naming the foreign
	// engine's version and the remediation).
	LastError string `json:"last_error,omitempty"`
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
	mux.HandleFunc("/waired/v1/models/", s.handleModelsItem)
	mux.HandleFunc("/waired/v1/models/pull", s.handleModelsPull)
}

func (s *Server) handleInferenceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	body := s.inference.Status(r.Context())
	if s.shareControl != nil {
		_, desired := s.shareControl.State()
		body.ShareWithMesh = string(desired)
	}
	if s.workerControl != nil {
		_, desired := s.workerControl.State()
		wr := &WorkerResponse{
			Mode:               desired.Mode,
			PinnedPeerDeviceID: desired.PinnedPeerDeviceID,
		}
		if desired.Mode == state.RoutingModePinned && desired.PinnedPeerDeviceID != "" {
			wr.PinnedPeerName, wr.PinnedPeerStatus = s.resolvePinStatus(r, desired.PinnedPeerDeviceID)
		}
		body.Worker = wr
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
	writeJSON(w, http.StatusOK, map[string]any{"models": s.inference.ListModels(r.Context())})
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

// handleModelsItem serves DELETE /waired/v1/models/{model_id}.
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
	if err := s.inference.DeleteModel(r.Context(), rest); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("delete_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"model_id": rest, "status": "deleted"})
}

func errorBody(code, msg string) map[string]string {
	return map[string]string{"error_code": code, "message": msg}
}

func mapRouterStatus(err error) int {
	switch {
	case errors.Is(err, router.ErrModelNotFound):
		return http.StatusNotFound
	case errors.Is(err, router.ErrCapabilityNotMet),
		errors.Is(err, router.ErrHardwareInsufficient):
		return http.StatusUnprocessableEntity
	case errors.Is(err, router.ErrModelNotReady),
		errors.Is(err, router.ErrRuntimeNotInstalled):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
