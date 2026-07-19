package main

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// publicShareController owns the in-memory live "is this agent
// currently serving inference to foreign public-grant consumers?"
// flag plus persistence of the operator's intent (public share spec
// §4.1, §8.3). Mirrors shareController — same atomic.Bool hot path,
// same desired-state-file persistence — with two deliberate
// differences:
//
//   - the default is OFF: only an explicit persisted "public" choice
//     enables serving strangers, and an unreadable/absent state file
//     stays OFF (strictly opt-in);
//   - the OFF transition fires onDisable (wired to the inference
//     server's AbortPublicInFlight) so in-flight public streams are
//     terminated immediately — the §8.3 kill switch step 1. The CP
//     notify / grant-revoke half is C2 (waired#825).
//
// Toggle surfaces (management routes, CLI, Tray) also land with C2;
// until then the controller is driven by the persisted state at boot.
type publicShareController struct {
	stateDir string
	logger   *slog.Logger

	// public holds the current live state; lock-free for the
	// per-request publicShareGate read.
	public atomic.Bool

	// onDisable, when non-nil, runs after every transition to OFF.
	onDisable func()
}

// newPublicShareController builds the controller from the persisted
// desired state. Only an explicit PublicShareOn enables serving.
func newPublicShareController(stateDir string, initial state.PublicShareState, logger *slog.Logger) *publicShareController {
	pc := &publicShareController{
		stateDir: stateDir,
		logger:   logger,
	}
	pc.public.Store(initial == state.PublicShareOn)
	return pc
}

// IsPublic is the lock-free read of the live serving state.
func (pc *publicShareController) IsPublic() bool { return pc.public.Load() }

// IsPublicShareDenied is the negation alias consumed by the inference
// server's publicShareGate (gates name themselves after the rejected
// state, like IsShareDenied).
func (pc *publicShareController) IsPublicShareDenied() bool { return !pc.public.Load() }

// SetOnDisable registers the kill-switch hook fired on every OFF
// transition. Called once during wiring, before the management
// surfaces (C2) can drive transitions.
func (pc *publicShareController) SetOnDisable(fn func()) { pc.onDisable = fn }

// Enable / Disable persist the operator's choice and flip the live
// flag. Disable additionally fires the kill-switch hook.
func (pc *publicShareController) Enable() error {
	return pc.transition(state.PublicShareOn)
}

func (pc *publicShareController) Disable() error {
	return pc.transition(state.PublicShareOff)
}

// State reports both the live value and the persisted operator intent.
func (pc *publicShareController) State() (current, desired state.PublicShareState) {
	if pc.public.Load() {
		current = state.PublicShareOn
	} else {
		current = state.PublicShareOff
	}
	desired = current
	if d, err := state.ReadDesiredPublicShare(pc.stateDir); err == nil && d != "" {
		desired = d
	}
	return
}

func (pc *publicShareController) transition(target state.PublicShareState) error {
	if err := state.WriteDesiredPublicShare(pc.stateDir, target); err != nil {
		return fmt.Errorf("persist desired-public-share: %w", err)
	}
	pc.public.Store(target == state.PublicShareOn)
	if pc.logger != nil {
		pc.logger.Info("public share controller state change", "state", string(target))
	}
	if target == state.PublicShareOff && pc.onDisable != nil {
		pc.onDisable()
	}
	return nil
}
