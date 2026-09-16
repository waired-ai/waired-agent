package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Background Public Share grant acquirer/renewer (waired#821 second
// half, spec §6). Consumer side, "hold what you use" (waired#898): while
// the operator's public-use mode is on (consented, D1), acquire a grant
// only when the router actually wants a public candidate and holds none
// (the Demand signal), and keep it only while it carries traffic. This
// is spec §6's own model — acquire OFF the request hot path, triggered
// by the router observing it wants a public candidate but has no valid
// grant. An earlier revision instead pre-acquired K=3 on every periodic
// tick regardless of use, which left an idle consumer peered to three
// strangers' machines forever (§15-8 break); demand-driven acquisition
// plus last-used renew gating replaces it.
//
// The acquirer only manages grant LIFECYCLE. Routing consumes the
// netmap (PeerView.Grant, injected CP-side by waired#820); it reports
// which grant it routed through via the shared grantUsage tracker
// (Deps.Usage). No local grant store exists or is needed — the acquire
// response always returns the device's full active set, so state
// self-corrects across restarts.
const (
	publicGrantTTL  = 10 * time.Minute
	publicGrantTick = 75 * time.Second
	// publicGrantWant is how many grants a single demand acquires and the
	// ceiling the loop holds. K=1 (waired#898): a held grant is a live WG
	// peering to a stranger, so we hold exactly the one the router is
	// using. The CP still supports up to K=3 per device (spec §6); this is
	// the consumer's posture, not the CP ceiling.
	publicGrantWant = 1
	// publicGrantIdleTTL is the traffic-idle window past which a held grant
	// is no longer renewed and is allowed to lapse (spec §6 renew row: renew
	// only grants with traffic in the last IdleTTL window; §7.3 idle
	// teardown). Equal to the grant TTL: once a grant has gone a full TTL
	// without carrying a request, stopping its renewal lets the CP expire it
	// and the reconciler GC the WG peer.
	publicGrantIdleTTL  = publicGrantTTL
	publicGrantBatchMax = 16
	// publicGrantBackoff pauses acquire attempts after 403 not_eligible,
	// 429, or an empty candidate list (all "try later", none errors).
	publicGrantBackoff = 5 * time.Minute
	// publicGrantMapGrace: a freshly acquired grant may not appear in
	// the netmap until the epoch bump propagates — don't drop it as
	// map-absent before this.
	publicGrantMapGrace = 2 * time.Minute
	// publicGrantDemandMinInterval floors the gap between two
	// demand-driven acquire cycles (waired#827). Deliberately far below
	// publicGrantTick: a floor at the tick length would only ever admit
	// a demand in the sliver between the floor expiring and the jittered
	// tick firing, buying a couple of seconds and defeating the point.
	//
	// Measured against the last ACTUAL acquire attempt, not the last
	// loop cycle, so mode-off and read-error cycles cannot push the
	// window out. A demand rejected by this floor is simply dropped: the
	// router re-signals on the next request that wants a public
	// candidate, so nothing needs to be remembered.
	publicGrantDemandMinInterval = 15 * time.Second
)

// publicGrantAPI is the controlclient seam (fake in tests).
type publicGrantAPI interface {
	AcquirePublicGrants(ctx context.Context, req controlclient.AcquirePublicGrantsRequest) (controlclient.AcquirePublicGrantsResponse, error)
	RenewPublicGrants(ctx context.Context, grantIDs []string) (controlclient.RenewPublicGrantsResponse, error)
	ReleasePublicGrants(ctx context.Context, grantIDs []string) (controlclient.ReleasePublicGrantsResponse, error)
}

type publicGrantDeps struct {
	API publicGrantAPI
	// Mesh yields the latest netmap view; grants absent from it (past
	// the propagation grace) are not renewed — the map is the CP's own
	// projection of grant validity, so this kills the
	// "renew a revoked grant" failure mode.
	Mesh interface{ Snapshot() inferencemesh.Snapshot }
	// PublicUsePath / WarningVersion feed the D1 consent gate: the loop
	// is inert while EffectiveMode(WarningVersion) == off.
	PublicUsePath  string
	WarningVersion int
	Logger         *slog.Logger

	// Demand is the router's "a request wanted a public candidate and
	// there is no grant to use" signal (waired#827). Receive-only: the
	// sender owns the channel and uses a non-blocking send onto a
	// buffered-1 chan, so bursts coalesce and the routing hot path never
	// waits here. A wake runs one acquire cycle early instead of waiting
	// out the periodic tick, which is what keeps the first request after
	// consent from paying a full tick of cold start (spec §4.3).
	//
	// nil leaves the loop purely periodic.
	Demand <-chan struct{}

	// DemandWindow returns the largest context-window floor the demands
	// since its last call asked for, and forgets it: 200704 or 1048576 for
	// a Waired row, 0 for a request that is not one (waired-agent#1399).
	// The loop calls it once per admitted demand wake. nil reads as 0,
	// which is the acquirer this replaced.
	DemandWindow func() int

	// Ready is the agent's "this node's own engine just became reachable"
	// edge (waired-agent#806). Receive-only, buffered-1 with a
	// non-blocking send at the source, exactly like Demand.
	//
	// It does NOT acquire, and that is the whole design. It clears an
	// eligibility backoff and nothing else, so the next real demand can
	// be served immediately instead of waiting out a wall-clock timer for
	// a condition that has already resolved. No new outbound path is
	// created: acquisition still happens only on a Demand wake, still
	// under publicGrantDemandMinInterval, and a node nobody is asking
	// anything of still holds nothing.
	//
	// nil means no readiness signal is wired and the backoff runs its
	// full length, which is what every build before this did.
	Ready <-chan struct{}

	// Usage is the shared last-used tracker the router writes to on every
	// committed public route (waired#898). The loop reads LastUsed at renew
	// time to lapse traffic-idle grants, and calls Forget when it drops a
	// grant so the map does not grow unbounded. nil disables last-used
	// gating (every due grant is renewed — the pre-#898 behaviour) and
	// pruning; production and the gating tests wire it.
	Usage interface {
		LastUsed(grantID string) time.Time
		Forget(grantID string)
	}

	Tick time.Duration    // 0 → publicGrantTick
	Now  func() time.Time // nil → time.Now
}

// publicGrantDemandSignal is the router's demand signal with the window it
// asked for: a coalescing wake channel, exactly the buffered-1 non-blocking
// shape Demand always had, and the largest window floor seen since the
// acquirer last took it (waired-agent#1399).
//
// The largest, because the wake coalesces: a 200k and a 1M request in one
// burst make one wake, and a grant that holds 1M serves both rows.
type publicGrantDemandSignal struct {
	ch     chan struct{}
	mu     sync.Mutex
	window int
}

func newPublicGrantDemandSignal() *publicGrantDemandSignal {
	return &publicGrantDemandSignal{ch: make(chan struct{}, 1)}
}

// Notify records the window and wakes the acquirer without blocking. It is
// router.Inputs.OnPublicGrantDemand, called on the routing hot path.
func (d *publicGrantDemandSignal) Notify(minContextWindow int) {
	d.mu.Lock()
	if minContextWindow > d.window {
		d.window = minContextWindow
	}
	d.mu.Unlock()
	select {
	case d.ch <- struct{}{}:
	default:
	}
}

// C is the wake channel, publicGrantDeps.Demand.
func (d *publicGrantDemandSignal) C() <-chan struct{} { return d.ch }

// Take returns the largest window since the last Take and forgets it,
// publicGrantDeps.DemandWindow.
func (d *publicGrantDemandSignal) Take() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.window
	d.window = 0
	return w
}

// publicAcquireWindow is the min_context_window an acquire sends for a demand
// with floor window: 1048576 for a 1M demand, and nothing otherwise. A control
// plane from waired#1442 on holds an absent floor to 200704, and one from
// before rejects the field, so the 200k demand — the one every public turn
// makes — never risks a 400 (waired-agent#1399).
func publicAcquireWindow(window int) int {
	if window >= hostfit.ServingWindow1M {
		return hostfit.ServingWindow1M
	}
	return 0
}

// publicGrantWindows maps each public provider grant the map carries to the
// context window its provider declares (0 = none).
func publicGrantWindows(snap inferencemesh.Snapshot) map[string]int {
	out := map[string]int{}
	for _, p := range snap.Peers {
		if !inferencemesh.IsPublicGrant(p.Grant) || p.Grant.Role != signer.GrantRoleProvider || p.Grant.ID == "" {
			continue
		}
		w := 0
		if p.InferenceState != nil {
			w = p.InferenceState.ContextWindow
		}
		out[p.Grant.ID] = w
	}
	return out
}

type heldGrant struct {
	providerDeviceID string
	expiresAt        time.Time
	acquiredAt       time.Time
	nextRenewAt      time.Time
}

// runPublicGrantLoop drives the acquire/renew/release cycle until ctx
// ends; on shutdown it best-effort releases the held set on a fresh
// short-lived context (spec §6 release row).
func runPublicGrantLoop(ctx context.Context, deps publicGrantDeps) {
	tick := deps.Tick
	if tick <= 0 {
		tick = publicGrantTick
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	held := map[string]*heldGrant{}
	var backoffUntil time.Time
	// Why the loop is backing off, because the three reasons resolve
	// differently and only one of them resolves HERE (waired-agent#806).
	//
	// not_eligible is a refusal about OUR state — the reciprocity check
	// says this node is not offering capacity, which on a fresh install is
	// usually its own engine still coming up, and which the node itself
	// resolves seconds later. rate_limited is about the control plane's
	// load and an empty candidate list is about the fleet's; nothing local
	// makes either of them go away, so nothing local may shorten them.
	backoffIsEligibility := false
	// backOff records both together, so the reason can never be left
	// describing a previous wait.
	backOff := func(until time.Time, eligibility bool) {
		backoffUntil, backoffIsEligibility = until, eligibility
	}

	// forgetUsage prunes a grant's last-used record when the loop stops
	// tracking it, so the shared map can't grow across a long-lived
	// agent's churn of grants (waired#898). No-op when usage is unwired.
	forgetUsage := func(id string) {
		if deps.Usage != nil {
			deps.Usage.Forget(id)
		}
	}

	release := func(rctx context.Context, reason string) {
		if len(held) == 0 {
			return
		}
		ids := make([]string, 0, len(held))
		for id := range held {
			ids = append(ids, id)
		}
		for len(ids) > 0 {
			batch := ids
			if len(batch) > publicGrantBatchMax {
				batch = ids[:publicGrantBatchMax]
			}
			ids = ids[len(batch):]
			if _, err := deps.API.ReleasePublicGrants(rctx, batch); err != nil {
				logger.Warn("public grants: release failed", "reason", reason, "err", err)
				break // best-effort: the CP's idle TTL lapses them anyway
			}
		}
		logger.Info("public grants: released", "reason", reason, "count", len(held))
		for id := range held {
			forgetUsage(id)
		}
		held = map[string]*heldGrant{}
	}

	timer := time.NewTimer(jitterTick(tick))
	defer timer.Stop()
	// lastAcquireAt gates demand-driven cycles. Zero until the first
	// acquire attempt, so the very first demand — the one right after
	// the user consents — is always admitted.
	var lastAcquireAt time.Time
	for {
		// demandWake is per-iteration: acquisition runs only when THIS
		// wake came from the router's demand signal (waired#898). A
		// periodic tick still runs renew + map-GC + release, but never
		// acquires — so an idle consumer (mode on, no requests) holds
		// nothing. Re-declared each iteration, so it resets naturally.
		demandWake := false
		// demandWindow is the window floor THIS wake's demand asked for
		// (waired-agent#1399); 0 on any other wake.
		demandWindow := 0
		// The timer is re-armed per arm, not unconditionally after the
		// select: with more than one non-ctx arm, an unconditional
		// Reset on a timer that has NOT fired leaves its pending value
		// behind and the next wake is immediate.
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			release(rctx, "shutdown")
			cancel()
			return
		case <-timer.C:
			timer.Reset(jitterTick(tick))
		case <-deps.Demand:
			// Throttle only the demand path, and leave the timer
			// untouched: re-arming here would let a stream of requests
			// postpone the periodic cycle indefinitely.
			if now().Sub(lastAcquireAt) < publicGrantDemandMinInterval {
				continue
			}
			demandWake = true
			// Taken only on an admitted wake: a throttled one leaves the
			// window for the next.
			if deps.DemandWindow != nil {
				demandWindow = deps.DemandWindow()
			}
		case <-deps.Ready:
			// This node's engine became reachable (waired-agent#806).
			//
			// The eligibility check is reciprocity: to consume public
			// capacity you must be offering it, and a node whose engine
			// has not finished starting is not. On a fresh install that is
			// the ordinary case and it clears in seconds — but the
			// acquirer was asleep on a five-minute wall-clock timer that
			// the readiness change did not touch, so the very first
			// request that wanted public capacity could wait out the whole
			// interval on a condition that had gone ten seconds in.
			//
			// Clearing the wait, not acquiring. No request is outstanding
			// here — this is the node noticing something about ITSELF —
			// and acquiring on it would take a grant for a node nobody is
			// asking anything of, which is exactly what the demand-driven
			// shape (waired#898) exists to prevent. The next real demand
			// acquires, still under publicGrantDemandMinInterval, so this
			// adds no outbound path at all.
			//
			// The timer is left alone for the reason the Demand arm gives.
			if backoffIsEligibility && now().Before(backoffUntil) {
				logger.Info("public grants: this node became reachable; ending the eligibility backoff early")
				backOff(time.Time{}, false)
			}
			continue
		}

		pu, _, err := agentconfig.LoadPublicUse(deps.PublicUsePath)
		if err != nil {
			logger.Warn("public grants: read public_use", "err", err)
			continue
		}
		if pu.EffectiveMode(deps.WarningVersion) == agentconfig.PublicUseModeOff {
			release(ctx, "mode off")
			continue
		}

		// Netmap-presence check: drop held grants the CP no longer
		// projects (revoked/lapsed), with a propagation grace for
		// fresh ones.
		inMap := map[string]bool{}
		if deps.Mesh != nil {
			for _, p := range deps.Mesh.Snapshot().Peers {
				// Public grants only: this loop holds guest passes, and a
				// teammate's grant is not one this acquirer took or renews.
				if inferencemesh.IsPublicGrant(p.Grant) && p.Grant.Role == signer.GrantRoleProvider && p.Grant.ID != "" {
					inMap[p.Grant.ID] = true
				}
			}
		}
		tnow := now()
		for id, h := range held {
			if inMap[id] || tnow.Sub(h.acquiredAt) < publicGrantMapGrace {
				continue
			}
			logger.Info("public grants: dropping map-absent grant", "grant_id", id)
			delete(held, id)
			forgetUsage(id)
		}

		// Renew due grants (TTL/2 + jitter schedule set at admission), but
		// only those that carried traffic within the last IdleTTL window
		// (waired#898, spec §6 renew row). A grant with no recent use — or
		// none ever — is left out of `due`, so its renewal stops, the CP
		// expires it at its TTL, and the reconciler GCs the WG peering
		// (§7.3 idle teardown). It is NOT deleted from `held` here: if
		// traffic resumes before the CP expiry, the next cycle sees fresh
		// use and renews it again (robust to a paused-then-resumed
		// conversation); otherwise the map-absent drop above removes it
		// once the CP drops it from the netmap.
		var due []string
		for id, h := range held {
			if !tnow.After(h.nextRenewAt) {
				continue
			}
			if deps.Usage != nil && tnow.Sub(deps.Usage.LastUsed(id)) >= publicGrantIdleTTL {
				continue
			}
			due = append(due, id)
			if len(due) == publicGrantBatchMax {
				break
			}
		}
		if len(due) > 0 {
			res, err := deps.API.RenewPublicGrants(ctx, due)
			switch {
			case errors.Is(err, controlclient.ErrPublicShareNotEligible):
				// §7.2 mutuality lost — the CP renews none of these, so
				// they lapse at their TTL. Stop tracking them: kept in
				// `held` they stay due, and every tick re-sent the same
				// refused renew until the CP expired them
				// (waired-agent#1380). If eligibility comes back while
				// they are still active, the next acquire returns them in
				// the full active set and they are held again.
				logger.Warn("public grants: renew rejected (not eligible); letting the grants lapse and backing off",
					"grants", len(due))
				for _, id := range due {
					delete(held, id)
					forgetUsage(id)
				}
				backOff(tnow.Add(publicGrantBackoff), true)
			case err != nil:
				logger.Warn("public grants: renew failed", "err", err) // transport/5xx: next tick retries
			default:
				renewed := map[string]bool{}
				for _, id := range res.Renewed {
					renewed[id] = true
				}
				exp, _ := time.Parse(time.RFC3339, res.ExpiresAt)
				for _, id := range due {
					h := held[id]
					if h == nil {
						continue
					}
					if !renewed[id] {
						logger.Info("public grants: dropped by renew", "grant_id", id)
						delete(held, id)
						forgetUsage(id)
						continue
					}
					if !exp.IsZero() {
						h.expiresAt = exp
					} else {
						h.expiresAt = tnow.Add(publicGrantTTL)
					}
					h.nextRenewAt = renewAt(tnow, h.expiresAt)
				}
			}
		}

		// A demand with a window floor: let go of the held grants whose
		// provider declares less (waired-agent#1399). The router asked
		// because none of them could take the request, and holding one
		// keeps the acquirer from asking for a grant that could — before
		// this, len(held) >= want stopped it, and the row that refused the
		// provider kept refusing every turn until the grant lapsed. A grant
		// the map does not carry yet is not judged: its window is unknown,
		// and the map grace above exists for it.
		released := map[string]bool{} // provider device ids let go this cycle
		if demandWake && demandWindow > 0 && deps.Mesh != nil && len(held) > 0 {
			windows := publicGrantWindows(deps.Mesh.Snapshot())
			var below []string
			for id, h := range held {
				if w, ok := windows[id]; ok && w < demandWindow {
					below = append(below, id)
					released[h.providerDeviceID] = true
				}
			}
			if len(below) > 0 {
				if _, err := deps.API.ReleasePublicGrants(ctx, below); err != nil {
					// The grant is still dropped here: renew stops for it,
					// so the control plane lapses it at its TTL.
					logger.Warn("public grants: release of a grant below the window failed", "err", err)
				}
				logger.Info("public grants: released grants whose provider's context window is below the request's",
					"count", len(below), "window", demandWindow)
				for _, id := range below {
					delete(held, id)
					forgetUsage(id)
				}
			}
		}

		// Acquire is demand-driven (waired#898): only a router demand wake
		// acquires, and only up to K=publicGrantWant, out of backoff. A
		// periodic tick reaches here with demandWake false and stops,
		// having done just renew + map-GC.
		if !demandWake || len(held) >= publicGrantWant || tnow.Before(backoffUntil) {
			continue
		}
		consentVersion := 0
		if pu.Consent != nil {
			consentVersion = pu.Consent.WarningVersion
		}
		lastAcquireAt = tnow
		acquireReq := controlclient.AcquirePublicGrantsRequest{
			Class:            "",
			MinModelSize:     pu.MinModelSize,
			MinQualityTier:   pu.MinQualityTier,
			MinContextWindow: publicAcquireWindow(demandWindow),
			Want:             publicGrantWant, // K=1 (waired#898)
			ConsentVersion:   consentVersion,
		}
		res, err := deps.API.AcquirePublicGrants(ctx, acquireReq)
		if errors.Is(err, controlclient.ErrPublicShareBadRequest) && acquireReq.MinContextWindow > 0 {
			// A control plane that predates min_context_window decodes
			// with DisallowUnknownFields and refuses the whole request.
			// Ask once more without it: a grant it gives may not hold 1M,
			// and the next 1M demand lets go of it (waired-agent#1399).
			logger.Info("public grants: the control plane does not take a window floor; acquiring without it",
				"window", acquireReq.MinContextWindow)
			acquireReq.MinContextWindow = 0
			res, err = deps.API.AcquirePublicGrants(ctx, acquireReq)
		}
		switch {
		case errors.Is(err, controlclient.ErrPublicShareNotEligible),
			errors.Is(err, controlclient.ErrPublicShareRateLimited):
			logger.Info("public grants: acquire deferred", "err", err)
			// The two errors arrive at one arm but are not one condition.
			// not_eligible is about this node's own state and a local
			// readiness edge may cut it short; rate_limited is the control
			// plane asking for room, and only the clock ends it.
			backOff(tnow.Add(publicGrantBackoff),
				errors.Is(err, controlclient.ErrPublicShareNotEligible))
			continue
		case err != nil:
			logger.Warn("public grants: acquire failed", "err", err)
			continue
		}
		if len(res.Grants) == 0 {
			// Eligible but no candidates right now — not an error, no
			// warn spam; just don't hammer the CP. A fact about the
			// FLEET, so this node becoming readier changes nothing about
			// it and the wait runs its full length.
			backOff(tnow.Add(publicGrantBackoff), false)
			continue
		}
		// The response is the FULL active set: replace wholesale,
		// preserving acquiredAt/renew schedule for grants we knew.
		next := make(map[string]*heldGrant, len(res.Grants))
		var regranted []string
		for _, g := range res.Grants {
			// A provider let go of this cycle for its window, granted
			// again — a control plane that cannot filter on the window
			// had nothing better. Let go of it again and wait, rather than
			// hold a grant the request refuses or churn on it every
			// demand (waired-agent#1399).
			if released[g.ProviderDeviceID] {
				regranted = append(regranted, g.GrantID)
				continue
			}
			if prev, ok := held[g.GrantID]; ok {
				next[g.GrantID] = prev
				continue
			}
			exp, _ := time.Parse(time.RFC3339, g.ExpiresAt)
			if exp.IsZero() {
				exp = tnow.Add(publicGrantTTL)
			}
			next[g.GrantID] = &heldGrant{
				providerDeviceID: g.ProviderDeviceID,
				expiresAt:        exp,
				acquiredAt:       tnow,
				nextRenewAt:      renewAt(tnow, exp),
			}
			logger.Info("public grants: acquired",
				"grant_id", g.GrantID, "provider_pseudonym", g.ProviderPseudonym, "created", g.Created)
		}
		if len(regranted) > 0 {
			if _, err := deps.API.ReleasePublicGrants(ctx, regranted); err != nil {
				logger.Warn("public grants: release of a regranted provider failed", "err", err)
			}
			logger.Info("public grants: the control plane granted the provider just let go of for its window again; backing off",
				"count", len(regranted), "window", demandWindow)
			backOff(tnow.Add(publicGrantBackoff), false)
		}
		// Any previously-held grant absent from the fresh active set is
		// gone CP-side; prune its usage record along with it.
		for id := range held {
			if _, ok := next[id]; !ok {
				forgetUsage(id)
			}
		}
		held = next
	}
}

// renewAt schedules the next renew at half the remaining TTL plus up
// to 10% jitter, never in the past.
func renewAt(now, expiresAt time.Time) time.Time {
	half := expiresAt.Sub(now) / 2
	if half <= 0 {
		return now
	}
	return now.Add(half + time.Duration(rand.Int64N(int64(half)/5+1)))
}

// jitterTick spreads loop wakeups ±20% so agent fleets don't
// synchronize their acquire attempts against the CP throttle.
func jitterTick(d time.Duration) time.Duration {
	fifth := int64(d) / 5
	return d - time.Duration(fifth/2) + time.Duration(rand.Int64N(fifth+1))
}
