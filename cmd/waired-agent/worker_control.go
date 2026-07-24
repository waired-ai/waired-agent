package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// workerController owns the live "where do this agent's outbound
// inference requests route?" preference plus persistence of the
// operator's choice. It implements management.WorkerController and
// feeds the Selector hot path on every request: SelectK calls Routing()
// once per Select to decide whether the locality filter follows the
// auto / local-only / peer-preferred / pinned branch.
//
// Mirrors shareController in structure (transition + State() reporting
// + initial-from-caller pattern) so future maintenance edits can be
// applied uniformly. The notable difference is the in-memory field
// type: routing carries (mode, pinned_peer_device_id) which has to
// flip atomically together, so the controller uses atomic.Pointer
// instead of atomic.Bool.
type workerController struct {
	stateDir string
	logger   *slog.Logger

	// pref holds the current live RoutingPreference. atomic.Pointer
	// keeps the Selector hot path lock-free; readers Load() once per
	// SelectK so a concurrent transition cannot tear the (mode, peer)
	// pair (= what an atomic.Bool plus a separate string field would
	// expose if the agent grew that variant first).
	pref atomic.Pointer[state.RoutingPreference]

	// ring is the optional observability sink. transition() emits a
	// KindRoutingModeChange event on every successful state flip so
	// the tray's "Recent activity" submenu and the audit log share a
	// single source of truth. nil disables emission (kept optional so
	// tests can construct the controller without dragging in the
	// Phase 9 ring).
	ring *observability.Ring
}

// newWorkerController builds the controller with the initial decision
// resolved by the caller (typically: agentconfig.Routing.AsPreference()
// overlaid by state.ReadDesiredWorker when the operator has touched
// the toggle).
func newWorkerController(stateDir string, initial state.RoutingPreference, logger *slog.Logger) *workerController {
	wc := &workerController{
		stateDir: stateDir,
		logger:   logger,
	}
	normal := normalizeRouting(initial)
	wc.pref.Store(&normal)
	return wc
}

// Routing is the lock-free read consumed on every Selector pass. The
// returned value is a copy — callers must not mutate it.
func (wc *workerController) Routing() state.RoutingPreference {
	p := wc.pref.Load()
	if p == nil {
		return state.RoutingPreference{Mode: state.RoutingModeAuto}
	}
	return *p
}

// SetMode flips to a non-pinned mode (auto / local-only / peer-preferred)
// and clears any pinned peer. Pinned mode goes through SetPin instead so
// the caller cannot forget to supply a peer device ID.
func (wc *workerController) SetMode(ctx context.Context, mode state.RoutingMode) error {
	_ = ctx
	switch mode {
	case state.RoutingModeAuto, state.RoutingModeLocalOnly, state.RoutingModePeerPreferred:
	case state.RoutingModePinned:
		return fmt.Errorf("worker controller: SetMode(%q) requires a peer device ID — call SetPin instead", mode)
	case "":
		mode = state.RoutingModeAuto
	default:
		return fmt.Errorf("worker controller: unknown mode %q", mode)
	}
	if wc.logger != nil {
		wc.logger.Debug("worker set mode", "mode", string(mode))
	}
	return wc.transition(state.RoutingPreference{Mode: mode})
}

// SetPin flips to RoutingModePinned with the given peer device ID.
// Empty peerDeviceID is rejected so a misclicked tray entry cannot
// pin to "nothing".
func (wc *workerController) SetPin(ctx context.Context, peerDeviceID string) error {
	_ = ctx
	if peerDeviceID == "" {
		return fmt.Errorf("worker controller: SetPin requires a non-empty peer device ID")
	}
	if wc.logger != nil {
		wc.logger.Debug("worker set pin", "peer_device_id", peerDeviceID)
	}
	return wc.transition(state.RoutingPreference{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: peerDeviceID,
	})
}

// Clear is shorthand for SetMode(auto). Exists so the tray "(clear pin)"
// click and the CLI `waired worker set --mode=auto` go through the same
// intention-named call path, distinct from "the operator hasn't decided
// yet" (= empty desired-worker file).
func (wc *workerController) Clear(ctx context.Context) error {
	return wc.SetMode(ctx, state.RoutingModeAuto)
}

// State reports both the in-memory live value and the persisted
// operator intent. They differ briefly while a transition is being
// applied, or persistently when the operator hand-edits desired-worker
// and the daemon hasn't been signalled.
func (wc *workerController) State() (current, desired state.RoutingPreference) {
	current = wc.Routing()
	desired = current
	if d, err := state.ReadDesiredWorker(wc.stateDir); err == nil && !d.IsZero() {
		desired = d
	}
	return
}

func (wc *workerController) transition(target state.RoutingPreference) error {
	target = normalizeRouting(target)
	if err := state.WriteDesiredWorker(wc.stateDir, target); err != nil {
		return fmt.Errorf("persist desired-worker: %w", err)
	}
	prev := wc.Routing()
	wc.pref.Store(&target)
	if wc.logger != nil {
		wc.logger.Info("worker controller state change",
			"from_mode", prev.Mode,
			"from_pin", prev.PinnedPeerDeviceID,
			"to_mode", target.Mode,
			"to_pin", target.PinnedPeerDeviceID,
		)
	}
	if wc.ring != nil && prev != target {
		wc.ring.Append(observability.Event{
			Kind: observability.KindRoutingModeChange,
			RoutingModeChange: &observability.RoutingModeChangeEvent{
				From:               string(prev.Mode),
				To:                 string(target.Mode),
				PinnedPeerDeviceID: target.PinnedPeerDeviceID,
			},
		})
	}
	return nil
}

// WithObservability wires the optional event ring so transitions
// emit KindRoutingModeChange events. Returns the receiver for
// chaining; passing nil unwires emission.
func (wc *workerController) WithObservability(r *observability.Ring) *workerController {
	wc.ring = r
	return wc
}

// normalizeRouting collapses the empty-mode form into RoutingModeAuto
// so downstream consumers (the Selector, the management API) see a
// single canonical value.
func normalizeRouting(p state.RoutingPreference) state.RoutingPreference {
	if p.Mode == "" {
		p.Mode = state.RoutingModeAuto
	}
	if p.Mode != state.RoutingModePinned {
		p.PinnedPeerDeviceID = ""
	}
	return p
}
