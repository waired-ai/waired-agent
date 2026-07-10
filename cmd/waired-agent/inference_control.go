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
// management.InferenceController and feeds gateway.Deps.IsInferenceDisabled.
//
// Mirrors pauseManager but for the LLM gateway axis: pause/resume gates
// the WireGuard tunnel reachability semantics; this controller gates
// whether the local LLM gateway accepts inference requests.
type inferenceController struct {
	stateDir string
	logger   *slog.Logger

	// disabled holds the current live state (true = disabled). atomic.Bool
	// keeps the gateway hot-path lock-free.
	disabled atomic.Bool
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
	desired = current
	if d, err := state.ReadDesiredInferenceState(ic.stateDir); err == nil {
		desired = d
	}
	return
}

func (ic *inferenceController) transition(target state.InferenceState) error {
	if err := state.WriteDesiredInferenceState(ic.stateDir, target); err != nil {
		return fmt.Errorf("persist desired-inference: %w", err)
	}
	ic.disabled.Store(target == state.InferenceDisabled)
	if ic.logger != nil {
		ic.logger.Info("inference controller state change", "state", string(target))
	}
	return nil
}
