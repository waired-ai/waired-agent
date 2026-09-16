// Package modelrank is the model-selection ladder: given a catalog and
// a host, which model should this machine be pointed at, and in what
// order do the alternatives stand behind it.
//
// It lives in proto because two sides answer that question about the
// same machine. The agent answers it for its own catalog surfaces, and
// the control plane answers it for the browser setup page — and a second
// implementation of exactly this kind of rule is how the fit rules
// drifted before proto/hostfit existed (waired-ai/waired#942). The
// control plane's copy had already grown its own spelling of narrow(),
// its own candidate loop and its own tie-break.
//
// The split from proto/hostfit is by question, not by size: hostfit
// answers "does this model fit, and would this host serve the window",
// per model; this package answers "which of them, in what order", over a
// whole catalog. Everything here reads hostfit's verdicts and never
// re-derives them.
package modelrank

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
	"github.com/waired-ai/waired-agent/proto/version"
)

// The reasons RankModels declines to produce a ranking. They are
// separate sentinels because callers act differently on them: a host
// that fits nothing is a hardware-shaped answer an installer reports as
// "no pick", while an unmet capability or an unknown model id is a
// caller-shaped one.
var (
	ErrModelNotFound        = errors.New("modelrank: model not found in catalog")
	ErrHardwareInsufficient = errors.New("modelrank: hardware does not meet variant requirements")
)

// PickInput is everything the ladder reads. Every field is an input a
// caller supplies; nothing here is discovered.
type PickInput struct {
	Catalog []catalog.Manifest

	// Host is the shared hardware shape (proto/hostfit.Host). Each side
	// adapts once — the control plane via hostfit.FromHardwareSummary,
	// the agent via its Profile adapter — and everything downstream sees
	// the same small set of numbers.
	Host hostfit.Host

	// GPUs is the per-device detail the vLLM sizing needs and Host
	// deliberately does not carry: vendor, model name, VRAM and compute
	// capability per device, for the tensor-parallel and fp8-KV rules.
	//
	// Empty is a valid input and not an error. It means "no per-device
	// detail reported", under which VLLMVRAMBudgetMB answers exactly
	// Host.EffectiveVRAMMB() and the vLLM context-floor gate passes
	// permissively — which is what a consumer with no GPU list should
	// get, and what the control plane compared against before this
	// package existed.
	GPUs []signer.HardwareGPUSummary

	Engine string

	// EngineVersion is the SERVING engine's version, used against
	// per-variant MinEngineVersion floors. "" = unknown.
	//
	// What "unknown" MEANS differs by caller, which is why
	// UnknownEngineVersionPasses exists rather than this field carrying
	// a convention.
	EngineVersion string

	// UnknownEngineVersionPasses decides what an empty EngineVersion
	// does to a variant that declares a floor.
	//
	// False (the default) fails closed, which is right for a caller
	// about to SERVE: a variant the engine cannot load fails
	// server-side with no useful indication.
	//
	// True fails open, which is right for a caller that only OFFERS and
	// whose "unknown" means "the device has not told me". Withholding
	// top-tier variants from every device there is a worse answer than
	// the silence it replaces (waired-ai/waired#1225).
	//
	// The rule is the same on both sides; only the meaning of the empty
	// string differs, so it is stated here rather than reimplemented.
	UnknownEngineVersionPasses bool

	// PreferredModelID, when non-empty, restricts the search to that
	// manifest's variants and bypasses every narrowing pass — somebody
	// pinned that model.
	PreferredModelID string

	// Three escape hatches the agent's copy of this ladder carries are
	// deliberately ABSENT here: RequireCapability, NoContextFloor and
	// NoRecommendGate. None has a production writer any more —
	// waired-agent#522 removed the tier filter that made the
	// stand-downs necessary, and the agent's own install picker records
	// that in its doc ("the recommendation gate no longer needs standing
	// down at this level"), because narrow() already falls through
	// rather than emptying the set.
	//
	// Publishing them would freeze three knobs nobody turns, which the
	// agent's decision 20260804/1937 §4 rejected in these words when it
	// declined to add a speed hatch: a field that disables nothing is
	// worse than no field. proto is additive, so the right order is to
	// add a hatch when a caller needs one — not to freeze three on the
	// chance that someone might.

	// Measured is what specific variants actually decoded on this host,
	// keyed by catalog.VariantSHA. Empty means nothing has been measured
	// here yet, which is the state every fresh install is in.
	//
	// RAW FIGURES, not verdicts, so the floor stays the caller's (see
	// FloorTokps). Keyed by VariantSHA rather than model id because the
	// rate belongs to the weights that were run: a re-quantized variant
	// is a different artifact and its predecessor's figure says nothing
	// about it.
	Measured map[string]MeasuredRate

	// FloorTokps is the rate below which a MEASURED variant stops being
	// recommended. Zero means "no claim" and disables that pass, which
	// is what every caller that does not care about speed gets by
	// leaving it unset. CodingAgentSelectionFloorTokps is the default a
	// caller with no operator setting should pass.
	FloorTokps float64

	// TurnBudgetSeconds is the line a MEASURED variant's request time is
	// held against (waired-ai/waired-agent#1341): above it, the variant
	// stops being recommended. When set it replaces FloorTokps as pass 3's
	// test, and a variant is judged on its MeasuredRate.TurnSeconds (or its
	// lower bound). Zero means "no claim". hostfit.ModelTurnBudgetSeconds is
	// what a caller passes.
	TurnBudgetSeconds float64 `json:"-"`
}

// MeasuredRate is one variant's measurement on this host.
type MeasuredRate struct {
	// Tokps is the measured decode rate.
	Tokps float64

	// TurnSeconds is one request's cost at
	// hostfit.SpeedMeasurementDepthTokens, and TurnFloorSeconds a lower
	// bound on it for a measurement that did not finish. Zero is "no
	// claim" for each.
	TurnSeconds      float64 `json:"-"`
	TurnFloorSeconds float64 `json:"-"`
}

// slowAgainstBudget is pass 3's test in seconds: over the line on the
// finished figure, or on a bound that is already past it.
func (r MeasuredRate) slowAgainstBudget(budget float64) bool {
	if budget <= 0 {
		return false
	}
	if r.TurnSeconds > 0 {
		return r.TurnSeconds > budget
	}
	return r.TurnFloorSeconds > budget
}

// Pick is the ladder's verdict for one candidate. Reasons traces the
// decision so a status surface can show why one variant won.
type Pick struct {
	Manifest catalog.Manifest
	Variant  catalog.Variant
	Reasons  []string

	// ContextFloorSatisfied reports whether this candidate meets the
	// coding-agent context floor on this host: the model's own window
	// reaches ~200k AND, on the ollama path, the serve tuning would
	// actually size that window here.
	//
	// Reported, not enforced as one gate: the manifest half is what
	// RankModels narrows on unconditionally, and the host half is part
	// of the recommendation, which a caller may stand down.
	ContextFloorSatisfied bool

	// ExpectedSpillFraction is the predicted spill fraction of serving
	// the effective floor window on this host (0 when the window fits
	// fully GPU-resident, or on non-ollama engines).
	ExpectedSpillFraction float64

	// DecodeEstimate is the roofline decode PREDICTION for this
	// candidate (zero on non-ollama engines). It is what separates "the
	// host can run this" from "the host can run this usefully" — a
	// distinction weight alone cannot make, because a dense 27B and a
	// 3B-active mixture of experts of the same size decode seven times
	// apart. It annotates; it does not exclude.
	DecodeEstimate hostfit.Estimate

	// MeasuredTokps is what this host ACTUALLY decoded with these
	// weights, 0 when nobody has run them here. Not the estimate above
	// and never a substitute for it: that one answers "what should this
	// host manage" for every catalog entry, this answers "what did it
	// manage" for the few that have been downloaded and timed.
	MeasuredTokps float64

	// MeasuredTurnSeconds is what one request with these weights cost on
	// this host, when the caller judged in seconds (PickInput.TurnBudgetSeconds):
	// the finished figure, or the lower bound of a measurement that did not
	// finish. 0 when nobody has timed them here.
	MeasuredTurnSeconds float64 `json:"-"`

	// Recommendation is hostfit.OllamaRecommendModel's verdict.
	Recommendation hostfit.Verdict
}

// RankModels returns every candidate this host can run, best first.
//
// The order is quality tier descending, then the lighter footprint, then
// catalog order — and the set has been narrowed by up to three passes,
// each of which stands down rather than empty the result.
func RankModels(in PickInput) ([]Pick, error) {
	if in.Engine == "" {
		return nil, errors.New("modelrank: RankModels requires Engine to be set")
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

	// Step 1.5: manual_only. This is THE place the field is honoured —
	// every automatic choice reaches a model through here — so one skip
	// covers all of them and none of them can forget it.
	//
	// It belongs at the manifest level, before variants are expanded,
	// for a reason a later filter could not give: a withheld model must
	// not consume a candidate slot and must not turn up in the reason
	// strings as something that was considered and rejected. It was
	// never in the running.
	//
	// PreferredModelID bypasses it because that is what the field means
	// (proto/catalog/manifest.go): withholding a model from automatic
	// choice must not break an explicit pin somebody already wrote down.
	//
	// Deliberately NOT written as a narrow() pass below: narrow falls
	// through when a pass would empty the set, which would resurrect a
	// manual-only model on exactly the host where it is the only
	// candidate — the case this exists for.
	//
	// internal_only is skipped the same way (waired-ai/waired-agent#1400).
	// Callers hand in the complete set — the agent resolves against it —
	// and the only thing that used to keep the CI fixture granite4-350m out
	// of an automatic choice was its 32k native window failing the native
	// floor, which is gone. An explicit pin still reaches it.
	withheldAll := false
	if in.PreferredModelID == "" {
		chooseable := make([]catalog.Manifest, 0, len(manifests))
		for _, m := range manifests {
			if m.ManualOnly != "" || m.InternalOnly != "" {
				continue
			}
			chooseable = append(chooseable, m)
		}
		withheldAll = len(manifests) > 0 && len(chooseable) == 0
		manifests = chooseable
	}

	// A catalog that offers this host nothing it may choose on its own is
	// a hardware-shaped answer, not a misconfiguration: callers turn
	// ErrHardwareInsufficient into "no pick" rather than into a failure.
	if len(manifests) == 0 && withheldAll {
		return nil, fmt.Errorf("%w: every candidate is manual_only (engine=%s)",
			ErrHardwareInsufficient, in.Engine)
	}
	capable := manifests

	// Steps 2+3: variant expansion + host-fit filter.
	type candidate struct {
		manifestIdx int
		manifest    catalog.Manifest
		variant     catalog.Variant
		floorOK     bool // reported: this host serves the ~200k window
		gateOK      bool // narrowed on by pass 1
		spill       float64
		est         hostfit.Estimate
		rec         hostfit.Verdict
		// measured is this host's own rate for these weights, 0 when
		// nobody has run them here. measuredSlow is that rate against
		// in.FloorTokps — narrowed on by pass 3.
		measured     float64
		measuredTurn float64
		measuredSlow bool
	}
	var fits []candidate
	for i, m := range capable {
		for _, v := range m.Variants {
			if !engineSupports(v, in.Engine) {
				continue
			}
			if !engineVersionSatisfies(v, in.EngineVersion, in.UnknownEngineVersionPasses) {
				continue
			}
			if !variantSupportedByVendor(v, in.Engine, in.GPUs) {
				continue
			}
			if !hostFits(in.Engine, m, v, in.Host, in.GPUs) {
				continue
			}
			// No speed claim by default — vLLM has no roofline model
			// here, and hostfit spells "no claim" as a passing floor.
			// Both engines start recommendable; each fills in its own
			// answer below.
			c := candidate{manifestIdx: i, manifest: m, variant: v,
				est: hostfit.Estimate{MeetsSpeedFloor: true},
				rec: hostfit.Verdict{Fits: true}}
			// The coding-agent context floor, REPORTED here: the
			// per-engine host gate — the serve tuning's own sizing on
			// ollama, the utilization-budget window check on vllm (vLLM
			// clamps instead of spilling, so no spill fraction there).
			// The model's own window used to be half of it; the catalog
			// admits only builds whose window reaches the floor since
			// waired-ai/waired-agent#1400 (decisions 3 and 4 of
			// docs/decisions/20260916/0340), so that half is gone. Which
			// pass acts on it is a separate question — see the passes.
			c.floorOK = true
			if in.Engine == catalog.RuntimeOllama {
				hostOK, spill := OllamaServesContextFloor(m, v, in.Host)
				c.spill = spill
				c.floorOK = hostOK
				c.est = hostfit.EstimateOllamaDecode(v, in.Host)
				c.rec = hostfit.OllamaRecommendModel(m, v, in.Host)
			}
			if in.Engine == catalog.RuntimeVLLM {
				c.floorOK = VLLMServesContextFloor(m, v, in.GPUs)
				// Reported, not narrowed on: both of this verdict's
				// clauses are already inside floorOK above, so pass 2
				// removes nothing pass 1 has not. What it adds is a
				// REASON on the Pick — before waired-agent#1061 the vLLM
				// arm left Recommendation permanently {Fits:true}, so a
				// caller could see a model drop out of the pool with
				// nothing saying why.
				c.rec = hostfit.VLLMRecommendModelOnHost(m, v, in.Host, in.GPUs)
			}
			// What pass 1 narrows on. On ollama the host half moved to
			// the recommendation, which a caller may stand down, and the
			// native half left with #1400, so pass 1 removes nothing
			// there; on vLLM the host half stays here, because that
			// engine has no residency or spill story for the
			// recommendation to be about.
			c.gateOK = c.floorOK
			if in.Engine == catalog.RuntimeOllama {
				c.gateOK = true
			}
			// What pass 3 narrows on. Looked up per variant rather than
			// per model: the figure belongs to the weights that were
			// run, and a model's other variants are different artifacts
			// this host has said nothing about.
			if len(in.Measured) > 0 {
				if r, ok := in.Measured[catalog.VariantSHA(v)]; ok {
					c.measured = r.Tokps
					if in.TurnBudgetSeconds > 0 {
						c.measuredTurn = r.TurnSeconds
						if c.measuredTurn <= 0 {
							c.measuredTurn = r.TurnFloorSeconds
						}
						c.measuredSlow = r.slowAgainstBudget(in.TurnBudgetSeconds)
					} else {
						c.measuredSlow = in.FloorTokps > 0 && r.Tokps > 0 && r.Tokps < in.FloorTokps
					}
				}
			}
			fits = append(fits, c)
		}
	}
	if len(fits) == 0 {
		return nil, fmt.Errorf("%w: no variant fits hardware (engine=%s)",
			ErrHardwareInsufficient, in.Engine)
	}

	// Three-pass gating, best bar first. Pass 1 is a hard filter on vLLM;
	// passes 2 and 3 fall through when they would leave nothing. An
	// explicit PreferredModelID bypasses all of it, with the status still
	// reported on the Pick.
	//
	//  1. The coding-agent context floor. It was the model's own
	//     advertised window until waired-ai/waired-agent#1400, when the
	//     catalog began admitting only builds whose window reaches ~200k
	//     (decisions 3 and 4 of docs/decisions/20260916/0340). What is left
	//     is the vLLM host gate — does the KV pool hold the 200k window
	//     here — and on ollama this pass removes nothing.
	//  2. hostfit.OllamaRecommendModel: would this host actually declare
	//     the ~200k coding window with this model? That is what
	//     "recommended" means since the 2026-08-03 owner decision
	//     (waired-ai/waired#1056 decision 3), and it is the same sizing
	//     the serve tuning exports, so the pick and the running engine
	//     agree by construction rather than by two matching comments.
	//  3. MEASURED speed: variants this host has actually run and timed
	//     over in.TurnBudgetSeconds per request (or, for a caller that
	//     still passes a rate, below in.FloorTokps) drop out. Nothing is measured until a model
	//     is on disk and the benchmark has run, so this pass is inert on
	//     a fresh install and only ever refines a set the first two
	//     already settled.
	//
	// There is no PREDICTED speed pass, and pass 3 is not one. The
	// roofline is computed and reported — it is what separates a dense
	// 27B from a 3B-active mixture of experts of the same size — but it
	// does not EXCLUDE, on any class. It rests on population bandwidth
	// constants (hostfit.BandwidthSystemRAMGBs) that ClassCPUOnly is
	// exempt from and ClassDiscrete-spilled was not, so the same number
	// excluded a 19.96 tok/s host while admitting a 17.65 tok/s one —
	// the faster machine being the one refused. And it prices decode
	// only, while a coding agent's work is ~21:1 prefill-heavy.
	//
	// A measurement carries neither defect: there is no denominator for
	// a class to be exempt from, so the ordering cannot invert, and it
	// is the rate this machine produced rather than a decode-only
	// estimate of one. That is the distinction the agent's decision
	// 20260804/1937 §4 drew when it removed the predicted pass and
	// reserved this one (waired-ai/waired-agent#466, #784).
	//
	// Passes 2 and 3 are narrow() rungs rather than hard filters for the
	// same reason: on a host where nothing clears a bar, excluding
	// everything would leave an installer with nothing to offer and the
	// machine with no local inference, which waired-ai/waired#1056
	// decision 1 forbade. Pass 1 on vLLM was one too until #1434: a build
	// whose KV pool cannot hold the 200k window is one this product does
	// not serve (owner decision 2026-09-16, waired-agent#1396), and a host
	// with no such build has no vLLM model rather than a best-effort one.
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
	if in.PreferredModelID == "" {
		// Pass 1 is a hard filter on vLLM, not a rung: a build whose KV
		// pool cannot hold the 200k session is one this host does not
		// serve (owner decision 2026-09-16, waired-agent#1396; #1434),
		// even when it is the only one. On ollama pass 1 removes nothing
		// (gateOK is always true there): the capacity gate above already
		// prices every build at the 200k rung and refuses one that does
		// not fit.
		if in.Engine == catalog.RuntimeVLLM {
			var pass []candidate
			for _, c := range fits {
				if c.gateOK {
					pass = append(pass, c)
				}
			}
			if len(pass) == 0 {
				return nil, fmt.Errorf("%w: no build holds the 200,704-token window within GPU memory (engine=%s)",
					ErrHardwareInsufficient, in.Engine)
			}
			fits = pass
		}
		narrow(func(c candidate) bool { return c.rec.Fits })
		narrow(func(c candidate) bool { return !c.measuredSlow })
	}

	// Step 5: sort by tier desc, then MinVRAM/MinRAM asc, then catalog order.
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
			MeasuredTokps:         c.measured,
			MeasuredTurnSeconds:   c.measuredTurn,
			Recommendation:        c.rec,
		}
		switch {
		case c.measuredSlow && in.TurnBudgetSeconds > 0:
			p.Reasons = append(p.Reasons, fmt.Sprintf(
				"measured %.0f s per request on this host (target: %.0f s or less)",
				c.measuredTurn, in.TurnBudgetSeconds))
		case c.measuredSlow:
			p.Reasons = append(p.Reasons, fmt.Sprintf(
				"measured %.0f tok/s on this host, below the %.0f tok/s floor",
				c.measured, in.FloorTokps))
		}
		switch {
		case c.floorOK && c.spill > 0:
			p.Reasons = append(p.Reasons, fmt.Sprintf(
				"serves the ~200k coding window with ~%.0f%% of the model expected in system RAM",
				c.spill*100))
		case !c.floorOK:
			p.Reasons = append(p.Reasons,
				"this host would serve it below the ~200k coding-agent context floor; best-effort candidate")
		}
		if !c.rec.Fits {
			p.Reasons = append(p.Reasons, notRecommendedReason(c.rec))
		}
		out = append(out, p)
	}
	return out, nil
}

// notRecommendedReason turns a declined hostfit.OllamaRecommendModel
// verdict into the ladder's reason-trace line. It says "not
// preselected", never "cannot run": the model reached this point because
// capacity admitted it, and stating otherwise would re-create the defect
// waired-agent#229 removed, in prose.
//
// These strings are diagnostics, not user-facing product copy; the
// wizard and the tray get reason CODES and word them themselves
// (waired-agent#321).
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
	case hostfit.ReasonWindowExceedsMemory:
		return "not preselected here: this host would serve it below the ~200k coding-agent " +
			"target (runs, but long sessions truncate or compact)"
	default:
		return "not preselected here"
	}
}

func engineSupports(v catalog.Variant, engine string) bool {
	for _, r := range v.RuntimeSupport {
		if r == engine {
			return true
		}
	}
	return false
}

// engineVersionSatisfies applies the per-variant MinEngineVersion floor.
// Unfloored variants always pass; floored variants need a KNOWN version
// at or above the floor. What an UNKNOWN version does is the caller's
// (PickInput.UnknownEngineVersionPasses) — the rule is the same on both
// sides, the meaning of the empty string is not.
func engineVersionSatisfies(v catalog.Variant, engineVersion string, unknownPasses bool) bool {
	if v.MinEngineVersion == "" {
		return true
	}
	if engineVersion == "" {
		return unknownPasses
	}
	return version.AtLeast(engineVersion, v.MinEngineVersion)
}

func variantSupportedByVendor(v catalog.Variant, engine string, gpus []signer.HardwareGPUSummary) bool {
	if v.VendorSupport == nil {
		return true
	}
	vendor := primaryGPUVendor(gpus)
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

func primaryGPUVendor(gpus []signer.HardwareGPUSummary) string {
	if len(gpus) == 0 {
		return ""
	}
	return strings.ToLower(gpus[0].Vendor)
}

// hostFits is the capacity gate — certain OOM, the one refusal
// waired-ai/waired#1056 decision 1 reserves.
func hostFits(
	engine string, m catalog.Manifest, v catalog.Variant,
	host hostfit.Host, gpus []signer.HardwareGPUSummary,
) bool {
	switch engine {
	case catalog.RuntimeVLLM:
		// Pre-hostfit this branch answered "fits" for a variant with no
		// declared minimum even on a host with no GPU at all; hostfit
		// reports no_gpu there instead. Unreachable in practice — every
		// bundled vLLM variant declares min_vram_mb, and vllm is only
		// selected on an NVIDIA host — and "it fits" was the worse
		// answer of the two.
		return hostfit.VLLMFit(v, VLLMVRAMBudgetMB(host, gpus)).Fits
	case catalog.RuntimeOllama:
		return hostfit.OllamaCapacityFit(m, v, host).Fits
	default:
		// Unknown engine: be conservative.
		return false
	}
}
