package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// publicSharePushFn syncs the toggle to the CP self endpoint
// (controlclient.PushPublicShare bound to the session's device
// identity + machine key).
type publicSharePushFn func(ctx context.Context, enabled bool, maxClients int) (controlclient.PublicSharePushResult, error)

const (
	// publicSharePushTimeout bounds the synchronous CP push attempted
	// inside an operator transition; on expiry the change stays applied
	// locally and RunSync retries in the background.
	publicSharePushTimeout = 10 * time.Second

	// publicSharePendingWindow caps how long an unacknowledged local
	// transition suppresses adopting the CP's netmap echo. Past it the
	// CP (the authority, spec §5.1) wins again even if our push never
	// landed.
	publicSharePendingWindow = 60 * time.Second

	// publicShareRetryMin/Max bound RunSync's backoff between re-push
	// attempts of an unacknowledged transition.
	publicShareRetryMin = 5 * time.Second
	publicShareRetryMax = 60 * time.Second
)

// publicShareController owns the in-memory live "is this agent
// currently serving inference to foreign public-grant consumers?"
// flag plus persistence of the operator's intent (public share spec
// §4.1, §8.3). Mirrors shareController — same atomic.Bool hot path,
// same desired-state-file persistence — with deliberate differences:
//
//   - the default is OFF: only an explicit persisted "public" choice
//     enables serving strangers, and an unreadable/absent state file
//     stays OFF (strictly opt-in);
//   - the OFF transition fires onDisable (wired to the inference
//     server's AbortPublicInFlight) so in-flight public streams are
//     terminated immediately — the §8.3 kill switch step 1;
//   - operator transitions are synced to the CP (§4.1/§6, the §8.3
//     step 2: the CP revokes the device's grants on OFF), with a
//     background retry while unacknowledged;
//   - enabling public share first auto-enables in-account mesh share:
//     public eligibility depends on the CP receiving fresh inference
//     state, which only flows while mesh share is on (§4.1).
//
// Sync semantics (§5.1 "the CP is authoritative"): the CP's netmap
// echo (Self.InferenceState.PublicShare) is adopted on change in both
// directions, EXCEPT (a) while a local transition is pending
// acknowledgement (bounded by publicSharePendingWindow), and (b) the
// OFF direction is only adopted after this process has seen the CP
// assert true at least once — a CP that predates the B2 map fold
// (waired#820) reports false unconditionally, and adopting that would
// instantly revert every local enable.
type publicShareController struct {
	stateDir string
	logger   *slog.Logger

	// public holds the current live state; lock-free for the
	// per-request publicShareGate read.
	public atomic.Bool

	// onDisable, when non-nil, runs after every transition to OFF.
	onDisable func()

	// mu serializes operator transitions and remote adoption against
	// each other so onDisable fires exactly once per real transition.
	mu             sync.Mutex
	pusher         publicSharePushFn
	meshAutoEnable func(ctx context.Context) (changed bool, err error)

	// pushPending is set while the latest operator transition has not
	// been acknowledged by the CP; pendingSince (unix nano) bounds the
	// echo-suppression window.
	pushPending  atomic.Bool
	pendingSince atomic.Int64
	// lastMaxClients is re-sent by RunSync retries.
	lastMaxClients atomic.Int32
	// echoTrueSeen: the CP has asserted PublicShare=true in a netmap
	// echo this process lifetime, proving it folds the field (B2
	// deployed) — prerequisite for adopting a false echo as OFF.
	echoTrueSeen atomic.Bool

	// kick wakes RunSync immediately after a failed synchronous push.
	kick chan struct{}

	// now / retryMin / retryMax are test seams (real clock + the
	// publicShareRetry* bounds in production).
	now                func() time.Time
	retryMin, retryMax time.Duration
}

// newPublicShareController builds the controller from the persisted
// desired state. Only an explicit PublicShareOn enables serving.
func newPublicShareController(stateDir string, initial state.PublicShareState, logger *slog.Logger) *publicShareController {
	pc := &publicShareController{
		stateDir: stateDir,
		logger:   logger,
		kick:     make(chan struct{}, 1),
		now:      time.Now,
		retryMin: publicShareRetryMin,
		retryMax: publicShareRetryMax,
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
// surfaces can drive transitions.
func (pc *publicShareController) SetOnDisable(fn func()) { pc.onDisable = fn }

// SetPusher registers the CP sync function. Called once during session
// wiring; nil (never wired — tests, pre-enrollment) means transitions
// stay local-only and report CPSynced=false without retrying.
func (pc *publicShareController) SetPusher(fn publicSharePushFn) { pc.pusher = fn }

// SetMeshAutoEnable registers the "public ON requires mesh share ON"
// prerequisite hook. It returns whether it changed anything; an error
// aborts the public enable (spec §4.1 — a public node whose engine
// state cannot reach the CP would never be matched, so fail loudly).
func (pc *publicShareController) SetMeshAutoEnable(fn func(ctx context.Context) (bool, error)) {
	pc.meshAutoEnable = fn
}

// Synced reports whether the CP has acknowledged the latest operator
// transition (management.PublicShareController).
func (pc *publicShareController) Synced() bool { return !pc.pushPending.Load() }

// MaxClients reports the guest cap this agent last asked the control
// plane for; 0 means "never set on this device — the control plane's
// default applies" (management.PublicShareController). The status route
// serves it so `waired public status` and the web console can show the
// operator the limit they configured (waired#901 L6).
func (pc *publicShareController) MaxClients() int { return int(pc.lastMaxClients.Load()) }

// Enable turns public sharing on: mesh-share prerequisite first, then
// the local transition, then the CP push (management route surface).
// maxClients 0 keeps the CP's default cap.
func (pc *publicShareController) Enable(ctx context.Context, maxClients int) (management.PublicShareResult, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.logger != nil {
		pc.logger.Debug("public share enable requested", "max_clients", maxClients)
	}
	var res management.PublicShareResult
	if pc.meshAutoEnable != nil {
		changed, err := pc.meshAutoEnable(ctx)
		if err != nil {
			return res, fmt.Errorf("enable mesh share (required for public sharing): %w", err)
		}
		res.MeshShareEnabled = changed
	}
	if err := pc.transition(state.PublicShareOn); err != nil {
		return res, err
	}
	pc.lastMaxClients.Store(int32(maxClients))
	pc.syncLocked(ctx, true, maxClients, &res)
	return res, nil
}

// Disable turns public sharing off. The local transition — including
// the in-flight abort via onDisable — always happens first and never
// waits on the network (§8.3 step 1); the CP push (step 2, grant
// revocation) follows and is retried in the background on failure.
//
// A transition error here can only be the desired-state write failing,
// which happens AFTER the local stop. The CP push still runs — a
// revoked grant is what stops consumers reconnecting — and the error is
// returned alongside the result so the operator learns the choice may
// not survive a restart.
func (pc *publicShareController) Disable(ctx context.Context) (management.PublicShareResult, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.logger != nil {
		pc.logger.Debug("public share disable requested")
	}
	var res management.PublicShareResult
	err := pc.transition(state.PublicShareOff)
	pc.syncLocked(ctx, false, int(pc.lastMaxClients.Load()), &res)
	return res, err
}

// syncLocked attempts the synchronous CP push for an operator
// transition; callers hold mu. On failure the transition stays applied
// locally, pushPending is set and RunSync takes over.
func (pc *publicShareController) syncLocked(ctx context.Context, enabled bool, maxClients int, res *management.PublicShareResult) {
	if pc.pusher == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, publicSharePushTimeout)
	defer cancel()
	if pc.logger != nil {
		pc.logger.Debug("public share CP push", "enabled", enabled, "max_clients", maxClients)
	}
	out, err := pc.pusher(pctx, enabled, maxClients)
	if err != nil {
		pc.pushPending.Store(true)
		pc.pendingSince.Store(pc.now().UnixNano())
		select {
		case pc.kick <- struct{}{}:
		default:
		}
		if pc.logger != nil {
			pc.logger.Warn("public share CP sync failed; retrying in background",
				"enabled", enabled, "err", err)
		}
		return
	}
	pc.pushPending.Store(false)
	res.CPSynced = true
	res.MaxClients = out.MaxClients
	res.RevokedGrants = out.RevokedGrants
}

// ReconcileRemote adopts the CP's netmap echo of the toggle (§5.1: the
// CP is authoritative). Called on every netmap frame that carries a
// Self InferenceState; acts only on change. TryLock keeps the netmap
// stream loop from blocking behind an in-flight operator transition —
// a skipped frame is re-delivered by the next one.
func (pc *publicShareController) ReconcileRemote(enabled bool) {
	if enabled {
		pc.echoTrueSeen.Store(true)
	}
	if pc.pushPending.Load() {
		if pc.now().UnixNano()-pc.pendingSince.Load() < int64(publicSharePendingWindow) {
			if pc.logger != nil {
				pc.logger.Debug("public share: CP echo suppressed (local push pending)", "enabled", enabled)
			}
			return
		}
		// The CP never acknowledged our push inside the window; stop
		// suppressing and let its echo win again.
		pc.pushPending.Store(false)
	}
	if enabled == pc.public.Load() {
		return
	}
	if !enabled && !pc.echoTrueSeen.Load() {
		// A CP that predates the B2 map fold reports false
		// unconditionally — do not let it revert a local enable.
		if pc.logger != nil {
			pc.logger.Debug("public share: ignoring CP false echo (never observed CP true)")
		}
		return
	}
	if !pc.mu.TryLock() {
		return
	}
	defer pc.mu.Unlock()
	if enabled == pc.public.Load() { // re-check under mu
		return
	}
	target := state.PublicShareOff
	if enabled {
		target = state.PublicShareOn
	}
	if enabled && pc.meshAutoEnable != nil {
		if _, err := pc.meshAutoEnable(context.Background()); err != nil {
			if pc.logger != nil {
				pc.logger.Warn("adopting CP public share enable: mesh share auto-enable failed", "err", err)
			}
			return
		}
	}
	if err := pc.transition(target); err != nil {
		if pc.logger != nil {
			pc.logger.Warn("adopting CP public share state failed", "state", string(target), "err", err)
		}
		// An OFF transition has already stopped serving by this point
		// (only its persistence failed), so keep going and log the
		// adoption; an ON transition genuinely did not take effect.
		if target != state.PublicShareOff {
			return
		}
	}
	if pc.logger != nil {
		pc.logger.Info("adopted CP public share state from network map", "state", string(target))
	}
}

// RunSync is the background retry loop for unacknowledged transitions:
// it re-pushes the current desired state with bounded backoff until
// the CP acknowledges. Run as a session goroutine.
func (pc *publicShareController) RunSync(ctx context.Context) {
	backoff := pc.retryMin
	for {
		if !pc.pushPending.Load() {
			select {
			case <-ctx.Done():
				return
			case <-pc.kick:
				backoff = pc.retryMin
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > pc.retryMax {
			backoff = pc.retryMax
		}
		if !pc.pushPending.Load() || pc.pusher == nil {
			continue
		}
		if pc.logger != nil {
			pc.logger.Debug("public share CP sync retry",
				"enabled", pc.public.Load(), "backoff", backoff.String())
		}
		pctx, cancel := context.WithTimeout(ctx, publicSharePushTimeout)
		out, err := pc.pusher(pctx, pc.public.Load(), int(pc.lastMaxClients.Load()))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		pc.pushPending.Store(false)
		if pc.logger != nil {
			pc.logger.Info("public share CP sync recovered",
				"enabled", out.Enabled, "revoked_grants", out.RevokedGrants)
		}
	}
}

// State reports both the live value and the persisted operator intent
// (management.PublicShareController).
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

// transition applies one state change. The two directions order their
// steps differently on purpose:
//
//   - OFF (the kill switch): flip the live flag and abort in-flight
//     public requests FIRST, persist afterwards. Stopping the serving
//     of strangers is a live-state operation; making it wait on a
//     filesystem write means a full disk / read-only remount /
//     permission change leaves the agent serving with the gate open and
//     streams running (§8.3 step 1 explicitly does not wait on
//     anything). A persistence error is still returned — the operator
//     needs to know the choice may not survive a restart — but only
//     after the stop has taken effect.
//   - ON: persist first, as before. A failure there is fail-safe: the
//     agent simply does not start sharing.
func (pc *publicShareController) transition(target state.PublicShareState) error {
	off := target == state.PublicShareOff
	if off {
		pc.public.Store(false)
		if pc.onDisable != nil {
			pc.onDisable()
		}
	}
	if err := state.WriteDesiredPublicShare(pc.stateDir, target); err != nil {
		if off {
			// Serving already stopped; only restart-persistence is lost.
			// The message is operator-facing (CLI / management API), so
			// it leads with what is true right now.
			if pc.logger != nil {
				pc.logger.Warn("public sharing stopped but the setting could not be saved",
					"err", err)
			}
			return fmt.Errorf("public sharing has stopped, but the setting could not be saved "+
				"and may come back after a restart: %w", err)
		}
		return fmt.Errorf("persist desired-public-share: %w", err)
	}
	if !off {
		pc.public.Store(true)
	}
	if pc.logger != nil {
		pc.logger.Info("public share controller state change", "state", string(target))
	}
	return nil
}
