package router

import "errors"

// InstallQualityFloorTier is the coding-quality floor for install-time
// bundled-model auto-selection (#517). At install / `waired init` the
// installer auto-selects the largest catalog model that fits the host
// (via the runtime fit machinery) AND clears this quality_tier floor.
// When even the best-fitting model is below it — i.e. only sub-coding
// tiny models fit — the host is treated as under-spec and local
// inference is skipped (the node still enrolls and runs as a
// gateway/relay; it can route inference to peers).
//
// 30 == qwen2.5-coder-3b-instruct, the smallest usable coding model we
// ship. qwen3.5-2b (tier 27) and qwen3.5-0.8b (tier 12) fall below the
// floor and do not qualify for auto-selection.
const InstallQualityFloorTier = 30

// SelectInstallModel chooses the bundled model to pre-pull at install
// time: the highest-quality_tier variant that both fits the host (via
// RankModels) AND clears minTier. It deliberately REUSES the runtime fit
// machinery — no new fit math — so the installer's pick matches what the
// agent would actually serve once enrolled.
//
// It returns the above-floor candidates in RankModels' canonical order
// (best first), so a caller facing a disk-space shortfall can step down
// to a smaller-but-still-above-floor model without re-ranking. ok is true
// when at least one fitting candidate clears the floor.
//
// ok=false with a nil error means "under-spec": either nothing fits the
// host at all (RankModels returned ErrHardwareInsufficient) or the
// best-fitting model is below the coding-quality floor. A non-nil error
// is a real misconfiguration (empty Engine, an unknown PreferredModelID,
// an unmet RequireCapability) that the caller should surface rather than
// silently treat as under-spec.
func SelectInstallModel(in PickInput, minTier int) (above []Pick, ok bool, err error) {
	ranked, err := RankModels(in)
	if err != nil {
		// "Nothing fits this host" is the under-spec signal, not a fault:
		// the caller skips local inference with a warning. Every other
		// error is a genuine misconfiguration worth surfacing.
		if errors.Is(err, ErrHardwareInsufficient) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// ranked is sorted quality_tier-desc; keep that order so above[0] is
	// the best fit and later entries are progressively lighter.
	for _, p := range ranked {
		if p.Variant.QualityTier >= minTier {
			above = append(above, p)
		}
	}
	// #624 must never turn a previously-working host into an under-spec
	// one: when the context floor left nothing above the quality-tier
	// floor (e.g. a 4 GB CPU host whose only tier-30+ fit is a 32k-window
	// model), retry without the context floor. The picks carry
	// ContextFloorSatisfied=false, so the install notes state the
	// compromise instead of silently disabling inference.
	if len(above) == 0 && !in.NoContextFloor {
		in.NoContextFloor = true
		return SelectInstallModel(in, minTier)
	}
	return above, len(above) > 0, nil
}
