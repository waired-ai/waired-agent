package router

import (
	"fmt"
	"sort"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/version"
)

// FamilyFit is the per-manifest verdict the catalog UI consumes:
// "this family's best-fit variant on this host is X" or "no variant
// fits, here's the deficit". One call per bundled manifest is cheap
// enough to run on every catalog endpoint hit.
type FamilyFit struct {
	// Variant is the representative variant for this family on this host.
	// When Fits=true it is the highest quality_tier variant that supports
	// the engine AND fits. When Fits=false it is the *least demanding*
	// engine-supported variant — the one the DeficitLabel is measured
	// against — so the catalog UI can still show recommended specs for an
	// over-capacity family. Zero value only when no variant supports the
	// engine at all (DeficitLabel "no variant supports <engine>").
	Variant catalog.Variant

	// Fits is true iff at least one variant satisfies both
	// engineSupports() and hostFits().
	Fits bool

	// DeficitLabel is a human-readable reason the family can't run on
	// this host, suitable for tray display
	// (e.g. "needs 24 GB VRAM (have 8 GB)" or "no variant supports vllm").
	// Empty when Fits=true.
	DeficitLabel string
}

// FamilyBestFit picks the best variant from one manifest given the
// host's engine (name + serving version) + hardware. When no variant
// fits, the verdict carries a deficit label derived from the *least
// demanding* engine-supported variant — that's the closest the user
// could get by upgrading — or, when the engine-version floor is what
// excludes the whole family, the lowest floor that would unlock it.
//
// Sort order for fit candidates: quality_tier desc, then min-resource
// asc, then manifest order. Mirrors PickModel's tiebreak so the catalog
// UI's "auto pick" matches what the agent would actually serve when
// pinned to this family.
func FamilyBestFit(m catalog.Manifest, engine, engineVersion string, hw hardware.Profile) FamilyFit {
	supported := make([]catalog.Variant, 0, len(m.Variants))
	for _, v := range m.Variants {
		if engineSupports(v, engine) {
			supported = append(supported, v)
		}
	}
	if len(supported) == 0 {
		return FamilyFit{DeficitLabel: fmt.Sprintf("no variant supports %s", engine)}
	}

	loadable := make([]catalog.Variant, 0, len(supported))
	for _, v := range supported {
		if engineVersionSatisfies(v, engineVersion) {
			loadable = append(loadable, v)
		}
	}
	if len(loadable) == 0 {
		// The version floor — not resources — excludes the family.
		have := engineVersion
		if have == "" {
			have = "unknown version"
		}
		return FamilyFit{
			Variant:      minResourceVariant(supported, engine),
			DeficitLabel: fmt.Sprintf("needs %s ≥ %s (running %s)", engine, lowestEngineFloor(supported), have),
		}
	}

	fits := make([]catalog.Variant, 0, len(loadable))
	for _, v := range loadable {
		if hostFits(engine, v, hw) {
			fits = append(fits, v)
		}
	}
	if len(fits) > 0 {
		sortVariantsByTier(fits, engine)
		return FamilyFit{Variant: fits[0], Fits: true}
	}

	// No fit: report the gap against the least-demanding variant the
	// engine could run.
	smallest := minResourceVariant(loadable, engine)
	return FamilyFit{Variant: smallest, DeficitLabel: deficitLabelFor(smallest, engine, hw)}
}

// lowestEngineFloor returns the smallest MinEngineVersion among vs —
// the easiest engine upgrade that unlocks the family. Caller
// guarantees at least one v carries a floor (the loadable set was
// empty).
func lowestEngineFloor(vs []catalog.Variant) string {
	low := ""
	for _, v := range vs {
		if v.MinEngineVersion == "" {
			continue
		}
		if low == "" {
			low = v.MinEngineVersion
			continue
		}
		if c, ok := version.Compare(v.MinEngineVersion, low); ok && c < 0 {
			low = v.MinEngineVersion
		}
	}
	return low
}

func sortVariantsByTier(vs []catalog.Variant, engine string) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if a.QualityTier != b.QualityTier {
			return a.QualityTier > b.QualityTier
		}
		if engine == catalog.RuntimeVLLM {
			return a.MinVRAMMB < b.MinVRAMMB
		}
		return a.MinRAMGB < b.MinRAMGB
	})
}

func minResourceVariant(vs []catalog.Variant, engine string) catalog.Variant {
	best := vs[0]
	for _, v := range vs[1:] {
		switch engine {
		case catalog.RuntimeVLLM:
			if v.MinVRAMMB < best.MinVRAMMB {
				best = v
			}
		case catalog.RuntimeOllama:
			if v.MinRAMGB < best.MinRAMGB {
				best = v
			}
		}
	}
	return best
}

func deficitLabelFor(v catalog.Variant, engine string, hw hardware.Profile) string {
	switch engine {
	case catalog.RuntimeVLLM:
		needGB := mbToGBCeil(v.MinVRAMMB)
		if len(hw.GPUs) == 0 {
			return fmt.Sprintf("needs %d GB VRAM (no GPU)", needGB)
		}
		// #678: the "have" figure is the engine-aware budget — the TP
		// aggregate on identical multi-NVIDIA hosts, GPUs[0] otherwise.
		haveGB := VLLMVRAMBudgetMB(hw) / 1024
		if tp := VLLMTensorParallelSize(hw); tp > 1 {
			return fmt.Sprintf("needs %d GB VRAM (have %d GB across %d GPUs)", needGB, haveGB, tp)
		}
		return fmt.Sprintf("needs %d GB VRAM (have %d GB)", needGB, haveGB)
	case catalog.RuntimeOllama:
		// On UMA hosts hostFits IGNORES the MinRAMGB gate and rejects
		// purely on GPU residency, so the deficit reason must too —
		// otherwise a model whose MinRAMGB exceeds the leftover system RAM
		// (e.g. qwen3.6-35b-a3b, min_ram 32, on a 16 GB Mac) mislabels as
		// "needs 32 GB RAM" when the real wall is the GPU-addressable
		// budget (#425). Reaching here on UMA means ollamaFitsVRAM
		// rejected, which only happens with EstimatedWeightGB > 0, so the
		// GPU-resident figure is always meaningful.
		if hw.UnifiedMemory {
			return fmt.Sprintf("needs ~%.0f GB GPU-resident (have %d MB VRAM)",
				v.EstimatedWeightGB, hw.EffectiveVRAMMB())
		}
		// When the RAM gate passes but the variant still doesn't fit,
		// the binding constraint is GPU residency (ollamaFitsVRAM).
		ramOK := v.MinRAMGB <= 0 || hw.RAMTotalGB <= 0 || hw.RAMTotalGB >= v.MinRAMGB
		if ramOK && !ollamaFitsVRAM(v, hw) {
			return fmt.Sprintf("needs ~%.0f GB GPU-resident (have %d MB VRAM)",
				v.EstimatedWeightGB, hw.EffectiveVRAMMB())
		}
		if hw.RAMTotalGB <= 0 {
			return fmt.Sprintf("needs %d GB RAM", v.MinRAMGB)
		}
		return fmt.Sprintf("needs %d GB RAM (have %d GB)", v.MinRAMGB, hw.RAMTotalGB)
	default:
		return "incompatible"
	}
}

// mbToGBCeil rounds MB up to the nearest GB so the deficit label
// communicates a number the user can actually compare against
// vendor specs ("24 GB card" rather than "23.4 GB").
func mbToGBCeil(mb int) int {
	if mb <= 0 {
		return 0
	}
	return (mb + 1023) / 1024
}
