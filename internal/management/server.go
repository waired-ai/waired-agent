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
	"github.com/waired-ai/waired-agent/proto/hostfit"
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

	// NodeKeyAgreement says whether the Node Key the control plane
	// publishes for this device is the one the device actually holds.
	// One of the NodeKeyAgreement* values below; empty before the first
	// network map arrives. NodePublicKey / PublishedNodePublicKey are
	// the two sides of that comparison, carried so an operator can line
	// them up against a peer's view. Both are public keys already
	// distributed to every peer in the network.
	NodeKeyAgreement       string `json:"node_key_agreement,omitempty"`
	NodePublicKey          string `json:"node_public_key,omitempty"`
	PublishedNodePublicKey string `json:"published_node_public_key,omitempty"`

	// Peers carries one entry per peer in the current Network Map with
	// the reconciler's per-path-quality state. testnet-fallback-* scripts
	// poll this field to decide whether a downgrade/upgrade has fired.
	Peers []PeerStatus `json:"peers,omitempty"`
}

// Status.NodeKeyAgreement values.
//
// A device's Node Key is its WireGuard static key: peers authenticate
// to it using the public half the control plane publishes for it. When
// the published half is not the one the device holds, every peer's
// handshake fails at that device and its own handshakes name a static
// key no peer can match — the overlay dies in both directions while
// every other surface still reports the device online.
//
// NodeKeyRotating is not a fault. A rotation tells the control plane
// first and promotes the local file second (see the agent's node key
// rotator), so the published key legitimately runs ahead of the local
// one for the length of that window; the map carries the outgoing key
// as prev_node_public_key for exactly this reason.
const (
	NodeKeyAgreementOK       = "ok"
	NodeKeyAgreementRotating = "rotating"
	NodeKeyAgreementDiverged = "diverged"
)

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
	DeviceName string `json:"device_name,omitempty"`
	// DisplayID is the only identifier for this peer that may appear on a
	// surface a person reads. For your own machines it is the DeviceID; for
	// a Public Share peer — a stranger's machine injected under a grant —
	// it is the grant pseudonym, and when the grant carries none it is
	// inferencemesh.PublicPeerLabelFor, which says what the machine is and
	// names the grant it came in under (public share spec §8.5, the rule
	// #739 closed on every other pinned-peer surface).
	//
	// That last case used to be empty, which left every consumer to invent
	// its own substitute: the tray row read "unknown" and `waired status`
	// had none at all. The daemon is the only layer holding the grant, so
	// it is the only one that can answer — the same argument #768 makes for
	// resolving DisplayID here in the first place. Read Public, not
	// emptiness, to ask whether this is a real identifier.
	//
	// Resolved once here on the daemon side rather than re-derived by each
	// client: DeviceID above stays the real identifier, because the pin the
	// router matches on and the testnet-fallback scripts that poll Peers
	// both need it, and a client holding only PeerStatus could not tell a
	// public machine from one of your own (#768).
	DisplayID string `json:"display_id,omitempty"`
	// Public marks a row as a Public Share peer — a stranger's machine
	// injected under a grant — so a client can apply §8.5 to it without
	// having to work out which rows it applies to. That question is not
	// answerable from this struct otherwise: DisplayID equals DeviceID for
	// your own machines and can be EMPTY for a public peer whose grant
	// carries no pseudonym, so neither a comparison nor an emptiness test
	// separates the two.
	//
	// Added because `waired status` prints this document verbatim and had
	// no material to key a substitution on, so a public peer's real device
	// id reached the terminal (waired-agent#809). DeviceID itself stays
	// unchanged on the wire for the reason #803 gives — the router pin and
	// the testnet-fallback scripts read it.
	Public                bool    `json:"public,omitempty"`
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

	// UnifiedMemory / UsableVRAMMB mirror the fields of the same name on
	// signer.HardwareSummary, and exist because VRAMTotalMB alone cannot
	// describe a host where the GPU and CPU share physical RAM
	// (waired-ai/waired-agent#662). Apple Silicon reports no per-GPU total
	// at all — its detector leaves that field 0 deliberately, because the
	// figure that means anything is the OS-reserved usable bound — so a
	// consumer reading only VRAMTotalMB rendered an M-series Mac as a GPU
	// with unknown memory while an AMD Strix Halo beside it showed 96 GB.
	//
	// Carried as the two raw facts rather than one pre-resolved number so
	// this projection keeps saying what the host reported; callers that
	// want the display figure ask EffectiveVRAMMB below.
	UnifiedMemory bool `json:"unified_memory,omitempty"`
	UsableVRAMMB  int  `json:"usable_vram_mb,omitempty"`
}

// EffectiveVRAMMB is the GPU memory figure to show for this peer: the
// usable unified-memory bound on a host that shares RAM with its GPU, the
// first GPU's raw total everywhere else. 0 means "nothing to show".
//
// The rule is proto/signer.HardwareSummary's own — "UsableVRAMMB is the
// GPU-addressable upper bound after the OS reserve; 0 means unknown, and a
// consumer must then fall back to GPUs[0].VRAMTotalMB" — and it is
// delegated to hostfit.Host.EffectiveVRAMMB rather than restated so the
// display and the fit rules cannot drift apart.
func (h *PeerHardware) EffectiveVRAMMB() int {
	if h == nil {
		return 0
	}
	return hostfit.Host{
		UnifiedMemory: h.UnifiedMemory,
		UsableVRAMMB:  h.UsableVRAMMB,
		VRAM0MB:       h.VRAMTotalMB,
	}.EffectiveVRAMMB()
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

// Auth states reported by IdentityView.AuthState. Empty means the same
// as AuthStateOK — a consumer that predates the field should read no
// news as good news.
const (
	// AuthStateOK: the device can still renew its own credentials.
	AuthStateOK = "ok"
	// AuthStateReauthRequired: auto-refresh has given up for good and
	// only `waired init` can restore the device's Control-Plane
	// credentials. The daemon stops its CP push loops in this state, so
	// consumers must not read the resulting silence as "offline".
	AuthStateReauthRequired = "reauth_required"
)

// IdentityView is the tray-facing projection of the agent's enrollment
// state. Mirrors the user-relevant fields from internal/identity.Identity
// plus an Enrolled flag so a future not-yet-enrolled daemon mode can
// surface "no signed-in account" without 404-ing the route.
//
// Enrolled and Active are deliberately separate (waired-agent#318).
// Enrolled answers "is there a signed-in account on this device"; Active
// answers "did the identity-dependent runtime actually come up". They
// disagree whenever activation fails — a boot where the WireGuard socket
// could not bind, say — and collapsing them is what made a signed-in
// device read as logged out in the tray.
type IdentityView struct {
	Enrolled     bool   `json:"enrolled"`
	AccountEmail string `json:"account_email,omitempty"`
	NetworkName  string `json:"network_name,omitempty"`
	NetworkID    string `json:"network_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	DeviceName   string `json:"device_name,omitempty"`
	OverlayIP    string `json:"overlay_ip,omitempty"`
	ControlURL   string `json:"control_url,omitempty"`

	// Active is whether a live session is published — the engine is up
	// and the agent is talking to its network. False with Enrolled true
	// means "signed in, but not connected".
	Active bool `json:"active"`
	// ActivationError is why Active is false, when the daemon knows.
	// Empty while activation is still in flight or has never failed.
	ActivationError string `json:"activation_error,omitempty"`

	// AuthState is one of the AuthState* constants; empty reads as OK.
	AuthState string `json:"auth_state,omitempty"`
	// AuthDetail is the classified cause behind a non-OK AuthState.
	AuthDetail string `json:"auth_detail,omitempty"`
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

// SharingController is implemented by the agent. It owns the one
// sharing answer that lives on the machine (waired#1297, owner ruling
// 2026-08-30): whether this computer lends itself out at all. Off stops
// every kind of serving at once — the account's own mesh, public
// guests, and anything added later.
//
// Who the computer is offered to is not here. That is the control
// plane's to decide and arrives on the signed map; MeshShare and
// PublicShare below are read-only reports of what it decided, so one
// status call can answer "who does this computer serve".
//
// Share/Unshare mutate both the in-memory live flag (so the probe loop
// and the peer-overlay middleware reflect the change immediately) and
// the persisted desired-sharing file. Unshare waits for nothing before
// it closes the gates and cuts what is running (public share spec §8.3).
//
// Suspend/Unsuspend are the live-only session override (#316): the app
// suspends sharing when the operator quits it and lifts the suspension
// on its next start. Nothing is persisted, so a daemon restart — or an
// explicit Share/Unshare — returns to the operator's actual choice.
type SharingController interface {
	Share(ctx context.Context) error
	Unshare(ctx context.Context) error
	Suspend(ctx context.Context) error
	Unsuspend(ctx context.Context) error
	IsSuspended() bool
	State() (current, desired state.SharingState)
	MeshShare() state.MeshShareState
	PublicShare() state.SharingState
	PublicMaxClients() int
}

// WorkerController is implemented by the agent for the Tailscale-
// exit-node-style manual inference routing toggle. The three setters
// (SetMode / SetPin / Clear) mutate both the in-memory atomic
// preference (so the Selector hot path reflects the change on the
// next request) and the persisted desired-worker file (so a
// subsequent restart honours the operator's last choice). State has
// the same shape as InferenceController.State, returning the current
// live RoutingPreference plus the persisted operator intent.
// SetPin takes the display identifier alongside the device id because
// this is the last moment it is knowable: it comes from the pinned
// peer's grant in the mesh snapshot, and the surface that most needs it
// is the one shown after that peer has dropped out (#739).
type WorkerController interface {
	SetMode(ctx context.Context, mode state.RoutingMode) error
	// SetRouting applies a partial update to the ordering preferences,
	// leaving the mode and any pin alone (waired-agent#1128). Pointers so
	// "not supplied" and "set to empty" stay different — clearing the
	// model floor is an empty string, not an absence.
	SetRouting(ctx context.Context, prefer *state.RoutingPrefer, minModelSize *string) error
	SetPin(ctx context.Context, peerDeviceID, peerDisplayID string) error
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
// intercept has served a Claude request on Waired (#602). Both are in-memory
// only and reset on agent restart.
type ClaudeRoutingState struct {
	Policy         state.ClaudeRoutingPolicy   `json:"policy"`
	LastFallback   *ClaudeRoutingFallbackEvent `json:"last_fallback,omitempty"`
	LastLocalModel string                      `json:"last_local_model,omitempty"`
	// LastServedBy is the mesh peer DeviceID that served the last
	// waired-served Claude request; empty when this device served it.
	LastServedBy string `json:"last_served_by,omitempty"`
	// LastServedAt is when that request was served. The served record is
	// never cleared, so without the time a record left over from before a
	// fallback or a route change reads as if Waired were still serving
	// (#755). Zero when nothing has been served yet — and when an agent
	// predating the field answered.
	LastServedAt time.Time `json:"last_served_at,omitempty"`
	// LastRequestModel is the model id the last main-conversation turn
	// carried, LastRequestRoute where that id sent it (auto / waired /
	// anthropic), and LastRequestAt when.
	//
	// Separate from the served record because they answer different
	// questions. A turn the user sent to the real Anthropic API by naming a
	// model in /model is served by nothing here, so the served fields stay on
	// whatever came before it — and read as current when they are not. Asking
	// the host what the last turn asked for is what makes a session routed
	// somewhere unexpected visible at all (waired-agent#1036).
	LastRequestModel string    `json:"last_request_model,omitempty"`
	LastRequestRoute string    `json:"last_request_route,omitempty"`
	LastRequestAt    time.Time `json:"last_request_at,omitempty"`
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
// the soft enable/disable (InferenceController) and sharing (SharingController)
// axes. "running" = engine process up; "stopped" = not up, because it was
// hard-stopped (parked) or has not been started; "starting" = a start is in
// flight; "failed" = not up, and nobody asked for that (waired-agent#964).
type EnginePowerState string

const (
	EnginePowerRunning  EnginePowerState = "running"
	EnginePowerStopped  EnginePowerState = "stopped"
	EnginePowerStarting EnginePowerState = "starting"
	// EnginePowerFailed is an engine waired owns that is not running and
	// did not stop because anyone asked (waired-agent#964).
	//
	// It is not a fourth flavour of "stopped". The distinction is what the
	// surfaces need: a stopped engine is waiting for a start, and a failed
	// one is waiting for someone to deal with a cause first — the tray
	// offers the same Start row for both, but the label beside it and the
	// warning line are the difference between "press this" and "read this".
	//
	// Before it existed the two engines answered differently and both
	// answers were wrong somewhere. The ollama arm let StateFailed fall
	// through to running, so a host whose engine had crashed and whose
	// recovery budget was spent reported "Engine power: running" and the
	// tray offered to Stop a process that was not there; the vLLM arm
	// answered stopped, which is true about the memory and silent about
	// the cause.
	//
	// A reader that predates it treats an unknown value the way it treats
	// any other non-"stopped" state, so nothing regresses to a worse
	// answer than the "running" it used to get.
	EnginePowerFailed EnginePowerState = "failed"
)

// EngineStopBudgetFor bounds a hard engine stop end to end: the adapter's
// graceful grace period plus its post-kill reap — both that engine's
// StopTimeout — plus headroom. The kill runs to completion regardless of who
// is still listening (#316), so this is deliberately larger than any client
// budget.
//
// Per engine because the two StopTimeouts differ. A single 15 s constant was
// sized for ollama's 5 s; vLLM's default is 10 s, so its worst case is 20 s
// and the daemon abandoned the wait mid-kill (waired-ai/waired-agent#945).
//
// It lives here, beside EnginePowerState, rather than in the daemon, because
// the CLI and the tray have to size their own budgets ABOVE it: a client that
// gives up first reports a timeout it caused itself while the stop is in fact
// succeeding, which is waired#316's defect in reverse. They used to do that
// against a number transcribed by hand.
func EngineStopBudgetFor(engine string) time.Duration {
	if engine == engineKindVLLM {
		return 30 * time.Second
	}
	return 15 * time.Second
}

// engineKindVLLM is catalog.RuntimeVLLM. Spelt out rather than imported:
// internal/management is below the catalog in the dependency order, and one
// string constant is a smaller price than inverting that.
const engineKindVLLM = "vllm"

// EngineStopClientBudget is what a CLI or tray must allow for a stop,
// whatever engine the host turns out to serve with — the largest daemon-side
// budget plus room for the round trip. A client cannot know the engine before
// it asks, so it sizes for the worst case.
func EngineStopClientBudget() time.Duration {
	longest := EngineStopBudgetFor(engineKindVLLM)
	if o := EngineStopBudgetFor("ollama"); o > longest {
		longest = o
	}
	return longest + 10*time.Second
}

// ErrEngineStartRefused marks a start the daemon declined on policy rather
// than failed to perform: today, a device whose persisted local-inference
// toggle is off (waired-agent#964). Wrapped by the agent's controller and
// unwrapped by the transition handler, which answers 409 instead of 500 so
// a client can tell a refusal from a fault.
var ErrEngineStartRefused = errors.New("engine start refused")

// EngineController is implemented by the agent for the hard engine power
// axis (#186): StopEngine kills the local `ollama serve` to free VRAM/RAM
// and latches it stopped (so request traffic doesn't revive it);
// StartEngine clears the latch and restarts asynchronously. EngineState
// reports the live power state plus whether the engine is waired-managed
// (false for an adopted orphan, where power control does not apply). Unlike the
// soft toggle this state is NOT persisted — a daemon restart returns to
// config-driven startup.
type EngineController interface {
	StopEngine(ctx context.Context) error
	StartEngine(ctx context.Context) error
	EngineState() (power EnginePowerState, managed bool)
}

// HostSpeedController re-takes the install-time host-speed measurement for a
// re-run of the install flow.
//
// Owner ruling 2026-08-09 (waired-agent#599): re-running `waired init` on a
// configured host replays the whole install conversation, and "各種の
// ベンチマークやゲートも新規インストールと同じように設定する" — the
// benchmarks and gates are re-taken, not inherited. The stored figure is
// keyed by install and agent build (docs/decisions/20260807/1700-host-speed-
// is-an-install-time-step.md), so without this a re-run reads whatever the
// last boot left behind. waired#1140 is what that costs: three machines
// carrying a figure that describes a residency rather than a host, with no
// way to retake it short of an upgrade.
//
// Remeasure reports whether a fresh measurement was started. False means the
// daemon already took one in this process — a fresh install, where the engine
// bootstrap measured seconds earlier — and re-taking it would measure the
// same host twice in one install, which the decision above rules out.
type HostSpeedController interface {
	Remeasure(ctx context.Context) (started bool)
}

// HostMemoryController re-takes the install-time available-memory
// measurement (waired-agent#568), the figure hostfit turns into the OS
// deduction.
//
// It is taken once per install/upgrade, at daemon start and before the
// engine bootstrap, so that a resident model is never charged against
// the host that serves it. That is the whole design — but it also means
// a host measured during a busy moment keeps that snapshot until the
// next install, with deleting runtime/host-memory.json as the only way
// out. Folklore is not a supported path, which is what #589 asks for.
//
// Remeasure reports what happened rather than just succeeding, because
// the interesting answers are the refusals: an engine holding memory
// right now would be measured INTO the figure, which is the exact
// contamination the install-time rule exists to avoid.
type HostMemoryController interface {
	RemeasureHostMemory(ctx context.Context) HostMemoryRemeasure
}

// HostMemoryRemeasure is what a re-measure attempt did.
type HostMemoryRemeasure struct {
	// Measured is true when a fresh figure was taken and persisted.
	Measured bool `json:"measured"`
	// AvailableGB is the figure now in force — the fresh one when
	// Measured, otherwise the record that was kept.
	AvailableGB int `json:"available_gb,omitempty"`
	// MeasuredAt dates AvailableGB, RFC3339. Empty when nothing has ever
	// been measured on this host.
	MeasuredAt string `json:"measured_at,omitempty"`
	// Reason names why a measurement did NOT happen, in the operator's
	// words. Empty when Measured is true.
	Reason string `json:"reason,omitempty"`
}

type Server struct {
	status              StatusProvider
	pinger              Pinger
	pause               PauseController            // optional; nil disables /waired/v1/pause and /waired/v1/resume
	inference           InferenceProvider          // optional; nil disables /waired/v1/inference/status etc. and /waired/v1/models*
	infControl          InferenceController        // optional; nil disables /waired/v1/inference/{enable,disable}
	engineControl       EngineController           // optional; nil disables /waired/v1/inference/engine/{stop,start}
	hostSpeedControl    HostSpeedController        // optional; nil disables /waired/v1/inference/host-speed/remeasure
	hostMemoryControl   HostMemoryController       // optional; nil disables /waired/v1/inference/memory/remeasure
	modelUnload         ModelUnloader              // optional; nil disables /waired/v1/inference/model/unload
	residencyControl    ResidencyController        // optional; nil disables /waired/v1/inference/residency
	shareControl        SharingController          // optional; nil disables /waired/v1/sharing{,/enable,/disable,/suspend,/unsuspend}
	workerControl       WorkerController           // optional; nil disables /waired/v1/worker and worker_routing in /v1/inference/status
	infMesh             InferenceMeshProvider      // optional; nil disables /waired/v1/inference/mesh
	identity            IdentityProvider           // optional; nil disables /waired/v1/identity (tray-facing)
	claudeIntegration   *ClaudeIntegrationConfig   // optional; nil disables /waired/v1/integration/claude
	claudeRouting       ClaudeRoutingControl       // optional; nil disables /waired/v1/integration/claude/route
	openCodeIntegration *OpenCodeIntegrationConfig // optional; nil disables /waired/v1/integration/opencode
	openClawIntegration *OpenClawIntegrationConfig // optional; nil disables /waired/v1/integration/openclaw
	catalog             *CatalogConfig             // optional; nil disables /waired/v1/inference/catalog and /preferred-model
	publicUse           *PublicUseConfig           // optional; nil disables /waired/v1/public/* (consumer Public Share settings + consent)
	observability       ObservabilityConfig        // optional; zero value disables all Phase 9 endpoints
	login               LoginController            // optional; nil disables /waired/v1/login/{start,status}
	update              UpdateController           // optional; nil disables /waired/v1/update/{check,status,settings}
	logControl          LogController              // optional; nil disables /waired/v1/log/{level,settings}
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
	// enforceSocketReads, when true, makes the loopback-TCP Serve path
	// serve only the tcpReadRoutes allow-list while the local IPC socket
	// is up, so every other read must come over the socket (waired#836).
	// Off by default for the same reason as enforceSocketWrites. Set via
	// WithSocketReadsOnly. See socket.go.
	enforceSocketReads bool
	// socketUp reflects whether ServeLocal currently has the local IPC
	// socket bound. writeGuard and readGuard read it so a socket bind
	// failure fails OPEN (TCP keeps serving, behind the #836 browserGuard)
	// instead of bricking control of the agent.
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

// WithHostSpeedControl attaches a HostSpeedController so the server exposes
// POST /waired/v1/inference/host-speed/remeasure, which an install-flow
// re-run calls before it waits for a figure (waired-agent#599). Pass nil to
// disable.
func (s *Server) WithHostSpeedControl(c HostSpeedController) *Server {
	s.hostSpeedControl = c
	return s
}

// ModelUnloader releases the serving model's memory while leaving the
// engine running (waired-agent#861).
//
// Separate from EngineController on purpose: stopping the engine also
// ends the ability to serve, whereas this returns the memory and keeps
// the host answering. Model residency is held indefinitely by default,
// so this is the operator's way to get it back — the same affordance
// LM Studio spells Eject and Ollama spells `ollama stop <model>`.
type ModelUnloader interface {
	// UnloadServingModel returns the tag it unloaded, empty when nothing
	// was resident (a success: the caller wanted the memory back).
	UnloadServingModel(ctx context.Context) (string, error)
}

// ErrUnloadNotSupported is returned by a ModelUnloader when the engine this
// host serves with has no unload axis at all — vLLM reserves its GPU pool at
// start-up and holds it to process exit, so the model cannot be released
// while the engine runs. handleModelUnload maps it to 409.
//
// A 409 rather than a 200 with a new field, deliberately. The shipped CLI
// reads any 200 without Unloaded as "No model was loaded." — which on such a
// host is simply false, and is the defect being fixed
// (waired-ai/waired-agent#943). A 409 carries the daemon's own sentence
// through readMgmtResponse, so even a CLI that predates this prints the
// truth instead of the falsehood.
var ErrUnloadNotSupported = errors.New("this engine has no unload axis")

// ModelUnloadResponse is the answer to POST
// /waired/v1/inference/model/unload.
type ModelUnloadResponse struct {
	// Unloaded is false when nothing was resident to unload.
	Unloaded bool `json:"unloaded"`
	// Model is the engine tag that was unloaded, empty when Unloaded is
	// false.
	Model string `json:"model,omitempty"`
}

// WithModelUnloader attaches a ModelUnloader so the server serves
// POST /waired/v1/inference/model/unload. Nil leaves the route 404, the
// same shape as the other optional controllers here.
func (s *Server) WithModelUnloader(u ModelUnloader) *Server {
	s.modelUnload = u
	return s
}

func (s *Server) handleModelUnload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.modelUnload == nil {
		http.Error(w, "model unloader not configured", http.StatusNotFound)
		return
	}
	tag, err := s.modelUnload.UnloadServingModel(r.Context())
	if errors.Is(err, ErrUnloadNotSupported) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ModelUnloadResponse{Unloaded: tag != "", Model: tag})
}

// WithHostMemoryControl attaches a HostMemoryController so the server
// exposes POST /waired/v1/inference/memory/remeasure — the supported way
// to retake the install-time available-memory figure (waired-agent#589).
// Pass nil to disable.
func (s *Server) WithHostMemoryControl(c HostMemoryController) *Server {
	s.hostMemoryControl = c
	return s
}

// WithShareControl attaches a SharingController so the server exposes
// GET /waired/v1/sharing and POST /waired/v1/sharing/{enable,disable,
// suspend,unsuspend}. Pass nil to disable. Independent of
// WithInferenceControl so an operator can keep the engine running
// (inference enabled) while the computer lends itself out to nobody.
func (s *Server) WithShareControl(c SharingController) *Server {
	s.shareControl = c
	return s
}

// WithWorkerControl attaches a WorkerController so the server exposes
// GET/POST /waired/v1/worker (the Tailscale-exit-node-style manual
// routing toggle) and embeds the resolved worker state in
// /waired/v1/inference/status responses. Pass nil to disable.
// Independent of InferenceController and SharingController — the
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
	if s.hostSpeedControl != nil {
		mux.HandleFunc("/waired/v1/inference/host-speed/remeasure", s.handleHostSpeedRemeasure)
	}
	if s.hostMemoryControl != nil {
		mux.HandleFunc("/waired/v1/inference/memory/remeasure", s.handleHostMemoryRemeasure)
	}
	if s.modelUnload != nil {
		mux.HandleFunc("/waired/v1/inference/model/unload", s.handleModelUnload)
	}
	if s.residencyControl != nil {
		mux.HandleFunc("/waired/v1/inference/residency", s.handleInferenceResidency)
	}
	// Sharing is its own noun rather than a corner of /inference: what it
	// answers is whether this computer lends itself out, which is not a
	// property of the engine (waired#1297).
	mux.HandleFunc("/waired/v1/sharing", s.handleSharingStatus)
	mux.HandleFunc("/waired/v1/sharing/enable", s.handleShareEnable)
	mux.HandleFunc("/waired/v1/sharing/disable", s.handleShareDisable)
	mux.HandleFunc("/waired/v1/sharing/suspend", s.handleShareSuspend)
	mux.HandleFunc("/waired/v1/sharing/unsuspend", s.handleShareUnsuspend)
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
	if s.logControl != nil {
		mux.HandleFunc("/waired/v1/log/level", s.handleLogLevel)
		mux.HandleFunc("/waired/v1/log/settings", s.handleLogSettings)
	}
	if s.claudeIntegration != nil {
		mux.HandleFunc("/waired/v1/integration/claude", s.handleClaudeIntegration)
	}
	if s.claudeRouting != nil {
		mux.HandleFunc("/waired/v1/integration/claude/route", s.handleClaudeRouting)
	}
	if s.openCodeIntegration != nil {
		mux.HandleFunc("/waired/v1/integration/opencode", s.handleOpenCodeIntegration)
	}
	if s.openClawIntegration != nil {
		mux.HandleFunc("/waired/v1/integration/openclaw", s.handleOpenClawIntegration)
	}
	if s.catalog != nil && s.catalog.PreferencePath != "" {
		mux.HandleFunc("/waired/v1/inference/catalog", s.handleInferenceCatalog)
		mux.HandleFunc("/waired/v1/inference/preferred-model", s.handleInferencePreferredModel)
		mux.HandleFunc("/waired/v1/inference/model-choice-pending", s.handleModelChoicePending)
		mux.HandleFunc("/waired/v1/inference/benchmark", s.handleInferenceBenchmark)
		mux.HandleFunc("/waired/v1/inference/benchmark/status", s.handleInferenceBenchmarkStatus)
		mux.HandleFunc("/waired/v1/inference/recommendation/dismiss", s.handleInferenceRecommendationDismiss)
	}
	if s.publicUse != nil && s.publicUse.Path != "" {
		mux.HandleFunc("/waired/v1/public/use", s.handlePublicUse)
		mux.HandleFunc("/waired/v1/public/consent", s.handlePublicConsent)
		mux.HandleFunc("/waired/v1/public/warning", s.handlePublicWarning)
	}
	if s.observability.Ring != nil {
		mux.HandleFunc("/waired/v1/observability/events", s.handleObservabilityEvents)
	}
	if s.observability.State != nil {
		mux.HandleFunc("/waired/v1/observability/state", s.handleObservabilityState)
	}
	if s.observability.MetricsHandler != nil {
		// Every other route enforces its own method; this one delegates to
		// promhttp, which answers any verb. Pin it to reads here so the
		// route's method policy does not depend on a third-party handler.
		mux.Handle("/waired/v1/metrics", getOrHeadOnly(s.observability.MetricsHandler))
	}
	return mux
}

// Serve listens on addr (default 127.0.0.1:9476) until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	if addr == "" {
		addr = DefaultListen
	}
	srv := &http.Server{
		Addr: addr,
		// The transport guards wrap Handler() (which already carries
		// loopbackOnly + browserGuard) rather than living inside it: the
		// IPC socket serves the same mux and must not be subject to
		// either. See socket.go.
		Handler:           writeGuard(readGuard(s.Handler(), s.enforceSocketReads, &s.socketUp), s.enforceSocketWrites, &s.socketUp),
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
// state plus whether the engine is waired-managed (false when adopted).
type EngineStateResponse struct {
	Power   string `json:"power"`
	Managed bool   `json:"managed"`
}

// HostSpeedRemeasureResponse says whether this call started a fresh
// measurement or the daemon's own is being reused.
type HostSpeedRemeasureResponse struct {
	Started bool `json:"started"`
}

// handleHostSpeedRemeasure asks for a fresh install-time measurement. It
// returns as soon as the request is admitted: the measurement is minutes of
// engine time and the caller polls /waired/v1/inference/status for the
// figure, exactly as it already does on a fresh install.
func (s *Server) handleHostSpeedRemeasure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.hostSpeedControl == nil {
		http.Error(w, "host-speed controller not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, HostSpeedRemeasureResponse{
		Started: s.hostSpeedControl.Remeasure(r.Context()),
	})
}

func (s *Server) handleHostMemoryRemeasure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.hostMemoryControl == nil {
		http.Error(w, "host-memory controller not configured", http.StatusNotFound)
		return
	}
	// 200 even when nothing was measured: a refusal is a real answer
	// about the host ("the engine is holding memory right now"), not a
	// failure of the request, and the caller renders Reason either way.
	writeJSON(w, http.StatusOK, s.hostMemoryControl.RemeasureHostMemory(r.Context()))
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
	// No waired-owned process to stop/start (an adopted orphan, #489), so
	// the power axis does not apply. 409 lets the CLI/tray render a clear
	// "engine not managed" message instead of a generic error.
	if _, managed := s.engineControl.EngineState(); !managed {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "engine is not managed by waired; power control unavailable",
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
		// A refusal, not a fault: the device is configured not to run
		// models, so there was nothing to start and nothing went wrong.
		// 409 rather than 500 so a client can say which it was — the same
		// code the not-managed refusal above uses (waired-agent#964).
		if errors.Is(err, ErrEngineStartRefused) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	power, managed := s.engineControl.EngineState()
	writeJSON(w, http.StatusOK, EngineStateResponse{
		Power:   string(power),
		Managed: managed,
	})
}

// ShareStateResponse is the body returned by GET /waired/v1/sharing and
// by POST /waired/v1/sharing/{enable,disable,suspend,unsuspend}.
// Mirrors InferenceStateResponse so the CLI and app can share a parser
// pattern with the inference enable/disable endpoints. Suspended reports
// the live-only session override, which State cannot express on its own:
// while suspended, State is ("off", "on").
//
// MeshShare and PublicShare are the control plane's settings, reported
// here so one call answers who this computer serves. They are read-only
// on this surface — nothing on the machine writes them (waired#1297) —
// and empty until the first signed map of this run has been applied.
type ShareStateResponse struct {
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
	Suspended    bool   `json:"suspended,omitempty"`
	MeshShare    string `json:"mesh_share,omitempty"`
	PublicShare  string `json:"public_share,omitempty"`
	// PublicMaxClients is the guest ceiling the control plane last sent.
	// 0 means it has not sent one, and its own default applies.
	PublicMaxClients int `json:"public_max_clients,omitempty"`
}

func (s *Server) handleShareEnable(w http.ResponseWriter, r *http.Request) {
	s.handleShareTransition(w, r, SharingController.Share)
}

func (s *Server) handleShareDisable(w http.ResponseWriter, r *http.Request) {
	s.handleShareTransition(w, r, SharingController.Unshare)
}

// handleShareSuspend / handleShareUnsuspend drive the session override.
// They are their own verbs rather than a reuse of disable/enable because
// disable persists the operator's choice and this must not: the tray
// suspends on Quit, and a persisted "not_shared" would silently outlive
// the reason for it (#316).
func (s *Server) handleShareSuspend(w http.ResponseWriter, r *http.Request) {
	s.handleShareTransition(w, r, SharingController.Suspend)
}

func (s *Server) handleShareUnsuspend(w http.ResponseWriter, r *http.Request) {
	s.handleShareTransition(w, r, SharingController.Unsuspend)
}

// handleSharingStatus is the read side, and the one route here a
// read-only caller may reach: `waired share status` and the app's
// status rows both want the whole picture in one call.
func (s *Server) handleSharingStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.shareControl == nil {
		http.Error(w, "sharing controller not configured", http.StatusNotFound)
		return
	}
	s.writeSharingState(w)
}

func (s *Server) handleShareTransition(w http.ResponseWriter, r *http.Request, apply func(SharingController, context.Context) error) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.shareControl == nil {
		http.Error(w, "share controller not configured", http.StatusNotFound)
		return
	}
	if err := apply(s.shareControl, r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeSharingState(w)
}

func (s *Server) writeSharingState(w http.ResponseWriter) {
	cur, desired := s.shareControl.State()
	writeJSON(w, http.StatusOK, ShareStateResponse{
		State:        string(cur),
		DesiredState: string(desired),
		Suspended:    s.shareControl.IsSuspended(),
		MeshShare:    string(s.shareControl.MeshShare()),
		PublicShare:  string(s.shareControl.PublicShare()),

		PublicMaxClients: s.shareControl.PublicMaxClients(),
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

// getOrHeadOnly answers anything but GET/HEAD with 405. Used for routes
// whose handler comes from outside this package.
func getOrHeadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
			return
		}
		next.ServeHTTP(w, r)
	})
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
