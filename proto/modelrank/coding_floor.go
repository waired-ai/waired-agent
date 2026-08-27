package modelrank

import (
	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The ~200k coding-agent context floor (waired-agent#624).
//
// Real coding-agent sessions peak at 75k–200k input tokens (heavy ones
// 300k+), with 35–50k of fixed overhead (system prompt + tool schemas +
// project instructions) before any conversation. A model that cannot
// hold ~200k either truncates or compacts constantly, so auto-selection
// prefers models that can actually serve that window. Two independent
// gates:
//
//   - Native floor (engine-independent): the manifest's own
//     context_length must reach hostfit.NativeContextFloorTokens.
//     Applied to auto-selection only; an explicit PreferredModelID
//     bypasses it. This is the half RankModels narrows on
//     unconditionally, and the half a caller may not stand down.
//   - Host gate (ollama path): whether this host would actually SERVE
//     the floor window here. Since the 2026-08-03 owner decision that
//     question has one answer and one implementation,
//     hostfit.OllamaPlannedRung — the same sizing the serve tuning
//     exports — so "the picker says it serves 200k" and "the engine was
//     started at 200k" cannot disagree (waired-ai/waired#1056 decision 3).
//   - Host gate (vllm path): the floor window's KV (fp8 on Ada+, else
//     fp16) plus activation-padded weights must fit the default
//     gpu-memory-utilization budget at the auto tensor-parallel size.
//     vLLM has no spill semantics — an unfittable window is clamped at
//     serve time — so this gate is a plain window comparison with no
//     spill allowance.

// CodingAgentSelectionFloorTokps is the decode throughput (tok/s,
// shallow-context boot benchmark, TRUE decode: engine counters or the
// overhead-corrected slope, median of 3) below which a model is
// considered too slow for coding-agent use on that host
// (waired-agent#765, decision 20260711). 60 is anchored on industry
// data: hosted Claude Sonnet 5 serves 67–90 tok/s output, and NVIDIA's
// agentic-coding benchmark evaluates at 20 and 60 tok/s SLOs. The
// previous value (100) was calibrated against the wall-clock benchmark
// waired-agent#764 replaced, which under-measured fast hosts ~35% — in
// true-decode terms the felt threshold already sat in this 60–80 band,
// so this is a re-expression on the corrected scale more than a
// loosening.
//
// It is the DEFAULT for PickInput.FloorTokps, not a value this package
// applies on its own: the floor is configurable per host, and a caller
// that has an operator's setting must pass that instead.
const CodingAgentSelectionFloorTokps = 60.0

// MeetsNativeContextFloor reports whether the manifest's native window
// qualifies it for the coding-agent auto-selection pool.
//
// The gate lives in hostfit because both sides need the same one: the
// control plane's wizard once offered 131072-window models as defaults
// for coding work while the agent on the same machine would not serve
// them (waired-ai/waired#988).
func MeetsNativeContextFloor(m catalog.Manifest) bool {
	return m.ContextLength >= hostfit.NativeContextFloorTokens
}

// EffectiveContextFloor is the window the host gate aims for: the ~200k
// floor, capped at the manifest's own native window for sub-floor models
// reached via the preferred-override bypass. Unknown manifest windows
// get the floor.
func EffectiveContextFloor(m catalog.Manifest) int {
	return hostfit.OllamaEffectiveContextFloor(m)
}

// OllamaServesContextFloor is the host gate for the ollama path: would
// this host actually SERVE (m, v) at its effective floor window? It
// returns the verdict and the expected /api/ps spill fraction of doing
// so, which callers surface either way.
//
// It is exactly "what would the serve tuning size here, and does it
// reach the floor" — hostfit.OllamaPlannedRung, the same function the
// tuner exports from (a rung the reachability rules passed; a forced
// lowest rung reports false, waired-agent#587). Before, this was a
// second implementation of the same byte math with a looser rule bolted
// on, and the looseness was doing the work an escape hatch should do: it
// let a host that could not hold the window still be given a model, by
// pretending it could.
//
// Which is why the gate MOVED rather than tightened. RankModels does not
// narrow the non-standable native-floor pass on this answer; it narrows
// the RECOMMENDATION pass, which a caller may stand down before
// concluding a host is below the recommended spec (waired-ai/waired#1056
// decision 1: refusal is reserved for certain OOM). A host that cannot
// hold 200k is told so and given the best model it can hold, instead of
// being told nothing and given none.
//
// Permissive on unknown sizing inputs, like the rest of this package.
func OllamaServesContextFloor(m catalog.Manifest, v catalog.Variant, host hostfit.Host) (bool, float64) {
	plan := hostfit.OllamaPlannedRung(m, v, host, hostfit.OllamaKVFactorQ8_0, 0)
	if plan.ContextLength <= 0 {
		return true, 0
	}
	return plan.Fits && plan.ContextLength >= EffectiveContextFloor(m), plan.ExpectedSpillFraction
}

// VLLMServesContextFloor is hostfit.VLLMServesContextFloor: the host gate
// for the vllm path — can this (manifest, variant) serve its effective
// floor window within the default gpu-memory-utilization budget at the
// auto tensor-parallel size?
//
// It moved to hostfit with the rest of the vLLM sizing so the
// recommendation could reach it (waired-agent#1061); see the note at the
// top of vllm.go.
func VLLMServesContextFloor(
	m catalog.Manifest, v catalog.Variant, gpus []signer.HardwareGPUSummary,
) bool {
	return hostfit.VLLMServesContextFloor(m, v, gpus)
}
