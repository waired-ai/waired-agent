package hostfit

import (
	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

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
// OLLAMA_KV_CACHE_TYPE options.
//
// OllamaKVFactorQ8_0 rounds ggml's q8_0 block (34 bytes per 32 values,
// 0.53125 of f16) down to a half, which under-priced a 200k window by
// 389 MiB on a dense 27B. The sizing reads OllamaKVCacheFactor now; the
// constant keeps meaning "q8_0" to OllamaPlannedRung's factor parameter
// (waired-ai/waired-agent#1337).
const (
	OllamaKVFactorF16  = 1.0
	OllamaKVFactorQ8_0 = 0.5
)

const (
	// OllamaSpillCalibration mapped the byte-math spill prediction to
	// ollama's own /api/ps accounting. Single-point calibration on the
	// 24 GB anchor host: predicted 3.9 % ↔ measured 13.5 %
	// (waired-ai/waired-agent#625).
	//
	// Deprecated: the prediction is priced term by term now
	// (OllamaEstimateMemory, OllamaPredictPlacement) and nothing reads
	// this; decision 8 of
	// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md
	// retires the single-point constants (waired-ai/waired-agent#1337). It
	// stays because proto is additive-only across published tags.
	OllamaSpillCalibration = 3.0

	// OllamaMaxExpectedSpillFraction bounds the share of the weights the
	// window sizing will deliberately put in system RAM to reach the coding
	// window, for a model the user chose (rung rule 2). It no longer takes
	// part in the recommendation, which asks for the whole window on the
	// accelerator (decision 10 of
	// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
	// Since waired-ai/waired-agent#1337 it is compared with a predicted
	// byte share of the weights (OllamaPredictPlacement.CPUWeightShare),
	// which is the quantity the derivation below is written in; it used to
	// be compared with a calibrated /api/ps figure.
	//
	// Derived from the #664 A/B on the anchor host, where the
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

	// OllamaCPUOnlyRAMHeadroomGB was the system headroom left out of the
	// window SIZING budget when the weights are read from system RAM: the
	// OS allowance above plus as much again for the agent, the engine's
	// own process, and page cache.
	//
	// Deprecated: the 2 GB-gate / 4 GB-sizing asymmetry was dissolved by
	// the 2026-08-08 owner rulings on waired-ai/waired#1067 — both
	// questions now charge Host.OSMemoryDeductionGB (the install-time
	// measurement, floored at OSMemoryAllowanceGB), and the sizing budget
	// reserves the engine's own overhead explicitly
	// (OllamaSizingBudgetGB) instead of approximating it with this
	// doubled constant. Nothing reads it; it stays because proto is
	// additive-only across published tags.
	OllamaCPUOnlyRAMHeadroomGB = 2 * OSMemoryAllowanceGB

	// OllamaMinContextTokens is the smallest window the deprecated
	// continuous sizing (OllamaPlannedWindow) ever exports — the pinned
	// engine's own default.
	//
	// Deprecated: the serve window is one of OllamaServedWindows' rungs
	// since the 2026-08-08 rulings on waired-ai/waired#1067
	// (waired-ai/waired-agent#587); rungs are already at or above the
	// model's own window floor, so no separate minimum applies. Retained
	// because proto is additive-only across published tags.
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

// OllamaSystemRAMBudgetGB is the raw system-RAM pool available to hold
// weights and KV: total RAM less the OS deduction — the same
// Host.OSMemoryDeductionGB the capacity gate's TotalMemoryMB charges, so
// the two questions stop deducting different amounts for the same
// operating system (the 2 GB-gate / 4 GB-sizing asymmetry dissolved by
// the 2026-08-08 owner rulings on waired-ai/waired#1067). 0 when RAM is
// unknown or smaller than the deduction.
//
// It does NOT reserve the engine's own overhead; a caller pairing it
// with MaxContextTokens must subtract that reservation itself, exactly
// as OllamaAcceleratorBudgetGB does on the accelerator side —
// OllamaSizingBudgetGB is the entry point that keeps the two legs
// symmetric.
func OllamaSystemRAMBudgetGB(h Host) float64 {
	if ded := h.OSMemoryDeductionGB(); h.RAMTotalGB > ded {
		return float64(h.RAMTotalGB - ded)
	}
	return 0
}

// OllamaSizingBudgetGB is the memory the window sizing has to place
// weights and KV in: GPU-addressable memory where there is any, system
// RAM otherwise. It is the one budget the tuner and every window
// question share, so a host cannot be sized one way and judged another.
//
// Both legs subtract the engine's own overhead (OllamaVRAMOverheadMB —
// the same reservation the capacity gate charges through
// OllamaWeightsResidentMB), so MaxContextTokens sees weights RAW on
// either leg. The RAM leg used not to: its overhead allowance was baked
// into the doubled OllamaCPUOnlyRAMHeadroomGB constant, which the
// 2026-08-08 waired-ai/waired#1067 rulings replaced with the measured OS
// deduction — leaving the reservation to be charged explicitly here.
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
	ram := OllamaSystemRAMBudgetGB(h) - mibToGB(OllamaVRAMOverheadMB(h.UnifiedMemory, weightGB))
	if ram <= 0 {
		return 0
	}
	return ram
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

// OllamaExpectedSpillFraction predicts the share of the weights llama.cpp's
// fit places in system RAM when serving ctxTokens on this host: the
// CPUWeightShare of OllamaPredictPlacement, for the KV-cache type the
// factor names. 0 means everything is expected on the accelerator; a host
// with no accelerator has nothing to overflow and returns 0 as well — a
// statement about where the weights live, not a claim that reading them is
// free.
//
// It used to scale a byte overshoot by OllamaSpillCalibration to predict
// what /api/ps would report. Both halves of that are gone: the terms the
// calibration stood in for are now priced (OllamaEstimateMemory), and
// /api/ps is not a placement witness (waired-ai/waired-agent#1337).
func OllamaExpectedSpillFraction(v catalog.Variant, h Host, kvFactor float64, ctxTokens int) float64 {
	if ctxTokens <= 0 || kvFactor <= 0 || h.OllamaVRAMBudgetMB() <= 0 || v.KVBytesPerTokenFP16 <= 0 {
		return 0
	}
	return OllamaPredictPlacement(v, h, kvCacheTypeForFactor(kvFactor), ctxTokens, 1).CPUWeightShare
}

// OllamaMaxContextAtSpill inverts OllamaExpectedSpillFraction: the
// largest window (rounded down to a multiple of 1024) whose expected
// spill stays at or under maxExpected. 0 when the inputs are unknown or
// when even a zero-token window would exceed the bound — the weights
// alone already spill too far.
//
// Deprecated: only the frozen continuous sizing (OllamaPlannedWindow)
// still calls it. Since the 2026-08-08 waired-ai/waired#1067 rulings the
// serve window is a rung of OllamaServedWindows, never a window solved
// back from a spill bound (waired-ai/waired-agent#587). Retained because
// proto is additive-only across published tags.
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

// OllamaWindowPlan is what the deprecated continuous sizing
// (OllamaPlannedWindow) decided. New callers read OllamaRungPlan; this
// type stays undeprecated only so tooling that enumerates it (the
// protoconsumer exemption table) lints clean.
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

	// ExpectedSpillFraction is the predicted share of the weights in
	// system RAM at ContextLength (see OllamaRungPlan's field of the same
	// name).
	ExpectedSpillFraction float64
}

// OllamaPlannedWindow is the window the pre-rung sizing would serve
// (m, v) at, and the spill that costs.
//
// Deprecated: the serve tuning and OllamaDeclaresWindow moved to
// OllamaPlannedRung under the 2026-08-08 owner rulings on
// waired-ai/waired#1067 — the engine is started at a rung of
// OllamaServedWindows, never at a continuously shrunk window between
// them (waired-ai/waired-agent#587). Nothing in production calls this;
// it stays because proto is additive-only across published tags. It is
// NOT byte-frozen: it reads the shared budgets, which now charge the
// measured OS deduction, so its outputs shift with them.
//
// Its three rules survive inside OllamaPlannedRung as the per-rung
// reachability test; the rule documentation below is kept because it is
// still the ratified reasoning for those rules.
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

// OllamaRungPlan is which rung of OllamaServedWindows this host serves
// (m, v) at, and enough of how it got there for the tuner to word the
// user-visible consequence without re-deriving anything.
type OllamaRungPlan struct {
	// ContextLength is the rung to export. 0 means the inputs were
	// unknown — an unannotated variant or a machine reporting no memory
	// at all — and nothing may be exported or declared.
	ContextLength int

	// Fits is true when ContextLength is a rung the per-rung
	// reachability rules passed: this host holds it outright, or reaches
	// it under the bounded-spill / card-never-shrinks rules. False means
	// no rung passed and ContextLength is the ladder's lowest rung
	// anyway — serving a window between the rungs is not a smaller
	// version of the product, so the host that cannot prove any rung is
	// still started at the lowest one (2026-08-08 owner rulings on
	// waired-ai/waired#1067; waired-ai/waired-agent#587). A false plan is
	// served but never DECLARED: OllamaDeclaresWindow reads it as no.
	Fits bool

	// NoSpillCapacityTokens is how many tokens of KV cache the sizing
	// budget holds outright, alongside the weights. Dividing the rung
	// into it says how many full-window request slots the budget
	// affords; it is deliberately not capped at the model's own window
	// (see OllamaWindowPlan's field of the same name).
	NoSpillCapacityTokens int

	// ExpectedSpillFraction is the predicted share of the weights
	// llama.cpp's fit places in system RAM at ContextLength
	// (OllamaPredictPlacement) — 0 when the rung is held outright or the
	// host has no accelerator to overflow. It must be reported honestly on
	// every branch: the verify pass compares the engine's own placement
	// with the prediction before it calls a load degraded, so
	// under-reporting it makes the engine restart into a lower rung the
	// plan did not ask for.
	ExpectedSpillFraction float64
}

// OllamaPlannedRung is the window this host actually serves (m, v) at:
// the highest rung of OllamaServedWindows the reachability rules pass,
// or the ladder's lowest rung — reported with Fits=false — when none
// passes. It is the single implementation of that arithmetic: the serve
// tuning exports what it returns, and OllamaDeclaresWindow — and through
// it the recommendation shown by both the agent's picker and the control
// plane's wizard — asks it whether the coding window is reachable here.
//
// kvFactor is the cache format the tuning will export (see
// OllamaPlannedWindow's doc for why q8_0 is a safe default for callers
// that only ask reachability).
//
// ceiling, when > 0, drops every rung above it from the ladder. It is
// the verify pass's seam: after a load at one rung measured unreliable,
// the recompute is capped so it can only step down, never back up. 0
// considers the full ladder.
//
// A rung R is reachable by the same three rules the continuous sizing
// applied, evaluated at R instead of maximized (their reasoning is
// documented on OllamaPlannedWindow):
//
//  1. What fits GPU-addressable memory outright — or system RAM, on a
//     host with none (OllamaSizingBudgetGB).
//  2. On discrete GPUs only, by deliberately spilling, bounded by
//     OllamaMaxExpectedSpillFraction, up to the model's effective floor.
//  3. On discrete GPUs only, whatever the same machine would reach with
//     the accelerator REMOVED — up to the effective floor, so fitting a
//     card never shrinks the window (waired-ai/waired#1056).
//
// What no rule does any more is pick a window BETWEEN the rungs: a
// window the mesh cannot route on and a coding agent cannot work in is
// not a smaller version of the product, so sub-rung shrinking is retired
// entirely — the host that cannot prove any rung serves the lowest one,
// slowly and with the cost reported, instead of a quietly trimmed window
// (2026-08-08 owner rulings on waired-ai/waired#1067, superseding the
// intentional-spill selection; waired-ai/waired-agent#587).
func OllamaPlannedRung(m catalog.Manifest, v catalog.Variant, h Host, kvFactor float64, ceiling int) OllamaRungPlan {
	return OllamaPlannedRungFor(m, v, h, kvCacheTypeForFactor(kvFactor), ceiling)
}

// OllamaPlannedRungFor is OllamaPlannedRung with the KV-cache type named
// rather than approximated by a factor (catalog.KVCache*). Every rule is
// priced with OllamaEstimateMemory, term by term the way llama.cpp's fit
// adds them, and rule 2 reads the predicted placement: the share of the
// device-placeable weights the fit would put in system RAM
// (waired-ai/waired-agent#1337).
func OllamaPlannedRungFor(m catalog.Manifest, v catalog.Variant, h Host, kvType string, ceiling int) OllamaRungPlan {
	rungs := OllamaServedWindows(m)
	ramMB := 0
	if h.RAMTotalGB > h.OSMemoryDeductionGB() {
		ramMB = (h.RAMTotalGB - h.OSMemoryDeductionGB()) * 1024
	}
	if len(rungs) == 0 || v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 || (h.OllamaVRAMBudgetMB() <= 0 && ramMB <= 0) {
		return OllamaRungPlan{}
	}
	if ceiling > 0 {
		for len(rungs) > 1 && rungs[0] > ceiling {
			rungs = rungs[1:]
		}
	}
	maxCtx := OllamaDeviceCapacityTokens(v, h, kvType)

	discrete := h.Class() == ClassDiscrete
	floorCtx := OllamaEffectiveContextFloor(m)
	reachable := func(rung int) bool {
		if maxCtx >= rung {
			return true
		}
		if !discrete || rung > floorCtx {
			return false
		}
		// Rule 2 — bounded intentional spill toward the floor. The bound
		// reads as a share of the weights in system RAM, the quantity the
		// #664 decode model it was derived from is written in.
		if p := OllamaPredictPlacement(v, h, kvType, rung, 1); p.CPUWeightShare > 0 && p.CPUWeightShare <= OllamaMaxExpectedSpillFraction {
			return true
		}
		// Rule 3 — the accelerator may not make the window smaller: the
		// same machine with the card removed sizes from system RAM.
		cardless := Host{RAMTotalGB: h.RAMTotalGB, RAMAvailableGB: h.RAMAvailableGB}
		return OllamaDeviceCapacityTokens(v, cardless, kvType) >= rung
	}

	plan := OllamaRungPlan{NoSpillCapacityTokens: maxCtx}
	for _, rung := range rungs {
		if reachable(rung) {
			plan.ContextLength = rung
			plan.Fits = true
			break
		}
	}
	if !plan.Fits {
		plan.ContextLength = rungs[len(rungs)-1]
	}
	if h.HasGPU() && h.OllamaVRAMBudgetMB() > 0 {
		plan.ExpectedSpillFraction = OllamaPredictPlacement(v, h, kvType, plan.ContextLength, 1).CPUWeightShare
	}
	return plan
}

// OllamaDeclaresWindow reports whether this host would actually serve
// (m, v) at window — the model's own window reaches it AND the rung plan
// above reaches it within the reachability rules.
//
// It is the predicate "recommended" is defined as at window =
// ServingWindow200k, and it is deliberately the same function the serve
// tuning exports from, so "the wizard recommends it" and "the machine
// serves it at 200k" cannot drift apart.
//
// It is a PREDICTION about a model this host is not necessarily running,
// which is a different question from what a running agent reports about
// the model it IS running (the agent's DeclaredContextWindow). The two
// are allowed to disagree on a forced rung and the asymmetry below says
// why. Do not "reconcile" them: the owner ruling of 2026-08-11
// (waired-ai/waired-agent#657, recorded on the 2026-08-02 window-contract
// decision) is about not withholding a true fact from a machine the
// operator already chose to run a model on. Nothing there asks this
// function to promise a window on a host that has not been shown to hold
// one.
func OllamaDeclaresWindow(m catalog.Manifest, v catalog.Variant, h Host, window int) bool {
	if window <= 0 {
		return true
	}
	if DeclarableNativeWindow(m) < window {
		return false
	}
	return OllamaDeclaresWindowFor(m, v, h, ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, ""), window)
}

// OllamaDeclaresWindowFor is OllamaDeclaresWindow for a named KV-cache
// type (catalog.KVCache*). OllamaDeclaresWindow reads the type the serve
// tuning exports by default (OllamaDefaultKVCacheType); it used to assume
// q8_0 unconditionally.
func OllamaDeclaresWindowFor(m catalog.Manifest, v catalog.Variant, h Host, kvType string, window int) bool {
	if window <= 0 {
		return true
	}
	if DeclarableNativeWindow(m) < window {
		return false
	}
	plan := OllamaPlannedRungFor(m, v, h, kvType, 0)
	// Fits=false means the sizing could not be proved — an unannotated
	// variant, a host whose accelerator budget the engine overhead
	// consumes entirely, or a lowest rung the host was given anyway
	// because sub-rung windows are not served. Every OTHER rule in this
	// package is permissive there, and this one is deliberately not.
	//
	// The asymmetry is the difference between refusing and promising.
	// Being permissive about a refusal costs a user nothing: the model is
	// offered and either works or does not. Being permissive about a
	// DECLARATION publishes a window this node cannot hold — the engine
	// is serving it out of memory it does not have — and a requester
	// routes a 200k session to it. That is the failure
	// waired-ai/waired#1031's window contract exists to remove.
	return plan.Fits && plan.ContextLength >= window
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
	return OllamaRecommendModelFor(m, v, h, ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, ""))
}

// OllamaRecommendModelFor is OllamaRecommendModel with the KV-cache type
// named (catalog.KVCache*): the recommendation prices the cache the serve
// tuning will export (decision 1 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
//
// Clause 3 on a host with GPU-addressable memory — discrete or unified —
// is full residency: the load priced by OllamaEstimateMemory at the coding
// window fits the accelerator budget outright, so no layer lands in system
// RAM (decision 10 of the same record, waired-ai/waired-agent#1347). The
// bounded intentional spill a user-chosen model may still be served with
// (rung rule 2) takes no part here. A host with no accelerator keeps the
// earlier clause: the serve tuning's own sizing reaches the coding window.
func OllamaRecommendModelFor(m catalog.Manifest, v catalog.Variant, h Host, kvType string) Verdict {
	out := Verdict{Fits: true}
	budget := h.OllamaVRAMBudgetMB()
	accelerated := h.HasGPU() && budget > 0 && v.EstimatedWeightGB > 0
	switch {
	case DeclarableNativeWindow(m) < ServingWindow200k:
		out = Verdict{Reason: ReasonWindowTooSmall}

	case accelerated && v.GGUF != nil && OllamaEstimateMemory(v, h, kvType, 0, 1).DeviceMB() > budget:
		out = Verdict{
			Reason: ReasonWeightsSpill,
			NeedMB: OllamaEstimateMemory(v, h, kvType, 0, 1).DeviceMB(),
			HaveMB: budget,
		}

	case h.HasGPU() && v.GGUF == nil && !weightsResident(v, h):
		out = Verdict{
			Reason: ReasonWeightsSpill,
			NeedMB: OllamaWeightsResidentMB(v, h.UnifiedMemory),
			HaveMB: budget,
		}

	case accelerated && v.KVBytesPerTokenFP16 > 0 &&
		OllamaPlannedRungFor(m, v, h, kvType, 0).NoSpillCapacityTokens < ServingWindow200k:
		out = Verdict{
			Reason: ReasonWindowExceedsMemory,
			NeedMB: OllamaEstimateMemory(v, h, kvType, ServingWindow200k, 1).DeviceMB(),
			HaveMB: budget,
		}

	case !accelerated && !OllamaDeclaresWindowFor(m, v, h, kvType, ServingWindow200k):
		out = Verdict{
			Reason: ReasonWindowExceedsMemory,
			NeedMB: OllamaEstimateMemory(v, h, kvType, ServingWindow200k, 1).TotalMB(),
			HaveMB: h.TotalMemoryMB(),
		}
	}
	out.Estimate = EstimateOllamaDecode(v, h)
	return out
}

// VLLMRecommendModel is OllamaRecommendModel's counterpart for the vLLM
// surface. Today it carries ONE clause, and that clause is the one no
// engine can change: the model's own window has to reach the coding
// window at all (DeclarableNativeWindow, clause 1 of OllamaRecommendModel).
//
// It exists because the surfaces asked the same question per engine tab
// and got opposite answers. On the ollama tab a 131072-native model is
// annotated "not recommended on any computer"; on the vLLM tab the same
// model carried no verdict and was the CHECKED DEFAULT, on a host where
// the engine then clamped it to 124928 (waired-agent#1029). "On any
// computer" was already the honest phrasing — the clause is a fact about
// the manifest, and running it under a different engine does not move it.
//
// The clause about THIS host — would the engine clamp the window below
// the coding target here — is NOT in this entry point. It reads the
// per-device GPU list, which this signature cannot carry, so it lives in
// VLLMRecommendModelOnHost below; the vLLM sizing it needs moved into this
// package for that (waired-agent#1061). This one remains the honest answer
// for a caller holding no device detail: a fact about the manifest, which
// no engine and no hardware moves.
//
// False is NOT "cannot run", exactly as in OllamaRecommendModel: capacity
// (VLLMFit) is the only rule allowed to refuse, and a model that fits is
// still offered — annotated and sorted below the recommended ones
// (waired-agent#229).
func VLLMRecommendModel(m catalog.Manifest, _ catalog.Variant, _ Host) Verdict {
	if DeclarableNativeWindow(m) < ServingWindow200k {
		return Verdict{Reason: ReasonWindowTooSmall}
	}
	return Verdict{Fits: true}
}

// VLLMRecommendModelOnHost is VLLMRecommendModel plus the clause about
// THIS host: would the engine have to clamp the window below the coding
// target here (waired-agent#1061)?
//
// It is the vLLM answer to clause 3 of OllamaRecommendModel, and it took a
// second entry point because the arithmetic reads the per-device GPU list
// and VLLMRecommendModel's signature is published. Both entry points stay:
// the older one is the manifest-only verdict, which is the honest answer
// when no device detail is in hand.
//
// vLLM's clause 2 — do the weights fit outright — is not repeated here.
// That is VLLMFit against the VRAM budget, and Project/ProjectModelFrom
// already ask it as CAPACITY, which is the only rule allowed to refuse
// (waired-agent#229). Answering it a second time as a recommendation would
// demote a row that capacity had already turned down.
//
// Permissive on unknown inputs, inherited from VLLMServesContextFloor: an
// unannotated weight, an unknown per-token KV size, or a host with no
// NVIDIA device reported is not evidence against the host. NeedMB/HaveMB
// stay unset — what does not fit here is a token count, and translating
// that into megabytes would be a second, uncalibrated arithmetic; the
// console's copy for this reason takes no sizes.
func VLLMRecommendModelOnHost(
	m catalog.Manifest, v catalog.Variant, h Host, gpus []signer.HardwareGPUSummary,
) Verdict {
	return VLLMRecommendModelOnHostFor(m, v, h, gpus, VLLMKVCacheType(gpus, ""))
}

// VLLMRecommendModelOnHostFor is VLLMRecommendModelOnHost priced at a
// named KV-cache type (see VLLMKVCacheType).
func VLLMRecommendModelOnHostFor(
	m catalog.Manifest, v catalog.Variant, h Host, gpus []signer.HardwareGPUSummary, kvType string,
) Verdict {
	if out := VLLMRecommendModel(m, v, h); !out.Fits {
		return out
	}
	if !VLLMServesContextFloorFor(m, v, gpus, kvType) {
		return Verdict{Reason: ReasonWindowExceedsMemory}
	}
	return Verdict{Fits: true}
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
