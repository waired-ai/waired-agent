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

	// NoContextFloor disables the #624 coding-agent context-floor
	// gating (candidates still carry their floor status on the Pick).
	// Escape hatch for callers whose own constraints would otherwise
	// turn a previously-working host into an under-spec one — e.g.
	// SelectInstallModel retries with this set when the floor leaves
	// nothing above the quality-tier floor.
	NoContextFloor bool
}

// Pick is the model picker's verdict. Reasons traces the decision so
// "waired runtimes status" / refresh prompts can show why one variant
// won over the others.
type Pick struct {
	Manifest catalog.Manifest
	Variant  catalog.Variant
	Reasons  []string

	// ContextFloorSatisfied reports whether this candidate passed the
	// #624 coding-agent context floor (native window ≥ ~200k AND, on
	// the ollama path, the host serves the floor window within the
	// bounded-spill gate). False on best-effort fallback picks and on
	// preferred-override picks of sub-floor models.
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
		floorOK     bool
		spill       float64
		est         hostfit.Estimate
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
			if !hostFits(in.Engine, v, in.Hardware) {
				continue
			}
			// No speed claim by default — vLLM has no roofline model
			// here, and hostfit spells "no claim" as a passing floor.
			c := candidate{manifestIdx: i, manifest: m, variant: v,
				est: hostfit.Estimate{MeetsSpeedFloor: true}}
			// #624 coding-agent context floor: native window plus the
			// per-engine host gate — bounded-spill on ollama, the
			// utilization-budget window check on vllm (#675/#678; vLLM
			// clamps instead of spilling, so no spill fraction there).
			c.floorOK = MeetsNativeContextFloor(m)
			if in.Engine == catalog.RuntimeOllama {
				hostOK, spill := OllamaServesContextFloor(m, v, in.Hardware)
				c.spill = spill
				c.floorOK = c.floorOK && hostOK
				c.est = hostfit.EstimateOllamaDecode(v, in.Hardware.HostFit())
			}
			if in.Engine == catalog.RuntimeVLLM {
				c.floorOK = c.floorOK && VLLMServesContextFloor(m, v, in.Hardware)
			}
			fits = append(fits, c)
		}
	}
	if len(fits) == 0 {
		return nil, fmt.Errorf("%w: no variant fits hardware (engine=%s)", ErrHardwareInsufficient, in.Engine)
	}

	// Three-pass quality gating, best bar first, each falling through
	// only when it would leave nothing. An explicit PreferredModelID
	// bypasses all of it — the user asked for that model — with the
	// status still reported on the Pick.
	//
	//  1. #624 coding-agent context floor: native window plus the
	//     per-engine host gate.
	//  2. #229 decode floor: fast enough to be worth serving at all.
	//     This pass is the counterweight to the capacity gate no longer
	//     requiring GPU residency on discrete hosts — without it a host
	//     would auto-select a model that spills most of its layers
	//     whenever that model carried a higher quality tier.
	//
	//     It applies ONLY where the estimate is an upper bound, which is
	//     the spilled-discrete case: there the card's own reads are
	//     priced at zero, a margin no unknown hardware can eat. The
	//     unified-memory figure rests on a bandwidth constant set at the
	//     FLOOR of its population, so it is a lower bound, and excluding
	//     on one would withhold from an M4 Max what an M4 base runs. The
	//     CPU-only figure rests on a constant meant as an upper bound but
	//     with no such margin behind it, so a host whose memory beats the
	//     constant would be excluded on the constant alone. Both get an
	//     annotation rather than a smaller catalog until measured
	//     per-device bandwidth lands (#251).
	//  3. Everything that fits, so neither floor can newly turn a
	//     working host into an under-spec one.
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
		narrow(func(c candidate) bool { return c.floorOK })
	}
	if in.PreferredModelID == "" {
		narrow(func(c candidate) bool { return !c.est.UpperBound || c.est.MeetsSpeedFloor })
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
		out = append(out, p)
	}
	return out, nil
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
				reasons = append(reasons,
					fmt.Sprintf("VRAM fit: ~%.1f GB weights + KV/overhead resident within GPU0=%d MB",
						winner.Variant.EstimatedWeightGB, in.Hardware.EffectiveVRAMMB()))
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
// vLLM consults the host's engine-aware VRAM budget (VLLMVRAMBudgetMB,
// #678: a single GPU keeps Profile.EffectiveVRAMMB semantics, while
// identical multi-NVIDIA hosts aggregate across the tensor-parallel
// set). That aggregation stays here rather than in the shared package:
// the control plane sees a broadcast summary and cannot reproduce it,
// and it is a serving-topology decision rather than a fit rule. Ollama
// is never aggregated — it does not shard — so it is judged on the
// shared Host directly.
func hostFits(engine string, v catalog.Variant, hw hardware.Profile) bool {
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
		return hostfit.OllamaFit(v, hw.HostFit()).Fits
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
