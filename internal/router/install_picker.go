package router

import (
	"errors"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

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
//
// The value lives in proto/hostfit so the control plane's onboarding
// wizard can recommend what this host would have picked for itself
// rather than re-deriving the floor from prose (waired-ai/waired#941).
const InstallQualityFloorTier = hostfit.InstallQualityFloorTier

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
	// The RECOMMENDATION gate may not turn a working host into one with no
	// local inference, so it is stood down when nothing clears the tier
	// floor. Since 2026-08-03 that gate asks "would this host declare the
	// ~200k coding window with this model" (waired-ai/waired#1056
	// decision 3), and plenty of real machines answer no for every model
	// they can hold: an 8 GB Mac holds ~120k of qwen3.5-4b's window, an
	// 8 GB card holds the full 200k, and the difference between those two
	// is not the difference between having a coding assistant and having
	// none.
	//
	// Standing it down is what the owner decision requires rather than
	// what it tolerates: refusal is reserved for certain OOM
	// (hostfit.OllamaCapacityFit), and everything softer warns and then
	// honours the choice. The host still learns what it gave up — the
	// verdict rides on every Pick, and #465 owns the surfaces that say so.
	//
	// The #624 floor's NATIVE half is not stood down here and must not be
	// (waired#1031): a 32k-window model cannot answer a coding-agent
	// session on any hardware, and the wire has no way to say "I serve
	// 32k" that a requester could route on — Claude Code resolves a
	// session window from the model id alone, in two steps. That half is
	// a manifest comparison, so no machine is refused anything by it; the
	// 131072-native class is what it costs, and that class was never
	// servable for this workload.
	if len(above) == 0 && !in.NoRecommendGate {
		in.NoRecommendGate = true
		return SelectInstallModel(in, minTier)
	}
	return above, len(above) > 0, nil
}
