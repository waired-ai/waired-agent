package router

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/version"
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
			c := candidate{manifestIdx: i, manifest: m, variant: v}
			// #624 coding-agent context floor: native window plus the
			// per-engine host gate — bounded-spill on ollama, the
			// utilization-budget window check on vllm (#675/#678; vLLM
			// clamps instead of spilling, so no spill fraction there).
			c.floorOK = MeetsNativeContextFloor(m)
			if in.Engine == catalog.RuntimeOllama {
				hostOK, spill := OllamaServesContextFloor(m, v, in.Hardware)
				c.spill = spill
				c.floorOK = c.floorOK && hostOK
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

	// Two-pass floor gating (#624). Auto-selection keeps only the
	// candidates that satisfy the coding-agent context floor; when NONE
	// do (small hosts), every fitting candidate stays as a best-effort
	// fallback so the floor never newly disables inference. An explicit
	// PreferredModelID bypasses the floor entirely — the user asked for
	// that model — with the floor status still reported on the Pick.
	if in.PreferredModelID == "" && !in.NoContextFloor {
		var pass []candidate
		for _, c := range fits {
			if c.floorOK {
				pass = append(pass, c)
			}
		}
		if len(pass) > 0 {
			fits = pass
		}
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

// hostFits is the per-engine fit predicate. vllm consults the host's
// engine-aware VRAM budget (VLLMVRAMBudgetMB, #678: single GPU keeps
// Profile.EffectiveVRAMMB semantics — UnifiedMemory hosts like Apple
// Silicon / Strix Halo use UsableVRAMMB instead of the raw VRAMTotalMB
// so OS-reserved memory isn't double-counted — while identical
// multi-NVIDIA hosts aggregate across the tensor-parallel set). Ollama
// consults RAMTotalGB on non-UMA hosts and additionally, on discrete-GPU
// hosts, requires the variant to fit GPU-resident (ollamaFitsVRAM) —
// never aggregated, ollama does not shard. On UMA hosts the model loads
// into the GPU-addressable pool, so the residency check against
// EffectiveVRAMMB governs and the MinRAMGB gate is skipped (see below).
// A variant with no declared minimum (0) trivially fits.
func hostFits(engine string, v catalog.Variant, hw hardware.Profile) bool {
	switch engine {
	case catalog.RuntimeVLLM:
		if v.MinVRAMMB <= 0 {
			return true
		}
		eff := VLLMVRAMBudgetMB(hw)
		if eff <= 0 {
			return false
		}
		return eff >= v.MinVRAMMB
	case catalog.RuntimeOllama:
		// On UMA carve-out hosts (Strix Halo, Apple Silicon) the weights
		// load into the GPU-addressable pool (UsableVRAMMB), not the
		// leftover system RAM. On a BIOS carve-out box RAMTotalGB reports
		// only the ~31 GB the OS keeps after a 96 GB iGPU allocation, so
		// the MinRAMGB gate (authored for a host that loads into system
		// RAM) would wrongly reject every large MoE. Skip it on UMA and
		// let ollamaFitsVRAM's residency check against EffectiveVRAMMB be
		// authoritative. Discrete-GPU and CPU-only paths are unchanged.
		if !hw.UnifiedMemory {
			// RAMTotalGB == 0 likely means detection failed (e.g. non-Linux
			// host); treat as "skip the fit check rather than reject all".
			if v.MinRAMGB > 0 && hw.RAMTotalGB > 0 && hw.RAMTotalGB < v.MinRAMGB {
				return false
			}
		}
		return ollamaFitsVRAM(v, hw)
	default:
		// Unknown engine: be conservative.
		return false
	}
}

// Inputs of the ollama VRAM-residency fit check (ollamaFitsVRAM).
const (
	// ollamaKVBudgetTokens is the KV-cache budget reserved at fit time.
	// 16k tokens is the floor for useful coding-agent context; variants
	// whose weights leave less than that spill layers to the CPU.
	ollamaKVBudgetTokens = 16384

	// Discrete-GPU overhead model: base + per-weight slope, replacing
	// the old flat 4096 MiB. The flat constant was calibrated against
	// an ollama-defaults load (f16 KV, no flash attention); the #621
	// serve tuning always spawns with OLLAMA_FLASH_ATTENTION=1, which
	// shrinks the compute graph substantially. Measured on a 24 GB
	// RTX PRO 4000 (docs/reports/20260704-mtp-vs-spill-24gb.md):
	// qwen3.6-35b mtp (22.62 GB weights) shows ~1.9 GB effective
	// overhead — the flat 4096 was floor()ing the context window to
	// 32768 while 114688 demonstrably fit. Single-point calibration:
	// base 1024 (device context, matches the UMA measurement below) +
	// 40 MiB per decimal GB of weights (compute/scratch buffers scale
	// with layer width). 22.62 GB → 1024+904 ≈ 1928 MiB. If a card
	// family under-reserves, the #621 post-load verify probe detects
	// the spill and shrinks the window — that safety net is what makes
	// the optimistic calibration acceptable.
	ollamaVRAMOverheadBaseDiscreteMB = 1024
	ollamaVRAMOverheadPerWeightGBMB  = 40
	// ollamaVRAMOverheadUnknownWeightMB is the conservative fallback
	// when the variant has no weight annotation (keeps the historical
	// flat reservation).
	ollamaVRAMOverheadUnknownWeightMB = 4096

	// ollamaVRAMOverheadUMAMB is the UMA (Apple Silicon / Strix Halo)
	// counterpart. A unified-memory host has no multi-GB device context
	// to reserve — the model lives in the shared pool, so the only
	// beyond-weights cost is the compute/scratch graph. The discrete
	// 4 GB constant ~2× over-estimated Metal: on a real Apple M4,
	// qwen2.5-coder-7b q4 (4.7 GB weights) resided at runner.vram=4.4 GiB,
	// yet the discrete math budgeted ~9 GB and collapsed an 8 GB Mac's
	// auto-pick to a 1.9 GB model (#424). 1024 MB is the largest reduction
	// that still keeps the real-M4-confirmed 16 GB pick (qwen3.5-9b) and
	// the qwen2.5-coder-14b GPU-residency rejection intact. Strix Halo
	// (UMA HIP/Vulkan) shares the unified-memory argument; its value is
	// extrapolated from the Metal measurement pending a real-host probe.
	ollamaVRAMOverheadUMAMB = 1024
)

// OllamaVRAMOverheadMB returns the fit-time overhead reservation for the
// host: the small flat UMA constant on unified-memory hosts (Apple
// Silicon / Strix Halo), the weight-scaled discrete model otherwise
// (base + slope, falling back to the conservative flat reservation when
// the weight is unknown, #624). Keyed on UnifiedMemory — the same axis
// EffectiveVRAMMB() uses — so the overhead matches the budget it is
// compared against (#424). Exported so the #621 context-length clamp
// subtracts the same overhead the fit gate assumes; scoring's
// MaxContextTokens counts RAW weights precisely because the whole
// overhead lives in this budget subtraction (never double-count).
func OllamaVRAMOverheadMB(hw hardware.Profile, weightGB float64) int {
	if hw.UnifiedMemory {
		return ollamaVRAMOverheadUMAMB
	}
	if weightGB <= 0 {
		return ollamaVRAMOverheadUnknownWeightMB
	}
	return ollamaVRAMOverheadBaseDiscreteMB + int(float64(ollamaVRAMOverheadPerWeightGBMB)*weightGB)
}

// ollamaFitsVRAM reports whether v fits fully resident in the host's
// GPU-addressable budget. The system-RAM gate alone let multi-GB models
// "fit" hosts whose GPU could never hold them — ollama then silently
// spills layers to the CPU and decode collapses to single-digit tok/s
// (a 120 GB-RAM host with a 24 GB card would auto-pick a 62 GB model).
//
// On discrete-GPU hosts the budget is GPUs[0].VRAMTotalMB; on UMA hosts
// (Strix Halo, Apple Silicon) it is UsableVRAMMB — both surfaced via
// EffectiveVRAMMB(). UMA hosts run the same residency math (the model
// still has to fit the GPU-addressable pool, which on a BIOS carve-out
// is the 96 GB iGPU allocation, NOT the leftover system RAM) but reserve
// a smaller engine overhead — they have no discrete device context (see
// ollamaVRAMOverheadMB). CPU-only hosts spill to system RAM by design,
// and variants without a weight annotation or with an unknown budget
// fall back to the RAM gate.
func ollamaFitsVRAM(v catalog.Variant, hw hardware.Profile) bool {
	if len(hw.GPUs) == 0 && !hw.UnifiedMemory {
		return true // CPU-only: spilling is the design; RAM gate governs.
	}
	if v.EstimatedWeightGB <= 0 {
		return true
	}
	eff := hw.EffectiveVRAMMB()
	if eff <= 0 {
		return true // budget unknown: don't reject the whole catalog
	}
	weightMiB := int(math.Ceil(v.EstimatedWeightGB * 1e9 / (1 << 20)))
	kvMiB := v.KVBytesPerTokenFP16 * ollamaKVBudgetTokens / (1 << 20)
	return weightMiB+kvMiB+OllamaVRAMOverheadMB(hw, v.EstimatedWeightGB) <= eff
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
