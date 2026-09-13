package main

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// teamShareController holds whether this agent is currently serving
// inference to its teammates' devices under Team Share grants (team
// share spec §6.2, waired#1374).
//
// It is publicShareController's twin, for the same reasons. The setting
// is the control plane's — the owner turns "Share with team" on and off
// in the console, and the value arrives on the signed map's Self entry
// as InferenceState.TeamShare — so nothing here writes or pushes it.
// What stays local is stopping:
//
//   - the kill switch's abort (onDisable → AbortTeamInFlight), fired on
//     every transition to OFF, so running team requests are cut the
//     moment the owner's switch reaches this computer;
//   - the boot default, OFF. A computer that comes up before it has
//     heard from the control plane serves no teammate rather than acting
//     on a remembered yes, and nothing is persisted.
//
// A team admin taking the node out of the team pool does not reach this
// controller: the control plane removes the team peers from the map and
// leaves TeamShare alone, so requests already running finish (spec §6.2,
// "admin フラグ OFF（graceful）").
type teamShareController struct {
	logger *slog.Logger

	// team holds the live state; lock-free for the per-request
	// teamShareGate read.
	team atomic.Bool

	// mu serializes adoption so onDisable fires exactly once per real
	// transition.
	mu        sync.Mutex
	onDisable func()
}

func newTeamShareController(logger *slog.Logger) *teamShareController {
	return &teamShareController{logger: logger}
}

// IsTeamShared is the lock-free read of the live serving state.
func (tc *teamShareController) IsTeamShared() bool { return tc.team.Load() }

// SetOnDisable registers the kill-switch hook fired on every transition
// to OFF. Called once during wiring, before any map frame can arrive.
func (tc *teamShareController) SetOnDisable(fn func()) { tc.onDisable = fn }

// StopServing is the hard kill's half: the machine has stopped lending
// itself out, so whatever teammates are running is cut. Like
// publicShareController.StopServing it does not clear the setting,
// which belongs to the control plane; the gate composes the two.
func (tc *teamShareController) StopServing() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if !tc.team.Load() {
		return
	}
	if tc.onDisable != nil {
		tc.onDisable()
	}
	if tc.logger != nil {
		tc.logger.Info("teammates cut off: this computer stopped lending itself out")
	}
}

// ReconcileRemote adopts the control plane's state from a signed map
// frame. Called on every frame; a repeat is a no-op. TryLock for the
// reason publicShareController.ReconcileRemote gives: the map stream
// must not block, and a skipped frame is re-delivered by the next one.
func (tc *teamShareController) ReconcileRemote(enabled bool) {
	if enabled == tc.team.Load() {
		return
	}
	if !tc.mu.TryLock() {
		return
	}
	defer tc.mu.Unlock()
	if enabled == tc.team.Load() { // re-check under mu
		return
	}
	tc.team.Store(enabled)
	if !enabled && tc.onDisable != nil {
		tc.onDisable()
	}
	if tc.logger != nil {
		tc.logger.Info("adopted team sharing from the network map", "enabled", enabled)
	}
}

// State reports the live value in the sharing vocabulary the management
// API speaks, for the read-only status surface.
func (tc *teamShareController) State() state.SharingState {
	if tc.team.Load() {
		return state.SharingOn
	}
	return state.SharingOff
}

// teamShareDenied composes the two independent reasons to refuse a
// teammate (waired#1297's shape for public guests): the owner's team
// switch in the console, and this computer's own hard kill. Folding them
// into one flag would leave team serving off after the machine came
// back, because the console's value did not change and nothing would
// re-assert it. A nil sharing controller (tests, inference-less wiring)
// leaves only the console's answer.
func teamShareDenied(team *teamShareController, sharing interface{ IsSharing() bool }) func() bool {
	if team == nil {
		return nil
	}
	if sharing == nil {
		return func() bool { return !team.IsTeamShared() }
	}
	return func() bool { return !sharing.IsSharing() || !team.IsTeamShared() }
}
