package router

import (
	"fmt"
	"sort"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/version"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
	//
	// Superseded by Fit for everything Fit can express, and kept for the
	// one thing it cannot: the engine-VERSION floor. hostfit deliberately
	// does not model that (it is serving-time policy the control plane has
	// no inputs for), so there is no code for it and this sentence stays
	// the only answer. A renderer therefore reads Fit.Reason first and
	// falls back here — see the tray's formatCatalogEntry.
	//
	// Worded from Fit's own NeedMB/HaveMB since #625, so the two cannot
	// disagree again. Deliberately short: a surface with room composes
	// the longer sentence from Fit and CatalogHost's own figures rather
	// than taking a second slab of prose from here (waired-agent#321 —
	// values on the wire, wording at the surface).
	DeficitLabel string

	// Fit is the shared projection of this verdict
	// (proto/hostfit.Presentation): the same shape the control plane's
	// onboarding catalog emits, so the tray, the CLI and the setup wizard
	// render one contract instead of three similar ones
	// (waired-agent#321).
	//
	// Fit.Runnable is the same answer as Fits. Both are kept because they
	// are read by different generations of consumer, and neither may
	// disagree with the other — asserted in the tests.
	Fit hostfit.Presentation
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
		// The tier still ranks the MODEL, not its fit, so it rides along:
		// the pickers sort by it, and this row is greyed at the bottom of a
		// list rather than dropped (waired-agent#321 F36).
		return FamilyFit{
			DeficitLabel: fmt.Sprintf("no variant supports %s", engine),
			Fit:          hostfit.NoVariantForEngineModel(m, bestQualityTier(m.Variants)),
		}
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
		// Fit is left at its zero value — not runnable, and no code. The
		// wall here is the engine's VERSION, which hostfit deliberately
		// does not model (it is serving-time policy the control plane has
		// no inputs for), so there is nothing true for Reason to carry.
		// Projecting the variant and then forcing Runnable=false would be
		// worse in two ways: it would print size figures beside a row the
		// memory has nothing to do with, and it would make this package the
		// second writer of a shape that is deliberately built in exactly
		// one place. DeficitLabel is the answer for this branch, and the
		// renderers fall back to it.
		return FamilyFit{
			Variant:      minResourceVariant(supported, engine),
			DeficitLabel: fmt.Sprintf("needs %s ≥ %s (running %s)", engine, lowestEngineFloor(supported), have),
		}
	}

	fits := make([]catalog.Variant, 0, len(loadable))
	for _, v := range loadable {
		if hostFits(engine, m, v, hw) {
			fits = append(fits, v)
		}
	}
	if len(fits) > 0 {
		sortVariantsByTier(fits, engine)
		return FamilyFit{
			Variant: fits[0],
			Fits:    true,
			Fit:     familyPresentation(m, fits[0], engine, hw),
		}
	}

	// No fit: report the gap against the least-demanding variant the
	// engine could run.
	//
	// The projection is built FIRST and the label is worded from it
	// (#625). They used to be two expressions in this one literal,
	// computed from different inputs, and that is exactly how a label
	// came to contradict the verdict standing next to it.
	smallest := minResourceVariant(loadable, engine)
	pres := familyPresentation(m, smallest, engine, hw)
	return FamilyFit{
		Variant:      smallest,
		DeficitLabel: deficitLabelFor(smallest, engine, hw, pres),
		Fit:          pres,
	}
}

// RecommendedFamily is the model this host would choose for ITSELF on
// this engine — the model_id a catalog UI marks "recommended".
//
// It is RankModels' own answer, not a second policy: the badge a person
// sees and the model the installer would commit to have to be the same
// answer, and the way to guarantee that is to ask the same function.
// SelectInstallModel is RankModels plus an ok flag, so this reads the
// ranking directly.
//
// It used to run a two-step ladder — SelectInstallModel above the
// quality floor, then RankModels when nothing cleared it — because the
// tier filter could empty a set that RankModels had filled. #522 removed
// the floor, so the two steps became the same query and the fallback
// became unreachable. What the ladder existed to protect still holds and
// is now structural: RankModels' narrow() falls through rather than
// returning nothing, so a host that fits anything gets a mark. A picker
// with no mark at all tells the operator nothing, and "the best this
// machine can do" is still true.
//
// Empty only when nothing fits at all, or the input is misconfigured —
// there is genuinely nothing to point at then.
func RecommendedFamily(in PickInput) string {
	ranked, err := RankModels(in)
	if err != nil || len(ranked) == 0 {
		return ""
	}
	return ranked[0].Manifest.ModelID
}

// familyPresentation projects one variant onto the shared shape, choosing
// the engine-aware budget the same way hostFits does: the
// tensor-parallel aggregate for vLLM (#678), the pool that
// Host.OllamaVRAMBudgetMB computes internally for ollama.
//
// It exists so the budget argument is chosen in exactly one place. The
// vLLM figure is the agent's own aggregate and is NOT what the control
// plane passes — it holds only the broadcast summary — which is
// precisely why the projection takes it rather than deriving it.
//
// ProjectModel rather than Project: the manifest is what prices capacity
// at the window this host would serve and what makes the recommendation
// the window question (waired-ai/waired#1056 decision 3). It is also
// what populates RequiredWindowResidentMB, the figure every surface
// prints when it says what a model needs.
func familyPresentation(m catalog.Manifest, v catalog.Variant, engine string, hw hardware.Profile) hostfit.Presentation {
	return hostfit.ProjectModel(m, v, engine, hw.HostFit(), VLLMVRAMBudgetMB(hw))
}

// bestQualityTier is the ranking of the strongest variant in a set.
//
// Used where there is no variant to pick BY — a family this engine
// cannot serve at all — so the row keeps the place in the list it would
// hold on a machine that could run it, rather than sorting to the very
// bottom for owning no tier.
func bestQualityTier(vs []catalog.Variant) int {
	best := 0
	for _, v := range vs {
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	return best
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

// deficitLabelFor words the verdict that rejected v, from the figures
// that verdict actually compared.
//
// p is the projection FamilyBestFit returns beside this label. Taking it
// rather than recomputing is the whole of waired-ai/waired-agent#625:
// the two used to be built in the same return statement from different
// inputs and drifted until the label contradicted the decision it was
// explaining. On a 16 GB Mac the row read "needs ~7 GB GPU-resident
// (have 12288 MB VRAM)" — 7 is less than 12 — beside a rejection whose
// own figures were need 10455 / have 6144.
//
// The premise the ollama arm used to rest on is the thing that expired.
// It said UMA hosts reject purely on GPU residency so the reason must
// too (#425), and that was true when written. Capacity became a
// total-memory computation (#497) and the OS deduction became a
// measurement (#568), so a unified host now rejects on the system-memory
// term like any other — see the sweep in
// docs/decisions/20260809/0016-measure-the-os-deduction-at-install.md,
// where uma16 rejects qwen3.5-4b at need 7403 / have 6144.
//
// Deliberately short — what a tray menu row and a table column can
// hold. A surface with a paragraph composes the breakdown itself from
// Fit's figures and the catalog's host block; this package does not
// grow a second slab of prose for it (waired-agent#321).
func deficitLabelFor(v catalog.Variant, engine string, hw hardware.Profile, p hostfit.Presentation) string {
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
		if p.NeedMB <= 0 || p.HaveMB <= 0 {
			// The projection could not price this pair — an unannotated
			// weight on a host whose RAM probe failed. min_ram_gb is the
			// only figure there is, and it is the one the capacity gate
			// falls back to as well.
			if v.MinRAMGB <= 0 {
				return "does not fit in this computer's memory"
			}
			return fmt.Sprintf("needs %d GB of memory", v.MinRAMGB)
		}
		switch p.Reason {
		case hostfit.ReasonInsufficientRAM:
			// The min_ram_gb fallback, for a variant the arithmetic
			// cannot price. Both figures are RAM as installed — the
			// verdict compares RAMTotalGB here, not the allocatable
			// total — so the sentence has to say RAM and not
			// "allocatable", which would claim a deduction this branch
			// never took.
			return fmt.Sprintf("needs %d GB RAM (have %d GB)",
				mbToGBCeil(p.NeedMB), mbToGBCeil(p.HaveMB))
		case hostfit.ReasonInsufficientVRAM:
			// The residency wall rather than the capacity one. Reached
			// through the variant-only entry point; named apart because
			// "allocatable" would point at system memory the GPU cannot
			// address.
			return fmt.Sprintf("needs %d GB of graphics memory (have %d GB)",
				mbToGBCeil(p.NeedMB), mbToGBCeil(p.HaveMB))
		}
		// ReasonInsufficientMemory: weights + the window's KV cache +
		// engine overhead against everything this machine can allocate —
		// RAM net of its measured OS deduction, plus dedicated VRAM.
		return fmt.Sprintf("needs %d GB — %d GB allocatable",
			mbToGBCeil(p.NeedMB), mbToGBCeil(p.HaveMB))
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
