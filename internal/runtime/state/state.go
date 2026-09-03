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

	"github.com/waired-ai/waired-agent/internal/platform/atomicfile"
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

// SharingState is whether this computer lends itself out at all — the
// one sharing answer that stays on the machine (waired#1297, owner
// ruling 2026-08-30). Off stops every kind of serving: the account's own
// mesh, public guests, and anything added later.
//
// Persisted to <state-dir>/runtime/desired-sharing, written only by
// `waired share on|off` through the management API. Who the computer is
// offered to — the account's own machines, people outside it — is the
// control plane's to decide, and arrives on the signed map instead.
type SharingState string

const (
	SharingOn  SharingState = "on"
	SharingOff SharingState = "off"
)

// MeshShareState is the control plane's last word on whether this
// computer serves the rest of its owner's own machines. Cached to
// <state-dir>/runtime/applied-mesh-share so a restart does not open a
// gap before the first signed map arrives, in the shape
// runtime/applied-residency already uses.
//
// It is a CACHE, not an intent: nothing on this machine writes the
// setting, and the value here is replaced by every map frame that
// carries one.
type MeshShareState string

const (
	MeshShareOn  MeshShareState = "on"
	MeshShareOff MeshShareState = "off"
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
	// RoutingModePeerOnly is the mirror image of RoutingModeLocalOnly:
	// serve from another device on the mesh or fail. Unlike
	// RoutingModePeerPreferred it never falls back to the local engine,
	// so an operator who chose it because this machine must stay free
	// (thermals, battery, a GPU held by something else) gets an error
	// instead of a silent local run (#327).
	RoutingModePeerOnly RoutingMode = "peer-only"
	RoutingModePinned   RoutingMode = "pinned"
)

// RoutingPrefer is what the operator asked the mesh ordering to optimise
// for when several computers could answer (waired-agent#1128).
//
// The value is spelled `size`, not `quality`. waired-agent#537 removed the
// quality number from operator-facing surfaces precisely because
// "low/high would smuggle back the good-or-bad reading the number was
// removed for" — and on the mesh that produced #1082 the reading would
// have been wrong anyway: the peer running the biggest model was both the
// slowest AND, by the catalog's own quality ladder, the lower-quality one.
// What the setting does is prefer the bigger model, so that is what it is
// called (owner ruling, 2026-08-29).
type RoutingPrefer string

const (
	// RoutingPreferSpeed answers as fast as possible. The default: the
	// ordering had no term for what a turn costs the person waiting for
	// it, and the result was nine minutes for a turn another of the same
	// person's machines answered in forty-three seconds (#1082).
	RoutingPreferSpeed RoutingPrefer = "speed"
	// RoutingPreferSize uses the biggest model available.
	RoutingPreferSize RoutingPrefer = "size"
)

// RoutingPreference is the on-disk form of the operator's routing
// choice. PinnedPeerDeviceID is meaningful only when Mode ==
// RoutingModePinned and is cleared on transitions to other modes.
type RoutingPreference struct {
	Mode               RoutingMode `json:"mode"`
	PinnedPeerDeviceID string      `json:"pinned_peer_device_id,omitempty"`
	// PinnedPeerDisplayID is what that peer may be called on a surface an
	// operator reads — its grant pseudonym when the pin is a public
	// machine, its DeviceID when it is one of your own (public share spec
	// §8.5).
	//
	// Recorded here because it is only knowable while the peer is in the
	// mesh snapshot, and the surface that most needs it is the one shown
	// after the peer has dropped out of it: the tray's "(absent)" row and
	// `waired worker get` both used to fall back to the raw
	// PinnedPeerDeviceID there (#739). Empty for a pin set by an agent
	// predating the field, and for a public machine that carried no
	// pseudonym.
	PinnedPeerDisplayID string `json:"pinned_peer_display_id,omitempty"`

	// Prefer is what the ordering optimises for when several computers
	// could answer. Empty == RoutingPreferSpeed, which is both the
	// default and what an agent predating the field behaves as.
	Prefer RoutingPrefer `json:"prefer,omitempty"`

	// MinModelSize is the smallest model class this device will route to
	// — "" (no floor, the default), "small", "medium" or "large", the
	// same vocabulary `waired public use --min-model-size` already uses
	// (proto/hostfit.ModelSize*).
	//
	// It EXCLUDES rather than demotes, and applies to this device's own
	// engine as well as to peers (owner ruling, 2026-08-29): a request
	// with nothing above the floor falls back and names the reason.
	MinModelSize string `json:"min_model_size,omitempty"`
}

// IsZero reports whether the preference is the all-defaults form a
// caller should treat as "operator has not touched this". Used at boot
// to decide whether the persisted file overrides the agentconfig
// default.
func (p RoutingPreference) IsZero() bool {
	// Every operator-settable field, or a saved choice stops taking
	// effect: main.go reads IsZero to decide whether the persisted file
	// overrides the agentconfig default, so a preference that carries
	// only Prefer or only MinModelSize would be discarded at every boot
	// (waired-agent#1128).
	return p.Mode == "" && p.PinnedPeerDeviceID == "" &&
		p.Prefer == "" && p.MinModelSize == ""
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

// Claude traffic classes (#645). The gateway derives the class from the
// original client model id: requests labelled with the managed-settings
// subagent alias are "sub", everything else — including traffic from
// older setups that never wrote the label — stays "main".
//
// The class no longer picks a route (there is none to pick since
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
// It sizes the peer leg's grace period and keys the served/requested records.
const (
	ClaudeClassMain = "main"
	ClaudeClassSub  = "sub"
)

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

// DesiredSharingPath is the on-disk location of the hard kill: whether
// this computer lends itself out at all. Missing file means nobody has
// turned it off, which is a computer that shares.
func DesiredSharingPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "desired-sharing")
}

// AppliedMeshSharePath is where the last mesh-share instruction from the
// control plane is cached. Missing file means none has arrived, and the
// boot default is to share — the answer this agent gave before the
// setting had a home (waired#1297).
func AppliedMeshSharePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "applied-mesh-share")
}

// retiredSharingPaths are the two files that used to hold sharing
// intent on the machine, before waired#1297 moved the distributions to
// the control plane and left one switch here.
//
// They are deleted rather than read. The owner ruling is that every
// computer starts sharing again (the values meant something else — one
// was "not to my own mesh", the other "not to strangers" — and neither
// is the new question), and a file nobody reads is one a later reader
// can resurrect with the wrong meaning.
func retiredSharingPaths(stateDir string) []string {
	return []string{
		filepath.Join(stateDir, "runtime", "desired-share"),
		filepath.Join(stateDir, "runtime", "desired-public-share"),
	}
}

// RemoveRetiredSharingFiles deletes the pre-waired#1297 sharing files.
// Called once at daemon start; a missing file is the expected case and
// not an error.
func RemoveRetiredSharingFiles(stateDir string) error {
	for _, p := range retiredSharingPaths(stateDir) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("runtime/state: remove %s: %w", filepath.Base(p), err)
		}
	}
	return nil
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
	prev := w.cur
	w.cur = s
	return w.persistOrRollBack(prev)
}

// SetPhase mutates only the phase field.
func (w *Writer) SetPhase(p Phase) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.cur
	w.cur.Phase = p
	return w.persistOrRollBack(prev)
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
	prev := w.cur
	w.cur.InferenceReachableLocal = v
	return w.persistOrRollBack(prev)
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
	prev := w.cur
	w.cur.InferenceReachableInMesh = v
	return w.persistOrRollBack(prev)
}

// Heartbeat refreshes Updated to the supplied moment and persists.
// The agent runs this on a ticker so paused / stopped state is
// detectable to the shell rc within DefaultStaleAfter.
func (w *Writer) Heartbeat(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.cur
	w.cur.Updated = now
	return w.persistOrRollBack(prev)
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

// persistOrRollBack writes the current state, restoring prev if the write
// fails. Every mutator above calls this rather than persist directly.
//
// The roll-back is what makes a lost write recoverable. The mutators change
// w.cur BEFORE persisting, and the two reachability setters return early
// when the value is unchanged — so a write that failed once used to leave
// w.cur permanently ahead of the file: the next tick compared equal,
// returned without touching disk, and the stale value stayed on disk for the
// life of the process. One lost race became a permanently wrong state file
// rather than a few seconds of lag.
//
// That is not hypothetical on Windows, where a reader holding the
// destination open is enough to fail the replacing rename
// (waired-agent#698). Rolling back means the next heartbeat, five seconds
// later, sees a difference again and rewrites.
//
// A whole-struct copy is safe here: State is scalars, a string pair and a
// time.Time — no slice or map whose backing array a shallow copy would
// share. A field added later that does not have that property has to be
// rolled back by hand.
func (w *Writer) persistOrRollBack(prev State) error {
	if err := w.persist(); err != nil {
		w.cur = prev
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
// A missing or empty file returns the empty string so callers can fall
// back to the agentconfig default (Inference.Enabled) instead of being
// forced into a binary choice — the same shape as ReadDesiredShareMesh
// and ReadDesiredPublicShare, for the same reason: "never touched the
// toggle" and "explicitly chose on" are different facts.
//
// It used to answer InferenceEnabled for a missing file, which left the
// boot path no way to consult the install-time default — so
// agentconfig.Inference.Enabled gated the whole subsystem instead, and a
// host it turned off had no product-side way back (#465,
// waired-ai/waired#1056).
func ReadDesiredInferenceState(stateDir string) (InferenceState, error) {
	body, err := os.ReadFile(DesiredInferencePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch InferenceState(v) {
	case "":
		return "", nil
	case InferenceEnabled:
		return InferenceEnabled, nil
	case InferenceDisabled:
		return InferenceDisabled, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired inference state %q", v)
	}
}

// WriteDesiredInferenceState persists the operator's enable/disable intent.
//
// It also drops the host measurement's "and that is why local inference
// is off" claim (#496), because whoever is writing here is now the reason
// the toggle reads the way it does. The install-time cutoff re-asserts
// the claim immediately after its own write; every other writer — the
// tray, `waired inference on|off`, the boot plan — leaves it cleared.
func WriteDesiredInferenceState(stateDir string, s InferenceState) error {
	if s != InferenceEnabled && s != InferenceDisabled {
		return fmt.Errorf("runtime/state: invalid inference state %q", s)
	}
	if err := atomicWrite(DesiredInferencePath(stateDir), []byte(string(s)+"\n"), 0o644); err != nil {
		return err
	}
	clearHostSpeedCutoffFlag(stateDir)
	return nil
}

// ReadDesiredSharing parses <state-dir>/runtime/desired-sharing. A
// missing or empty file returns the empty string, which callers read as
// "nobody has turned sharing off" — the hard kill is opt-OUT, so absence
// is a computer that shares. The empty return also lets a caller tell
// "never touched" from an explicit "on".
func ReadDesiredSharing(stateDir string) (SharingState, error) {
	body, err := os.ReadFile(DesiredSharingPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch SharingState(v) {
	case "":
		return "", nil
	case SharingOn:
		return SharingOn, nil
	case SharingOff:
		return SharingOff, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown desired sharing state %q", v)
	}
}

// WriteDesiredSharing persists the hard kill.
func WriteDesiredSharing(stateDir string, s SharingState) error {
	if s != SharingOn && s != SharingOff {
		return fmt.Errorf("runtime/state: invalid sharing state %q", s)
	}
	return atomicWrite(DesiredSharingPath(stateDir), []byte(string(s)+"\n"), 0o644)
}

// ReadAppliedMeshShare parses the cached control-plane instruction. A
// missing or empty file returns the empty string: no instruction has
// arrived, and the caller's boot default (share) stands.
func ReadAppliedMeshShare(stateDir string) (MeshShareState, error) {
	body, err := os.ReadFile(AppliedMeshSharePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(body))
	switch MeshShareState(v) {
	case "":
		return "", nil
	case MeshShareOn:
		return MeshShareOn, nil
	case MeshShareOff:
		return MeshShareOff, nil
	default:
		return "", fmt.Errorf("runtime/state: unknown applied mesh-share state %q", v)
	}
}

// WriteAppliedMeshShare caches the control plane's last word.
func WriteAppliedMeshShare(stateDir string, s MeshShareState) error {
	if s != MeshShareOn && s != MeshShareOff {
		return fmt.Errorf("runtime/state: invalid mesh-share state %q", s)
	}
	return atomicWrite(AppliedMeshSharePath(stateDir), []byte(string(s)+"\n"), 0o644)
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
	if err := validateRoutingPrefer(p.Prefer); err != nil {
		return err
	}
	if err := ValidateMinModelSize(p.MinModelSize); err != nil {
		return err
	}
	switch p.Mode {
	case "", RoutingModeAuto, RoutingModeLocalOnly, RoutingModePeerPreferred, RoutingModePeerOnly:
		if p.PinnedPeerDeviceID != "" {
			return fmt.Errorf("runtime/state: mode %q must not carry pinned_peer_device_id", p.Mode)
		}
		// The display identifier names the pinned peer, so it travels
		// with the pin rather than outliving it.
		if p.PinnedPeerDisplayID != "" {
			return fmt.Errorf("runtime/state: mode %q must not carry pinned_peer_display_id", p.Mode)
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
	// atomicfile.Replace, not os.Rename: on Windows the replacing rename is
	// refused while anyone holds either file open, and `state` has readers by
	// design — state.Read is how every consumer of this file learns anything
	// (waired-agent#698).
	return atomicfile.Replace(tmpName, path)
}

// validateRoutingPrefer accepts the two spellings and the empty value,
// which reads as RoutingPreferSpeed.
func validateRoutingPrefer(p RoutingPrefer) error {
	switch p {
	case "", RoutingPreferSpeed, RoutingPreferSize:
		return nil
	default:
		return fmt.Errorf("runtime/state: unknown routing prefer %q", p)
	}
}

// ValidateMinModelSize accepts the model-size vocabulary and the empty
// value, which is "no floor".
//
// The vocabulary is hostfit's — the same one `waired public use
// --min-model-size` already speaks — but this package cannot import
// proto/hostfit without pulling the wire module into the state layer, so
// the three words are listed here and pinned against hostfit by a test in
// the router package, where both are already in scope.
func ValidateMinModelSize(size string) error {
	switch size {
	case "", "small", "medium", "large":
		return nil
	default:
		return fmt.Errorf("runtime/state: unknown min model size %q", size)
	}
}
