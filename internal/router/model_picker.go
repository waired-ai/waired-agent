package router

import (
	"errors"
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/version"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/modelrank"
)

// PickInput is the agent's door onto the shared selection ladder
// (proto/modelrank). It carries hardware.Profile where the shared shape
// carries hostfit.Host plus a device list, because this side has a
// Profile and adapting once — here — is the whole point of the split.
//
// The ladder itself is NOT here any more. It moved to proto/modelrank so
// the control plane runs the same code rather than a mirror of it: its
// copy had grown its own candidate loop, its own spelling of narrow()
// and its own tie-break, and waired-ai/waired#986 is that mirror
// drifting — a 16 GB card defaulted to a 22.6 GB mixture of experts
// because one gate was in prose here and in code there
// (waired-agent#970).
type PickInput struct {
	Catalog  []catalog.Manifest
	Hardware hardware.Profile
	Engine   string

	// EngineVersion is the SERVING engine's version, used against
	// per-variant MinEngineVersion floors. "" = unknown, which EXCLUDES
	// floored variants: this side is about to SERVE, and a variant the
	// engine cannot load fails server-side with no useful indication.
	//
	// The control plane passes the same rule with the opposite answer
	// for unknown, because its "unknown" means "the device has not told
	// me" (modelrank.PickInput.UnknownEngineVersionPasses,
	// waired-ai/waired#1225). One rule, two meanings of silence.
	EngineVersion string

	// PreferredModelID, when non-empty, restricts the search to that
	// manifest's variants and bypasses every narrowing pass.
	PreferredModelID string

	// Measured is what specific variants actually decoded on this host,
	// keyed by catalog.VariantSHA, and FloorTokps is the rate below
	// which such a variant stops being recommended. Zero floor is "no
	// claim" and disables that pass (waired-agent#784).
	Measured   map[string]MeasuredRate
	FloorTokps float64
}

// MeasuredRate and Pick are the shared shapes, aliased so this package's
// callers keep their spellings.
type (
	MeasuredRate = modelrank.MeasuredRate
	Pick         = modelrank.Pick
)

// RankModels returns every candidate this host can run, best first.
//
// The decision is modelrank.RankModels; this adapts the agent's hardware
// shape onto the shared one, hands it over, and translates the refusals
// back into this package's sentinels.
//
// The translation is not ceremony. ErrModelNotFound and
// ErrHardwareInsufficient are this package's, not the ladder's: the mesh
// router raises the same two for routing reasons that have nothing to do
// with model selection, and the gateway maps them to HTTP status codes
// by identity. Adopting the shared sentinels would have made a mesh
// routing failure say "modelrank: ..."; keeping two identities and not
// translating would have made errors.Is stop matching at every one of
// those call sites, silently.
func RankModels(in PickInput) ([]Pick, error) {
	ranked, err := modelrank.RankModels(in.shared())
	return ranked, translateRankError(err)
}

func translateRankError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, modelrank.ErrModelNotFound):
		return fmt.Errorf("%w: %s", ErrModelNotFound, err)
	case errors.Is(err, modelrank.ErrHardwareInsufficient):
		return fmt.Errorf("%w: %s", ErrHardwareInsufficient, err)
	default:
		return err
	}
}

// shared projects the agent's input onto the published shape. The two
// hardware adapters are hardware.Profile's own, so this package holds no
// fit or sizing arithmetic of its own — which is the property that keeps
// it from drifting away from the control plane again.
func (in PickInput) shared() modelrank.PickInput {
	return modelrank.PickInput{
		Catalog:          in.Catalog,
		Host:             in.Hardware.HostFit(),
		GPUs:             in.Hardware.GPUSummaries(),
		Engine:           in.Engine,
		EngineVersion:    in.EngineVersion,
		PreferredModelID: in.PreferredModelID,
		Measured:         in.Measured,
		FloorTokps:       in.FloorTokps,
		// Left false deliberately: this side serves. See EngineVersion.
		UnknownEngineVersionPasses: false,
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
// the system-RAM gate.
//
// It used to be what deficitLabelFor branched on to say WHICH constraint
// bound. It no longer is: the label reads the verdict's own reason code
// now, because deciding that a second time from different inputs is how
// the label came to contradict the verdict (#625). What survives here is
// the residency question itself, which the tests and the UMA tier tables
// still ask directly.
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

// notRecommendedReason turns a declined hostfit.OllamaRecommendModel
// verdict into PickModel's reason-trace line. It says "not preselected",
// never "cannot run": the model reached this point because capacity
// admitted it, and stating otherwise would re-create the defect
// waired-agent#229 removed, in prose.
//
// proto/modelrank words the same verdict for its own Reasons, and this
// is a second copy of that sentence rather than a second copy of a RULE.
// PickModel builds a different, richer trace — engine, budget arithmetic,
// context-floor status — so it cannot simply carry the shared list over,
// and the sentence is a diagnostic for `waired runtimes status` rather
// than product copy. The wizard and the tray get reason CODES and word
// them themselves (waired-agent#321).
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
