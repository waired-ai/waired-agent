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
	// The RECOMMENDATION gate may not turn a previously-working host into
	// an under-spec one, so it is stood down when nothing clears the tier
	// floor: giving it up restores a model that spills weights to system
	// RAM — slower, and exactly what the host used to be given. It is not
	// monotone in hardware (an 8 GB laptop with a 4 GB card has nothing
	// above tier 27 resident, while the same laptop without the card
	// installs qwen3.5-4b), and no machine may lose local inference for
	// owning a small GPU (waired-ai/waired#988).
	//
	// The #624 CONTEXT FLOOR used to be stood down the same way, one step
	// later, and no longer is (waired#1031). The window is now a contract
	// a node either declares or does not: a host serving a 32k window
	// cannot answer a coding-agent session, and the wire has no way to say
	// "I serve 32k" that a requester could route on — Claude Code resolves
	// a session window from the model id alone, in two steps. So the
	// concession that fall-through bought is no longer available to buy.
	// A host with no model above the tier floor that also reaches the
	// window is under-spec, and takes the same path as a host where
	// nothing fits at all: it enrols, routes to peers, and runs no local
	// engine.
	//
	// The concession is smaller than the old comment's example suggests,
	// but it is NOT nil, and this comment used to say it was. The
	// 262144-native class scales down to variants an 8 GB CARD holds with
	// a full 200k KV cache — qwen3.5-4b at 7539 MiB, which hostfit's
	// TestOllamaWindowResidentMB_SmallHostCanDeclare200k finds by walking
	// the shipped catalog. It does NOT scale down to an 8 GB unified-
	// memory host: the same variant on a 6144 MiB carve-out holds ~120k,
	// so an 8 GB Mac has nothing above the tier floor that also reaches
	// the window, and this function calls it under-spec. That case was
	// obscured until waired-agent#448 corrected qwen3.5-4b's KV
	// annotation from its 2b sibling's value; the model was never holding
	// 200k there, the manifest just said it was.
	//
	// What is genuinely lost regardless is the 131072-native class, which
	// no host can serve a coding session on however large it is.
	if len(above) == 0 && !in.NoRecommendGate {
		in.NoRecommendGate = true
		return SelectInstallModel(in, minTier)
	}
	return above, len(above) > 0, nil
}
