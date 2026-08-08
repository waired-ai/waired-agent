package router

import (
	"errors"
)

// SelectInstallModel chooses the bundled model to pre-pull at install
// time: the fitting candidates in RankModels' canonical order, best
// first. It deliberately REUSES the runtime fit machinery — no new fit
// math — so the installer's pick matches what the agent would actually
// serve once enrolled.
//
// The order lets a caller facing a disk-space shortfall step down to a
// smaller model without re-ranking. ok is true when anything fits.
//
// There is no quality floor any more (#522, owner decision 2026-08-08).
// A tier comparison could not say what it was being asked to say: within
// the one generation the catalog carries, quality_tier is
// 10*log10(params) - 5*log10(footprint) (#518), so a tier floor over it
// is a size cutoff written the long way round, and the one measurement
// that could have ranked those models — the agent-grade harness — is not
// monotone in size across them. Refusal is now what waired#1056 says it
// is: certain OOM (hostfit.OllamaCapacityFit) and the #624 native
// window, both of which RankModels already applies.
//
// ok=false with a nil error means nothing fits this host at all
// (RankModels returned ErrHardwareInsufficient) and the caller skips
// local inference with a warning. A non-nil error is a real
// misconfiguration (empty Engine, an unknown PreferredModelID, an unmet
// RequireCapability) that the caller should surface instead.
func SelectInstallModel(in PickInput) (above []Pick, ok bool, err error) {
	ranked, err := RankModels(in)
	if err != nil {
		// "Nothing fits this host" is the below-recommended-spec signal, not a fault:
		// the caller skips local inference with a warning. Every other
		// error is a genuine misconfiguration worth surfacing.
		if errors.Is(err, ErrHardwareInsufficient) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// RankModels never returns an empty slice with a nil error, so a
	// fitting host always gets a candidate here. That is why the
	// recommendation gate no longer needs standing down at this level:
	// RankModels' own narrow() falls through when a pass would empty the
	// set, and the tier filter that USED to empty it independently — and
	// so required the stand-down — is gone.
	return ranked, len(ranked) > 0, nil
}
