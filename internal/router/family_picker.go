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
	// Superseded by Fit for everything Fit can express, which since
	// waired-agent#836 includes the engine-VERSION floor: that refusal
	// carries ReasonEngineTooOld and the two versions, so a renderer
	// reads Fit.Reason first and falls back here only for a wire written
	// by an older agent. It is kept rather than removed because both
	// wires still carry it and the CLI's `models ls --detail` prints it
	// verbatim.
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
		//
		// The verdict says so in machine form (waired-agent#836). It used
		// to be a zero-value Fit on the argument that hostfit does not
		// model an engine version, so there was "nothing true for Reason
		// to carry" — but a REFUSAL is exactly what Reason carries, and
		// ReasonNoVariantForEngine was already the precedent: a code for a
		// wall that is not the machine's memory. A row with no code is not
		// silent, it is unattributed, and every surface then guessed. The
		// tray guessed "memory" (waired-agent#850, on a 63 GB host), and
		// so did the CLI.
		//
		// The policy stays here, and only the vocabulary is shared:
		// engineVersionSatisfies fails CLOSED on an unknown version
		// because this process is about to serve. The control plane, which
		// only offers, fails open (waired-ai/waired#1225) and sets the
		// same code from its own inputs.
		//
		// NeedMB / HaveMB stay empty — naming a memory figure beside this
		// row is the thing that went wrong. The tier and the size class do
		// ride along, for the reason NoVariantForEngineModel carries them:
		// they rank the row, not its fit, so it keeps its place among its
		// neighbours instead of sinking to the bottom for owning no tier.
		//
		// The tier is the REPRESENTATIVE variant's, not the family's best.
		// That is deliberately the number this row already sorted on: with
		// Fit at its zero value, catalogRankTier fell through to
		// Recommended.QualityTier, which is projected from this same
		// variant. Taking bestQualityTier here would be a defensible
		// choice and a silent re-ordering of the catalog, which this
		// change is not about.
		representative := minResourceVariant(supported, engine)
		need := lowestEngineFloor(supported)
		return FamilyFit{
			Variant:      representative,
			DeficitLabel: engineFloorLabel(need, engineVersion, engineOnHost(hw, engine)),
			Fit: hostfit.Presentation{
				Reason:            hostfit.ReasonEngineTooOld,
				NeedEngineVersion: need,
				HaveEngineVersion: engineVersion,
				QualityTier:       representative.QualityTier,
				ModelSize:         hostfit.ModelSize(m),
			},
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

// engineFloorLabel words the engine-version deficit for a surface with
// one line: a catalog row, and the tray dialog that repeats it.
//
// It does not name the engine. Every user-facing sentence in the product
// calls it "the AI engine" — the installer, the setup wizard, the CLI's
// pull and benchmark narration — because it is not something a person
// picks: waired installs it and `waired update` converges it (#826). The
// row used to print "needs ollama ≥ 0.32.13", which is the only place
// the internal name reached a user, and it reached them at exactly the
// moment they needed to know what to DO — where "ollama" is not the
// answer (waired-agent#850 found it on a real host, waired-agent#836
// carries it). The engine's own NAME stays verbatim where it is a
// field: `waired runtimes ls`'s NAME column, the wire's engine key.
//
// The floor version stays, because it is checkable — `waired runtimes
// ls` prints the version beside it — and because a floor the fleet has
// not converged to yet is a different sentence from one it never will.
//
// Three arms, because an empty have is two different hosts.
//
// This function used to have two, on the reasoning that an unreadable
// version "invites the reader to conclude the engine is missing when it
// is installed and merely not started". Real hardware contradicted the
// premise rather than the wording: on pc-dell-premium the engine WAS
// missing, and the row said "could not be read" ten lines under a header
// that said there was no AI engine on the computer (#852). Nothing could
// read the version because there was nothing to read.
//
// So installed is asked first. The remaining two arms are unchanged: an
// unknown version on a host that HAS an engine (never started, so it was
// never probed) excludes floored variants the same way an old one does —
// the gate fails closed — but the remedy differs, and an old version is
// checkable against what `waired runtimes ls` prints beside it.
func engineFloorLabel(need, have string, installed bool) string {
	if !installed {
		return fmt.Sprintf("needs AI engine %s (no AI engine on this computer)", need)
	}
	if have == "" {
		return fmt.Sprintf("needs AI engine %s (this computer's version could not be read)", need)
	}
	return fmt.Sprintf("needs AI engine %s (this computer has %s)", need, have)
}

// engineOnHost reports whether the hardware profile says this engine is
// present, for the label above and nothing else.
//
// The daemon deliberately does NOT decide with this field — it is
// TTL-cached for 30 s, and engine_resolve.go says why that is too late
// for a fresh install. A label can afford it: being half a minute behind
// on a just-installed engine only picks a different TRUE sentence about
// a row that is refused either way.
//
// An engine kind this rule does not know answers true, so an
// unrecognised engine keeps the version-based wording rather than
// gaining an assertion about absence that nothing here checked.
func engineOnHost(hw hardware.Profile, engine string) bool {
	switch engine {
	case catalog.RuntimeOllama:
		return hw.Engines.Ollama.Installed
	case catalog.RuntimeVLLM:
		return hw.Engines.VLLM.Installed
	}
	return true
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
