package hostfit

import "github.com/waired-ai/waired-agent/proto/catalog"

// Speed codes — how a variant is expected to PERFORM here, which
// Runnable deliberately does not answer.
//
// Two negative answers, and a consumer has to be able to tell them
// apart because they are different KINDS of claim. SpeedSlow rests on
// an estimate that is an upper bound (Estimate.UpperBound), so no
// unknown hardware can rescue it and it may keep a model out of a
// recommendation. SpeedMayBeSlow rests on a population constant that is
// not a bound in either direction, so it is worth SAYING and nothing
// more — excluding on it would withhold from a fast machine what a slow
// one of the same kind runs.
//
// The empty string is "no claim", NOT "fast". It is what a resident
// discrete GPU and an unannotated variant both produce, and reading it
// as a positive claim is the mirror of the bug waired-agent#364 was:
// there, a zero Estimate was read as a confirmed-slow verdict.
//
// The vocabulary was the control plane's (it is what the setup wizard
// already words). It moves here because the agent's own pickers need
// the same three answers, and a second implementation of a three-value
// enum in the other repository is exactly how the fit rules came to
// disagree before this package existed.
const (
	SpeedSlow      = "slow"
	SpeedMayBeSlow = "may_be_slow"
)

// ReasonNoVariantForEngine says the model has no variant this engine
// can serve at all — a fact about the catalog, not about the machine.
//
// It is a fit reason rather than an absence because the surfaces have
// to render it: the tray greys such a row and says so, and the setup
// wizard used to drop it entirely, which is the half of
// waired-agent#321 that made one catalog look like two different
// catalogs depending on which picker you opened. "It cannot run here"
// and "it does not exist" read identically when the row is missing.
const ReasonNoVariantForEngine = "no_variant_for_engine"

// SpeedCode projects a decode estimate onto the wire vocabulary above.
//
// The distinction it preserves is whether the figure is an upper bound:
// only then is "too slow" a property of the computer rather than of
// what the estimator happens to know about it.
func SpeedCode(e Estimate) string {
	if e.MeetsSpeedFloor {
		return ""
	}
	if e.UpperBound {
		return SpeedSlow
	}
	return SpeedMayBeSlow
}

// Presentation is one (variant, engine, host) verdict projected onto the
// shape every model picker renders: the tray's catalog submenu,
// `waired models ls --detail`, the NAVI setup wizard, and the device
// page's model switcher.
//
// It is a PAYLOAD, and that is what separates it from Verdict. Verdict
// is a decision — tagged json:"-" because nothing marshals it and each
// consumer projected the parts it needed onto its own wire shape. That
// worked while there was one consumer per repository and stopped
// working when the answer had to look the same on four surfaces: the
// tray's wire baked English deficit prose composed agent-side and
// carried no recommendation flag, while the control plane's wire
// carried machine codes and a recommendation but no size figure for a
// row that RUNS. Same rules, incompatible shapes (waired-agent#321).
//
// So the projection is written once, here, and both wires embed it. The
// JSON names are the control plane's existing ones on purpose: adopting
// this type there adds required_resident_mb and changes nothing else.
//
// Every field is omitempty except Runnable, which is the question being
// answered and whose false is the interesting value.
type Presentation struct {
	// Runnable answers CAPACITY only — can this machine hold and run
	// this at all. It is Verdict.Fits, and it keeps that meaning: the
	// monotonicity invariant waired-agent#229 restored (adding a
	// graphics card may never REMOVE a model) lives in this bit, so a
	// consumer must not narrow it with a speed or recommendation term.
	Runnable bool `json:"runnable"`

	// Reason is the machine code for a false Runnable — the Reason*
	// vocabulary of this package, plus ReasonNoVariantForEngine. Empty
	// when it runs. Never prose: every surface owns its own wording,
	// which is the split that lets the copy be rewritten without a
	// protocol change.
	Reason string `json:"reason,omitempty"`

	// NeedMB / HaveMB are the shortfall that decided a false Runnable,
	// and only that one — naming the RAM figure when the GPU was the
	// wall sends the operator to buy the wrong hardware. Both absent
	// when it runs, and HaveMB is absent when there is no figure to
	// compare against (no card at all).
	NeedMB int `json:"need_mb,omitempty"`
	HaveMB int `json:"have_mb,omitempty"`

	// RequiredResidentMB is the honest "how much graphics memory does
	// this need" figure — weights, the reserved 16k KV budget and the
	// engine's own overhead for ollama (OllamaResidentMB), min_vram_mb
	// for vLLM.
	//
	// It is populated for rows that RUN, which is the whole reason it
	// exists: NeedMB only ever appears on a row that does not, so until
	// now every picker showing a runnable model had nothing true to
	// print and printed min_ram_gb instead — a threshold authored for a
	// host loading into system RAM.
	//
	// Zero on a host with no GPU-addressable memory at all, where the
	// quantity is not merely unknown but meaningless: the weights are
	// read from system RAM, and min_ram_gb is the figure that bounds
	// them. A consumer must render the RAM figure there rather than
	// call this one "graphics memory".
	RequiredResidentMB int `json:"required_resident_mb,omitempty"`

	// QualityTier is the maintainers' ranking of the variant this
	// verdict describes. Higher is better; it is what the pickers order
	// by, and it is deliberately a raw number rather than a coarse
	// scale — the catalog's own value, not a re-bucketing that would
	// have to be kept in step with it.
	//
	// True even where nothing else is: it ranks the MODEL, not its fit
	// on this host, so it stays meaningful for a row that cannot run
	// and for a host that has reported no hardware.
	QualityTier int `json:"quality_tier,omitempty"`

	// NotRecommended marks a model this computer CAN run but should not
	// be pointed at by default (OllamaRecommend), with
	// NotRecommendedReason carrying the code.
	//
	// Spelled negatively so the common case stays off the wire, and
	// because a consumer must read it as a demotion rather than a
	// rejection: Runnable is still true, and hiding these is the bug
	// waired-agent#229 removed. Grey them, annotate them, sort them
	// below the recommended ones — do not drop them.
	NotRecommended       bool   `json:"not_recommended,omitempty"`
	NotRecommendedReason string `json:"not_recommended_reason,omitempty"`

	// Speed and EstimatedTokps are the decode prediction. Both absent
	// when no claim is made — see the Speed* codes for why absent is not
	// "fast", and Estimate for why this package declines to price a
	// discrete GPU at all.
	//
	// Advisory: arithmetic over the catalog and the reported hardware,
	// never a measurement. A surface is expected to treat the figure as
	// an order of magnitude rather than a number to print.
	Speed          string  `json:"speed,omitempty"`
	EstimatedTokps float64 `json:"estimated_tokps,omitempty"`
}

// Project builds the Presentation for one variant on one host under one
// engine.
//
// budgetMB is the vLLM VRAM budget and is the caller's to compute, for
// the same reason VLLMFit takes it: the agent aggregates across an
// identical multi-GPU tensor-parallel set (#678) and the control plane,
// holding only the broadcast summary, passes Host.EffectiveVRAMMB. It
// is ignored for ollama, which pools through Host.OllamaVRAMBudgetMB.
//
// The recommendation is asked only for a runnable ollama row. It is a
// separate question from capacity and only meaningful for something
// that runs, and vLLM has no recommendation rule here — the same way
// the agent's own picker has none for it.
//
// An engine this package does not know yields a not-runnable row with
// no reason, which is what the control plane already did for one. It is
// deliberately NOT ReasonNoVariantForEngine: that code says the CATALOG
// has nothing for this engine, and an unrecognised engine name is a
// statement about the caller instead.
func Project(v catalog.Variant, engine string, h Host, budgetMB int) Presentation {
	out := Presentation{QualityTier: v.QualityTier}
	var got Verdict
	switch engine {
	case catalog.RuntimeOllama:
		got = OllamaFit(v, h)
		// Meaningless without GPU-addressable memory — see the field doc.
		if h.HasGPU() {
			out.RequiredResidentMB = OllamaResidentMB(v, h.UnifiedMemory)
		}
	case catalog.RuntimeVLLM:
		got = VLLMFit(v, budgetMB)
		out.RequiredResidentMB = v.MinVRAMMB
	default:
		return out
	}
	out.Runnable = got.Fits
	out.Reason = got.Reason
	out.NeedMB = got.NeedMB
	out.HaveMB = got.HaveMB
	out.Speed = SpeedCode(got.Estimate)
	out.EstimatedTokps = got.Estimate.TokpsEstimate
	if out.Runnable && engine == catalog.RuntimeOllama {
		if rec := OllamaRecommend(v, h); !rec.Fits {
			out.NotRecommended = true
			out.NotRecommendedReason = rec.Reason
		}
	}
	return out
}

// NoVariantForEngine is the row for a model this engine cannot serve at
// all. qualityTier ranks the model itself, so it is carried: the
// pickers sort by it, and a row dropped to the bottom of a list still
// has a place among its neighbours there.
//
// A constructor rather than a struct literal at each site because there
// are three of them across two repositories, and the shape of "this
// combination does not exist" is exactly the kind of agreement that
// went missing before this package.
func NoVariantForEngine(qualityTier int) Presentation {
	return Presentation{Reason: ReasonNoVariantForEngine, QualityTier: qualityTier}
}
