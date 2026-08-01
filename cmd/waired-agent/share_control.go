package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// shareController owns the in-memory live "is this agent currently
// sharing its local engine with the mesh?" flag plus persistence of
// the operator's enable/disable intent. It implements
// management.ShareController and feeds two consumers: the inference
// probe loop (skips the CP push when shared=false) and the
// peer-overlay listener middleware (returns 503
// waired_inference_not_shared when shared=false).
//
// Mirrors inferenceController in every meaningful way — same
// atomic.Bool hot-path pattern, same desired-state-file persistence,
// same (current, desired) reporting — so future maintenance edits can
// be applied uniformly to both.
type shareController struct {
	stateDir string
	logger   *slog.Logger

	// shared holds the current live state. atomic.Bool keeps the
	// probe + listener hot paths lock-free.
	shared atomic.Bool

	// suspended is a LIVE-ONLY override that withholds sharing without
	// touching the operator's persisted choice (#316). The tray sets it
	// on Quit — "nobody is at this machine right now" — and clears it on
	// its next start; a daemon restart clears it by construction. That
	// asymmetry is deliberate and mirrors the engine power axis: an
	// operational "not now" is never persisted, only a policy is. Writing
	// not_shared on Quit instead would silently revoke the user's sharing
	// preference for good.
	suspended atomic.Bool
}

// newShareController builds the controller with the initial decision
// resolved by the caller (typically: agentconfig default overlaid by
// state.ReadDesiredShareMesh).
func newShareController(stateDir string, initial state.ShareMeshState, logger *slog.Logger) *shareController {
	sc := &shareController{
		stateDir: stateDir,
		logger:   logger,
	}
	// Treat the empty initial value as "shared" — the default Phase 4
	// behaviour. Only an explicit not_shared persisted choice flips
	// the boot-time state to off.
	sc.shared.Store(initial != state.ShareMeshNotShared)
	return sc
}

// IsShared is the lock-free read consumed on the inference probe tick
// and on every peer-overlay request. A suspended agent reads as
// not-shared without its persisted choice having changed.
func (sc *shareController) IsShared() bool { return sc.shared.Load() && !sc.suspended.Load() }

// IsShareDenied is the negation alias used by middleware that names
// gates after the rejected state (IsPaused, IsInferenceDisabled,
// IsShareDenied). Returning true means "deny the request".
func (sc *shareController) IsShareDenied() bool { return !sc.IsShared() }

// IsSuspended reports the live-only override, so status can distinguish
// "the operator turned sharing off" from "the tray is not running".
func (sc *shareController) IsSuspended() bool { return sc.suspended.Load() }

// Share / Unshare satisfy management.ShareController. Both are explicit
// operator actions, so they also clear any suspension — otherwise a
// stale latch could swallow the very toggle the operator just used.
func (sc *shareController) Share(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(false)
	return sc.transition(state.ShareMeshShared)
}

func (sc *shareController) Unshare(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(false)
	return sc.transition(state.ShareMeshNotShared)
}

// Suspend withholds sharing for this session only. Nothing is persisted:
// the next daemon start, or the tray's next Unsuspend, restores whatever
// the operator actually chose.
func (sc *shareController) Suspend(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(true)
	return nil
}

// Unsuspend lifts the session override. It does NOT turn sharing on — if
// the operator persisted not_shared, it stays off.
func (sc *shareController) Unsuspend(ctx context.Context) error {
	_ = ctx
	sc.setSuspended(false)
	return nil
}

func (sc *shareController) setSuspended(v bool) {
	if sc.suspended.Swap(v) == v {
		return
	}
	if sc.logger != nil {
		sc.logger.Info("share suspension change", "suspended", v, "persisted_share", sc.shared.Load())
	}
}

// State reports both the in-memory live value and the persisted
// operator intent. They differ briefly while a transition is being
// applied, while sharing is suspended for the session, or persistently
// when the operator edits desired-share by hand and the daemon hasn't
// been signalled yet.
func (sc *shareController) State() (current, desired state.ShareMeshState) {
	chosen := state.ShareMeshNotShared
	if sc.shared.Load() {
		chosen = state.ShareMeshShared
	}
	// current is what peers actually get right now; desired stays the
	// operator's choice, so a session suspension never reads as "the
	// operator turned sharing off".
	current = chosen
	if sc.suspended.Load() {
		current = state.ShareMeshNotShared
	}
	desired = chosen
	if d, err := state.ReadDesiredShareMesh(sc.stateDir); err == nil && d != "" {
		desired = d
	}
	return
}

func (sc *shareController) transition(target state.ShareMeshState) error {
	if sc.logger != nil {
		sc.logger.Debug("share transition requested",
			"from_shared", sc.shared.Load(), "target", string(target))
	}
	if err := state.WriteDesiredShareMesh(sc.stateDir, target); err != nil {
		return fmt.Errorf("persist desired-share: %w", err)
	}
	sc.shared.Store(target == state.ShareMeshShared)
	if sc.logger != nil {
		sc.logger.Info("share controller state change", "state", string(target))
	}
	return nil
}
