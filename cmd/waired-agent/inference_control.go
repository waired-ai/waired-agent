package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// inferenceController owns the in-memory current-inference flag plus
// persistence of the operator's enable/disable intent. It implements
// management.InferenceController and feeds router.Inputs.LocalServingOff
// (via the provider's baseRouterInputs) and the overlay listener's
// inference gate.
//
// Mirrors pauseManager but for the LLM gateway axis: pause/resume gates
// the WireGuard tunnel reachability semantics; this controller says
// whether this device runs models ITSELF. It is deliberately not a
// gateway-wide gate any more: as one it 503'd every loopback request
// before routing, so a node with no engine could not borrow a peer's
// (waired-agent#829).
type inferenceController struct {
	stateDir string
	logger   *slog.Logger

	// disabled holds the current live state (true = disabled). atomic.Bool
	// keeps the gateway hot-path lock-free.
	disabled atomic.Bool

	// onEnable, when set, is called after a transition to enabled has
	// been persisted. It is what makes the opt-in do something: the
	// engine and the model pre-pull stand down while inference is off
	// (#465), so turning it on has to ask for them. Set by run() once
	// the inference subsystem exists — nil before that, and on a daemon
	// started with --disable-inference, where there is nothing to ask.
	onEnable func()
}

func newInferenceController(stateDir string, initial state.InferenceState, logger *slog.Logger) *inferenceController {
	ic := &inferenceController{
		stateDir: stateDir,
		logger:   logger,
	}
	ic.disabled.Store(initial == state.InferenceDisabled)
	return ic
}

// IsDisabled is the function the gateway middleware consumes.
func (ic *inferenceController) IsDisabled() bool { return ic.disabled.Load() }

// Enable / Disable satisfy management.InferenceController.
func (ic *inferenceController) Enable(ctx context.Context) error {
	return ic.transition(state.InferenceEnabled)
}

func (ic *inferenceController) Disable(ctx context.Context) error {
	return ic.transition(state.InferenceDisabled)
}

func (ic *inferenceController) State() (current, desired state.InferenceState) {
	if ic.disabled.Load() {
		current = state.InferenceDisabled
	} else {
		current = state.InferenceEnabled
	}
	// An unset desired-inference file means nobody has moved this
	// toggle, so the live state IS the desired one — there is no
	// separate intent on disk to report.
	desired = current
	if d, err := state.ReadDesiredInferenceState(ic.stateDir); err == nil && d != "" {
		desired = d
	}
	return
}

func (ic *inferenceController) transition(target state.InferenceState) error {
	if ic.logger != nil {
		ic.logger.Debug("inference transition requested",
			"from_disabled", ic.disabled.Load(), "target", string(target))
	}
	if err := state.WriteDesiredInferenceState(ic.stateDir, target); err != nil {
		return fmt.Errorf("persist desired-inference: %w", err)
	}
	ic.disabled.Store(target == state.InferenceDisabled)
	if ic.logger != nil {
		ic.logger.Info("inference controller state change", "state", string(target))
	}
	if target == state.InferenceEnabled && ic.onEnable != nil {
		ic.onEnable()
	}
	return nil
}
