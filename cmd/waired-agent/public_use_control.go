package main

import (
	"log/slog"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
)

// publicUseController owns the consumer-side Public Share posture as
// the router sees it (waired#827): it collapses the persisted settings
// plus the consent record into a router.PublicPolicy, caches it for the
// routing hot path, and owns the one-shot pre-consent nudge.
//
// Cache shape follows workerController: an atomic.Pointer read once per
// SelectK, so a routing pass costs one atomic load and no lock. The
// value is refreshed on demand rather than polled — the loopback
// management API is public_use.json's only writer, so
// management.PublicUseConfig.OnChange is an exact invalidation signal.
//
// Lifetime is the process, not the session: the management server
// outlives node-key rotations, so main.go constructs this next to the
// observability ring rather than inside the re-entrant activate
// closure.
type publicUseController struct {
	path           string
	warningVersion int
	logger         *slog.Logger

	// policy is the resolved posture. Never nil after the constructor.
	policy atomic.Pointer[router.PublicPolicy]

	// ring receives the one-shot nudge event. nil disables emission.
	ring *observability.Ring

	// nudged latches the one-shot hint for the lifetime of the process.
	//
	// Deliberately NOT persisted. The event ring is process-lifetime
	// memory, so a consumer can only ever see events from this run —
	// persisting the marker would suppress the hint for a UI that has no
	// way to have seen it. It would also make this a second writer to a
	// file D1 documents as management-API-only, and put a
	// read-modify-write on the inference hot path. See
	// waired/docs/decisions.md (20260720).
	nudged atomic.Bool
}

// newPublicUseController loads the initial posture. A load failure is
// not fatal: the controller starts fully closed (no public candidates)
// and recovers on the next settings write.
func newPublicUseController(path string, warningVersion int, ring *observability.Ring, logger *slog.Logger) *publicUseController {
	c := &publicUseController{
		path:           path,
		warningVersion: warningVersion,
		ring:           ring,
		logger:         logger,
	}
	c.Reload()
	return c
}

// Policy is the lock-free read the Selector performs once per pass.
func (c *publicUseController) Policy() router.PublicPolicy {
	if c == nil {
		return router.PublicPolicy{}
	}
	if p := c.policy.Load(); p != nil {
		return *p
	}
	return router.PublicPolicy{}
}

// Reload re-reads public_use.json and republishes the resolved policy.
// Wired to management.PublicUseConfig.OnChange.
//
// Fails closed: a missing file, a parse error or an invalid stored mode
// all publish the zero policy (mode off, unconsented) rather than
// leaving a stale permissive value in place.
func (c *publicUseController) Reload() {
	if c == nil {
		return
	}
	pu, found, err := agentconfig.LoadPublicUse(c.path)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("public-use settings unreadable; public candidates disabled", "err", err)
		}
		c.policy.Store(&router.PublicPolicy{})
		return
	}
	if !found {
		c.policy.Store(&router.PublicPolicy{})
		return
	}
	p := resolvePublicPolicy(pu, c.warningVersion)
	c.policy.Store(&p)
}

// Nudge emits the one-shot pre-consent hint, at most once per process.
//
// Called from the Selector hot path, so the fast path after the first
// emit is a single atomic load. CompareAndSwap makes concurrent
// requests race to exactly one emit.
func (c *publicUseController) Nudge(n router.PublicNudge) {
	if c == nil || c.ring == nil {
		return
	}
	if !c.nudged.CompareAndSwap(false, true) {
		return
	}
	rec := observability.NewRecorder(c.ring, nil, c.logger)
	rec.RecordPublicShareNudge(n.ModelID, n.Reason)
}

// resolvePublicPolicy collapses the persisted settings and the consent
// record into the router's view.
//
// EffectiveMode already forces off without a consent record for the
// current warning version, so the mapping below cannot produce a
// permissive mode for an unconsented device. Consented is carried
// separately because the nudge must distinguish "never consented" from
// "consented and deliberately off".
func resolvePublicPolicy(pu agentconfig.PublicUse, warningVersion int) router.PublicPolicy {
	return router.PublicPolicy{
		Mode:           publicModeOf(pu.EffectiveMode(warningVersion)),
		Consented:      pu.Consented(warningVersion),
		MinQualityTier: pu.MinQualityTier,
		Main:           pu.Main,
		Sub:            pu.Sub,
	}
}

// publicModeOf maps the stored mode string onto the router enum. An
// unrecognised value maps to off — the settings loader already rejects
// invalid modes, so reaching the default arm means the two vocabularies
// have drifted, and failing closed is the only safe answer.
func publicModeOf(mode string) router.PublicMode {
	switch mode {
	case agentconfig.PublicUseModeAuto:
		return router.PublicModeAuto
	case agentconfig.PublicUseModeExplicit:
		return router.PublicModeExplicit
	default:
		return router.PublicModeOff
	}
}

// publicUseWarningVersion is the served consent-warning version the
// controller resolves consent against. Indirection kept so the
// management package stays the single source of the number.
const publicUseWarningVersion = management.PublicShareWarningVersion
