package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// Background Public Share grant acquirer/renewer (waired#821 second
// half, spec §6). Consumer side: while the operator's public-use mode
// is on (consented, D1), pre-acquire up to K=3 grants so the router
// (D2) finds granted foreign peers already in the netmap when it wants
// them — spec §6 keeps acquire OFF the request hot path; periodic
// pre-acquisition is the v1 trigger (router-driven acquisition can
// layer on later without changing the CP contract).
//
// The acquirer only manages grant LIFECYCLE. Routing consumes the
// netmap (PeerView.Grant, injected CP-side by waired#820); no local
// grant store exists or is needed — the acquire response always
// returns the device's full active set, so state self-corrects across
// restarts.
const (
	publicGrantTTL      = 10 * time.Minute
	publicGrantTick     = 75 * time.Second
	publicGrantWant     = 3
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

	Tick time.Duration    // 0 → publicGrantTick
	Now  func() time.Time // nil → time.Now
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
		held = map[string]*heldGrant{}
	}

	timer := time.NewTimer(jitterTick(tick))
	defer timer.Stop()
	// lastAcquireAt gates demand-driven cycles. Zero until the first
	// acquire attempt, so the very first demand — the one right after
	// the user consents — is always admitted.
	var lastAcquireAt time.Time
	for {
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
				if p.Grant != nil && p.Grant.Role == "provider" && p.Grant.ID != "" {
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
		}

		// Renew due grants (TTL/2 + jitter schedule set at admission).
		var due []string
		for id, h := range held {
			if tnow.After(h.nextRenewAt) {
				due = append(due, id)
				if len(due) == publicGrantBatchMax {
					break
				}
			}
		}
		if len(due) > 0 {
			res, err := deps.API.RenewPublicGrants(ctx, due)
			switch {
			case errors.Is(err, controlclient.ErrPublicShareNotEligible):
				// §7.2 mutuality lost — grants will lapse CP-side too.
				logger.Warn("public grants: renew rejected (not eligible); backing off")
				backoffUntil = tnow.Add(publicGrantBackoff)
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

		// Top up to K when consented and out of backoff.
		if len(held) >= publicGrantWant || tnow.Before(backoffUntil) {
			continue
		}
		consentVersion := 0
		if pu.Consent != nil {
			consentVersion = pu.Consent.WarningVersion
		}
		lastAcquireAt = tnow
		res, err := deps.API.AcquirePublicGrants(ctx, controlclient.AcquirePublicGrantsRequest{
			Class:          "",
			MinQualityTier: pu.MinQualityTier,
			Want:           0, // server default (3)
			ConsentVersion: consentVersion,
		})
		switch {
		case errors.Is(err, controlclient.ErrPublicShareNotEligible),
			errors.Is(err, controlclient.ErrPublicShareRateLimited):
			logger.Info("public grants: acquire deferred", "err", err)
			backoffUntil = tnow.Add(publicGrantBackoff)
			continue
		case err != nil:
			logger.Warn("public grants: acquire failed", "err", err)
			continue
		}
		if len(res.Grants) == 0 {
			// Eligible but no candidates right now — not an error, no
			// warn spam; just don't hammer the CP.
			backoffUntil = tnow.Add(publicGrantBackoff)
			continue
		}
		// The response is the FULL active set: replace wholesale,
		// preserving acquiredAt/renew schedule for grants we knew.
		next := make(map[string]*heldGrant, len(res.Grants))
		for _, g := range res.Grants {
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
