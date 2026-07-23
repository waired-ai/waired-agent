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
// and on every peer-overlay request.
func (sc *shareController) IsShared() bool { return sc.shared.Load() }

// IsShareDenied is the negation alias used by middleware that names
// gates after the rejected state (IsPaused, IsInferenceDisabled,
// IsShareDenied). Returning true means "deny the request".
func (sc *shareController) IsShareDenied() bool { return !sc.shared.Load() }

// Share / Unshare satisfy management.ShareController.
func (sc *shareController) Share(ctx context.Context) error {
	_ = ctx
	return sc.transition(state.ShareMeshShared)
}

func (sc *shareController) Unshare(ctx context.Context) error {
	_ = ctx
	return sc.transition(state.ShareMeshNotShared)
}

// State reports both the in-memory live value and the persisted
// operator intent. They differ briefly while a transition is being
// applied, or persistently when the operator edits desired-share by
// hand and the daemon hasn't been signalled yet.
func (sc *shareController) State() (current, desired state.ShareMeshState) {
	if sc.shared.Load() {
		current = state.ShareMeshShared
	} else {
		current = state.ShareMeshNotShared
	}
	desired = current
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
