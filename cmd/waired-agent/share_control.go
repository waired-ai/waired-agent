package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// sharingController owns the one sharing answer that lives on this
// machine (waired#1297, owner ruling 2026-08-30): does this computer
// lend itself out at all. Off stops every kind of serving — the
// account's own mesh, public guests, and anything added later.
//
// Who the computer is offered to is not decided here. The control plane
// owns that, and its answer arrives on the signed map; this controller
// holds the mesh half of it (meshShare) so the gates can read one place,
// and reads the public half from the public controller beside it.
//
// It implements management.SharingController and feeds three consumers:
// the inference probe loop (reports the machine's own refusal as
// InferenceState.NotShared), the peer-overlay listener middleware (503
// waired_inference_not_shared), and the public admission chain.
type sharingController struct {
	stateDir string
	logger   *slog.Logger

	// sharing is the persisted answer, lock-free for the probe and
	// listener hot paths.
	sharing atomic.Bool

	// suspended is a LIVE-ONLY override that withholds sharing without
	// touching the operator's persisted choice (#316). The app sets it
	// on Quit — "nobody is at this machine right now" — and clears it on
	// its next start; a daemon restart clears it by construction. That
	// asymmetry is deliberate and mirrors the engine power axis: an
	// operational "not now" is never persisted, only a policy is.
	// Writing "off" on Quit instead would silently revoke the operator's
	// preference for good.
	//
	// Since waired#1297 it covers public serving as well as the mesh:
	// quitting the app stops the lot, which is what the ruling asks of
	// it, and what a person who closed the app expects of a computer
	// they are lending out.
	suspended atomic.Bool

	// meshShare is the control plane's last word on serving the owner's
	// own machines. Nothing on this machine writes the setting — it is
	// re-asserted on every map frame that carries one — so this is a
	// cache, backed by runtime/applied-mesh-share so a restart does not
	// open a gap before the first frame.
	meshShare atomic.Bool

	// onStop runs when serving stops, before anything is persisted: the
	// public in-flight abort (public share spec §8.3 step 1). Never nil
	// in production; nil in tests that do not care.
	//
	// publicFn / publicMaxFn report the control plane's public setting
	// for the status surface. Seams rather than a direct dependency
	// because the public controller is built after this one.
	mu          sync.Mutex
	onStop      func()
	publicFn    func() bool
	publicMaxFn func() int
}

// newSharingController builds the controller from what is on disk: the
// operator's persisted hard kill, and the control plane's last
// mesh-share instruction. Both default to sharing when absent — the
// answer this agent gave before either had a home.
func newSharingController(stateDir string, initial state.SharingState, mesh state.MeshShareState, logger *slog.Logger) *sharingController {
	sc := &sharingController{stateDir: stateDir, logger: logger}
	sc.sharing.Store(initial != state.SharingOff)
	sc.meshShare.Store(mesh != state.MeshShareOff)
	return sc
}

// SetOnStop registers the abort that runs when serving stops.
func (sc *sharingController) SetOnStop(fn func()) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.onStop = fn
}

// SetPublicReporters wires the read-only view of the control plane's
// public setting, so one status call can answer who this computer
// serves.
func (sc *sharingController) SetPublicReporters(enabled func() bool, maxClients func() int) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.publicFn, sc.publicMaxFn = enabled, maxClients
}

// IsSharing is the machine's own answer, and the only half of the
// question this computer decides. A suspended agent reads as not
// sharing without its persisted choice having changed.
func (sc *sharingController) IsSharing() bool {
	return sc.sharing.Load() && !sc.suspended.Load()
}

// IsShared reports whether MESH peers may be served: the machine is
// lending itself out AND the control plane still has it in the owner's
// own mesh.
func (sc *sharingController) IsShared() bool { return sc.IsSharing() && sc.meshShare.Load() }

// IsShareDenied is the negation alias used by middleware that names
// gates after the rejected state (IsPaused, IsInferenceDisabled,
// IsShareDenied). Returning true means "deny the request".
func (sc *sharingController) IsShareDenied() bool { return !sc.IsShared() }

// IsSuspended reports the live-only override, so status can distinguish
// "the operator stopped sharing" from "the app is not running".
func (sc *sharingController) IsSuspended() bool { return sc.suspended.Load() }

// Share / Unshare satisfy management.SharingController. Both are
// explicit operator actions, so they also clear any suspension —
// otherwise a stale latch could swallow the very command just used.
func (sc *sharingController) Share(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(false)
	return sc.transition(state.SharingOn)
}

// Unshare stops serving and waits for nothing to do it (public share
// spec §8.3): the live flag goes first, then the in-flight abort, and
// only then the write to disk. Persistence is what survives a restart,
// not a precondition for stopping now — so a failed write is reported
// after the machine has already stopped serving, never instead of it.
func (sc *sharingController) Unshare(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(false)
	return sc.transition(state.SharingOff)
}

// Suspend withholds sharing for this session only. Nothing is
// persisted: the next daemon start, or the app's next Unsuspend,
// restores whatever the operator actually chose.
func (sc *sharingController) Suspend(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(true)
	return nil
}

// Unsuspend lifts the session override. It does NOT start sharing — if
// the operator turned it off, it stays off.
func (sc *sharingController) Unsuspend(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(false)
	return nil
}

func (sc *sharingController) setSuspended(v bool) {
	if sc.suspended.Swap(v) == v {
		return
	}
	if v {
		sc.stopServing()
	}
	if sc.logger != nil {
		sc.logger.Info("sharing suspension change", "suspended", v, "persisted_sharing", sc.sharing.Load())
	}
}

// SetMeshShareFromControlPlane applies the control plane's instruction.
// The device never writes this setting (waired-agent#646), so it is
// re-asserted on every frame that carries one and a repeat is a no-op.
// The cache write is best-effort: it only shortens the window after a
// restart, and losing it costs a frame, not correctness.
func (sc *sharingController) SetMeshShareFromControlPlane(v state.MeshShareState) {
	if v != state.MeshShareOn && v != state.MeshShareOff {
		return
	}
	on := v == state.MeshShareOn
	if sc.meshShare.Swap(on) == on {
		return
	}
	if sc.logger != nil {
		sc.logger.Info("mesh sharing changed by the control plane", "mesh_share", string(v))
	}
	if err := state.WriteAppliedMeshShare(sc.stateDir, v); err != nil && sc.logger != nil {
		sc.logger.Warn("cache mesh-share instruction", "err", err)
	}
}

// MeshShare / PublicShare / PublicMaxClients are the read-only report of
// the control plane's settings (management.SharingController).
func (sc *sharingController) MeshShare() state.MeshShareState {
	if sc.meshShare.Load() {
		return state.MeshShareOn
	}
	return state.MeshShareOff
}

func (sc *sharingController) PublicShare() state.SharingState {
	sc.mu.Lock()
	fn := sc.publicFn
	sc.mu.Unlock()
	if fn == nil {
		return ""
	}
	if fn() {
		return state.SharingOn
	}
	return state.SharingOff
}

func (sc *sharingController) PublicMaxClients() int {
	sc.mu.Lock()
	fn := sc.publicMaxFn
	sc.mu.Unlock()
	if fn == nil {
		return 0
	}
	return fn()
}

// State reports the live value and the persisted intent. They differ
// while sharing is suspended for the session, or persistently when the
// operator edits desired-sharing by hand and the daemon has not been
// signalled yet.
func (sc *sharingController) State() (current, desired state.SharingState) {
	chosen := state.SharingOff
	if sc.sharing.Load() {
		chosen = state.SharingOn
	}
	current = chosen
	if sc.suspended.Load() {
		current = state.SharingOff
	}
	desired = chosen
	if d, err := state.ReadDesiredSharing(sc.stateDir); err == nil && d != "" {
		desired = d
	}
	return
}

func (sc *sharingController) transition(target state.SharingState) error {
	if sc.logger != nil {
		sc.logger.Debug("sharing transition requested",
			"from_sharing", sc.sharing.Load(), "target", string(target))
	}
	// Order matters, and is the §8.3 requirement: stop first, record
	// afterwards. Turning sharing ON is the mirror image — persist
	// first, so a machine that cannot record the choice does not start
	// serving on the strength of it.
	if target == state.SharingOff {
		sc.sharing.Store(false)
		sc.stopServing()
		if err := state.WriteDesiredSharing(sc.stateDir, target); err != nil {
			return fmt.Errorf("persist desired-sharing: %w", err)
		}
	} else {
		if err := state.WriteDesiredSharing(sc.stateDir, target); err != nil {
			return fmt.Errorf("persist desired-sharing: %w", err)
		}
		sc.sharing.Store(true)
	}
	if sc.logger != nil {
		sc.logger.Info("sharing state change", "state", string(target))
	}
	return nil
}

func (sc *sharingController) stopServing() {
	sc.mu.Lock()
	fn := sc.onStop
	sc.mu.Unlock()
	if fn != nil {
		fn()
	}
}
