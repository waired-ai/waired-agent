package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/version"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// PickInput is the world the model picker reasons over. Engine is
// the result of the engine_picker (Step 2.4) and is mandatory; the
// model picker does not attempt to pick the engine itself.
type PickInput struct {
	Catalog  []catalog.Manifest
	Hardware hardware.Profile
	Engine   string

	// EngineVersion is the SERVING engine's version (live /api/version,
	// binary --version fallback), used against per-variant
	// MinEngineVersion floors. "" = unknown, which EXCLUDES floored
	// variants (fail closed — see catalog.Variant.MinEngineVersion).
	// Unfloored variants are unaffected, so leaving this empty keeps
	// the pre-field behaviour for the whole catalog except mtp-class
	// entries.
	EngineVersion string

	// PreferredModelID, when non-empty, restricts the search to that
	// manifest's variants. Useful for honouring InferenceConfig.PreferredModelID.
	PreferredModelID string

	// RequireCapability lists capability identifiers (e.g. "tool_use",
	// "json_mode") that the chosen manifest MUST advertise. Empty means
	// "no extra capability filter" (manifests still need at least one
	// of their own capabilities; the picker doesn't enforce a baseline).
	RequireCapability []string

	// NoContextFloor disables the #624 NATIVE coding-agent context-floor
	// gating — the manifest's own advertised window (candidates still
	// carry their floor status on the Pick).
	//
	// It is a manifest comparison and nothing more. The HOST half of the
	// #624 floor — whether this machine would actually serve that window
	// — moved to the recommendation pass below when the two became the
	// same question (waired-ai/waired#1056 decision 3), which is also
	// what made it standable: a host that cannot hold 200k is told so and
	// given the best model it can hold, rather than given none.
	NoContextFloor bool

	// NoRecommendGate disables the hostfit.OllamaRecommendModel
	// narrowing (candidates still carry their verdict on the Pick). The
	// escape hatch is not optional: the gate answers "would this host
	// declare the coding window with this model", and on a host where
	// nothing does, narrowing on it would leave the installer with
	// nothing above the quality floor and the machine with no local
	// inference at all.
	//
	// Refusal is reserved for certain OOM (hostfit.OllamaCapacityFit);
	// everything else warns and honours the choice, so this gate may
	// demote a model but may never cost a host its engine
	// (waired-ai/waired#1056 decision 1, 2026-08-03).
	NoRecommendGate bool
}

// Pick is the model picker's verdict. Reasons traces the decision so
// "waired runtimes status" / refresh prompts can show why one variant
// won over the others.
type Pick struct {
	Manifest catalog.Manifest
	Variant  catalog.Variant
	Reasons  []string

	// ContextFloorSatisfied reports whether this candidate meets the #624
	// coding-agent context floor on this host: the model's own window
	// reaches ~200k AND, on the ollama path, the serve tuning would
	// actually size that window here. False on best-effort fallback picks
	// and on preferred-override picks of sub-floor models.
	//
	// It is reported, not enforced as one gate: the manifest half is what
	// RankModels narrows on unconditionally, and the host half is part of
	// the recommendation, which a caller may stand down.
	ContextFloorSatisfied bool

	// ExpectedSpillFraction is the predicted /api/ps spill fraction of
	// serving the effective floor window on this host (0 when the
	// window fits fully GPU-resident, or on non-ollama engines).
	ExpectedSpillFraction float64

	// DecodeEstimate is the roofline decode prediction for this candidate
	// on this host (hostfit.EstimateOllamaDecode; zero on non-ollama
	// engines). It is what separates "the host can run this" from "the
	// host can run this usefully" — a distinction weight alone cannot
	// make, because a dense 27B and a 3B-active mixture of experts of the
	// same size decode seven times apart (#229).
	DecodeEstimate hostfit.Estimate

	// Recommendation is hostfit.OllamaRecommendModel's verdict: Fits
	// reports whether this is a model the host should be POINTED AT by
	// default — since 2026-08-03 that means "this host can declare the
	// ~200k coding window with it" — and Reason / NeedMB / HaveMB say why
	// not when it is false.
	//
	// False is NOT "cannot run" — capacity already admitted this
	// candidate, and a caller listing models must keep showing it, greyed
	// or annotated. Hiding it is the #229 bug.
	//
	// On the vLLM path it carries VLLMServesContextFloor's answer to the
	// same question, since that engine has no separate residency or spill
	// story. A preferred-override pick BYPASSES the gate but is still
	// reported honestly: the user gets the model they asked for, and this
	// says what the host thinks of it.
	Recommendation hostfit.Verdict
}

// RankModels applies the Step 2.5 filter + sort and returns EVERY
// fitting (manifest, variant) candidate in the picker's canonical order:
//
//  1. Restrict to manifests honouring PreferredModelID (if any).
//  2. Discard manifests missing any RequireCapability entry.
//  3. For each manifest, expand to the variants that name Engine in
//     runtime_support and are vendor-supported.
//  4. Drop variants that don't fit the host (vllm: VLLMVRAMBudgetMB ≥
//     MinVRAMMB — TP-aggregated on identical multi-NVIDIA hosts, #678;
//     ollama: RAMTotalGB ≥ MinRAMGB, plus — on discrete-GPU hosts — the
//     weights + KV budget must fit GPU-resident, see ollamaFitsVRAM).
//  5. Sort by (quality_tier desc, MinVRAMMB asc, MinRAMGB asc, manifest
//     position asc).
//
// PickModel is RankModels(in)[0] with a richer reason trace. Returns the
// same errors PickModel does (ErrModelNotFound, ErrCapabilityNotMet,
// ErrHardwareInsufficient, or a plain error when Engine is empty). The
// returned slice is never empty on a nil error. Each Pick carries a
// short per-candidate reason; callers that want the full "why it won"
// trace should use PickModel (or build their own, as LighterCandidate
// does).
func RankModels(in PickInput) ([]Pick, error) {
	if in.Engine == "" {
		return nil, errors.New("router: RankModels requires Engine to be set")
	}

	// Step 1: PreferredModelID gate.
	manifests := in.Catalog
	if in.PreferredModelID != "" {
		filtered := make([]catalog.Manifest, 0, 1)
		for _, m := range in.Catalog {
			if m.ModelID == in.PreferredModelID {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrModelNotFound, in.PreferredModelID)
		}
		manifests = filtered
	}

	// Step 2: capability filter (manifest-level).
	var capable []catalog.Manifest
	for _, m := range manifests {
		if !manifestHasAll(m, in.RequireCapability) {
			continue
		}
		capable = append(capable, m)
	}
	if len(capable) == 0 {
		return nil, fmt.Errorf("%w: required %v", ErrCapabilityNotMet, in.RequireCapability)
	}

	// Steps 3+4: variant expansion + host-fit filter.
	type candidate struct {
		manifestIdx int
		manifest    catalog.Manifest
		variant     catalog.Variant
		floorOK     bool // reported: native window AND this host serves it
		gateOK      bool // narrowed on by pass 1
		spill       float64
		est         hostfit.Estimate
		rec         hostfit.Verdict
	}
	var fits []candidate
	for i, m := range capable {
		for _, v := range m.Variants {
			if !engineSupports(v, in.Engine) {
				continue
			}
			if !engineVersionSatisfies(v, in.EngineVersion) {
				continue
			}
			if !variantSupportedByVendor(v, in.Engine, in.Hardware) {
				continue
			}
			if !hostFits(in.Engine, m, v, in.Hardware) {
				continue
			}
			// No speed claim by default — vLLM has no roofline model
			// here, and hostfit spells "no claim" as a passing floor.
			// Both engines start recommendable; each fills in its own
			// answer below.
			c := candidate{manifestIdx: i, manifest: m, variant: v,
				est: hostfit.Estimate{MeetsSpeedFloor: true},
				rec: hostfit.Verdict{Fits: true}}
			// #624 coding-agent context floor, REPORTED here: the native
			// window plus the per-engine host gate — the serve tuning's
			// own sizing on ollama, the utilization-budget window check
			// on vllm (#675/#678; vLLM clamps instead of spilling, so no
			// spill fraction there). Which half each narrowing pass below
			// acts on is a separate question — see the passes.
			c.floorOK = MeetsNativeContextFloor(m)
			if in.Engine == catalog.RuntimeOllama {
				hostOK, spill := OllamaServesContextFloor(m, v, in.Hardware)
				c.spill = spill
				c.floorOK = c.floorOK && hostOK
				c.est = hostfit.EstimateOllamaDecode(v, in.Hardware.HostFit())
				c.rec = hostfit.OllamaRecommendModel(m, v, in.Hardware.HostFit())
			}
			if in.Engine == catalog.RuntimeVLLM {
				c.floorOK = c.floorOK && VLLMServesContextFloor(m, v, in.Hardware)
			}
			// What pass 1 narrows on. On ollama the host half moved to the
			// recommendation, which a caller may stand down; on vLLM it
			// stays here, because that engine has no residency or spill
			// story for the recommendation to be about and nothing in the
			// 2026-08-03 decision asks it to change.
			c.gateOK = c.floorOK
			if in.Engine == catalog.RuntimeOllama {
				c.gateOK = MeetsNativeContextFloor(m)
			}
			fits = append(fits, c)
		}
	}
	if len(fits) == 0 {
		return nil, fmt.Errorf("%w: no variant fits hardware (engine=%s)", ErrHardwareInsufficient, in.Engine)
	}

	// Two-pass quality gating, best bar first, each falling through only
	// when it would leave nothing. An explicit PreferredModelID bypasses
	// all of it — the user asked for that model — with the status still
	// reported on the Pick.
	//
	//  1. #624 NATIVE coding-agent context floor: the model's own
	//     advertised window reaches ~200k. A manifest comparison, so it
	//     says nothing about this machine and no hardware changes it.
	//  2. hostfit.OllamaRecommendModel: would this host actually declare
	//     the ~200k coding window with this model? That is what
	//     "recommended" means since the 2026-08-03 owner decision
	//     (waired-ai/waired#1056 decision 3), and it is the same sizing
	//     the serve tuning exports, so the pick and the running engine
	//     agree by construction rather than by two matching comments.
	//
	//     It replaces a three-armed rule that asked a different question
	//     per class — CPU-only exempt, unified judged on published-peak
	//     decode, discrete on resident weights — which is how "no GPU"
	//     became the most permissive configuration a host could be in.
	//     waired-ai/waired#986's failure (a 22.6 GB mixture of experts
	//     auto-selected onto a 16 GB card, prefilling at 388 tok/s) is
	//     still caught: that host cannot hold the window either.
	//
	//  3. Everything that fits, so no gate can newly turn a working host
	//     into one below the recommended spec.
	//
	// There is no longer a speed pass. The #229 roofline is still
	// computed and still reported — it is what separates a dense 27B from
	// a 3B-active mixture of experts of the same size — but it no longer
	// EXCLUDES, on any class. It rests on population bandwidth constants
	// (BandwidthSystemRAMGBs = 60) that ClassCPUOnly is exempt from and
	// ClassDiscrete-spilled was not, so the same number excluded a 19.96
	// tok/s host while admitting a 17.65 tok/s one — the faster machine
	// being the one refused. And it prices decode only, while a coding
	// agent's work is ~21:1 prefill-heavy. Speed becomes a recommendation
	// input again when it is MEASURED rather than assumed
	// (waired-ai/waired-agent#466); the boot benchmark already measures
	// the real rate once a model is on disk.
	narrow := func(keep func(candidate) bool) {
		var pass []candidate
		for _, c := range fits {
			if keep(c) {
				pass = append(pass, c)
			}
		}
		if len(pass) > 0 {
			fits = pass
		}
	}
	if in.PreferredModelID == "" && !in.NoContextFloor {
		narrow(func(c candidate) bool { return c.gateOK })
	}
	if in.PreferredModelID == "" && !in.NoRecommendGate {
		narrow(func(c candidate) bool { return c.rec.Fits })
	}

	// Step 5: sort by tier desc, then MinVRAM/MinRAM asc, then manifest order.
	sort.SliceStable(fits, func(i, j int) bool {
		a, b := fits[i].variant, fits[j].variant
		if a.QualityTier != b.QualityTier {
			return a.QualityTier > b.QualityTier
		}
		if in.Engine == catalog.RuntimeVLLM {
			if a.MinVRAMMB != b.MinVRAMMB {
				return a.MinVRAMMB < b.MinVRAMMB
			}
		} else {
			if a.MinRAMGB != b.MinRAMGB {
				return a.MinRAMGB < b.MinRAMGB
			}
		}
		return fits[i].manifestIdx < fits[j].manifestIdx
	})

	out := make([]Pick, 0, len(fits))
	for _, c := range fits {
		p := Pick{
			Manifest: c.manifest,
			Variant:  c.variant,
			Reasons: []string{fmt.Sprintf("fitting candidate %s/%s (quality_tier=%d)",
				c.manifest.ModelID, c.variant.VariantID, c.variant.QualityTier)},
			ContextFloorSatisfied: c.floorOK,
			ExpectedSpillFraction: c.spill,
			DecodeEstimate:        c.est,
			Recommendation:        c.rec,
		}
		switch {
		case c.floorOK && c.spill > 0:
			p.Reasons = append(p.Reasons, fmt.Sprintf(
				"serves the ~200k coding window with ~%.0f%% of the model expected in system RAM",
				c.spill*100))
		case !c.floorOK:
			p.Reasons = append(p.Reasons, fmt.Sprintf(
				"below the ~200k coding-agent context floor (native window %d tokens); best-effort candidate",
				c.manifest.ContextLength))
		}
		if !c.rec.Fits {
			p.Reasons = append(p.Reasons, notRecommendedReason(c.rec))
		}
		out = append(out, p)
	}
	return out, nil
}

// notRecommendedReason turns a declined hostfit.OllamaRecommend verdict
// into the picker's reason-trace line. It says "not preselected", never
// "cannot run": the model reached this point because capacity admitted
// it, and stating otherwise would re-create the #229 bug in prose.
//
// These strings are diagnostics for `waired runtimes status` and the
// refresh prompts, not user-facing product copy; the wizard and the tray
// get reason CODES and word them themselves (waired-ai/waired-agent#321).
func notRecommendedReason(v hostfit.Verdict) string {
	switch v.Reason {
	case hostfit.ReasonWeightsSpill:
		return fmt.Sprintf(
			"not preselected here: the weights need ~%d MB GPU-resident and this host offers %d MB, "+
				"so they would be re-read from system RAM on every prompt (runs, but slowly)",
			v.NeedMB, v.HaveMB)
	case hostfit.ReasonTooSlow:
		return fmt.Sprintf(
			"not preselected here: ~%.0f tok/s expected at this machine's published memory bandwidth, "+
				"below the %.0f tok/s floor (runs, but slowly)",
			v.Estimate.TokpsEstimate, hostfit.DecodeFloorTokps)
	case hostfit.ReasonInsufficientVRAM:
		return fmt.Sprintf(
			"not preselected here: needs ~%d MB in the shared memory pool, which offers %d MB",
			v.NeedMB, v.HaveMB)
	default:
		return "not preselected here"
	}
}

// PickModel returns the single highest-ranked fitting variant — the head
// of RankModels — with a detailed "why it won" reason trace. See
// RankModels for the algorithm and error semantics.
func PickModel(in PickInput) (Pick, error) {
	ranked, err := RankModels(in)
	if err != nil {
		return Pick{}, err
	}
	winner := ranked[0]
	reasons := []string{
		fmt.Sprintf("engine=%s, evaluated %d candidate variant(s)", in.Engine, len(ranked)),
		fmt.Sprintf("selected %s/%s (quality_tier=%d) — highest tier that fits the host",
			winner.Manifest.ModelID, winner.Variant.VariantID, winner.Variant.QualityTier),
	}
	if in.Engine == catalog.RuntimeVLLM && len(in.Hardware.GPUs) > 0 {
		if tp := VLLMTensorParallelSize(in.Hardware); tp > 1 {
			// #678: report the aggregated tensor-parallel budget, not the
			// misleading single-device figure.
			reasons = append(reasons,
				fmt.Sprintf("VRAM fit: variant min=%d MB ≤ host budget=%d MB (TP=%d × %d MB per GPU)",
					winner.Variant.MinVRAMMB, VLLMVRAMBudgetMB(in.Hardware),
					tp, in.Hardware.GPUs[0].VRAMTotalMB))
		} else {
			reasons = append(reasons,
				fmt.Sprintf("VRAM fit: variant min=%d MB ≤ host GPU0=%d MB",
					winner.Variant.MinVRAMMB, in.Hardware.GPUs[0].VRAMTotalMB))
		}
	}
	if in.Engine == catalog.RuntimeOllama {
		if in.Hardware.UnifiedMemory {
			// On UMA hosts the model loads into the GPU-addressable pool,
			// not system RAM, so report the residency budget rather than a
			// misleading "min RAM ≤ system RAM" line (system RAM is only
			// the leftover after the BIOS carve-out).
			reasons = append(reasons,
				fmt.Sprintf("UMA fit: ~%.1f GB weights + KV/overhead resident within UsableVRAM=%d MB",
					winner.Variant.EstimatedWeightGB, in.Hardware.EffectiveVRAMMB()))
		} else {
			reasons = append(reasons,
				fmt.Sprintf("RAM fit: variant min=%d GB ≤ host total=%d GB",
					winner.Variant.MinRAMGB, in.Hardware.RAMTotalGB))
			if len(in.Hardware.GPUs) > 0 && winner.Variant.EstimatedWeightGB > 0 {
				// "GPU0" was accurate while the budget was one device.
				// Since #264 it is the pool where there is one, so the
				// label has to name what was actually compared.
				where := fmt.Sprintf("GPU0=%d MB", in.Hardware.OllamaVRAMBudgetMB())
				if n := ollamaPooledGPUs(in.Hardware); n > 1 {
					where = fmt.Sprintf("%d GPUs=%d MB pooled", n, in.Hardware.OllamaVRAMBudgetMB())
				}
				reasons = append(reasons,
					fmt.Sprintf("VRAM fit: ~%.1f GB weights + KV/overhead resident within %s",
						winner.Variant.EstimatedWeightGB, where))
			}
		}
	}
	if in.PreferredModelID != "" {
		reasons = append(reasons, fmt.Sprintf("PreferredModelID=%q honoured", in.PreferredModelID))
	}
	// #624 context-floor status (informational tone — a bounded spill
	// and a best-effort fallback are working configurations, not errors).
	switch {
	case winner.ContextFloorSatisfied && winner.ExpectedSpillFraction > 0:
		reasons = append(reasons, fmt.Sprintf(
			"coding context floor: serves ~200k with ~%.0f%% of the model expected in system RAM (larger window traded for some decode speed)",
			winner.ExpectedSpillFraction*100))
	case !winner.ContextFloorSatisfied && in.PreferredModelID != "":
		reasons = append(reasons, fmt.Sprintf(
			"preferred model overrides the ~200k coding-agent context floor (native window %d tokens)",
			winner.Manifest.ContextLength))
	case !winner.ContextFloorSatisfied:
		reasons = append(reasons,
			"no model on this host can serve the ~200k coding-agent context; best-effort selection")
	}
	// A winner that is not recommended means either an explicit override
	// or a host where nothing cleared the gate and pass 3 fell through.
	// Both are working configurations; neither should be silent.
	if !winner.Recommendation.Fits {
		reasons = append(reasons, notRecommendedReason(winner.Recommendation))
	}
	winner.Reasons = reasons
	return winner, nil
}

// manifestHasAll returns true iff every required capability is
// advertised by m.Capabilities (case-insensitive).
func manifestHasAll(m catalog.Manifest, required []string) bool {
	for _, r := range required {
		if !hasCapability(m.Capabilities, r) {
			return false
		}
	}
	return true
}

// engineSupports returns true iff v.RuntimeSupport contains engine
// (case-sensitive: RuntimeSupport values come from manifest JSON,
// which we keep lower-case by spec).
func engineSupports(v catalog.Variant, engine string) bool {
	for _, r := range v.RuntimeSupport {
		if r == engine {
			return true
		}
	}
	return false
}

// engineVersionSatisfies applies the per-variant MinEngineVersion
// floor. Unfloored variants always pass; floored variants need a KNOWN
// engineVersion >= floor — unknown ("") fails closed, because serving
// a variant the engine cannot load fails server-side with no useful
// indication (the qwen3.6 mtp incident).
func engineVersionSatisfies(v catalog.Variant, engineVersion string) bool {
	if v.MinEngineVersion == "" {
		return true
	}
	if engineVersion == "" {
		return false
	}
	return version.AtLeast(engineVersion, v.MinEngineVersion)
}

// FirstPullableVariant generalizes the historical "PullModel always
// pulls Variants[0]" rule: the first variant (manifest order = author
// preference) that the engine both supports and is new enough to
// load. A too-old engine thus pulls the plain variant instead of an
// mtp one its registry would refuse. ok=false when nothing passes.
func FirstPullableVariant(m catalog.Manifest, engine, engineVersion string) (catalog.Variant, bool) {
	for _, v := range m.Variants {
		if !engineSupports(v, engine) {
			continue
		}
		if !engineVersionSatisfies(v, engineVersion) {
			continue
		}
		return v, true
	}
	return catalog.Variant{}, false
}

// hostFits is the per-engine fit predicate.
//
// The rules themselves live in proto/hostfit, the module the control
// plane also imports — see hostfit's package doc for why. This function
// is the agent's binding of them: which budget vLLM is judged against,
// and the mapping from a Verdict to the bool the picker wants.
//
// BOTH engines aggregate across devices, differently, and only one of
// the two aggregations lives here.
//
// vLLM shards each tensor, so it can only aggregate across an IDENTICAL
// set, and the size of that set is a serving-topology decision the
// control plane cannot reproduce from a broadcast summary. Hence
// VLLMVRAMBudgetMB in this package, passed to hostfit as a budget
// argument (#678: a single GPU keeps Profile.EffectiveVRAMMB
// semantics).
//
// Ollama does NOT shard — it splits by layer, and pools a whole
// ml.ByLibrary group only when a model will not fit one card (#264).
// That is a property of the devices themselves, derivable from the
// device list the summary has always carried, so it is a fit rule and
// lives in the shared package: hostfit computes it and both adapters
// get the same answer. This function passes the whole Host and the
// budget is chosen inside.
//
// The older claim here — "ollama is never aggregated, it does not
// shard" — conflated sharding with pooling and was wrong about the
// engine.
//
// The manifest is a parameter because the ollama capacity rule prices
// the window the model would actually be given: min(the coding window,
// the model's own). Pricing a 131072-native model's KV cache at 200k
// would refuse it for a window no caller would ever ask it to serve.
func hostFits(engine string, m catalog.Manifest, v catalog.Variant, hw hardware.Profile) bool {
	switch engine {
	case catalog.RuntimeVLLM:
		// Pre-hostfit this branch answered "fits" for a variant with no
		// declared minimum even on a host with no GPU at all; hostfit
		// reports no_gpu there instead. Unreachable in practice — every
		// bundled vLLM variant declares min_vram_mb, and PickEngine only
		// selects vllm on an NVIDIA host — and "it fits" was the worse
		// answer of the two.
		return hostfit.VLLMFit(v, VLLMVRAMBudgetMB(hw)).Fits
	case catalog.RuntimeOllama:
		return hostfit.OllamaCapacityFit(m, v, hw.HostFit()).Fits
	default:
		// Unknown engine: be conservative.
		return false
	}
}

// OllamaVRAMOverheadMB returns the fit-time overhead reservation for the
// host. Kept as the agent-facing name over hostfit.OllamaVRAMOverheadMB
// because callers hold a hardware.Profile, not a Host: the #621
// context-length clamp subtracts the same overhead this gate assumes,
// and scoring's MaxContextTokens counts RAW weights precisely because
// the whole overhead lives in that subtraction (never double-count).
func OllamaVRAMOverheadMB(hw hardware.Profile, weightGB float64) int {
	return hostfit.OllamaVRAMOverheadMB(hw.UnifiedMemory, weightGB)
}

// ollamaFitsVRAM reports whether v fits fully resident in the host's
// GPU-addressable budget — the residency half of the ollama fit, without
// the system-RAM gate. Callers that need to explain WHICH constraint
// bound (deficitLabelFor) depend on that separation.
func ollamaFitsVRAM(v catalog.Variant, hw hardware.Profile) bool {
	return hostfit.OllamaResident(v, hw.HostFit()).Fits
}

// ollamaPooledGPUs is how many devices the ollama budget is spread
// across: >1 only when hostfit actually pooled them, 1 otherwise.
//
// For display only — the budget itself comes from
// Profile.OllamaVRAMBudgetMB, and this exists so a reason string can say
// "across N GPUs" the way the vLLM arm already does rather than naming a
// single card's VRAM after judging the host on several (#264). It reads
// the pool back off the Host instead of re-deriving which devices
// qualified, so it cannot disagree with the figure it annotates.
func ollamaPooledGPUs(hw hardware.Profile) int {
	h := hw.HostFit()
	if h.OllamaVRAMBudgetMB() <= h.EffectiveVRAMMB() {
		return 1
	}
	var n int
	for _, g := range hw.GPUs {
		if g.Vendor == "nvidia" && g.VRAMTotalMB > 0 {
			n++
		}
	}
	return n
}

// hasCapabilityCI is a case-insensitive variant of hasCapability used
// only by the picker. The original hasCapability stays for backward
// compatibility with endpoint_router.go.
//
// (Re-using strings.EqualFold from endpoint_router.go's hasCapability
// would create an import cycle in the test file otherwise; both
// helpers ultimately do the same thing.)
var _ = strings.EqualFold // keep the import live in case future helpers need it

// variantSupportedByVendor consults Variant.VendorSupport to drop
// variants the manifest author marked as "unsupported" on the host's
// GPU vendor for the chosen engine. Empty / nil VendorSupport is
// permissive (every cell defaults to "stable") so manifests can omit
// the field for the common NVIDIA-everywhere case.
//
// Hosts with no GPU vendor (CPU-only) are not filtered: Ollama
// gracefully falls back to CPU inference and any vendor restriction
// on a GPU runtime is irrelevant.
func variantSupportedByVendor(v catalog.Variant, engine string, hw hardware.Profile) bool {
	if v.VendorSupport == nil {
		return true
	}
	vendor := primaryGPUVendor(hw)
	if vendor == "" {
		return true
	}
	var cell catalog.VendorRuntimeSupport
	switch vendor {
	case "nvidia":
		cell = v.VendorSupport.Nvidia
	case "amd":
		cell = v.VendorSupport.AMD
	case "apple":
		cell = v.VendorSupport.Mac
	default:
		return true
	}
	var status string
	switch engine {
	case catalog.RuntimeVLLM:
		status = cell.VLLM
	case catalog.RuntimeOllama:
		status = cell.Ollama
	default:
		return true
	}
	return status != catalog.VendorSupportUnsupported
}

// primaryGPUVendor returns the lowercase vendor string of the first
// GPU in hw.GPUs, or "" when the host has no GPU. Vendor strings the
// hardware package emits today are "nvidia", "amd", "apple" — anything
// else falls through and is treated as "no preference" by the
// vendor-aware filters.
func primaryGPUVendor(hw hardware.Profile) string {
	if len(hw.GPUs) == 0 {
		return ""
	}
	return strings.ToLower(hw.GPUs[0].Vendor)
}
