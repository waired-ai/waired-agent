// Package management implements the localhost-only HTTP API that the waired
// CLI talks to. The base path matches docs/specs/waired_product_spec.md §12.5
// so we can extend it into the full Local Management API later without
// breaking the CLI.
package management

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

const DefaultListen = "127.0.0.1:9476"

type Status struct {
	// NetworkID and DeviceID identify the agent + the network it has
	// enrolled into. Surfaced primarily so the testnet CI fallback
	// runner (scripts/dev/testnet-fallback-runner.sh) can discover
	// per-VM device ids and the shared network id from Cloud Logging
	// alone, without SSH. omitempty so a pre-enrollment Status (no
	// identity loaded) emits a clean payload.
	NetworkID  string `json:"network_id,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name"`
	OverlayIP  string `json:"overlay_ip"`
	ListenPort int    `json:"listen_port"`
	PeerCount  int    `json:"peer_count"`

	// NAT-traversal observability. Empty / "unknown" when the disco
	// subsystem is disabled (--punch-enabled=false / --force-relay) or
	// hasn't yet completed an observation.
	DiscoEnabled bool   `json:"disco_enabled"`
	ObservedAddr string `json:"observed_addr,omitempty"`
	// ObservedAddrV6 is the most recent v6 STUN observation. Stays
	// populated even when the current round's bestObs picked v4 (v6
	// can flap due to GCE path RTT variance), so a downstream
	// verifier can assert v6 reachability monotonically.
	ObservedAddrV6 string `json:"observed_addr_v6,omitempty"`
	// FirstObservedV6Unix is the wall-clock unix seconds of the agent's
	// first ever v6 STUN observation. 0 until the disco service stamps
	// it on the first v6 sample. Used by the testnet verifier to
	// histogram v6 convergence latency per-agent (vs agent process
	// start time / kickoff event in Cloud Logging) and to localise
	// the per-agent BGP propagation tail described in docs/todo.md
	// "IPv6 STUN observation: per-agent v6 path flake on GCE testnet".
	FirstObservedV6Unix int64 `json:"first_observed_v6_unix,omitempty"`
	// STUNAttempts{V4,V6} and STUNResponses{V4,V6} are lifetime counts
	// since process start. attempts ≫ responses on v6 (with v4 in
	// balance) attributes the v6 flake to network loss after the agent
	// sent (relay receive or return-path drop); attempts ≈ responses
	// but FirstObservedV6Unix unset attributes it to bestObs race
	// (responses arrived but observeOnce timed out). Together the
	// per-family counters let the verifier classify each missing
	// agent's failure mode without ssh.
	STUNAttemptsV4  uint64 `json:"stun_attempts_v4,omitempty"`
	STUNAttemptsV6  uint64 `json:"stun_attempts_v6,omitempty"`
	STUNResponsesV4 uint64 `json:"stun_responses_v4,omitempty"`
	STUNResponsesV6 uint64 `json:"stun_responses_v6,omitempty"`
	NATType         string `json:"nat_type,omitempty"`

	// Pause/resume state. Empty when the server has no PauseController
	// attached (older builds, tests). When populated:
	//   Phase        — the agent's live mode ("active" or "paused")
	//   DesiredPhase — the operator's persisted intent; differs from
	//                  Phase when the daemon hasn't yet applied a
	//                  pending pause/resume across a restart.
	Phase        string `json:"phase,omitempty"`
	DesiredPhase string `json:"desired_phase,omitempty"`

	// Peers carries one entry per peer in the current Network Map with
	// the reconciler's per-path-quality state. testnet-fallback-* scripts
	// poll this field to decide whether a downgrade/upgrade has fired.
	Peers []PeerStatus `json:"peers,omitempty"`
}

// PeerStatus is the management-API view of one peer's path-selection
// state. All fields are populated by the agent's reconciler; consumers
// (testnet scripts, future CLI status command) treat empty/zero as
// "no signal yet".
type PeerStatus struct {
	DeviceID string `json:"device_id"`
	// DeviceName is the human-readable label from the peer's
	// NetworkMap entry. Surfaced for tray / CLI rendering ("alice-laptop
	// — RTX 4090") since DeviceID alone is opaque. Empty for peers
	// that have not yet pushed an identity name to the CP.
	DeviceName            string  `json:"device_name,omitempty"`
	CurrentPath           string  `json:"current_path"` // "direct" | "relay"
	LastSwitchAt          string  `json:"last_switch_at,omitempty"`
	LastSwitchReason      string  `json:"last_switch_reason,omitempty"`
	DirectRTTMS           float64 `json:"direct_rtt_ms,omitempty"`
	RelayRTTMS            float64 `json:"relay_rtt_ms,omitempty"`
	DirectSampleCount     int     `json:"direct_sample_count,omitempty"`
	RelaySampleCount      int     `json:"relay_sample_count,omitempty"`
	DirectMissStreak      int     `json:"direct_miss_streak,omitempty"`
	LastDirectEvidence    string  `json:"last_direct_evidence,omitempty"`
	HasDiscoHint          bool    `json:"has_disco_hint,omitempty"`
	ObservedAddr          string  `json:"observed_addr,omitempty"`
	CallMeMaybeSentAt     string  `json:"call_me_maybe_sent_at,omitempty"`
	CallMeMaybeSentCount  int     `json:"call_me_maybe_sent_count,omitempty"`
	CallMeMaybeRecvAt     string  `json:"call_me_maybe_recv_at,omitempty"`
	CallMeMaybeRecvCount  int     `json:"call_me_maybe_recv_count,omitempty"`
	CallMeMaybeFailStreak int     `json:"call_me_maybe_fail_streak,omitempty"`
	// LastUpgradeRejectReason / RecentDirectPongs surface the
	// reconciler's relay→direct upgrade-gate decision. Used by the
	// testnet fallback-runner to attribute a stuck-on-relay state
	// to a specific gate ("samples","ewma_zero","ring_not_full",
	// "ratio","dwell","force_relay") without live instrumentation.
	// Both empty/nil when currentPath==direct (no upgrade needed).
	LastUpgradeRejectReason string `json:"last_upgrade_reject_reason,omitempty"`
	RecentDirectPongs       []bool `json:"recent_direct_pongs,omitempty"`

	// Hardware summarises the peer's GPU/RAM as advertised in its
	// InferenceState (signer.HardwareSummary). Surfaced here so the
	// tray / waired CLI can render "alice-laptop — RTX 4090 (24 GB)"
	// without re-fetching the inference mesh snapshot. nil when the
	// peer has not yet pushed an inference state, or predates Phase 7's
	// Hardware field. Re-encoded into a management-API-local view type
	// so this package keeps no signer/* dependency.
	Hardware *PeerHardware `json:"hardware,omitempty"`
}

// PeerHardware is the management-API projection of
// signer.HardwareSummary. Only the first GPU is surfaced because the
// tray/CLI rows are single-line "model + VRAM" labels; multi-GPU
// hosts can still appear distinct via their device name. ComputeCap
// is included so a future filter ("hosts with CUDA ≥ 8.0") can read
// it without an extra round trip.
type PeerHardware struct {
	GPUModel    string `json:"gpu_model,omitempty"`
	VRAMTotalMB int    `json:"vram_total_mb,omitempty"`
	ComputeCap  string `json:"compute_cap,omitempty"`
	RAMTotalGB  int    `json:"ram_total_gb,omitempty"`
}

type PingResult struct {
	Peer           string  `json:"peer"`
	OK             bool    `json:"ok"`
	LatencyMS      float64 `json:"latency_ms"`
	DeviceFromPeer string  `json:"device_from_peer"`
	TimeFromPeer   string  `json:"time_from_peer"`
}

type StatusProvider interface {
	Status() Status
}

type Pinger interface {
	PingPeer(ctx context.Context, peer string) (PingResult, error)
}

// PauseController is implemented by the agent. Pause/Resume mutate both
// the in-memory phase flag (so the gateway middleware reflects the
// change immediately) and the persisted desired-phase file (so a
// subsequent daemon restart starts in the right phase). Phase reports
// the current live phase plus the persisted operator intent — they
// differ briefly while a pause/resume is being applied.
type PauseController interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Phase() (current, desired state.Phase)
}

// IdentityView is the tray-facing projection of the agent's enrollment
// state. Mirrors the user-relevant fields from internal/identity.Identity
// plus an Enrolled flag so a future not-yet-enrolled daemon mode can
// surface "no signed-in account" without 404-ing the route.
type IdentityView struct {
	Enrolled     bool   `json:"enrolled"`
	AccountEmail string `json:"account_email,omitempty"`
	NetworkName  string `json:"network_name,omitempty"`
	NetworkID    string `json:"network_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	DeviceName   string `json:"device_name,omitempty"`
	OverlayIP    string `json:"overlay_ip,omitempty"`
	ControlURL   string `json:"control_url,omitempty"`
}

type IdentityProvider interface {
	Identity() IdentityView
}

// InferenceController is implemented by the agent. Enable/Disable mutate
// both the in-memory disabled flag (so the gateway middleware reflects
// the change immediately) and the persisted desired-inference file (so
// a subsequent daemon restart honours the operator's last choice).
// State reports the current live state plus the persisted operator
// intent — they differ briefly while a transition is being applied.
type InferenceController interface {
	Enable(ctx context.Context) error
	Disable(ctx context.Context) error
	State() (current, desired state.InferenceState)
}

// ShareController is implemented by the agent. Share/Unshare mutate
// both the in-memory live flag (so the inference probe loop and the
// peer-overlay listener middleware reflect the change immediately)
// and the persisted desired-share file (so a subsequent restart
// honours the operator's last choice). State has the same shape as
// InferenceController.State.
type ShareController interface {
	Share(ctx context.Context) error
	Unshare(ctx context.Context) error
	State() (current, desired state.ShareMeshState)
}

// WorkerController is implemented by the agent for the Tailscale-
// exit-node-style manual inference routing toggle. The three setters
// (SetMode / SetPin / Clear) mutate both the in-memory atomic
// preference (so the Selector hot path reflects the change on the
// next request) and the persisted desired-worker file (so a
// subsequent restart honours the operator's last choice). State has
// the same shape as InferenceController.State, returning the current
// live RoutingPreference plus the persisted operator intent.
type WorkerController interface {
	SetMode(ctx context.Context, mode state.RoutingMode) error
	SetPin(ctx context.Context, peerDeviceID string) error
	Clear(ctx context.Context) error
	State() (current, desired state.RoutingPreference)
}

// ClaudeRouteControl is implemented by the agent for the Claude Code route
// escape hatch (#580). SetMode / SetFallback mutate both the boot-level
// in-memory atomics (so the intercept hot path reflects the change on the
// next request) and the persisted desired-claude-route file (so a restart
// honours the last choice). State returns the live preference plus the
// most recent post-dispatch fallback event, for `waired claude status`.
type ClaudeRoutingControl interface {
	// SetClass persists + applies one traffic class's route. class is
	// "main" or "sub"; route is auto|waired|anthropic (and, for sub only,
	// "same" to inherit main).
	SetClass(ctx context.Context, class string, route state.ClaudeRouteClass) error
	State() ClaudeRoutingState
}

// ClaudeRoutingState is the body of GET /waired/v1/integration/claude/route
// and the 200 body of a POST. LastFallback is nil until a fallback has fired
// at least once this process lifetime; LastLocalModel is empty until the
// intercept has served a mapped local response (#602). Both are in-memory
// only and reset on agent restart.
type ClaudeRoutingState struct {
	Policy         state.ClaudeRoutingPolicy   `json:"policy"`
	LastFallback   *ClaudeRoutingFallbackEvent `json:"last_fallback,omitempty"`
	LastLocalModel string                      `json:"last_local_model,omitempty"`
	// LastServedBy is the mesh peer DeviceID that served the last
	// waired-served Claude request; empty when this device served it.
	LastServedBy string `json:"last_served_by,omitempty"`
}

// ClaudeRoutingFallbackEvent records the last time a class's chosen route
// could not serve and the request was rerouted — the visibility signal the
// CLI and tray surface so a degrade is never silent. Direction is
// "anthropic" when an auto request was rescued by the real Anthropic API
// (Waired failed), or "local" when an anthropic/pinned-peer request was
// served locally instead (upstream/peer unavailable).
type ClaudeRoutingFallbackEvent struct {
	When      time.Time `json:"when"`
	Class     string    `json:"class,omitempty"`
	Reason    string    `json:"reason"`
	Peer      string    `json:"peer_device_id,omitempty"`
	Direction string    `json:"direction"`
	Count     int64     `json:"count"`
}

// EnginePowerState is the live engine power axis (#186), orthogonal to
// the soft enable/disable (InferenceController) and share (ShareController)
// axes. "running" = engine process up; "stopped" = hard-stopped (parked),
// memory freed; "starting" = restart in flight after a start request.
type EnginePowerState string

const (
	EnginePowerRunning  EnginePowerState = "running"
	EnginePowerStopped  EnginePowerState = "stopped"
	EnginePowerStarting EnginePowerState = "starting"
)

// EngineController is implemented by the agent for the hard engine power
// axis (#186): StopEngine kills the local `ollama serve` to free VRAM/RAM
// and latches it stopped (so request traffic doesn't revive it);
// StartEngine clears the latch and restarts asynchronously. EngineState
// reports the live power state plus whether the engine is waired-managed
// (false in reuse mode, where power control does not apply). Unlike the
// soft toggle this state is NOT persisted — a daemon restart returns to
// config-driven startup.
type EngineController interface {
	StopEngine(ctx context.Context) error
	StartEngine(ctx context.Context) error
	EngineState() (power EnginePowerState, managed bool)
}

type Server struct {
	status              StatusProvider
	pinger              Pinger
	pause               PauseController            // optional; nil disables /waired/v1/pause and /waired/v1/resume
	inference           InferenceProvider          // optional; nil disables /waired/v1/inference/status etc. and /waired/v1/models*
	infControl          InferenceController        // optional; nil disables /waired/v1/inference/{enable,disable}
	engineControl       EngineController           // optional; nil disables /waired/v1/inference/engine/{stop,start}
	shareControl        ShareController            // optional; nil disables /waired/v1/inference/share/{enable,disable}
	publicShare         PublicShareController      // optional; nil disables /waired/v1/public/share{,/enable,/disable}
	workerControl       WorkerController           // optional; nil disables /waired/v1/worker and worker_routing in /v1/inference/status
	infMesh             InferenceMeshProvider      // optional; nil disables /waired/v1/inference/mesh
	identity            IdentityProvider           // optional; nil disables /waired/v1/identity (tray-facing)
	claudeIntegration   *ClaudeIntegrationConfig   // optional; nil disables /waired/v1/integration/claude
	claudeRouting       ClaudeRoutingControl       // optional; nil disables /waired/v1/integration/claude/route
	openCodeIntegration *OpenCodeIntegrationConfig // optional; nil disables /waired/v1/integration/opencode{,/reconfigure}
	openClawIntegration *OpenClawIntegrationConfig // optional; nil disables /waired/v1/integration/openclaw{,/reconfigure}
	catalog             *CatalogConfig             // optional; nil disables /waired/v1/inference/catalog and /preferred-model
	publicUse           *PublicUseConfig           // optional; nil disables /waired/v1/public/* (consumer Public Share settings + consent)
	observability       ObservabilityConfig        // optional; zero value disables all Phase 9 endpoints
	login               LoginController            // optional; nil disables /waired/v1/login/{start,status}
	update              UpdateController           // optional; nil disables /waired/v1/update/{check,status,settings}
	setupExecutor       SetupExecutorController    // optional; nil disables /waired/v1/setup/{state,executor}

	// browserHardening, when true, wraps the mux in browserGuard (Host /
	// Origin allow-listing + Content-Type-on-writes). Off by default so
	// unit tests drive Handler() without loopback Hosts; production wiring
	// enables it via WithBrowserHardening. See security.go.
	browserHardening bool

	// enforceSocketWrites, when true, makes the loopback-TCP Serve path
	// refuse mutating verbs while the local IPC socket is up, so writes can
	// only arrive over the peer-local socket/pipe (waired#838). Off by
	// default: unit tests drive Handler() (which never applies the guard),
	// and production leaves it off until the CLI/tray are migrated to the
	// socket. Set via WithSocketWritesOnly. See socket.go.
	enforceSocketWrites bool
	// socketUp reflects whether ServeLocal currently has the local IPC
	// socket bound. writeGuard reads it so a socket bind failure fails OPEN
	// (TCP writes keep working, behind the #836 browserGuard) instead of
	// bricking control of the agent.
	socketUp atomic.Bool
}

func New(status StatusProvider, pinger Pinger) *Server {
	return &Server{status: status, pinger: pinger}
}

// WithInference attaches an InferenceProvider so the server exposes
// /waired/v1/inference/* and /waired/v1/models* routes. Pass nil to
// disable. Returns the receiver for chaining.
func (s *Server) WithInference(p InferenceProvider) *Server {
	s.inference = p
	return s
}

// WithPause attaches a PauseController so the server exposes
// /waired/v1/pause and /waired/v1/resume, and includes phase/desired_phase
// in /waired/v1/status responses.
func (s *Server) WithPause(p PauseController) *Server {
	s.pause = p
	return s
}

// WithIdentity attaches an IdentityProvider so the server exposes
// GET /waired/v1/identity for the tray UI. Pass nil to disable.
// Kept off the existing Status struct because Status is a hot path
// consumed by testnet-fallback scripts and per-peer reconciler
// observability — tray-only fields would muddy that contract.
func (s *Server) WithIdentity(p IdentityProvider) *Server {
	s.identity = p
	return s
}

// WithInferenceControl attaches an InferenceController so the server
// exposes POST /waired/v1/inference/enable and /waired/v1/inference/disable.
// Pass nil to disable.
func (s *Server) WithInferenceControl(c InferenceController) *Server {
	s.infControl = c
	return s
}

// WithEngineControl attaches an EngineController so the server exposes
// POST /waired/v1/inference/engine/stop and /waired/v1/inference/engine/start
// (the hard engine power axis, #186) and surfaces engine_power in
// /waired/v1/inference/status. Pass nil to disable. Independent of
// WithInferenceControl: the soft toggle gates the gateway while the engine
// stays warm; this axis actually stops the process to free memory.
func (s *Server) WithEngineControl(c EngineController) *Server {
	s.engineControl = c
	return s
}

// WithShareControl attaches a ShareController so the server exposes
// POST /waired/v1/inference/share/enable and /waired/v1/inference/share/disable,
// and surfaces share_with_mesh in /waired/v1/inference/status. Pass nil
// to disable. Independent of WithInferenceControl so an operator can
// keep the engine running (inference enabled) but unshare it from the
// mesh.
func (s *Server) WithShareControl(c ShareController) *Server {
	s.shareControl = c
	return s
}

// WithWorkerControl attaches a WorkerController so the server exposes
// GET/POST /waired/v1/worker (the Tailscale-exit-node-style manual
// routing toggle) and embeds the resolved worker state in
// /waired/v1/inference/status responses. Pass nil to disable.
// Independent of InferenceController and ShareController — the
// routing axis is outbound (where this agent's requests go) while
// the other two govern the local-engine surface.
func (s *Server) WithWorkerControl(c WorkerController) *Server {
	s.workerControl = c
	return s
}

// WithClaudeRouting attaches a ClaudeRoutingControl so the server exposes
// GET/POST /waired/v1/integration/claude/route — the unified per-class
// Claude routing policy (main / subagents → auto|waired|anthropic) that
// `waired claude route`, the /waired-route slash command, and the tray
// drive. Boot-level (not session-scoped): the toggle works even before
// enrollment. Pass nil to disable.
func (s *Server) WithClaudeRouting(c ClaudeRoutingControl) *Server {
	s.claudeRouting = c
	return s
}

// WithInferenceMesh attaches an InferenceMeshProvider so the server
// exposes GET /waired/v1/inference/mesh — the snapshot of every
// peer's pushed inference engine state plus the agent's own. Used by
// `waired claude --waired-diagnose` and the tray for diagnostics.
// Pass nil to disable the route. (Phase 3 of the CP mesh inference
// aggregation feature.)
func (s *Server) WithInferenceMesh(p InferenceMeshProvider) *Server {
	s.infMesh = p
	return s
}

// WithLogin attaches a LoginController so the server exposes
// POST /waired/v1/login/start and GET /waired/v1/login/status — the
// daemon-driven (Tailscale-model) login surface the tray and CLI drive
// in place of spawning `pkexec waired init`. Pass nil to disable.
func (s *Server) WithLogin(c LoginController) *Server {
	s.login = c
	return s
}

// WithUpdateController attaches an UpdateController so the server exposes
// POST /waired/v1/update/check, GET /waired/v1/update/status, and POST
// /waired/v1/update/settings — the manual-update surface (#293) plus the
// background-check / update-prompt preference (#294) the CLI (`waired
// update`) and tray drive. The daemon only checks; the apply is
// client-driven under elevation. Pass nil to disable.
func (s *Server) WithUpdateController(c UpdateController) *Server {
	s.update = c
	return s
}

// Handler returns the loopback-TCP HTTP handler: the shared route mux
// wrapped in the loopback-source guard and the #836 browser hardening.
func (s *Server) Handler() http.Handler {
	return loopbackOnly(browserGuard(s.mux(), s.browserHardening))
}

// mux builds the route table shared by the loopback-TCP handler
// (Handler) and the local IPC socket handler (socketHandler, see
// socket.go). It carries no transport middleware so both listeners expose
// exactly the same routes.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/status", s.handleStatus)
	mux.HandleFunc("/waired/v1/ping", s.handlePing)
	mux.HandleFunc("/waired/v1/pause", s.handlePause)
	mux.HandleFunc("/waired/v1/resume", s.handleResume)
	mux.HandleFunc("/waired/v1/inference/enable", s.handleInferenceEnable)
	mux.HandleFunc("/waired/v1/inference/disable", s.handleInferenceDisable)
	if s.engineControl != nil {
		mux.HandleFunc("/waired/v1/inference/engine/stop", s.handleEngineStop)
		mux.HandleFunc("/waired/v1/inference/engine/start", s.handleEngineStart)
	}
	mux.HandleFunc("/waired/v1/inference/share/enable", s.handleShareEnable)
	mux.HandleFunc("/waired/v1/inference/share/disable", s.handleShareDisable)
	if s.workerControl != nil {
		mux.HandleFunc("/waired/v1/worker", s.handleWorker)
	}
	s.inferenceMux(mux)
	if s.infMesh != nil {
		mux.HandleFunc("/waired/v1/inference/mesh", s.handleInferenceMesh)
	}
	if s.identity != nil {
		mux.HandleFunc("/waired/v1/identity", s.handleIdentity)
	}
	if s.login != nil {
		mux.HandleFunc("/waired/v1/login/start", s.handleLoginStart)
		mux.HandleFunc("/waired/v1/login/status", s.handleLoginStatus)
	}
	if s.setupExecutor != nil {
		mux.HandleFunc("/waired/v1/setup/state", s.handleSetupState)
		mux.HandleFunc("/waired/v1/setup/executor", s.handleSetupExecutor)
	}
	if s.update != nil {
		mux.HandleFunc("/waired/v1/update/check", s.handleUpdateCheck)
		mux.HandleFunc("/waired/v1/update/status", s.handleUpdateStatus)
		mux.HandleFunc("/waired/v1/update/settings", s.handleUpdateSettings)
	}
	if s.claudeIntegration != nil {
		mux.HandleFunc("/waired/v1/integration/claude", s.handleClaudeIntegration)
	}
	if s.claudeRouting != nil {
		mux.HandleFunc("/waired/v1/integration/claude/route", s.handleClaudeRouting)
	}
	if s.openCodeIntegration != nil {
		mux.HandleFunc("/waired/v1/integration/opencode", s.handleOpenCodeIntegration)
		mux.HandleFunc("/waired/v1/integration/opencode/reconfigure", s.handleOpenCodeReconfigure)
	}
	if s.openClawIntegration != nil {
		mux.HandleFunc("/waired/v1/integration/openclaw", s.handleOpenClawIntegration)
		mux.HandleFunc("/waired/v1/integration/openclaw/reconfigure", s.handleOpenClawReconfigure)
	}
	if s.catalog != nil && s.catalog.PreferencePath != "" {
		mux.HandleFunc("/waired/v1/inference/catalog", s.handleInferenceCatalog)
		mux.HandleFunc("/waired/v1/inference/preferred-model", s.handleInferencePreferredModel)
		mux.HandleFunc("/waired/v1/inference/benchmark", s.handleInferenceBenchmark)
		mux.HandleFunc("/waired/v1/inference/benchmark/status", s.handleInferenceBenchmarkStatus)
		mux.HandleFunc("/waired/v1/inference/recommendation/dismiss", s.handleInferenceRecommendationDismiss)
	}
	if s.publicUse != nil && s.publicUse.Path != "" {
		mux.HandleFunc("/waired/v1/public/use", s.handlePublicUse)
		mux.HandleFunc("/waired/v1/public/consent", s.handlePublicConsent)
		mux.HandleFunc("/waired/v1/public/warning", s.handlePublicWarning)
	}
	if s.publicShare != nil {
		mux.HandleFunc("/waired/v1/public/share", s.handlePublicShareStatus)
		mux.HandleFunc("/waired/v1/public/share/enable", s.handlePublicShareEnable)
		mux.HandleFunc("/waired/v1/public/share/disable", s.handlePublicShareDisable)
	}
	if s.observability.Ring != nil {
		mux.HandleFunc("/waired/v1/observability/events", s.handleObservabilityEvents)
	}
	if s.observability.State != nil {
		mux.HandleFunc("/waired/v1/observability/state", s.handleObservabilityState)
	}
	if s.observability.MetricsHandler != nil {
		mux.Handle("/waired/v1/metrics", s.observability.MetricsHandler)
	}
	return mux
}

// Serve listens on addr (default 127.0.0.1:9476) until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	if addr == "" {
		addr = DefaultListen
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           writeGuard(s.Handler(), s.enforceSocketWrites, &s.socketUp),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body := s.status.Status()
	if s.pause != nil {
		cur, desired := s.pause.Phase()
		body.Phase = string(cur)
		body.DesiredPhase = string(desired)
	}
	writeJSON(w, http.StatusOK, body)
}

// PhaseResponse is the body returned by POST /waired/v1/pause and
// /waired/v1/resume. Mirrors the phase fields in Status so callers can
// share a parser.
type PhaseResponse struct {
	Phase        string `json:"phase"`
	DesiredPhase string `json:"desired_phase"`
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.handlePhaseTransition(w, r, true)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.handlePhaseTransition(w, r, false)
}

func (s *Server) handlePhaseTransition(w http.ResponseWriter, r *http.Request, pause bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.pause == nil {
		http.Error(w, "pause controller not configured", http.StatusNotFound)
		return
	}
	var err error
	if pause {
		err = s.pause.Pause(r.Context())
	} else {
		err = s.pause.Resume(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cur, desired := s.pause.Phase()
	writeJSON(w, http.StatusOK, PhaseResponse{
		Phase:        string(cur),
		DesiredPhase: string(desired),
	})
}

// InferenceStateResponse is the body returned by POST /waired/v1/inference/enable
// and /waired/v1/inference/disable. Mirrors the (current, desired) tuple in
// the same shape as PhaseResponse so callers can share the parser pattern.
type InferenceStateResponse struct {
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
}

func (s *Server) handleInferenceEnable(w http.ResponseWriter, r *http.Request) {
	s.handleInferenceTransition(w, r, true)
}

func (s *Server) handleInferenceDisable(w http.ResponseWriter, r *http.Request) {
	s.handleInferenceTransition(w, r, false)
}

func (s *Server) handleInferenceTransition(w http.ResponseWriter, r *http.Request, enable bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.infControl == nil {
		http.Error(w, "inference controller not configured", http.StatusNotFound)
		return
	}
	var err error
	if enable {
		err = s.infControl.Enable(r.Context())
	} else {
		err = s.infControl.Disable(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cur, desired := s.infControl.State()
	writeJSON(w, http.StatusOK, InferenceStateResponse{
		State:        string(cur),
		DesiredState: string(desired),
	})
}

// EngineStateResponse is the body returned by
// POST /waired/v1/inference/engine/{stop,start} — the live engine power
// state plus whether the engine is waired-managed (false in reuse mode).
type EngineStateResponse struct {
	Power   string `json:"power"`
	Managed bool   `json:"managed"`
}

func (s *Server) handleEngineStop(w http.ResponseWriter, r *http.Request) {
	s.handleEngineTransition(w, r, true)
}

func (s *Server) handleEngineStart(w http.ResponseWriter, r *http.Request) {
	s.handleEngineTransition(w, r, false)
}

func (s *Server) handleEngineTransition(w http.ResponseWriter, r *http.Request, stop bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engineControl == nil {
		http.Error(w, "engine controller not configured", http.StatusNotFound)
		return
	}
	// Reuse mode: there is no waired-owned process to stop/start, so the
	// power axis does not apply. 409 lets the CLI/tray render a clear
	// "engine reused — not managed" message instead of a generic error.
	if _, managed := s.engineControl.EngineState(); !managed {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "engine is reused (not managed by waired); power control unavailable",
		})
		return
	}
	var err error
	if stop {
		err = s.engineControl.StopEngine(r.Context())
	} else {
		err = s.engineControl.StartEngine(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	power, managed := s.engineControl.EngineState()
	writeJSON(w, http.StatusOK, EngineStateResponse{
		Power:   string(power),
		Managed: managed,
	})
}

// ShareStateResponse is the body returned by
// POST /waired/v1/inference/share/{enable,disable}. Mirrors
// InferenceStateResponse so the CLI and tray can share a parser
// pattern with the inference enable/disable endpoints.
type ShareStateResponse struct {
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
}

func (s *Server) handleShareEnable(w http.ResponseWriter, r *http.Request) {
	s.handleShareTransition(w, r, true)
}

func (s *Server) handleShareDisable(w http.ResponseWriter, r *http.Request) {
	s.handleShareTransition(w, r, false)
}

func (s *Server) handleShareTransition(w http.ResponseWriter, r *http.Request, enable bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.shareControl == nil {
		http.Error(w, "share controller not configured", http.StatusNotFound)
		return
	}
	var err error
	if enable {
		err = s.shareControl.Share(r.Context())
	} else {
		err = s.shareControl.Unshare(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cur, desired := s.shareControl.State()
	writeJSON(w, http.StatusOK, ShareStateResponse{
		State:        string(cur),
		DesiredState: string(desired),
	})
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.identity.Identity())
}

type pingRequest struct {
	Peer string `json:"peer"`
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Peer == "" {
		http.Error(w, "peer must not be empty", http.StatusBadRequest)
		return
	}
	res, err := s.pinger.PingPeer(r.Context(), req.Peer)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": err.Error(),
			"peer":  req.Peer,
		})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
