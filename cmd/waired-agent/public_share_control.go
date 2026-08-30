package main

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// publicShareController holds whether this agent is currently serving
// inference to foreign devices holding a Public Share grant (public
// share spec §4.1, §8).
//
// The setting is the control plane's (waired#1297, owner ruling
// 2026-08-30). Nothing here writes it and nothing here pushes it: the
// value arrives on the signed map's Self entry and is adopted, which is
// what "the CP is authoritative" (spec §5.1) always meant and now is
// without qualification. The 60-second pending window and the
// echo-true latch that used to guard a local writer went with it — a
// device that never asserts a value has nothing to protect from the
// echo.
//
// Two things are still local, and both are about stopping rather than
// choosing:
//
//   - the kill switch's abort (onDisable → AbortPublicInFlight), fired
//     whenever serving stops, so §8.3 step 1 waits on nothing;
//   - the boot default, which is OFF. Serving strangers stays strictly
//     opt-in, so a computer that comes up before it has heard from the
//     control plane serves nobody rather than acting on a remembered
//     yes. Nothing is persisted for the same reason.
type publicShareController struct {
	logger *slog.Logger

	// public holds the current live state; lock-free for the
	// per-request publicShareGate read.
	public atomic.Bool

	// maxClients is the guest ceiling the control plane last sent, for
	// the status surface. 0 means it has not sent one.
	maxClients atomic.Int32

	// mu serializes adoption so onDisable fires exactly once per real
	// transition.
	mu        sync.Mutex
	onDisable func()
}

func newPublicShareController(logger *slog.Logger) *publicShareController {
	return &publicShareController{logger: logger}
}

// IsPublic is the lock-free read of the live serving state.
func (pc *publicShareController) IsPublic() bool { return pc.public.Load() }

// IsPublicShareDenied is the negation alias consumed by the inference
// server's publicShareGate (gates name themselves after the rejected
// state, like IsShareDenied).
func (pc *publicShareController) IsPublicShareDenied() bool { return !pc.public.Load() }

// MaxClients reports the guest ceiling the control plane last sent; 0
// means it has not sent one and its own default applies.
func (pc *publicShareController) MaxClients() int { return int(pc.maxClients.Load()) }

// SetOnDisable registers the kill-switch hook fired on every transition
// to OFF. Called once during wiring, before any map frame can arrive.
func (pc *publicShareController) SetOnDisable(fn func()) { pc.onDisable = fn }

// StopServing is the hard kill's half: the machine has stopped lending
// itself out, so whatever guests are running is cut.
//
// It does NOT clear the setting. The setting belongs to the control
// plane, and a computer that comes back must find it where the owner
// left it — clearing it here would leave public serving off until the
// console happened to change its mind, which for a value that did not
// change is never. The gate composes the two instead (see
// IsPublicShareDenied's caller in main.go).
func (pc *publicShareController) StopServing() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if !pc.public.Load() {
		return
	}
	if pc.onDisable != nil {
		pc.onDisable()
	}
	if pc.logger != nil {
		pc.logger.Info("guests cut off: this computer stopped lending itself out")
	}
}

// ReconcileRemote adopts the control plane's state from a signed map
// frame. Called on every frame; a repeat is a no-op.
//
// TryLock rather than Lock, kept from the version that had operator
// transitions to race with: it keeps the map stream from blocking, and
// a skipped frame is re-delivered by the next one.
func (pc *publicShareController) ReconcileRemote(enabled bool, maxClients int) {
	if maxClients >= 0 {
		pc.maxClients.Store(int32(maxClients))
	}
	if enabled == pc.public.Load() {
		return
	}
	if !pc.mu.TryLock() {
		return
	}
	defer pc.mu.Unlock()
	if enabled == pc.public.Load() { // re-check under mu
		return
	}
	pc.public.Store(enabled)
	if !enabled && pc.onDisable != nil {
		pc.onDisable()
	}
	if pc.logger != nil {
		pc.logger.Info("adopted public sharing from the network map", "enabled", enabled)
	}
}

// State reports the live value in the sharing vocabulary the management
// API speaks, for the read-only status surface.
func (pc *publicShareController) State() state.SharingState {
	if pc.public.Load() {
		return state.SharingOn
	}
	return state.SharingOff
}
