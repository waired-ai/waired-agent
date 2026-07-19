// Package state owns the runtime-state files the agent writes and the
// CLI / shell-rc reads.
//
// Two files live under <state-dir>/runtime/:
//
//	state          JSON snapshot updated every few seconds by the agent.
//	               Read by the shell rc precmd hook to decide whether
//	               ANTHROPIC_BASE_URL etc. should be exported.
//	desired-phase  Plain text "active" or "paused", written by
//	               `waired pause` / `waired resume`. Survives daemon
//	               restarts so an explicit pause is not forgotten.
//
// Shell-side "active" judgement is encoded in State.Effective: phase
// must be active, the heartbeat must be fresh, and the recorded PID
// must still be alive on the host. That last check rescues the case
// where the agent was SIGKILLed and never got to remove its state file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Phase is the agent's externally-observable on/off state.
type Phase string

const (
	PhaseActive Phase = "active"
	PhasePaused Phase = "paused"
)

// InferenceState is the operator's desired ON/OFF intent for the local
// inference subsystem. Persisted independently of Phase so the WG/relay
// data plane and the LLM gateway can be toggled separately.
type InferenceState string

const (
	InferenceEnabled  InferenceState = "enabled"
	InferenceDisabled InferenceState = "disabled"
)

// ShareMeshState captures whether the operator has agreed to expose
// the local inference engine to mesh peers. Persisted to
// <state-dir>/runtime/desired-share, mutable at runtime via the
// management API (`/waired/v1/inference/share/{enable,disable}`) and
// the CLI/tray that wrap it.
type ShareMeshState string

const (
	ShareMeshShared    ShareMeshState = "shared"
	ShareMeshNotShared ShareMeshState = "not_shared"
)

// PublicShareState captures whether the operator has agreed to serve
// inference to foreign devices holding a Public Share grant (public
// share spec §4.1/§8). Persisted to <state-dir>/runtime/desired-public-share.
// Distinct from ShareMeshState (intra-account mesh share): the public
// toggle defaults to OFF and only an explicit "public" choice enables
// serving strangers.
type PublicShareState string

const (
	PublicShareOn  PublicShareState = "public"
	PublicShareOff PublicShareState = "not_public"
)

// RoutingMode is the operator's chosen inference routing policy —
// Tailscale-exit-node-style manual selection of where this agent's
// outbound inference requests go. Persisted to
// <state-dir>/runtime/desired-worker (JSON), mutable at runtime via
// the management API (`/waired/v1/worker`) and the CLI/tray that wrap
// it. Empty value == RoutingModeAuto (= current default behaviour).
type RoutingMode string

const (
	RoutingModeAuto          RoutingMode = "auto"
	RoutingModeLocalOnly     RoutingMode = "local-only"
	RoutingModePeerPreferred RoutingMode = "peer-preferred"
	RoutingModePinned        RoutingMode = "pinned"
)

// RoutingPreference is the on-disk form of the operator's routing
// choice. PinnedPeerDeviceID is meaningful only when Mode ==
// RoutingModePinned and is cleared on transitions to other modes.
type RoutingPreference struct {
	Mode               RoutingMode `json:"mode"`
	PinnedPeerDeviceID string      `json:"pinned_peer_device_id,omitempty"`
}

// IsZero reports whether the preference is the all-defaults form a
// caller should treat as "operator has not touched this". Used at boot
// to decide whether the persisted file overrides the agentconfig
// default.
func (p RoutingPreference) IsZero() bool {
	return p.Mode == "" && p.PinnedPeerDeviceID == ""
}

// UpdateNotifyState captures whether the operator wants the tray to
// proactively prompt them when a newer waired release is available
// (#294). Persisted to <state-dir>/runtime/desired-update-notify,
// mutable at runtime via the management API (`/waired/v1/update/settings`)
// and the CLI (`waired update --notify=on|off`) / tray that wrap it.
//
// Unlike the share toggle, there is no agentconfig default to fall back
// to, so a missing/empty file reads as UpdateNotifyOn (default ON): the
// prompt is the whole feature, and a user who finds it noisy opts out.
type UpdateNotifyState string

const (
	UpdateNotifyOn  UpdateNotifyState = "on"
	UpdateNotifyOff UpdateNotifyState = "off"
)

// Enabled reports whether update prompts should fire. Off disables them;
// any other value (including the empty/default state) is treated as on.
func (s UpdateNotifyState) Enabled() bool {
	return s != UpdateNotifyOff
}

// ClaudeRouteClass is where one Claude Code traffic class runs. It folds
// the former global route mode (#580) and the per-class node policy
// (#645/#665) into a single per-class vocabulary — one choice, made
// independently for the main conversation and for subagents:
//
//	auto       Waired first; on a pre-first-byte local failure a visible
//	           fallback to the real Anthropic API keeps the turn working.
//	           The default for the main conversation.
//	waired     Waired inference only; never contacts Anthropic. WHICH Waired
//	           node serves (this device or a mesh peer) follows the
//	           `waired worker` routing preference — node selection lives
//	           there, not here.
//	anthropic  Always the real Anthropic API (Claude Code's own subscription
//	           credentials pass through), degrading to local only if the API
//	           is transport-unreachable.
//	same       Subagents only: inherit the main conversation's class. The
//	           default for subagents, so an untouched host makes one decision.
//
// Persisted (as ClaudeRoutingPolicy) to <state-dir>/runtime/desired-claude-routing
// (JSON), mutable at runtime via the management API
// (`/waired/v1/integration/claude/route`) and `waired claude route`. The
// intercept reads it per request, so a switch takes effect on the next
// Claude Code request with no restart.
type ClaudeRouteClass string

const (
	ClaudeRouteSame      ClaudeRouteClass = "same" // subagents only: inherit main
	ClaudeRouteAuto      ClaudeRouteClass = "auto"
	ClaudeRouteWaired    ClaudeRouteClass = "waired"
	ClaudeRouteAnthropic ClaudeRouteClass = "anthropic"
)

// ClaudeRoutingPolicy is the per-class routing choice for intercepted
// Claude Code traffic. Main is auto|waired|anthropic; Sub adds the "same"
// sentinel meaning "inherit Main".
type ClaudeRoutingPolicy struct {
	Main ClaudeRouteClass `json:"main"`
	Sub  ClaudeRouteClass `json:"sub"`
}

// DefaultClaudeRoutingPolicy is the built-in behaviour a host that has never
// touched the setting gets: the main conversation auto-routes (Waired first,
// visible Anthropic fallback) and subagents follow it. A fresh host thus
// makes a single decision, and no Claude traffic leaves for Anthropic unless
// local inference fails.
func DefaultClaudeRoutingPolicy() ClaudeRoutingPolicy {
	return ClaudeRoutingPolicy{Main: ClaudeRouteAuto, Sub: ClaudeRouteSame}
}

// Claude traffic classes (#645). The gateway derives the class from the
// original client model id: requests labelled with the managed-settings
// subagent alias are "sub", everything else — including traffic from
// older setups that never wrote the label — stays "main".
const (
	ClaudeClassMain = "main"
	ClaudeClassSub  = "sub"
)

// Effective resolves one traffic class ("main"/"sub") to a concrete route,
// collapsing the subagent-only "same" sentinel onto the main class and any
// empty/zero value onto auto. The returned value is always auto, waired, or
// anthropic.
func (p ClaudeRoutingPolicy) Effective(class string) ClaudeRouteClass {
	r := p.Main
	if class == ClaudeClassSub && p.Sub != ClaudeRouteSame && p.Sub != "" {
		r = p.Sub
	}
	if r == "" || r == ClaudeRouteSame {
		return ClaudeRouteAuto
	}
	return r
}

// DefaultStaleAfter is the heartbeat-staleness window the shell rc uses
// when deciding whether to trust State as "active". Heartbeats fire on
// a tighter cadence (HeartbeatInterval); a missed heartbeat plus the
// margin here gives a SIGKILL detection window of a few seconds.
const (
	DefaultStaleAfter = 15 * time.Second
	HeartbeatInterval = 5 * time.Second
)

// State is the JSON document at <state-dir>/runtime/state.
//
// Two axes describe inference reachability:
//
//   - InferenceReachableLocal — "is THIS device's local inference
//     engine reachable right now?" (5 s probe in
//     cmd/waired-agent/inference_probe.go).
//   - InferenceReachableInMesh — "is at least one OTHER peer's engine
//     reachable + non-stale according to the inferencemesh
//     aggregator?" (re-derived from the snapshot every heartbeat).
//
// The wrapper (`waired claude`) gates on the OR of these two: as long
// as either axis is true, claude is routed via the local gateway
// (which Phase 4 then forwards to either local Ollama or a peer's
// engine via Selection.Runtime); otherwise the wrapper falls back to
// api.anthropic.com directly with a stderr breadcrumb.
//
// Splitting the two axes into separate fields (rather than collapsing
// them into one boolean) preserves diagnostic value: the tray /
// management API can show "local engine: down, mesh fallback: up"
// instead of a single ambiguous bit. The runtime/state file stays
// tiny (one extra bool over Phase 3) so the wrapper hot path reads
// remain near-zero overhead.
//
// Field rename history:
//
//   - pre-Phase 3: InferenceReachableInMesh, but populated with local
//     probe result only (the mesh aggregator did not exist).
//   - Phase 3: renamed to InferenceReachableLocal to honour what it
//     actually carried.
//   - Phase 4 (this commit): InferenceReachableInMesh comes back as a
//     SECOND bool, this time genuinely reflecting peers-only mesh
//     reachability per the aggregator.
type State struct {
	Phase                    Phase     `json:"phase"`
	PID                      int       `json:"pid"`
	Updated                  time.Time `json:"updated"`
	GatewayURL               string    `json:"gateway_url"`
	GatewayToken             string    `json:"gateway_token"`
	InferenceReachableLocal  bool      `json:"inference_reachable_local"`
	InferenceReachableInMesh bool      `json:"inference_reachable_in_mesh"`
}

// Effective reports whether shell integrations should treat this state
// as live. now is injected so tests can pin time.
//
// Effective preserves its historical meaning (agent reachable + active
// + fresh + PID alive). It deliberately does NOT consult
// InferenceReachableLocal — callers that gate on inference (the
// `waired claude` wrapper) compose this with Reason() and a separate
// inference check, so the legacy shell-rc consumers do not flip semantics.
func (s *State) Effective(now time.Time, staleAfter time.Duration) bool {
	ok, _ := s.Reason(now, staleAfter)
	return ok
}

// Reason values are stable strings. The `waired claude` wrapper prints
// these to stderr in the fallback message, so renames are user-visible.
const (
	ReasonAgentPaused          = "agent-paused"
	ReasonAgentStopped         = "agent-stopped"
	ReasonInferenceUnavailable = "inference-unavailable"
	ReasonGatewayUnhealthy     = "gateway-unhealthy"
)

// Reason returns (effective, reason). reason is "" when effective; one
// of the Reason* constants otherwise. Used by `waired claude` to print
// a user-visible fallback message before passthrough exec, so a stale
// state file or dead daemon never silently leaves claude pointing at a
// dead gateway. ReasonGatewayUnhealthy is never returned by Reason
// itself — it is reserved for callers that combine Reason with their
// own gateway probe and need to distinguish the failure mode.
func (s *State) Reason(now time.Time, staleAfter time.Duration) (bool, string) {
	if s == nil {
		return false, ReasonAgentStopped
	}
	if s.Phase != PhaseActive {
		return false, ReasonAgentPaused
	}
	if now.Sub(s.Updated) > staleAfter {
		return false, ReasonAgentStopped
	}
	if s.PID <= 0 || !pidAlive(s.PID) {
		return false, ReasonAgentStopped
	}
	return true, ""
}

// StatePath / DesiredPhasePath are exported so the management server
// and CLI can refer to the same paths the agent writes.
func StatePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "state")
}

func DesiredPhasePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-phase")
}

// DesiredInferencePath is the on-disk location of the user's persisted
// inference enabled/disabled choice.
func DesiredInferencePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-inference")
}

// DesiredSharePath is the on-disk location of the user's persisted
// mesh-share enabled/disabled choice. Missing file means the operator
// has never touched the toggle and the agentconfig default
// (Inference.ShareWithMesh) carries through.
func DesiredSharePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-share")
}

// DesiredPublicSharePath is the on-disk location of the operator's
// Public Share (serve-strangers) choice. Missing file means the
// operator never enabled it — the safe default is OFF.
func DesiredPublicSharePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-public-share")
}

// DesiredWorkerPath is the on-disk location of the operator's
// inference-routing choice (Tailscale-exit-node-style manual peer
// selection). Missing file means the operator has not touched the
// setting and the agentconfig default (Routing.Mode, typically
// RoutingModeAuto) carries through. Stored as JSON because the
// preference carries two fields (mode + peer device ID); the other
// desired-* files store a single token so plain-text was enough.
func DesiredWorkerPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-worker")
}

// DesiredUpdateNotifyPath is the on-disk location of the operator's
// update-prompt on/off choice (#294). Missing file means the operator
// has never touched the toggle and prompts default ON.
func DesiredUpdateNotifyPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-update-notify")
}

// DesiredClaudeRoutingPath is the on-disk location of the unified per-class
// Claude routing policy. Missing file means the operator has never touched
// the setting and DefaultClaudeRoutingPolicy (main=auto, sub=same) applies.
// The two legacy files it supersedes — desired-claude-route (#580) and
// desired-claude-node (#645/#665) — are folded into this one at boot by
// MigrateDesiredClaudeRouting.
func DesiredClaudeRoutingPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-claude-routing")
}

// Read loads the state file. Returns os.ErrNotExist when missing.
func Read(stateDir string) (*State, error) {
	body, err := os.ReadFile(StatePath(stateDir))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("runtime/state: parse: %w", err)
	}
	return &s, nil
}

// Writer atomically persists State. Callers typically build one in the
// agent's main, call Set once at startup, and run a goroutine that
// invokes Heartbeat on a ticker.
type Writer struct {
	stateDir string
	mu       sync.Mutex
	cur      State
}

func NewWriter(stateDir string, initial State) *Writer {
	return &Writer{stateDir: stateDir, cur: initial}
}

// Set replaces the in-memory state and persists. If Updated is zero
// it is auto-populated; PID is always overwritten with os.Getpid().
func (w *Writer) Set(s State) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur = s
	return w.persist()
}

// SetPhase mutates only the phase field.
func (w *Writer) SetPhase(p Phase) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur.Phase = p
	return w.persist()
}

// SetInferenceReachableLocal updates the local-engine reachability
// flag the wrapper consults. Called by the agent's local probe loop
// every HeartbeatInterval. No-op write when the value is unchanged.
func (w *Writer) SetInferenceReachableLocal(v bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur.InferenceReachableLocal == v {
		return nil
	}
	w.cur.InferenceReachableLocal = v
	return w.persist()
}

// SetInferenceReachableInMesh updates the mesh-aggregate reachability
// flag the wrapper composes with the local axis. Called by the agent's
// local probe loop alongside SetInferenceReachableLocal — both axes
// are derived from the same heartbeat tick. No-op when unchanged.
func (w *Writer) SetInferenceReachableInMesh(v bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur.InferenceReachableInMesh == v {
		return nil
	}
	w.cur.InferenceReachableInMesh = v
	return w.persist()
}

// Heartbeat refreshes Updated to the supplied moment and persists.
// The agent runs this on a ticker so paused / stopped state is
// detectable to the shell rc within DefaultStaleAfter.
func (w *Writer) Heartbeat(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur.Updated = now
	return w.persist()
}

// Snapshot returns a copy of the in-memory state without touching disk.
func (w *Writer) Snapshot() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

// Remove deletes the state file. The agent calls this on SIGTERM so
// shells switch to "stopped" without waiting for staleness.
func (w *Writer) Remove() error {
	if err := os.Remove(StatePath(w.stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (w *Writer) persist() error {
	w.cur.PID = os.Getpid()
	if w.cur.Updated.IsZero() {
		w.cur.Updated = time.Now().UTC()
	}
	body, err := json.MarshalIndent(&w.cur, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(StatePath(w.stateDir), append(body, '\n'), 0o644)
}

// ReadDesiredPhase parses <state-dir>/runtime/desired-phase. A missing
// or empty file means the user has not asked for a pause and the agent
// should start in active.
func ReadDesiredPhase(stateDir string) (Phase, error) {
	body, err := os.ReadFile(DesiredPhasePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PhaseActive, nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch Phase(v) {
	case PhaseActive, "":
		return PhaseActive, nil
	case PhasePaused:
		return PhasePaused, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired phase %q", v)
	}
}

// WriteDesiredPhase persists the operator's pause/resume intent.
func WriteDesiredPhase(stateDir string, p Phase) error {
	if p != PhaseActive && p != PhasePaused {
		return fmt.Errorf("runtime/state: invalid phase %q", p)
	}
	return atomicWrite(DesiredPhasePath(stateDir), []byte(string(p)+"\n"), 0o644)
}

// ReadDesiredInferenceState parses <state-dir>/runtime/desired-inference.
// Missing or empty means the operator has not asked for a disable, so
// the agent starts with the inference subsystem enabled.
func ReadDesiredInferenceState(stateDir string) (InferenceState, error) {
	body, err := os.ReadFile(DesiredInferencePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InferenceEnabled, nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch InferenceState(v) {
	case InferenceEnabled, "":
		return InferenceEnabled, nil
	case InferenceDisabled:
		return InferenceDisabled, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired inference state %q", v)
	}
}

// WriteDesiredInferenceState persists the operator's enable/disable intent.
func WriteDesiredInferenceState(stateDir string, s InferenceState) error {
	if s != InferenceEnabled && s != InferenceDisabled {
		return fmt.Errorf("runtime/state: invalid inference state %q", s)
	}
	return atomicWrite(DesiredInferencePath(stateDir), []byte(string(s)+"\n"), 0o644)
}

// ReadDesiredShareMesh parses <state-dir>/runtime/desired-share. A
// missing or empty file returns the empty string so callers can fall
// back to the agentconfig default (Inference.ShareWithMesh) instead of
// being forced into a binary choice. Returning the empty state from a
// read also disambiguates "user has never touched the toggle" from
// "user explicitly chose shared".
func ReadDesiredShareMesh(stateDir string) (ShareMeshState, error) {
	body, err := os.ReadFile(DesiredSharePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch ShareMeshState(v) {
	case "":
		return "", nil
	case ShareMeshShared:
		return ShareMeshShared, nil
	case ShareMeshNotShared:
		return ShareMeshNotShared, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired share state %q", v)
	}
}

// WriteDesiredShareMesh persists the operator's mesh-share choice.
func WriteDesiredShareMesh(stateDir string, s ShareMeshState) error {
	if s != ShareMeshShared && s != ShareMeshNotShared {
		return fmt.Errorf("runtime/state: invalid share state %q", s)
	}
	return atomicWrite(DesiredSharePath(stateDir), []byte(string(s)+"\n"), 0o644)
}

// ReadDesiredPublicShare parses <state-dir>/runtime/desired-public-share.
// A missing or empty file returns the empty string — callers treat that
// as OFF (public serving is strictly opt-in, spec §4.1), while still
// being able to distinguish "never touched" from an explicit choice.
func ReadDesiredPublicShare(stateDir string) (PublicShareState, error) {
	body, err := os.ReadFile(DesiredPublicSharePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch PublicShareState(v) {
	case "":
		return "", nil
	case PublicShareOn:
		return PublicShareOn, nil
	case PublicShareOff:
		return PublicShareOff, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired public-share state %q", v)
	}
}

// WriteDesiredPublicShare persists the operator's Public Share choice.
func WriteDesiredPublicShare(stateDir string, s PublicShareState) error {
	if s != PublicShareOn && s != PublicShareOff {
		return fmt.Errorf("runtime/state: invalid public-share state %q", s)
	}
	return atomicWrite(DesiredPublicSharePath(stateDir), []byte(string(s)+"\n"), 0o644)
}

// ReadDesiredWorker parses <state-dir>/runtime/desired-worker. A
// missing file returns the zero RoutingPreference (Mode="",
// PinnedPeerDeviceID=""), letting callers disambiguate "operator has
// never touched the toggle" from "operator explicitly chose auto" via
// RoutingPreference.IsZero. Returns an error only when the file
// exists but its JSON or mode value cannot be parsed.
func ReadDesiredWorker(stateDir string) (RoutingPreference, error) {
	body, err := os.ReadFile(DesiredWorkerPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RoutingPreference{}, nil
		}
		return RoutingPreference{}, err
	}
	var p RoutingPreference
	if err := json.Unmarshal(body, &p); err != nil {
		return RoutingPreference{}, fmt.Errorf("runtime/state: parse desired-worker: %w", err)
	}
	if err := validateRoutingPreference(p); err != nil {
		return RoutingPreference{}, err
	}
	return p, nil
}

// WriteDesiredWorker persists the operator's routing choice. Rejects
// pinned mode without a peer device ID and non-pinned modes that
// carry a stray peer device ID, so the on-disk shape is always
// self-consistent.
func WriteDesiredWorker(stateDir string, p RoutingPreference) error {
	if err := validateRoutingPreference(p); err != nil {
		return err
	}
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal desired-worker: %w", err)
	}
	return atomicWrite(DesiredWorkerPath(stateDir), append(body, '\n'), 0o644)
}

// ReadDesiredClaudeRouting parses <state-dir>/runtime/desired-claude-routing.
// A missing file returns DefaultClaudeRoutingPolicy (main=auto, sub=same).
// Empty/zero fields are coerced (main→auto, sub→same) so a hand-written
// partial file still reads as a self-consistent policy. Returns an error
// only when the file exists but its JSON or a class value cannot be parsed.
func ReadDesiredClaudeRouting(stateDir string) (ClaudeRoutingPolicy, error) {
	body, err := os.ReadFile(DesiredClaudeRoutingPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultClaudeRoutingPolicy(), nil
		}
		return ClaudeRoutingPolicy{}, err
	}
	var p ClaudeRoutingPolicy
	if err := json.Unmarshal(body, &p); err != nil {
		return ClaudeRoutingPolicy{}, fmt.Errorf("runtime/state: parse desired-claude-routing: %w", err)
	}
	coerceClaudeRoutingPolicy(&p)
	if err := validateClaudeRoutingPolicy(p); err != nil {
		return ClaudeRoutingPolicy{}, err
	}
	return p, nil
}

// WriteDesiredClaudeRouting persists the unified per-class routing policy.
// Fields are coerced (main→auto, sub→same) and validated so the on-disk
// shape is always self-consistent.
func WriteDesiredClaudeRouting(stateDir string, p ClaudeRoutingPolicy) error {
	coerceClaudeRoutingPolicy(&p)
	if err := validateClaudeRoutingPolicy(p); err != nil {
		return err
	}
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal desired-claude-routing: %w", err)
	}
	return atomicWrite(DesiredClaudeRoutingPath(stateDir), append(body, '\n'), 0o644)
}

// coerceClaudeRoutingPolicy fills zero/invalid-shape fields with their
// defaults: an empty or "same" main becomes auto ("same" is subagent-only),
// an empty sub becomes "same".
func coerceClaudeRoutingPolicy(p *ClaudeRoutingPolicy) {
	if p.Main == "" || p.Main == ClaudeRouteSame {
		p.Main = ClaudeRouteAuto
	}
	if p.Sub == "" {
		p.Sub = ClaudeRouteSame
	}
}

// validateClaudeRouteClass rejects any class outside the known set.
// allowSame permits the subagent-only "same" sentinel.
func validateClaudeRouteClass(c ClaudeRouteClass, allowSame bool) error {
	switch c {
	case ClaudeRouteAuto, ClaudeRouteWaired, ClaudeRouteAnthropic:
		return nil
	case ClaudeRouteSame:
		if allowSame {
			return nil
		}
	}
	return fmt.Errorf("runtime/state: invalid claude route class %q", c)
}

// validateClaudeRoutingPolicy rejects unknown classes. "same" is valid only
// for the subagent class.
func validateClaudeRoutingPolicy(p ClaudeRoutingPolicy) error {
	if err := validateClaudeRouteClass(p.Main, false); err != nil {
		return fmt.Errorf("runtime/state: claude route main: %w", err)
	}
	if err := validateClaudeRouteClass(p.Sub, true); err != nil {
		return fmt.Errorf("runtime/state: claude route sub: %w", err)
	}
	return nil
}

// legacy on-disk shapes, read only during MigrateDesiredClaudeRouting.
type legacyClaudeRoute struct {
	Mode          string `json:"mode"`
	AllowFallback bool   `json:"allow_fallback"`
}

type legacyClaudeTarget struct {
	Kind         string `json:"kind"`
	PeerDeviceID string `json:"peer_device_id,omitempty"`
}

type legacyClaudeNode struct {
	Main legacyClaudeTarget `json:"main"`
	Sub  legacyClaudeTarget `json:"sub"`
}

// MigrateDesiredClaudeRouting performs the one-time boot migration from the
// pre-unification split state — the global route mode
// (runtime/desired-claude-route, #580) plus the per-class node policy
// (runtime/desired-claude-node, #645/#665) — to the unified
// ClaudeRoutingPolicy (runtime/desired-claude-routing). It is a no-op when
// the new file already exists or neither legacy file is present. On success
// it writes the new file and best-effort removes both legacy files.
func MigrateDesiredClaudeRouting(stateDir string) (migrated bool, err error) {
	newPath := DesiredClaudeRoutingPath(stateDir)
	if _, statErr := os.Stat(newPath); statErr == nil {
		return false, nil // already migrated / natively written
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	routePath := filepath.Join(stateDir, "runtime", "desired-claude-route")
	nodePath := filepath.Join(stateDir, "runtime", "desired-claude-node")
	routeRaw, routeErr := os.ReadFile(routePath)
	nodeRaw, nodeErr := os.ReadFile(nodePath)
	if errors.Is(routeErr, os.ErrNotExist) && errors.Is(nodeErr, os.ErrNotExist) {
		return false, nil // fresh host — the default policy applies, nothing to write
	}
	// Legacy defaults (auto + fallback, all-local) so a missing or malformed
	// file collapses to the same behaviour the old readers produced.
	lr := legacyClaudeRoute{Mode: "auto", AllowFallback: true}
	if routeErr == nil {
		_ = json.Unmarshal(routeRaw, &lr)
	}
	ln := legacyClaudeNode{Main: legacyClaudeTarget{Kind: "local"}, Sub: legacyClaudeTarget{Kind: "local"}}
	if nodeErr == nil {
		_ = json.Unmarshal(nodeRaw, &ln)
	}
	pol := ClaudeRoutingPolicy{
		Main: migrateClaudeClass(lr.Mode, lr.AllowFallback, ln.Main),
		Sub:  migrateClaudeClass(lr.Mode, lr.AllowFallback, ln.Sub),
	}
	if pol.Sub == pol.Main {
		pol.Sub = ClaudeRouteSame // collapse an identical sub onto the default
	}
	if err := WriteDesiredClaudeRouting(stateDir, pol); err != nil {
		return false, err
	}
	// Best-effort cleanup; a leftover legacy file is harmless now that the
	// new file exists (a re-run of this migration short-circuits on it).
	_ = os.Remove(routePath)
	_ = os.Remove(nodePath)
	return true, nil
}

// migrateClaudeClass folds one class's old (route mode, allow_fallback, node
// target) into a single ClaudeRouteClass. The global route mode dominates
// (local→waired, anthropic→anthropic); under auto the per-class node target
// decides (anthropic→anthropic, peer→waired since peer selection now lives
// in `waired worker`, local→auto unless the fallback was disabled, which is
// the old privacy opt-out and maps to waired = never Anthropic).
func migrateClaudeClass(mode string, allowFallback bool, t legacyClaudeTarget) ClaudeRouteClass {
	switch mode {
	case "local":
		return ClaudeRouteWaired
	case "anthropic":
		return ClaudeRouteAnthropic
	default: // "auto" or unknown → consult the per-class node target
		switch t.Kind {
		case "anthropic":
			return ClaudeRouteAnthropic
		case "peer":
			return ClaudeRouteWaired
		default: // local / unknown
			if !allowFallback {
				return ClaudeRouteWaired
			}
			return ClaudeRouteAuto
		}
	}
}

// ReadDesiredUpdateNotify parses <state-dir>/runtime/desired-update-notify.
// A missing or empty file defaults to UpdateNotifyOn — there is no
// agentconfig default to defer to, and the prompt is the feature, so a
// host that has never touched the toggle still gets prompted (#294).
func ReadDesiredUpdateNotify(stateDir string) (UpdateNotifyState, error) {
	body, err := os.ReadFile(DesiredUpdateNotifyPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UpdateNotifyOn, nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch UpdateNotifyState(v) {
	case "", UpdateNotifyOn:
		return UpdateNotifyOn, nil
	case UpdateNotifyOff:
		return UpdateNotifyOff, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired update-notify state %q", v)
	}
}

// WriteDesiredUpdateNotify persists the operator's update-prompt choice.
func WriteDesiredUpdateNotify(stateDir string, s UpdateNotifyState) error {
	if s != UpdateNotifyOn && s != UpdateNotifyOff {
		return fmt.Errorf("runtime/state: invalid update-notify state %q", s)
	}
	return atomicWrite(DesiredUpdateNotifyPath(stateDir), []byte(string(s)+"\n"), 0o644)
}

func validateRoutingPreference(p RoutingPreference) error {
	switch p.Mode {
	case "", RoutingModeAuto, RoutingModeLocalOnly, RoutingModePeerPreferred:
		if p.PinnedPeerDeviceID != "" {
			return fmt.Errorf("runtime/state: mode %q must not carry pinned_peer_device_id", p.Mode)
		}
		return nil
	case RoutingModePinned:
		if p.PinnedPeerDeviceID == "" {
			return fmt.Errorf("runtime/state: mode %q requires pinned_peer_device_id", p.Mode)
		}
		return nil
	default:
		return fmt.Errorf("runtime/state: unknown routing mode %q", p.Mode)
	}
}

// pidAlive lives in pid_unix.go (signal-0 / EPERM semantics) and
// pid_windows.go (OpenProcess + GetExitCodeProcess STILL_ACTIVE check).
// The two OSes have incompatible "is this pid alive?" primitives so
// keeping them in build-tagged files is cheaper than a runtime branch.

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
