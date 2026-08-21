// #624: the ~200k coding-agent context floor.
//
// Real coding-agent sessions measured on this repo peak at 75k–200k
// input tokens (heavy ones 300k+), with 35–50k of fixed overhead
// (system prompt + tool schemas + project instructions) before any
// conversation. A model that cannot hold ~200k either truncates or
// compacts constantly, so auto-selection prefers models that can
// actually serve that window. Two independent gates:
//
//   - Native floor (engine-independent): the manifest's own
//     context_length must reach codingAgentNativeContextMin. Applied
//     to auto-selection only; an explicit PreferredModelID bypasses it
//     with a visible warning. This is the half RankModels still narrows
//     on, and the half a caller may not stand down.
//   - Host gate (ollama path): whether this host would actually SERVE
//     the floor window here. Since the 2026-08-03 owner decision that
//     question has one answer and one implementation,
//     hostfit.OllamaPlannedRung — the same sizing the serve tuning
//     exports — so "the picker says it serves 200k" and "the engine was
//     started at 200k" cannot disagree (waired-ai/waired#1056 decision 3).
//     It is reported on the Pick and narrowed on by the RECOMMENDATION
//     pass, which a caller may stand down; it is no longer part of the
//     non-standable native floor, because a host that cannot hold the
//     window is not thereby a host that should be left without a model.
//   - Host gate (vllm path, #675/#678): the floor window's KV (fp8 on
//     Ada+ per #676, else fp16) plus activation-padded weights must fit
//     the default gpu-memory-utilization budget at the auto
//     tensor-parallel size (VLLMServesContextFloor). vLLM has no spill
//     semantics — an unfittable window is clamped at serve time — so
//     this gate is a plain window comparison with no spill allowance.
package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/modelrank"
)

const (
	// CodingAgentSelectionFloorTokps is the decode throughput (tok/s,
	// shallow-context boot benchmark, TRUE decode per #764: engine
	// counters or the overhead-corrected slope, median of 3) below
	// which a model is considered too slow for coding-agent use on
	// that host (#765, decision 20260711). 60 is anchored on industry
	// data: hosted Claude Sonnet 5 serves 67–90 tok/s output (the
	// entire Claude Code user base works in that band daily) and
	// NVIDIA's agentic-coding benchmark evaluates at 20 and 60 tok/s
	// SLOs. The previous value (100, #670) was calibrated against the
	// wall-clock benchmark #764 replaced, which under-measured fast
	// hosts ~35% — in true-decode terms the felt threshold already sat
	// in this 60–80 band, so this is a re-expression on the corrected
	// scale more than a loosening.
	// This is the default for the #133 lighter/upgrade recommendation
	// floor (config interactive_floor_tokps overrides it); it is NOT
	// the Phase-7 admission divisor, which stays at 30 tok/s — that one
	// models sustained per-session consumption, not acceptable latency
	// (see cmd/waired-agent/inference_bench.go).
	CodingAgentSelectionFloorTokps = 60.0

	// CodingAgentDepthFloorFraction scales the selection floor for the
	// depth-benchmark leg of the #133 comparison: the shallow boot
	// decode must clear the floor itself, while decode measured at
	// 64k–200k depth must clear floor × this fraction (= 48 tok/s at
	// the 60 default). 0.8 matches the measured long-context
	// degradation band (~200k-depth decode runs at roughly 0.7–0.8×
	// the shallow rate on the anchor host,
	// docs/reports/20260704-mtp-vs-spill-24gb.md C1: 165→116 tok/s at
	// 115k), so a host at the shallow floor still lands at or above
	// the scaled floor at depth. The shallow floor already prices in
	// the expected depth degradation; demanding the full floor at
	// depth would double-count it and nag on every host.
	CodingAgentDepthFloorFraction = 0.8

	// CodingAgentContextFloorTokens is the serve-time floor window:
	// ~200k, pre-aligned to 1024 (196×1024) and identical to the #625
	// measurement window so the calibration data maps 1:1.
	CodingAgentContextFloorTokens = 200704

	// OllamaMaxExpectedSpillFraction bounds the *expected measured* spill
	// the window sizing will deliberately create to reach the coding
	// window: within this bound a spilled high-tier model still dominates
	// the no-spill lower-tier alternative on both quality and speed
	// (24 GB anchor: qwen3.6-35b-a3b mtp at 11.5 % expected decodes
	// 85–104 tok/s vs the no-spill tier-69 dense at ~32). The anchor's
	// 11.5 % expected passes; the corrected non-MTP tag (23.9 GB,
	// expected ≈ 25 %) does not.
	//
	// The value and its derivation live in proto/hostfit now, because the
	// control plane's wizard has to reach the same conclusion about a
	// host as the host does (waired-ai/waired#1056 decision 3). This name
	// stays as the agent-facing spelling.
	OllamaMaxExpectedSpillFraction = hostfit.OllamaMaxExpectedSpillFraction

	// OllamaIntentionalSpillCapExpected is the same number under the name
	// the serve tuning's warning strings use. It was derived separately —
	// from the #664 A/B on the anchor host, where the spilled fraction
	// executes on a single CPU thread: no-spill decode 158.6 tok/s,
	// 13.4 % measured spill → ~85 tok/s; modelling
	// 1/rate = (1-s)/158.6 + s/21.25 keeps decode at or above the 60
	// tok/s selection floor while measured spill s ≤ ~0.25, i.e. expected
	// ≤ ~0.22 — and clamps to the selection bound above. Re-run that
	// derivation whenever the floor or the #664 numbers change.
	OllamaIntentionalSpillCapExpected = hostfit.OllamaMaxExpectedSpillFraction
)

// MeetsNativeContextFloor reports whether the manifest's native window
// qualifies it for the coding-agent auto-selection pool.
func MeetsNativeContextFloor(m catalog.Manifest) bool {
	return modelrank.MeetsNativeContextFloor(m)
}

// EffectiveContextFloor is the window the host gate (and the serve
// tuning's intentional spill) aims for: the ~200k floor, capped at the
// manifest's own native window for sub-floor models reached via the
// preferred-override bypass. Unknown manifest windows get the floor.
func EffectiveContextFloor(m catalog.Manifest) int {
	return modelrank.EffectiveContextFloor(m)
}

// OllamaExpectedSpillFraction predicts the /api/ps-visible spill
// fraction of serving ctxTokens with the given KV factor on this host:
// byte-math overshoot of (weights + KV + engine overhead) over the
// GPU budget, scaled by the measured calibration factor. 0 = no spill
// expected; results are clamped to [0, 1].
//
// The arithmetic is hostfit's; this is the hardware.Profile-shaped door
// into it, like every other function in this file.
func OllamaExpectedSpillFraction(v catalog.Variant, hw hardware.Profile, kvFactor float64, ctxTokens int) float64 {
	return hostfit.OllamaExpectedSpillFraction(v, hw.HostFit(), kvFactor, ctxTokens)
}

// OllamaServesContextFloor is the #624 host gate for the ollama path:
// would this host actually SERVE (m, v) at its effective floor window?
// It returns the verdict and the expected /api/ps spill fraction of
// doing so, which callers surface either way.
//
// It is now exactly "what would the serve tuning size here, and does it
// reach the floor" — hostfit.OllamaPlannedRung, the same function the
// tuner exports from (a rung the reachability rules passed; a forced
// lowest rung reports false, waired-ai/waired-agent#587). Before, this
// was a second implementation of the same byte math with a looser rule
// bolted on (discrete hosts passed whatever the spill, UMA hosts were
// gated), and the looseness was doing the work an escape hatch should
// do: it let a host that could not hold the window still be given a
// model, by pretending it could.
//
// Which is why the gate MOVED rather than tightened. RankModels no
// longer narrows the non-standable native-floor pass on this answer; it
// narrows the RECOMMENDATION pass, which SelectInstallModel may stand
// down before concluding a host is below the recommended spec (waired-ai/waired#1056
// decision 1: refusal is reserved for certain OOM). A host that cannot
// hold 200k is now told so and given the best model it can hold, instead
// of being told nothing and given none.
//
// Permissive on unknown sizing inputs, like the rest of this package.
//
// The budget is Profile.OllamaVRAMBudgetMB, not EffectiveVRAMMB, so a
// multi-GPU host is priced on the pool it actually spreads layers over
// (#264).
func OllamaServesContextFloor(m catalog.Manifest, v catalog.Variant, hw hardware.Profile) (bool, float64) {
	return modelrank.OllamaServesContextFloor(m, v, hw.HostFit())
}

// VLLMServesContextFloor is the #624 host gate for the vllm path: can
// this (manifest, variant) serve its effective floor window within the
// default gpu-memory-utilization budget at the auto tensor-parallel
// size? Sized by VLLMMaxModelLen — the same estimator the serve-time
// clamp (#675) uses, so selection and serving agree. Unknown sizing
// inputs and hosts without an NVIDIA GPU pass permissively (hostFits
// owns the VRAM rejection and the serve-time clamp is the backstop),
// same philosophy as OllamaServesContextFloor. There is no spill
// allowance: vLLM clamps the window instead of spilling.
func VLLMServesContextFloor(m catalog.Manifest, v catalog.Variant, hw hardware.Profile) bool {
	return modelrank.VLLMServesContextFloor(m, v, hw.GPUSummaries())
}

// OllamaMaxContextAtSpill inverts OllamaExpectedSpillFraction: the
// largest context window (rounded down to a multiple of 1024) whose
// expected spill stays at or under maxExpected on this host. Returns 0
// when the inputs are unknown or even a zero-token window would exceed
// the bound (weights alone spill too far).
//
// Deprecated: mirrors the deprecated hostfit.OllamaMaxContextAtSpill —
// the serve window is a rung of hostfit.OllamaServedWindows, never a
// window solved back from a spill bound (waired-ai/waired-agent#587).
func OllamaMaxContextAtSpill(v catalog.Variant, hw hardware.Profile, kvFactor, maxExpected float64) int {
	return hostfit.OllamaMaxContextAtSpill(v, hw.HostFit(), kvFactor, maxExpected)
}
