package hostfit

import "github.com/waired-ai/waired-agent/proto/catalog"

// Window sizing: how large a context window this host would actually
// serve a model at, and therefore which window it may declare.
//
// This is one arithmetic, in one place, on purpose. It used to live in
// the agent alone — the serve tuning sized the window
// (cmd/waired-agent/inference_ollama_tuning.go) and the picker's context
// gate re-derived a yes/no from the same numbers — while the control
// plane's onboarding wizard had neither and recommended models the
// machine on the other end would then decline to serve at that window.
// Since the 2026-08-03 owner decision "recommended" MEANS "this host can
// declare ServingWindow200k with this model" (waired-ai/waired#1056
// decision 3; `waired` docs/decisions/20260803/1332-hard-vs-soft-model-limits.md),
// so the recommendation and the tuning cannot be allowed to disagree:
// they now call the same function.
//
// The calibration constants below were measured by the agent and are
// carried here unchanged. Nothing in this file is a new number — the
// decision explicitly ruled out inventing one ("×1.2 等の新定数は作らない").

// KV-cache quantization factors relative to fp16, matching ollama's
// OLLAMA_KV_CACHE_TYPE options. The serve tuning exports q8_0 on every
// coding path, so that is what the window arithmetic is priced at.
const (
	OllamaKVFactorF16  = 1.0
	OllamaKVFactorQ8_0 = 0.5
)

const (
	// OllamaSpillCalibration maps the byte-math spill prediction to
	// ollama's own /api/ps accounting. Single-point calibration on the
	// 24 GB anchor host: predicted 3.9 % ↔ measured 13.5 %
	// (docs/reports, waired-ai/waired-agent#625).
	OllamaSpillCalibration = 3.0

	// OllamaMaxExpectedSpillFraction bounds the expected measured spill
	// the window sizing will deliberately CREATE to reach the coding
	// window. Derived from the #664 A/B on the anchor host, where the
	// spilled fraction executes on a single CPU thread: no-spill decode
	// 158.6 tok/s, 13.4 % measured spill → ~85 tok/s. Modelling
	// 1/rate = (1-s)/158.6 + s/21.25 keeps decode at or above the 60
	// tok/s selection floor while measured spill stays under ~0.25, i.e.
	// expected ≤ ~0.22 at the anchor's expected↔measured ratio.
	//
	// It bounds the spill this sizing CHOOSES. It is not a ceiling on
	// spill in general — see OllamaPlannedWindow's floor, which lets a
	// host exceed it up to the point where removing its accelerator
	// would have put it anyway.
	OllamaMaxExpectedSpillFraction = 0.20

	// OSMemoryAllowanceGB is what the operating system needs of system
	// RAM before any model is loaded, and the only deduction the CAPACITY
	// gate takes (Host.TotalMemoryMB). It is the same 2 GB the catalog's
	// own min_ram_gb suggestions are authored with
	// (scoring.SuggestMinRAMGB), which is what makes replacing that field
	// with a computation a like-for-like swap rather than a loosening.
	OSMemoryAllowanceGB = 2

	// OllamaCPUOnlyRAMHeadroomGB is the system headroom left out of the
	// window SIZING budget when the weights are read from system RAM: the
	// OS allowance above plus as much again for the agent, the engine's
	// own process, and page cache.
	//
	// Sizing is allowed a comfort margin that a refusal is not. Charging
	// the full 4 GB in the capacity gate would refuse an 8 GB laptop a
	// 3.4 GB model it runs perfectly well; charging only 2 GB when sizing
	// a window would hand the engine a budget the machine cannot honour.
	// Different questions, deliberately different margins.
	OllamaCPUOnlyRAMHeadroomGB = 2 * OSMemoryAllowanceGB

	// OllamaMinContextTokens is the smallest window the tuning ever
	// exports — the pinned engine's own default. Going below it would
	// make hosts strictly worse than exporting nothing at all.
	OllamaMinContextTokens = 32768
)

// MaxContextTokens returns the largest window L (rounded down to a
// multiple of 1024) such that
//
//	weightGB + kvBytesPerTokFP16 × kvFactor × L / 1e9  ≤  budgetGB
//
// i.e. the biggest window whose weights and KV cache fit budgetGB
// without spilling. Weights are counted RAW because this pairs with an
// engine-overhead reservation the caller has already subtracted from the
// budget (OllamaVRAMOverheadMB); applying both would double-count it.
//
// 0 when any input is unknown, and 0 when the weights alone exceed the
// budget — the two are distinguished by the caller, which knows whether
// it supplied real inputs.
func MaxContextTokens(weightGB float64, kvBytesPerTokFP16 int, kvFactor, budgetGB float64) int {
	if weightGB <= 0 || kvBytesPerTokFP16 <= 0 || kvFactor <= 0 || budgetGB <= 0 {
		return 0
	}
	leftoverGB := budgetGB - weightGB
	if leftoverGB <= 0 {
		return 0
	}
	tokens := leftoverGB * 1e9 / (float64(kvBytesPerTokFP16) * kvFactor)
	return int(tokens/1024) * 1024
}

// mibToGB converts binary MiB to the decimal GB the sizing arithmetic and
// the manifests are both written in.
func mibToGB(mib int) float64 { return float64(mib) * (1 << 20) / 1e9 }

// OllamaAcceleratorBudgetGB is the GPU-addressable memory available to
// hold weights and KV, after the engine's own overhead. 0 on a host with
// no GPU-addressable memory at all, and 0 when the overhead alone
// exceeds the budget.
func OllamaAcceleratorBudgetGB(h Host, weightGB float64) float64 {
	eff := h.OllamaVRAMBudgetMB()
	if eff <= 0 {
		return 0
	}
	mib := eff - OllamaVRAMOverheadMB(h.UnifiedMemory, weightGB)
	if mib <= 0 {
		return 0
	}
	return mibToGB(mib)
}

// OllamaSystemRAMBudgetGB is the same quantity for weights read out of
// system RAM: total RAM less the headroom the OS and the agent need.
// 0 when RAM is unknown or smaller than the headroom.
func OllamaSystemRAMBudgetGB(h Host) float64 {
	if h.RAMTotalGB > OllamaCPUOnlyRAMHeadroomGB {
		return float64(h.RAMTotalGB - OllamaCPUOnlyRAMHeadroomGB)
	}
	return 0
}

// OllamaSizingBudgetGB is the memory the window sizing has to place
// weights and KV in: GPU-addressable memory where there is any, system
// RAM otherwise. It is the one budget the tuner and every window
// question share, so a host cannot be sized one way and judged another.
//
// The fall-through is on whether the host HAS GPU-addressable memory,
// not on whether any was left after overhead. A unified host whose
// carve-out the engine overhead consumes entirely has a budget of zero
// and no window — falling back to system RAM there would size a window
// out of memory the GPU cannot wire down, which is the one thing a
// single-pool machine cannot survive.
func OllamaSizingBudgetGB(h Host, weightGB float64) float64 {
	if h.OllamaVRAMBudgetMB() > 0 {
		return OllamaAcceleratorBudgetGB(h, weightGB)
	}
	return OllamaSystemRAMBudgetGB(h)
}

// OllamaEffectiveContextFloor is the window the sizing aims for: the
// coding-agent window, capped at the model's own advertised window so a
// sub-floor model is not priced for a window it can never serve.
func OllamaEffectiveContextFloor(m catalog.Manifest) int {
	if m.ContextLength > 0 && m.ContextLength < ServingWindow200k {
		return m.ContextLength
	}
	return ServingWindow200k
}

// OllamaServedWindows is the ladder of windows this product will actually
// serve a model at, highest first. Empty for a manifest with no window
// annotation, which every caller must read as "no opinion" rather than
// as a refusal.
//
// A node declares one of two windows or nothing (waired#1031), and a
// coding session is sized for the 200k rung (#624). A window between the
// rungs is therefore not a smaller version of the product — it is a
// window the mesh cannot route on and a coding agent cannot work in. So
// there are two rungs and, below them, the model's own window: a
// 131072-native model has nothing to trim TO, and serving it at 131072
// is the whole of what it can offer.
//
// This is where the capacity gate is priced (waired-ai/waired-agent#552).
// Pricing it at OllamaPlannedWindow's output instead made the gate
// unable to refuse anything: the sizing picks the largest window that
// fits, so re-checking that window against the same machine is a
// question that has already been answered yes. A 7 GiB Mac was admitted
// qwen3.5-4b at 54,272 — 200,704 of it needs 7403 MiB against a 4096 MiB
// budget, so the model was never servable there — loaded it, and
// returned HTTP 500 on the first generation.
//
// Deliberately platform-free, like everything else in this package: the
// rungs are a product contract, and the arithmetic below branches on
// Host.Class(), never on an operating system. macOS, Windows and Linux
// get the same answer for the same machine.
func OllamaServedWindows(m catalog.Manifest) []int {
	if m.ContextLength <= 0 {
		return nil
	}
	if m.ContextLength >= ServingWindow1M {
		// A 1M-native model may also be served — and declared — at the
		// coding rung, so a host that cannot hold 1M is not out of
		// options.
		return []int{ServingWindow1M, ServingWindow200k}
	}
	return []int{OllamaEffectiveContextFloor(m)}
}

// OllamaCeilingWindow is the top rung of OllamaServedWindows: the largest
// window this product will ever ask an engine to serve this model at. 0
// when the manifest carries no window, which callers read as "no cap".
func OllamaCeilingWindow(m catalog.Manifest) int {
	w := OllamaServedWindows(m)
	if len(w) == 0 {
		return 0
	}
	return w[0]
}

// OllamaExpectedSpillFraction predicts the /api/ps-visible spill fraction
// of serving ctxTokens on this host: the byte-math overshoot of
// (weights + KV + engine overhead) over the GPU budget, scaled by the
// measured calibration factor. 0 means no spill expected; the result is
// clamped to [0, 1].
//
// It is measured against the ACCELERATOR budget, because spill is by
// definition what does not fit there. A host with no accelerator has
// nothing to overflow and returns 0 — which is a statement about where
// the weights live, not a claim that reading them is free.
func OllamaExpectedSpillFraction(v catalog.Variant, h Host, kvFactor float64, ctxTokens int) float64 {
	eff := h.OllamaVRAMBudgetMB()
	if v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 || kvFactor <= 0 || ctxTokens <= 0 || eff <= 0 {
		return 0
	}
	budgetGB := mibToGB(eff)
	requiredGB := v.EstimatedWeightGB +
		float64(v.KVBytesPerTokenFP16)*kvFactor*float64(ctxTokens)/1e9 +
		mibToGB(OllamaVRAMOverheadMB(h.UnifiedMemory, v.EstimatedWeightGB))
	if requiredGB <= budgetGB {
		return 0
	}
	expected := OllamaSpillCalibration * (requiredGB - budgetGB) / requiredGB
	if expected > 1 {
		return 1
	}
	return expected
}

// OllamaMaxContextAtSpill inverts OllamaExpectedSpillFraction: the
// largest window (rounded down to a multiple of 1024) whose expected
// spill stays at or under maxExpected. 0 when the inputs are unknown or
// when even a zero-token window would exceed the bound — the weights
// alone already spill too far.
func OllamaMaxContextAtSpill(v catalog.Variant, h Host, kvFactor, maxExpected float64) int {
	eff := h.OllamaVRAMBudgetMB()
	if v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 || kvFactor <= 0 ||
		eff <= 0 || maxExpected <= 0 || maxExpected >= OllamaSpillCalibration {
		return 0
	}
	budgetGB := mibToGB(eff)
	overheadGB := mibToGB(OllamaVRAMOverheadMB(h.UnifiedMemory, v.EstimatedWeightGB))
	// expected = cal × (required − budget) / required  ⇒
	// required_max = budget / (1 − maxExpected/cal)
	requiredMax := budgetGB / (1 - maxExpected/OllamaSpillCalibration)
	kvGB := requiredMax - v.EstimatedWeightGB - overheadGB
	if kvGB <= 0 {
		return 0
	}
	tokens := kvGB * 1e9 / (float64(v.KVBytesPerTokenFP16) * kvFactor)
	return int(tokens/1024) * 1024
}

// OllamaWindowPlan is what the sizing decided, and enough of how it got
// there for the tuner to word the user-visible consequence without
// re-deriving anything.
type OllamaWindowPlan struct {
	// ContextLength is the window to export. 0 means the inputs were
	// unknown and nothing may be exported or declared.
	ContextLength int

	// NoSpillCapacityTokens is how many tokens of KV cache the sizing
	// budget holds outright, alongside the weights. Comparing it with
	// ContextLength says whether the plan is trading residency for
	// window; dividing by it says how many full-window request slots the
	// budget affords.
	//
	// Deliberately NOT capped at the model's own window: a budget that
	// holds three times what the model can use is the fact that lets a
	// caller grant parallel slots, and capping it would erase exactly
	// that.
	NoSpillCapacityTokens int

	// ExpectedSpillFraction is the /api/ps spill predicted at
	// ContextLength. It must be reported honestly on every branch: the
	// verify pass widens its tolerance to twice this figure before it
	// calls a load degraded, so under-reporting it makes the engine
	// restart into a smaller window than the plan asked for.
	ExpectedSpillFraction float64
}

// OllamaPlannedWindow is the window this host would actually serve
// (m, v) at, and the spill that costs. It is the single implementation
// of that arithmetic: the serve tuning exports what it returns, and
// OllamaDeclaresWindow — and through it the recommendation shown by both
// the agent's picker and the control plane's wizard — asks it whether
// the coding window is reachable here.
//
// kvFactor is the cache format the tuning will export. Callers that only
// want to know whether the window is reachable may pass
// OllamaKVFactorQ8_0 unconditionally: the tuner picks f16 only when f16
// affords at least twice the model's own window (its auto slot count), so
// on every host where it makes that choice both factors reach the full
// native window and the answer here cannot differ.
//
// allowSpill is false for a recompute after a load already proved
// unreliable — a sizing that just failed is never re-entered.
//
// Three rules, in order:
//
//  1. What fits GPU-addressable memory outright, capped at the model's
//     own window.
//  2. Widen toward the coding window by deliberately spilling, on
//     discrete GPUs only, bounded by OllamaMaxExpectedSpillFraction.
//     Unified memory is excluded because one pool has nowhere to spill
//     TO: oversubscribing the carve-out stalls the whole machine.
//  3. Never below what the same machine would size with the accelerator
//     REMOVED — up to the coding window, and no further.
//
// Rule 3 is what makes the result monotone in hardware, and it is not a
// tie-breaker — without it, fitting a card SHRINKS the window. The
// budget in rule 1 is the card's memory, while a host with no card is
// sized from system RAM, so a 64 GB machine sizes a 60 GB budget and the
// same machine with an 8 GB card sizes a 7 GB one. The card-less host
// then declares the coding window and the carded host does not, so the
// carded host is recommended a smaller model for owning a GPU — which
// inverts the thing the recommendation is for (owner statement on
// waired-ai/waired#1056: 一般的には CPU 推論よりも GPU での推論が早いから、
// GPU を搭載しているクライアントにはいいモデルを提示するべき).
//
// It is honest as well as monotone: ollama places layers in both VRAM
// and system RAM, so the carded machine really does serve that window,
// and it reads at least as fast as the card-less one because part of the
// model is on the card. The spill it costs is reported rather than
// hidden — rule 3 can exceed OllamaMaxExpectedSpillFraction, and that
// bound is a limit on the spill this sizing CHOOSES to create, not on
// what a machine already living in system RAM is doing.
//
// Both of its bounds matter. It stops at the coding window because that
// is the whole of what the monotonicity argument buys: past it, the two
// machines are equally declarable and the card-less one's larger budget
// is no longer evidence of anything the carded one is missing. And it
// does not run at all once rule 1 or 2 has already reached the window,
// so a host that was serving 200,704 at a 10 % spill is not widened to
// 262,144 at 18 % for a window nothing routes on.
//
// Rule 3 is discrete-only for the same reason rule 2 is.
func OllamaPlannedWindow(m catalog.Manifest, v catalog.Variant, h Host, kvFactor float64, allowSpill bool) OllamaWindowPlan {
	// "Unknown sizing" means we know NOTHING to size from — an
	// unannotated variant, or a machine that reports no memory at all. It
	// is not the same as a budget that came out zero: a 2 GB card whose
	// engine overhead exceeds it leaves nothing to hold weights in, but
	// the host still has whatever system RAM sits behind it, and rule 3
	// below can prove a window out of that. Reading the second as the
	// first is how fitting a small card took a window away from a 128 GB
	// machine.
	budgetGB := OllamaSizingBudgetGB(h, v.EstimatedWeightGB)
	ramGB := OllamaSystemRAMBudgetGB(h)
	if v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 || (budgetGB <= 0 && ramGB <= 0) {
		return OllamaWindowPlan{}
	}
	maxCtx := MaxContextTokens(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, kvFactor, budgetGB)

	// The ceiling is the top rung of OllamaServedWindows, not the model's
	// native window. A 262144-native model on a big host used to be
	// served at 262144 while DeclaredContextWindow could never claim more
	// than ServingWindow200k — 61,440 tokens of KV, about 960 MiB on
	// qwen3.5-4b at the q8_0 cache this tuning exports, for context the
	// mesh cannot route on (waired-ai/waired-agent#552).
	//
	// It is not free: the local overflow guard reads the APPLIED window
	// through ContextWindowFor, so this also lowers where a local request
	// gets its 400 and Claude Code compacts. That trade is the point —
	// the product serves two windows, so the rung is what a session is
	// sized for either way.
	ceiling := OllamaCeilingWindow(m)
	capNative := func(ctx int) int {
		if ceiling > 0 && ctx > ceiling {
			return ceiling
		}
		return ctx
	}

	plan := OllamaWindowPlan{
		ContextLength:         capNative(maxCtx),
		NoSpillCapacityTokens: maxCtx,
	}
	discrete := h.Class() == ClassDiscrete
	floorCtx := OllamaEffectiveContextFloor(m)

	// Rule 2 — intentional spill toward the coding window.
	if allowSpill && discrete && plan.ContextLength < floorCtx {
		target := floorCtx
		expected := OllamaExpectedSpillFraction(v, h, kvFactor, target)
		if expected > OllamaMaxExpectedSpillFraction {
			// The full floor spills past the bound: take the biggest
			// window the bound affords instead.
			target = OllamaMaxContextAtSpill(v, h, kvFactor, OllamaMaxExpectedSpillFraction)
			expected = OllamaExpectedSpillFraction(v, h, kvFactor, target)
		}
		if target > plan.ContextLength && expected > 0 && expected <= OllamaMaxExpectedSpillFraction {
			plan.ContextLength = target
			plan.ExpectedSpillFraction = expected
		}
	}

	// Rule 3 — the accelerator may not make the window smaller. Bounded
	// at floorCtx, and skipped entirely once the window already reaches
	// it: a host that serves the coding window has nothing to gain here,
	// and widening it further would spend rule 2's decode budget on
	// context nobody asked for.
	//
	// It respects allowSpill for the same reason rule 2 does. Both reach
	// their window by putting part of the model in system RAM, so a
	// recompute after a load already proved unreliable must not re-enter
	// either of them — otherwise the verify pass's degrade lands on the
	// same sizing it just rejected and the engine is never restarted.
	if allowSpill && discrete && plan.ContextLength < floorCtx {
		cardless := MaxContextTokens(
			v.EstimatedWeightGB, v.KVBytesPerTokenFP16, kvFactor, ramGB)
		if cardless > floorCtx {
			cardless = floorCtx
		}
		if cardless > plan.ContextLength {
			plan.ContextLength = cardless
			plan.ExpectedSpillFraction = OllamaExpectedSpillFraction(v, h, kvFactor, cardless)
		}
	}

	if plan.ContextLength < OllamaMinContextTokens {
		floored := OllamaMinContextTokens
		if m.ContextLength > 0 && m.ContextLength < floored {
			floored = m.ContextLength
		}
		plan.ContextLength = floored
	}
	return plan
}

// OllamaDeclaresWindow reports whether this host would actually serve
// (m, v) at window — the model's own window reaches it AND the sizing
// above lands there.
//
// It is the predicate "recommended" is defined as at window =
// ServingWindow200k, and it is deliberately the same function the serve
// tuning exports from, so "the wizard recommends it" and "the machine
// serves it at 200k" cannot drift apart.
func OllamaDeclaresWindow(m catalog.Manifest, v catalog.Variant, h Host, window int) bool {
	if window <= 0 {
		return true
	}
	if DeclarableNativeWindow(m) < window {
		return false
	}
	plan := OllamaPlannedWindow(m, v, h, OllamaKVFactorQ8_0, true)
	// A window of 0 means the sizing could not be proved — an unannotated
	// variant, or a host whose accelerator budget the engine overhead
	// consumes entirely. Every OTHER rule in this package is permissive
	// there, and this one is deliberately not.
	//
	// The asymmetry is the difference between refusing and promising.
	// Being permissive about a refusal costs a user nothing: the model is
	// offered and either works or does not. Being permissive about a
	// DECLARATION publishes a window this node never loaded — the tuner
	// exports nothing in that case and the engine keeps its own 32k
	// default — and a requester routes a 200k session to it. That is the
	// failure waired-ai/waired#1031's window contract exists to remove.
	return plan.ContextLength >= window
}

// OllamaRecommendModel decides whether (m, v) is what this host should be
// POINTED AT by default. Since the 2026-08-03 owner decision that is one
// question, spelled out in waired-ai/waired#1056 decision 3: can this
// host declare the coding window with this model — 「重み+オーバーヘッド
// 完全常駐、KV は既存 spill 上限（≈モデルの 20%）内、深部デコード条件」.
//
// Three clauses, in the order a reader can act on them:
//
//  1. The model's own window reaches the coding window. No hardware
//     changes this one, which is why it is named apart from the two an
//     operator could buy their way out of.
//  2. Weights and engine overhead fit GPU-addressable memory outright.
//     Unresident weights are re-read from system RAM on EVERY token of
//     every prompt, which is a different failure from an unresident KV
//     cache: waired-ai/waired#986's 22.6 GB mixture of experts on a
//     16 GB card prefilled a 30k prompt at 388 tok/s — 60-90 s to the
//     first token. A host with no accelerator has nothing to be resident
//     IN and is exempt, which is a real asymmetry and a known one: it
//     makes "no GPU" the more permissive configuration, and undoing it
//     needs a measured speed for the CPU-only arm rather than the
//     population constant that is there now (waired-ai/waired-agent#466).
//  3. This host would actually serve the coding window
//     (OllamaDeclaresWindow, i.e. the serve tuning's own sizing).
//
// False is NOT "cannot run". Capacity is the only rule allowed to refuse
// (OllamaCapacityFit), and a model that fits must still be offered —
// greyed, annotated, sorted below the recommended ones. Hiding it is the
// bug waired-ai/waired-agent#229 removed.
//
// Speed is deliberately absent, and that is the change from the rule
// this replaces. That one asked a different question per class — CPU-only
// exempt, unified judged on published-peak decode, discrete on residency
// — and the decode term excluded a 19.96 tok/s host while admitting a
// 17.65 tok/s one, both estimated from the same population constant. A
// decode-only estimate is also blind to prefill, which is most of a
// coding agent's work. Speed returns as a recommendation input when it is
// MEASURED (waired-ai/waired-agent#466); the boot benchmark already
// measures the real rate once a model is on disk.
func OllamaRecommendModel(m catalog.Manifest, v catalog.Variant, h Host) Verdict {
	out := Verdict{Fits: true}
	switch {
	case DeclarableNativeWindow(m) < ServingWindow200k:
		out = Verdict{Reason: ReasonWindowTooSmall}

	case h.HasGPU() && !weightsResident(v, h):
		out = Verdict{
			Reason: ReasonWeightsSpill,
			NeedMB: OllamaWeightsResidentMB(v, h.UnifiedMemory),
			HaveMB: h.OllamaVRAMBudgetMB(),
		}

	case !OllamaDeclaresWindow(m, v, h, ServingWindow200k):
		out = Verdict{
			Reason: ReasonWindowExceedsMemory,
			NeedMB: OllamaWindowResidentMB(v, ServingWindow200k, h.UnifiedMemory),
			HaveMB: h.TotalMemoryMB(),
		}
	}
	out.Estimate = EstimateOllamaDecode(v, h)
	return out
}

// weightsResident reports whether the weights and engine overhead fit
// this host's GPU-addressable memory. Permissive on missing inputs: an
// unannotated weight or an unknown budget is not evidence against the
// host.
func weightsResident(v catalog.Variant, h Host) bool {
	need := OllamaWeightsResidentMB(v, h.UnifiedMemory)
	have := h.OllamaVRAMBudgetMB()
	return need <= 0 || have <= 0 || need <= have
}
