package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// pauseManager owns the in-memory current-phase flag plus persistence
// of the operator's pause/resume intent. It implements the
// management.PauseController interface and feeds gateway.Deps.IsPaused.
type pauseManager struct {
	stateDir string
	logger   *slog.Logger
	writer   *state.Writer

	// paused holds the current live phase (true = paused). atomic.Bool
	// keeps gateway.Deps.IsPaused lock-free on the request path.
	paused atomic.Bool

	// forcePhase, when non-empty, overrides the value returned by
	// Phase().current. Used only by --dev-force-phase to surface
	// transient phases ("starting", "stopping", "error") that the
	// daemon never holds long enough for the tray to capture in normal
	// operation. Production builds leave this empty; the flag is hidden
	// from --help.
	forcePhase state.Phase
}

func newPauseManager(stateDir string, writer *state.Writer, initial state.Phase, logger *slog.Logger) *pauseManager {
	pm := &pauseManager{
		stateDir: stateDir,
		writer:   writer,
		logger:   logger,
	}
	pm.paused.Store(initial == state.PhasePaused)
	return pm
}

// IsPaused is the function gateway.Deps consumes.
func (pm *pauseManager) IsPaused() bool { return pm.paused.Load() }

// Pause / Resume satisfy management.PauseController.
func (pm *pauseManager) Pause(ctx context.Context) error {
	return pm.transition(state.PhasePaused)
}

func (pm *pauseManager) Resume(ctx context.Context) error {
	return pm.transition(state.PhaseActive)
}

func (pm *pauseManager) Phase() (current, desired state.Phase) {
	if pm.forcePhase != "" {
		current = pm.forcePhase
	} else if pm.paused.Load() {
		current = state.PhasePaused
	} else {
		current = state.PhaseActive
	}
	desired = current
	if d, err := state.ReadDesiredPhase(pm.stateDir); err == nil {
		desired = d
	}
	return
}

func (pm *pauseManager) transition(target state.Phase) error {
	if err := state.WriteDesiredPhase(pm.stateDir, target); err != nil {
		return fmt.Errorf("persist desired-phase: %w", err)
	}
	pm.paused.Store(target == state.PhasePaused)
	if pm.writer != nil {
		if err := pm.writer.SetPhase(target); err != nil {
			return fmt.Errorf("update state file: %w", err)
		}
	}
	if pm.logger != nil {
		pm.logger.Info("pause manager phase change", "phase", string(target))
	}
	return nil
}

// runStateHeartbeat keeps <state>/runtime/state's `updated` field
// fresh so the shell rc precmd hook can detect daemon liveness within
// state.DefaultStaleAfter. The first tick fires immediately, then
// state.HeartbeatInterval afterwards. Stops when ctx is done.
func runStateHeartbeat(ctx context.Context, writer *state.Writer, logger *slog.Logger) {
	if writer == nil {
		return
	}
	if err := writer.Heartbeat(time.Now().UTC()); err != nil && logger != nil {
		logger.Warn("state heartbeat write failed", "err", err)
	}
	t := time.NewTicker(state.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := writer.Heartbeat(now.UTC()); err != nil && logger != nil {
				logger.Warn("state heartbeat write failed", "err", err)
			}
		}
	}
}
