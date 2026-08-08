package setup

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// InstallDiskHeadroomGB is the slack added on top of a model's estimated
// weight size when pre-flighting free disk at install (#517): partial /
// temp download files, the engine binary, and a margin so a fresh box
// isn't driven to 0 bytes free. Decimal GB to match catalog weight units.
const InstallDiskHeadroomGB = 3.0

// BundledModelSelection is SelectBundledModel's verdict: which bundled
// model to configure, whether local inference should run at all on this
// host, whether the weight pull should be skipped now (disk-short), plus
// operator-facing notes explaining the decision.
type BundledModelSelection struct {
	// ModelID is the manifest model_id to configure for pre-pull. On a
	// host below the recommended spec (EnableInference=false) it is left
	// as the caller's configured id (unused while inference is off).
	ModelID string

	// EnableInference is false when the host is below the recommended
	// spec — nothing clears the coding-quality floor — and the operator
	// did not force inference on. It is the install-time DEFAULT for the
	// local-inference toggle, not a decision the user is stuck with: the
	// node enrolls, runs as a gateway/relay, routes inference to peers,
	// and can turn local inference on at any time (#465).
	EnableInference bool

	// BelowRecommendedSpec is true when no model above the coding-quality
	// floor fits the host (the branch that sets EnableInference=false
	// unless forced/pinned).
	BelowRecommendedSpec bool

	// BelowFloorModelID is the best-fitting model BELOW the coding-quality
	// floor when the host is below the recommended spec but can still run
	// *something*; "" when even the smallest catalog model won't fit. The
	// caller uses it to offer local inference on the tiny model as a
	// deliberate, low-quality opt-in rather than silently disabling.
	BelowFloorModelID string

	// SkipPull is true when the selected model fits memory but not free
	// disk: keep it configured, but don't pull now (avoid a mid-download
	// "disk full"). The caller turns off the foreground/startup pull and
	// surfaces the retry hint; a manual `waired models pull` works once
	// disk is freed.
	SkipPull bool

	// Notes are operator-facing lines (the selection summary, an
	// below-recommended-spec / disk warning, the retry hint).
	Notes []string
}

// BundledModelInputs is the world SelectBundledModel reasons over. Every
// side-effecting probe (hardware profile, free disk) is passed in, so the
// selection itself is a pure, table-testable function — matching the seam
// style of Deploy.
type BundledModelInputs struct {
	Hardware  hardware.Profile
	Manifests []catalog.Manifest

	// Inference supplies PreferredEngine and the configured
	// BundledModelID used as the pin / fallback id.
	Inference agentconfig.InferenceConfig

	StateDir string

	FloorTier int

	// Forced is set when the operator forced inference on
	// (--inference-enabled=true): honour the choice even on a host below
	// the recommended spec rather than defaulting it off.
	Forced bool

	// Pinned is set when the operator pinned a model id
	// (--inference-bundled-model-id): skip auto-selection and the
	// below-recommended-spec default, treating the pin as a deliberate choice.
	Pinned bool

	// FreeDiskBytes is the disk-probe seam. Production wires
	// hardware.FreeDiskBytes; tests inject a fixed value. nil disables the
	// disk pre-flight (selection proceeds on memory-fit alone).
	FreeDiskBytes func(path string) (int64, error)
}

// SelectBundledModel picks the bundled model to pre-pull at install time
// from the host's hardware profile (#517). It:
//
//  1. picks the engine (router.PickEngine);
//  2. selects the largest catalog model that fits the host AND clears the
//     coding-quality floor (router.SelectInstallModel);
//  3. pre-flights free disk at the download target and steps down to a
//     smaller-but-still-above-floor model when the best fit won't fit
//     disk (or skips the pull when even the smallest won't);
//  4. on a host below the recommended spec (nothing above the floor fits) reports
//     EnableInference=false with an actionable warning — unless the
//     operator pinned a model or forced inference on.
//
// It reuses the runtime fit/scoring machinery wholesale; the only
// install-specific logic is the quality floor and the disk pre-flight.
func SelectBundledModel(in BundledModelInputs) (BundledModelSelection, error) {
	sel := BundledModelSelection{
		ModelID:         in.Inference.BundledModelID,
		EnableInference: true,
	}

	// Operator pinned a specific model: honour it verbatim, skip
	// auto-selection and the below-recommended-spec default. The deploy-time defensive
	// disk check still guards a mid-download "disk full".
	if in.Pinned {
		// A pin naming a retired entry moves to the successor (#200) — an
		// installer flag or a preseeded config is written once and reused
		// across releases, so it outlives the catalog it was written
		// against. Said out loud in Notes, which is this function's channel
		// for "we did something you did not literally ask for".
		requested := sel.ModelID
		if m, retired, found := catalog.ResolveModel(requested, in.Manifests); found && retired.SuccessorModelID != "" {
			sel.ModelID = m.ModelID
			sel.Notes = append(sel.Notes, catalog.RetirementNotice(requested, retired))
		}
		sel.Notes = append(sel.Notes, fmt.Sprintf(
			"using pinned bundled model %q (hardware auto-selection skipped)", sel.ModelID))
		if m, found := catalog.LookupByAlias(sel.ModelID, in.Manifests); found && !router.MeetsNativeContextFloor(m) {
			sel.Notes = append(sel.Notes, fmt.Sprintf(
				"pinned model's native window is %d tokens — the ~200k coding-agent context floor is not enforced for pins",
				m.ContextLength))
		}
		return sel, nil
	}

	enginePick, err := router.PickEngine(router.EnginePickInput{
		Hardware:   in.Hardware,
		Preference: in.Inference.PreferredEngine,
		Catalog:    in.Manifests,
	})
	if err != nil {
		return sel, fmt.Errorf("pick engine: %w", err)
	}
	engine := enginePick.Engine

	engineVer := ""
	if engine == catalog.RuntimeOllama {
		engineVer = infruntime.OllamaPinnedVersion
	}

	above, ok, err := router.SelectInstallModel(router.PickInput{
		Catalog:       in.Manifests,
		Hardware:      in.Hardware,
		Engine:        engine,
		EngineVersion: engineVer,
	}, in.FloorTier)
	if err != nil {
		return sel, fmt.Errorf("select install model: %w", err)
	}

	if !ok {
		// Below the recommended spec: no model above the coding-quality floor fits.
		sel.BelowRecommendedSpec = true
		// Does anything fit at all, below the floor? On a 2–4 GB host the tiny
		// 0.5B may still run; expose it so the caller can offer local inference
		// on it as a deliberate low-quality opt-in rather than silently
		// disabling. (Guaranteed sub-floor: a tier ≥ floor variant would have
		// satisfied the floor pick above.)
		if below, belowOK, derr := router.SelectInstallModel(router.PickInput{
			Catalog:       in.Manifests,
			Hardware:      in.Hardware,
			Engine:        engine,
			EngineVersion: engineVer,
		}, 1); derr == nil && belowOK && len(below) > 0 {
			sel.BelowFloorModelID = below[0].Manifest.ModelID
		}
		if in.Forced {
			// Forcing inference on answers WHETHER local inference runs,
			// not WHICH model runs it. There is no compiled-in bundled
			// model to inherit any more, so on a fresh forced install the
			// configured id is empty and the below-floor fit is the only
			// honest answer — the one model this host can actually load.
			// Leaving it empty would boot inference with nothing to serve.
			if sel.ModelID == "" {
				sel.ModelID = sel.BelowFloorModelID
			}
			sel.Notes = append(sel.Notes, fmt.Sprintf(
				"hardware is below the recommended spec for a usable coding model, but inference was forced on — %q may fail to load (%s)",
				sel.ModelID, describeHardwareFit(in.Hardware, engine)))
			return sel, nil
		}
		sel.EnableInference = false
		// Both branches speak. This used to say nothing at all when a
		// below-floor model DID fit, deferring to "the interactive
		// opt-in dialog, or a non-interactive left-disabled note" —
		// neither of which was ever built. The result was that the host
		// that could run SOMETHING got less explanation than the host
		// that could run nothing (#465).
		sel.Notes = append(sel.Notes,
			"local inference starts off: this host is below the recommended spec for a usable coding model "+
				belowRecommendedSpecNeed(in.Manifests, engine, in.FloorTier, in.Hardware)+".")
		if sel.BelowFloorModelID != "" {
			sel.Notes = append(sel.Notes, fmt.Sprintf(
				"%s does fit and can be run here — its coding quality is very low, "+
					"but it is a real choice. Turn local inference on with `waired inference on`.",
				sel.BelowFloorModelID))
		} else {
			sel.Notes = append(sel.Notes,
				"This node still enrolls and runs as a gateway/relay — it can route inference to "+
					"mesh peers. To run models here, install a capable engine with "+
					"`waired runtimes install`, then `waired inference on`.")
		}
		return sel, nil
	}

	// A fitting model above the floor exists. Pick the best that also fits
	// free disk; fall back to the smallest-above-floor with the pull
	// skipped when even that won't fit disk.
	chosen, skipPull, notes := applyDiskPreflight(in, above, engine)
	sel.ModelID = chosen.Manifest.ModelID
	sel.SkipPull = skipPull
	sel.Notes = append(sel.Notes, notes...)
	return sel, nil
}

// applyDiskPreflight walks the above-floor candidates (best first) and
// returns the highest-tier one whose weights + headroom fit free disk at
// the download target. When even the smallest won't fit, it returns that
// smallest with skipPull=true so the caller keeps it configured but does
// not pull now. With no FreeDiskBytes seam the check is skipped and the
// best fit is taken as-is.
func applyDiskPreflight(in BundledModelInputs, above []router.Pick, engine string) (chosen router.Pick, skipPull bool, notes []string) {
	best := above[0]
	if in.FreeDiskBytes == nil {
		notes = append(notes, selectionNote(best, in.Hardware, engine))
		return best, false, notes
	}

	dir := bundledModelsDir(in)
	free, err := in.FreeDiskBytes(dir)
	if err != nil {
		// Probe failed: don't block the install on an unreadable path —
		// proceed with the memory-best pick and let deploy's defensive
		// check (and the pull itself) be the backstop.
		notes = append(notes,
			fmt.Sprintf("could not check free disk at %s (%v); proceeding without a disk pre-flight", dir, err),
			selectionNote(best, in.Hardware, engine))
		return best, false, notes
	}

	for i := range above {
		if free >= diskRequiredBytes(above[i].Variant) {
			notes = append(notes, selectionNote(above[i], in.Hardware, engine))
			if i > 0 {
				notes = append(notes, fmt.Sprintf(
					"stepped down from %q to fit %s free disk at %s",
					best.Manifest.ModelID, download.HumanBytes(free), dir))
			}
			return above[i], false, notes
		}
	}

	// Nothing above the floor fits disk: keep the smallest configured but
	// skip the pull so we don't fail mid-download.
	smallest := above[len(above)-1]
	notes = append(notes, fmt.Sprintf(
		"insufficient free disk at %s (%s) for any fitting model; skipping the model download. "+
			"Free up space, then run `waired models pull %s`.",
		dir, download.HumanBytes(free), smallest.Manifest.ModelID))
	return smallest, true, notes
}

// diskRequiredBytes is a variant's estimated on-disk footprint plus the
// install headroom, in bytes. A zero/unknown weight yields just the
// headroom (we never reject on an unknown size).
func diskRequiredBytes(v catalog.Variant) int64 {
	return int64((v.EstimatedWeightGB + InstallDiskHeadroomGB) * 1e9)
}

// bundledModelsDir resolves the directory the selected model would be
// pulled into, so the disk pre-flight checks the right filesystem. The
// waired-managed engine keeps its blobs under the state dir and nowhere
// else (#489).
func bundledModelsDir(in BundledModelInputs) string {
	return filepath.Join(infruntime.BundledOllamaDir(in.StateDir), "models")
}

// selectionNote renders the one-line "selected X for your hardware" note,
// plus the #624 context-floor status when it is worth stating: a bounded
// spill (working configuration, phrased informationally) or a best-effort
// pick below the ~200k coding floor.
func selectionNote(p router.Pick, hw hardware.Profile, engine string) string {
	note := fmt.Sprintf("selected bundled model %q (quality_tier %d) for %s via %s",
		p.Manifest.ModelID, p.Variant.QualityTier, describeProfile(hw), engine)
	switch {
	case p.ContextFloorSatisfied && p.ExpectedSpillFraction > 0:
		note += fmt.Sprintf("; serves a ~200k coding context with ~%.0f%% of the model expected in system RAM (larger window traded for some decode speed)",
			p.ExpectedSpillFraction*100)
	case !p.ContextFloorSatisfied:
		note += fmt.Sprintf("; below the ~200k coding-agent context floor (native window %d tokens) — best-effort pick for this hardware",
			p.Manifest.ContextLength)
	}
	return note
}

// underSpecNeed renders the actionable "what's needed" tail of the
// below-recommended-spec warning, derived from the least-demanding above-floor
// variant the engine could run.
func belowRecommendedSpecNeed(manifests []catalog.Manifest, engine string, floor int, hw hardware.Profile) string {
	minRAM, minVRAM := smallestAboveFloorReq(manifests, engine, floor)
	switch engine {
	case catalog.RuntimeVLLM:
		if minVRAM <= 0 {
			return "(no fitting GPU variant)"
		}
		return fmt.Sprintf("(needs ≥ %d GB VRAM; this host has %d MB)",
			(minVRAM+1023)/1024, router.VLLMVRAMBudgetMB(hw))
	default:
		// Not min_ram_gb. That is a hand-authored opinion, and since #552
		// the gate refuses on a computed figure — the memory a variant
		// needs to hold its SERVED window — so quoting the annotation
		// would name a spec that does not match the verdict. An 8 GB host
		// was being told it "needs ≥ 4 GB RAM".
		if gb := smallestAboveFloorRAMGB(manifests, engine, floor, hw.UnifiedMemory); gb > 0 {
			return fmt.Sprintf("(the smallest coding model needs ≥ %d GB of RAM to serve a %d-token window; this host has %d GB)",
				gb, hostfit.ServingWindow200k, hw.RAMTotalGB)
		}
		if minRAM <= 0 {
			return ""
		}
		return fmt.Sprintf("(needs ≥ %d GB RAM; this host has %d GB)", minRAM, hw.RAMTotalGB)
	}
}

// smallestAboveFloorRAMGB is the least total system RAM any above-floor
// variant would need to hold its served window here, in whole GB and
// including the OS allowance the capacity gate deducts. 0 when nothing
// above the floor can be priced.
//
// It is the same arithmetic the refusal was made with, so the number an
// operator reads is the number they would have to beat.
func smallestAboveFloorRAMGB(manifests []catalog.Manifest, engine string, floor int, unifiedMemory bool) int {
	best := 0
	for _, m := range manifests {
		ceiling := hostfit.OllamaCeilingWindow(m)
		if ceiling <= 0 {
			continue
		}
		for _, v := range m.Variants {
			if v.QualityTier < floor || !slices.Contains(v.RuntimeSupport, engine) {
				continue
			}
			need := hostfit.OllamaWindowResidentMB(v, ceiling, unifiedMemory)
			if need <= 0 {
				continue
			}
			gb := (need+1023)/1024 + hostfit.OSMemoryAllowanceGB
			if best == 0 || gb < best {
				best = gb
			}
		}
	}
	return best
}

// smallestAboveFloorReq returns the smallest MinRAMGB (ollama) / MinVRAMMB
// (vllm) among engine-supported variants at or above the floor tier — the
// closest spec an operator could upgrade to. Zero when none exists.
func smallestAboveFloorReq(manifests []catalog.Manifest, engine string, floor int) (minRAMGB, minVRAMMB int) {
	for _, m := range manifests {
		for _, v := range m.Variants {
			if v.QualityTier < floor {
				continue
			}
			if !slices.Contains(v.RuntimeSupport, engine) {
				continue
			}
			if v.MinRAMGB > 0 && (minRAMGB == 0 || v.MinRAMGB < minRAMGB) {
				minRAMGB = v.MinRAMGB
			}
			if v.MinVRAMMB > 0 && (minVRAMMB == 0 || v.MinVRAMMB < minVRAMMB) {
				minVRAMMB = v.MinVRAMMB
			}
		}
	}
	return minRAMGB, minVRAMMB
}

// describeProfile is a terse hardware summary for the selection note.
func describeProfile(hw hardware.Profile) string {
	if len(hw.GPUs) == 0 {
		return fmt.Sprintf("CPU host (%d GB RAM)", hw.RAMTotalGB)
	}
	g := hw.GPUs[0]
	label := g.Model
	if label == "" {
		label = g.Vendor
	}
	if hw.UnifiedMemory {
		return fmt.Sprintf("%s (%d MB usable VRAM, unified memory)", label, hw.EffectiveVRAMMB())
	}
	if n := len(hw.GPUs); n > 1 {
		// #678: make additional devices visible; the MB figure stays
		// per-device, because both engines aggregate but neither does
		// so as a plain sum — vLLM only across an identical
		// tensor-parallel set, ollama only across one ml.ByLibrary
		// group and net of a per-card reservation (#264). "×2 (24 GB
		// each)" is true for every engine; one summed number would be
		// true for none. Reasons that need the engine's own budget say
		// so explicitly (see router.deficitLabelFor).
		return fmt.Sprintf("%s ×%d (%d MB VRAM each)", label, n, hw.EffectiveVRAMMB())
	}
	return fmt.Sprintf("%s (%d MB VRAM)", label, hw.EffectiveVRAMMB())
}

// describeHardwareFit is a short reason phrase for the forced-on warning.
func describeHardwareFit(hw hardware.Profile, engine string) string {
	if engine == catalog.RuntimeVLLM {
		return fmt.Sprintf("%d MB VRAM", router.VLLMVRAMBudgetMB(hw))
	}
	return fmt.Sprintf("%d GB RAM", hw.RAMTotalGB)
}
