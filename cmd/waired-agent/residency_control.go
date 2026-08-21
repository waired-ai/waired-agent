package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// residencyApplier is the optional capability an inference provider
// offers when it can change the live residency setting. Type-asserted
// off the provider rather than added to management.InferenceProvider,
// for the same reason as ModelUnloader: one optional capability on one
// route, against an interface with three implementations plus fakes.
type residencyApplier interface {
	CurrentResidency() (time.Duration, bool)
	ApplyResidency(ctx context.Context, idle time.Duration) (management.ResidencyEffect, error)
	// ResidencySupported reports whether the SERVING engine has a residency
	// axis at all (waired-ai/waired-agent#943).
	ResidencySupported() bool
}

// residencyController implements management.ResidencyController. It owns
// both halves of a residency change (waired-agent#861): the live engine
// setting, so it lands on the model that is loaded right now, and
// agent.json, so it survives a restart.
type residencyController struct {
	// applierFn resolves the live engine, or nil when there is none. A
	// function rather than the switchboard itself so the seam sits BELOW
	// the behaviour under test: the ordering this type exists to get
	// right (live first, persist second, both reported) is otherwise only
	// reachable through an enrolled session.
	applierFn func() residencyApplier
	jsonPath  string
	// stateDir is where the local-choice record lives. Empty disables the
	// record entirely, which reads as "no claim" everywhere downstream —
	// the treatment desiredResidency already gives an absent state dir.
	stateDir string
	// now is the clock the record is stamped with, injectable for the
	// same reason desiredResidency injects one.
	now func() time.Time

	mu sync.Mutex // serializes the read-modify-write of agent.json
}

func newResidencyController(sb *switchboard, jsonPath, stateDir string) *residencyController {
	return &residencyController{jsonPath: jsonPath, stateDir: stateDir, now: time.Now, applierFn: func() residencyApplier {
		if sb == nil {
			return nil
		}
		s := sb.current()
		if s == nil {
			return nil
		}
		a, ok := s.infProvider.(residencyApplier)
		if !ok {
			return nil
		}
		return a
	}}
}

func (c *residencyController) applier() residencyApplier {
	if c.applierFn == nil {
		return nil
	}
	return c.applierFn()
}

// Residency prefers the live engine's value and falls back to the
// persisted one. The two agree in the normal case; they differ exactly
// when a write reached agent.json and the engine has not been through it
// yet, and reporting the live value keeps the answer to "what will
// happen to my model" true.
func (c *residencyController) Residency(context.Context) (time.Duration, error) {
	if a := c.applier(); a != nil {
		if d, ok := a.CurrentResidency(); ok {
			return d, nil
		}
	}
	return c.persisted()
}

func (c *residencyController) persisted() (time.Duration, error) {
	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(c.jsonPath); err != nil {
		return 0, err
	}
	return cfg.Inference.IdleTimeout.Duration(), nil
}

// SetResidency applies live first, then persists — the order
// logController uses, and for the same reason: a persistence failure
// must not cost the operator the change they asked for, and it has to be
// reported so they know it will not outlive a restart.
//
// A negative value is normalized to zero rather than rejected. Both mean
// "hold indefinitely" to ResolveKeepAlive, and storing the one the
// surfaces render keeps agent.json readable.
// SetResidency is the LOCAL entry point: `waired inference residency` and
// the app's preset rows, both of which arrive through the loopback
// management API. It records that a person here chose, which is what lets
// the control plane realign its own instruction onto this machine
// (waired#1232).
func (c *residencyController) SetResidency(ctx context.Context, idle time.Duration) (time.Duration, management.ResidencyEffect, error) {
	return c.setResidency(ctx, idle, true)
}

// SetResidencyFromControlPlane is the same write WITHOUT the provenance
// record. A value the desired-state reconciler applied is the instruction
// arriving, not an answer to it: recording it would let the control plane
// read its own echo as local intent and realign onto the value it had
// just sent, which is a loop rather than a convergence.
//
// A separate method rather than a flag on the interface: the management
// API's ResidencyController is implemented by fakes in three packages,
// and the distinction that matters here is WHICH CALLER, which a method
// name states and a parameter buries.
func (c *residencyController) SetResidencyFromControlPlane(ctx context.Context, idle time.Duration) (time.Duration, management.ResidencyEffect, error) {
	return c.setResidency(ctx, idle, false)
}

func (c *residencyController) setResidency(ctx context.Context, idle time.Duration, local bool) (time.Duration, management.ResidencyEffect, error) {
	if idle < 0 {
		idle = 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Nothing to do, and nothing to record. The no-op path is exactly
	// where the control plane's realignment echoes this machine's own
	// value back at it, and stamping a local-choice time there would have
	// the realignment chase its own tail.
	if c.alreadyInForce(idle) {
		return idle, management.ResidencyEffectLive, nil
	}

	var (
		liveErr error
		effect  management.ResidencyEffect
	)
	if a := c.applier(); a != nil {
		effect, liveErr = a.ApplyResidency(ctx, idle)
	} else {
		// No engine on this host: the value is stored and will be read by
		// whatever engine arrives. Reported as on-engine-start rather than
		// live for the same reason as the parked case.
		effect = management.ResidencyEffectOnEngineStart
	}
	slog.Info("model residency changed via management API",
		"idle_timeout", idle.String(), "holds_indefinitely", idle == 0, "effect", string(effect))

	if err := c.persist(idle); err != nil {
		return idle, effect, fmt.Errorf("residency set to %s (live) but persisting to agent.json failed: %w", idle, err)
	}
	// After the persist, because the record dates a SETTING and the
	// persist is where the setting becomes one. A live failure does not
	// un-choose it — the value governs from the next engine start, and
	// the person still chose.
	c.recordLocalChoice(local, idle)
	if liveErr != nil {
		// Persisted, so the value is in force from the next engine start.
		// Surfacing the live failure tells the operator the model loaded
		// right now is still on the old setting.
		return idle, management.ResidencyEffectOnEngineStart,
			fmt.Errorf("residency saved as %s but not applied to the running engine: %w", idle, liveErr)
	}
	return idle, effect, nil
}

// recordLocalChoice stamps the provenance the control plane orders
// against. A failure is logged and swallowed: the operator's setting has
// already landed, and losing the write costs a realignment that the next
// local change will make available again — refusing the whole call would
// cost them the setting itself.
func (c *residencyController) recordLocalChoice(local bool, idle time.Duration) {
	if !local || c.stateDir == "" {
		return
	}
	rec := state.LocalResidencyChoice{
		ChosenAt: c.now().UTC().Format(time.RFC3339Nano),
		Value:    idle.String(),
	}
	if err := state.WriteLocalResidencyChoice(c.stateDir, rec); err != nil {
		slog.Warn("could not record who chose this model residency; the control plane will keep its own instruction",
			"value", rec.Value, "err", err)
	}
}

// LocalChoiceAt reports when a person here last set residency, formatted
// for signer.InferenceState's field of the same name. Read live rather
// than cached: the answer arrives through the loopback management API at
// any time, and the control plane's use for it is an ordering against its
// own instruction, so a stale reading is worse than none.
//
// "" whenever there is no claim to make — no state dir, no file, or a
// record this build cannot parse.
func (c *residencyController) LocalChoiceAt() string {
	if c == nil || c.stateDir == "" {
		return ""
	}
	rec, err := state.ReadLocalResidencyChoice(c.stateDir)
	if err != nil {
		return ""
	}
	return rec.ChosenAt
}

// alreadyInForce reports whether BOTH halves of the setting already hold the
// requested value, in which case there is nothing to do.
//
// Not an optimisation. ApplyResidency re-spawns the engine when nothing is
// resident — that is the only way a new value can govern the next load, since
// the engine reads its keep-alive once at spawn — so re-applying a value that
// is already in force bounces a healthy engine for no gain. Two writers make
// that ordinary rather than rare: an operator repeating a command, and the
// control plane echoing a host's own value back at it as a desired state.
//
// BOTH halves, deliberately. Short-circuiting on the persisted value alone
// would skip the live apply during the window agent.json is ahead of the
// engine; on the live value alone it would skip the write that makes the
// setting outlive a restart — which is precisely the divergence
// residencyController.Residency exists to describe.
func (c *residencyController) alreadyInForce(idle time.Duration) bool {
	a := c.applier()
	if a == nil {
		return false
	}
	live, ok := a.CurrentResidency()
	if !ok || live != idle {
		return false
	}
	persisted, err := c.persisted()
	return err == nil && persisted == idle
}

// persist writes inference.idle_timeout back to agent.json, preserving
// every other field (read-modify-write via Defaults()+MergeJSON, then
// Save) — the logController.persist shape.
func (c *residencyController) persist(idle time.Duration) error {
	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(c.jsonPath); err != nil {
		return err
	}
	cfg.Inference.IdleTimeout = agentconfig.NewDuration(idle)
	return cfg.Save(c.jsonPath)
}

// ResidencySupported answers for the engine this host serves with, and
// defaults to true when there is no live provider to ask: an unattached
// daemon is not a claim that the axis is absent, and a surface that hid the
// setting on that basis would hide it on every host that has not enrolled.
func (c *residencyController) ResidencySupported() bool {
	if a := c.applier(); a != nil {
		return a.ResidencySupported()
	}
	return true
}

var _ management.ResidencyController = (*residencyController)(nil)
