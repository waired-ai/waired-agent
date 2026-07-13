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
//     with a visible warning.
//   - Host gate (ollama path): the host must serve the floor window
//     with q8_0 KV either fully GPU-resident, or — on discrete GPUs
//     only — within a bounded expected spill
//     (OllamaMaxExpectedSpillFraction). A bounded-spill flagship still
//     dominates the no-spill mid-tier fallback on both quality and
//     speed (24 GB anchor: tier-90 spilled at 85–104 tok/s vs the
//     tier-69 dense that fits un-spilled at ~32 tok/s), so selection
//     keeps a generous bound; the serve tuning separately caps the
//     spill it CREATES at OllamaIntentionalSpillCapExpected so decode
//     stays at the coding-agent selection floor (#670/#765 — at the
//     60 true-decode floor the cap clamps to the selection bound).
//   - Host gate (vllm path, #675/#678): the floor window's KV (fp8 on
//     Ada+ per #676, else fp16) plus activation-padded weights must fit
//     the default gpu-memory-utilization budget at the auto
//     tensor-parallel size (VLLMServesContextFloor). vLLM has no spill
//     semantics — an unfittable window is clamped at serve time — so
//     this gate is a plain window comparison with no spill allowance.
package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
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

	// codingAgentNativeContextMin gates manifest membership in the
	// coding-agent auto-selection pool. 200000 (not 200704) so exactly
	// the 262144-native manifests pass and the 131072 class does not.
	codingAgentNativeContextMin = 200000

	// ollamaSpillCalibration maps the byte-math spill prediction to
	// ollama's own /api/ps accounting. Single-point calibration on the
	// anchor host: predicted 3.9% ↔ measured 13.5% (#625 report).
	ollamaSpillCalibration = 3.0

	// OllamaMaxExpectedSpillFraction bounds the *expected measured*
	// spill the SELECTION gate accepts for a variant's floor-window
	// serviceability: within this bound a spilled high-tier model still
	// dominates the no-spill lower-tier alternative on both quality
	// and speed (24 GB anchor: qwen3.6-35b-a3b mtp at 11.5% expected
	// decodes 85–104 tok/s vs the no-spill tier-69 dense at ~32), so
	// excluding it from RankModels would produce strictly worse picks.
	// The anchor's 11.5% expected passes; the corrected non-MTP tag
	// (23.9 GB, expected ≈ 25%) does not.
	OllamaMaxExpectedSpillFraction = 0.20

	// OllamaIntentionalSpillCapExpected bounds the expected spill the
	// serve tuning deliberately CREATES when widening the window toward
	// the coding floor. Derived from the #664 A/B on the anchor host,
	// where the spilled fraction executes on a single CPU thread:
	// no-spill decode 158.6 tok/s, 13.4% measured spill → ~85 tok/s.
	// Modeling 1/rate = (1-s)/158.6 + s/21.25 (the second term is the
	// fitted effective rate of the CPU-executed share), decode stays at
	// or above the 60 tok/s selection floor (#765) while measured spill
	// s ≤ ~0.25, i.e. expected ≤ ~0.22 at the anchor's expected↔
	// measured ratio (11.5% ↔ 13.4%). That exceeds the selection gate's
	// outer bound, so the cap clamps to OllamaMaxExpectedSpillFraction:
	// every variant the gate admits now serves its full floor window,
	// and the tuner's trim only protects preferred-override models that
	// bypass the gate. (At the previous 100 floor the same model gave
	// ~0.075 and the 24 GB anchor traded window for decode.) Re-run
	// this derivation whenever the floor or the #664 numbers change
	// (an engine fix parallelizing the spilled phase raises the derived
	// bound further above the clamp).
	OllamaIntentionalSpillCapExpected = 0.20
)

// MeetsNativeContextFloor reports whether the manifest's native window
// qualifies it for the coding-agent auto-selection pool.
func MeetsNativeContextFloor(m catalog.Manifest) bool {
	return m.ContextLength >= codingAgentNativeContextMin
}

// EffectiveContextFloor is the window the host gate (and the serve
// tuning's intentional spill) aims for: the ~200k floor, capped at the
// manifest's own native window for sub-floor models reached via the
// preferred-override bypass. Unknown manifest windows get the floor.
func EffectiveContextFloor(m catalog.Manifest) int {
	if m.ContextLength > 0 && m.ContextLength < CodingAgentContextFloorTokens {
		return m.ContextLength
	}
	return CodingAgentContextFloorTokens
}

// OllamaExpectedSpillFraction predicts the /api/ps-visible spill
// fraction of serving ctxTokens with the given KV factor on this host:
// byte-math overshoot of (weights + KV + engine overhead) over the
// GPU budget, scaled by the measured calibration factor. 0 = no spill
// expected; results are clamped to [0, 1].
func OllamaExpectedSpillFraction(weightGB float64, kvBytesPerTokFP16 int, kvFactor float64, ctxTokens int, hw hardware.Profile) float64 {
	eff := hw.EffectiveVRAMMB()
	if weightGB <= 0 || kvBytesPerTokFP16 <= 0 || kvFactor <= 0 || ctxTokens <= 0 || eff <= 0 {
		return 0
	}
	const mib = float64(1 << 20)
	budgetGB := float64(eff) * mib / 1e9
	requiredGB := weightGB +
		float64(kvBytesPerTokFP16)*kvFactor*float64(ctxTokens)/1e9 +
		float64(OllamaVRAMOverheadMB(hw, weightGB))*mib/1e9
	if requiredGB <= budgetGB {
		return 0
	}
	expected := ollamaSpillCalibration * (requiredGB - budgetGB) / requiredGB
	if expected > 1 {
		return 1
	}
	return expected
}

// OllamaServesContextFloor is the #624 host gate for the ollama path:
// can this (manifest, variant) serve its effective floor window with
// q8_0 KV on this host? Passes when the window fits fully GPU-resident,
// or — discrete GPUs only — when the expected spill stays under
// OllamaMaxExpectedSpillFraction (returned so callers can surface it).
// Unknown sizing inputs and CPU-only hosts pass permissively: the #621
// serve tuning and its post-load verify probe are the backstop, same
// philosophy as ollamaFitsVRAM.
func OllamaServesContextFloor(m catalog.Manifest, v catalog.Variant, hw hardware.Profile) (bool, float64) {
	if v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 {
		return true, 0
	}
	if len(hw.GPUs) == 0 && !hw.UnifiedMemory {
		return true, 0 // CPU-only: spilling to RAM is the design.
	}
	eff := hw.EffectiveVRAMMB()
	if eff <= 0 {
		return true, 0
	}
	floorCtx := EffectiveContextFloor(m)
	budgetMiB := eff - OllamaVRAMOverheadMB(hw, v.EstimatedWeightGB)
	budgetGB := float64(budgetMiB) * float64(1<<20) / 1e9
	if scoring.MaxContextTokens(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, scoring.KVFactorQ8_0, budgetGB) >= floorCtx {
		return true, 0
	}
	expected := OllamaExpectedSpillFraction(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, scoring.KVFactorQ8_0, floorCtx, hw)
	if hw.UnifiedMemory {
		// One memory pool: "spill to system RAM" has no meaning, and
		// oversubscribing the carve-out stalls the whole host. No
		// bounded-spill allowance on UMA.
		return false, expected
	}
	return expected <= OllamaMaxExpectedSpillFraction, expected
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
	if v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 {
		return true
	}
	hasNVIDIA := false
	for _, g := range hw.GPUs {
		if g.Vendor == "nvidia" {
			hasNVIDIA = true
			break
		}
	}
	if !hasNVIDIA {
		return true
	}
	est := VLLMMaxModelLen(v.EstimatedWeightGB, v.KVBytesPerTokenFP16,
		VLLMTensorParallelSize(hw), DefaultVLLMGPUMemoryUtilization, VLLMKVFactor(hw), hw)
	return est >= EffectiveContextFloor(m)
}

// OllamaMaxContextAtSpill inverts OllamaExpectedSpillFraction: the
// largest context window (rounded down to a multiple of 1024) whose
// expected spill stays at or under maxExpected on this host. Used by
// the serve tuning to size the intentional spill so decode holds the
// selection floor when the full coding floor would spill past
// OllamaIntentionalSpillCapExpected. Returns 0 when the inputs are
// unknown or even a zero-token window would exceed the bound (weights
// alone spill too far).
func OllamaMaxContextAtSpill(weightGB float64, kvBytesPerTokFP16 int, kvFactor, maxExpected float64, hw hardware.Profile) int {
	eff := hw.EffectiveVRAMMB()
	if weightGB <= 0 || kvBytesPerTokFP16 <= 0 || kvFactor <= 0 || eff <= 0 || maxExpected <= 0 || maxExpected >= ollamaSpillCalibration {
		return 0
	}
	const mib = float64(1 << 20)
	budgetGB := float64(eff) * mib / 1e9
	overheadGB := float64(OllamaVRAMOverheadMB(hw, weightGB)) * mib / 1e9
	// expected = cal × (required − budget) / required  ⇒
	// required_max = budget / (1 − maxExpected/cal)
	requiredMax := budgetGB / (1 - maxExpected/ollamaSpillCalibration)
	kvGB := requiredMax - weightGB - overheadGB
	if kvGB <= 0 {
		return 0
	}
	tokens := kvGB * 1e9 / (float64(kvBytesPerTokFP16) * kvFactor)
	return int(tokens/1024) * 1024
}
