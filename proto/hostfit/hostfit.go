// Package hostfit is the single implementation of "can this machine
// serve this model variant" — shared by the agent's model picker and by
// the control plane's onboarding catalog.
//
// It exists because that question had TWO implementations, and they
// disagreed. The agent decided it from hardware.Profile
// (internal/router.hostFits / ollamaFitsVRAM); the control plane
// re-derived it from the broadcast signer.HardwareSummary and — having
// no VRAM term at all — offered a 62 GB model as the default first-run
// pick on a host with a 24 GB card, which the agent's own picker then
// refused to serve (waired-ai/waired#942). The control plane's own
// helper carried a doc comment saying it "reproduces
// hardware.Profile.EffectiveVRAMMB()", which is exactly the kind of
// promise a comment cannot keep across two repositories.
//
// So the rule lives here, in the module both sides already import, and
// each side adapts its own hardware type into Host once.
//
// What is deliberately NOT here, and stays in the agent: engine-version
// floors, the vendor-support matrix, tensor-parallel aggregation across
// identical GPUs, the coding-agent context floor, and the install-time
// disk pre-flight. The control plane has no inputs for most of those,
// and all of them are serving-time policy rather than "does it fit".
// VLLMFit therefore takes the VRAM budget as an argument: the agent
// passes its tensor-parallel aggregate, the control plane passes
// Host.EffectiveVRAMMB.
//
// Like the rest of the proto module this package is stdlib-only
// (dependency allowlist, CI-enforced) and additive-only across
// published proto tags.
package hostfit

import (
	"math"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Fit reason codes. They are the wire vocabulary the control plane
// already emits to the setup wizard, which owns the wording — the same
// split the setup-progress error enum uses. Emitting English prose from
// a fit decision would freeze user-facing copy into the protocol.
const (
	ReasonOK               = ""
	ReasonInsufficientRAM  = "insufficient_ram"
	ReasonInsufficientVRAM = "insufficient_vram"
	ReasonNoGPU            = "no_gpu"
)

// MinVLLMVRAMMB is the smallest VRAM size for which vLLM is worth
// choosing over Ollama. Below this, even GPU-equipped hosts fall
// through to Ollama because vLLM's overhead (CUDA context, engine
// workers, KV cache) eats most of a tiny GPU before any model loads.
// 8 GB matches the smallest reasonable model card we ship.
const MinVLLMVRAMMB = 8 * 1024

// InstallQualityFloorTier is the coding-quality floor for install-time
// model auto-selection (#517): the installer picks the largest catalog
// model that fits the host AND clears this quality_tier. When even the
// best-fitting model is below it — only sub-coding tiny models fit —
// the host is treated as under-spec and local inference is skipped (the
// node still enrolls and routes to peers).
//
// 30 == qwen2.5-coder-3b-instruct, the smallest usable coding model we
// ship. qwen3.5-2b (tier 27) and qwen3.5-0.8b (tier 12) fall below it.
const InstallQualityFloorTier = 30

// Inputs of the ollama VRAM-residency check.
const (
	// OllamaKVBudgetTokens is the KV-cache budget reserved at fit time.
	// 16k tokens is the floor for useful coding-agent context; variants
	// whose weights leave less than that spill layers to the CPU.
	OllamaKVBudgetTokens = 16384

	// Discrete-GPU overhead model: base + per-weight slope, replacing an
	// older flat 4096 MiB reservation. The flat constant was calibrated
	// against an ollama-defaults load (f16 KV, no flash attention); the
	// #621 serve tuning always spawns with OLLAMA_FLASH_ATTENTION=1,
	// which shrinks the compute graph substantially. Measured on a 24 GB
	// RTX PRO 4000: qwen3.6-35b mtp (22.62 GB weights) shows ~1.9 GB
	// effective overhead — the flat 4096 was floor()ing the context
	// window to 32768 while 114688 demonstrably fit. Single-point
	// calibration: base 1024 (device context, matching the UMA
	// measurement below) + 40 MiB per decimal GB of weights
	// (compute/scratch buffers scale with layer width). If a card family
	// under-reserves, the #621 post-load verify probe detects the spill
	// and shrinks the window — that safety net is what makes the
	// optimistic calibration acceptable.
	OllamaVRAMOverheadBaseDiscreteMB = 1024
	OllamaVRAMOverheadPerWeightGBMB  = 40

	// OllamaVRAMOverheadUnknownWeightMB is the conservative fallback
	// when the variant carries no weight annotation (it keeps the
	// historical flat reservation).
	OllamaVRAMOverheadUnknownWeightMB = 4096

	// OllamaVRAMOverheadUMAMB is the unified-memory counterpart. A UMA
	// host has no multi-GB device context to reserve — the model lives
	// in the shared pool, so the only beyond-weights cost is the
	// compute/scratch graph. The discrete 4 GB constant ~2×
	// over-estimated Metal: on a real Apple M4, qwen2.5-coder-7b q4
	// (4.7 GB weights) resided at runner.vram=4.4 GiB, yet the discrete
	// math budgeted ~9 GB and collapsed an 8 GB Mac's auto-pick to a
	// 1.9 GB model (#424). 1024 MB is the largest reduction that still
	// keeps the real-M4-confirmed 16 GB pick and the
	// qwen2.5-coder-14b GPU-residency rejection intact. Strix Halo
	// (UMA HIP/Vulkan) shares the argument; its value is extrapolated
	// from the Metal measurement pending a real-host probe.
	OllamaVRAMOverheadUMAMB = 1024
)

// Host is the minimal set of hardware facts a fit decision needs.
//
// It is deliberately neither of the two producer types: the agent's
// hardware.Profile carries detection detail no fit rule reads, and
// signer.HardwareSummary is a wire type whose shape is fixed by
// compatibility rather than by this decision. Each side adapts once —
// the control plane via FromHardwareSummary, the agent via a Profile
// adapter in internal/router — and everything downstream sees the same
// five numbers.
type Host struct {
	// RAMTotalGB is total system RAM. 0 means "detection failed", which
	// is treated as "skip the RAM gate", never as "no memory".
	RAMTotalGB int

	// GPUCount is how many accelerators were detected. Zero with
	// UnifiedMemory false is a CPU-only host.
	GPUCount int

	// UnifiedMemory marks hosts where GPU and CPU share physical RAM
	// (Apple Silicon, AMD Strix Halo).
	UnifiedMemory bool

	// UsableVRAMMB is the GPU-addressable upper bound after the OS
	// reserve, on unified-memory hosts. 0 means "unknown".
	UsableVRAMMB int

	// VRAM0MB is the first GPU's raw total VRAM — the budget on
	// discrete-GPU hosts, and the fallback when a UMA host reports no
	// usable figure.
	VRAM0MB int
}

// FromHardwareSummary adapts the broadcast hardware summary — what a
// device publishes and the control plane stores — into a Host.
//
// A nil summary is a device that has never reported, and yields the
// zero Host: no RAM figure, no GPU. Callers that must distinguish
// "never reported" from "reported a CPU-only host" have to do so before
// calling; a fit rule cannot tell them apart and must not pretend to.
func FromHardwareSummary(hw *signer.HardwareSummary) Host {
	if hw == nil {
		return Host{}
	}
	h := Host{
		RAMTotalGB:    hw.RAMTotalGB,
		GPUCount:      len(hw.GPUs),
		UnifiedMemory: hw.UnifiedMemory,
		UsableVRAMMB:  hw.UsableVRAMMB,
	}
	if len(hw.GPUs) > 0 {
		h.VRAM0MB = hw.GPUs[0].VRAMTotalMB
	}
	return h
}

// EffectiveVRAMMB is the VRAM budget a min_vram_mb or residency
// comparison may use. On unified-memory hosts the raw per-device figure
// overstates what the GPU can actually wire down, so the usable budget
// wins there; everyone else uses the first GPU's raw figure. Returns 0
// for CPU-only hosts, and for a unified-memory host that reports no
// usable figure it degrades to the raw one rather than to "no GPU".
func (h Host) EffectiveVRAMMB() int {
	if h.UnifiedMemory && h.UsableVRAMMB > 0 {
		return h.UsableVRAMMB
	}
	return h.VRAM0MB
}

// HasGPU reports whether the host has any GPU-addressable memory at
// all. A unified-memory host counts even when no discrete device was
// enumerated.
func (h Host) HasGPU() bool {
	return h.GPUCount > 0 || h.UnifiedMemory
}

// Verdict is one fit decision. NeedMB / HaveMB are populated only when
// the variant does NOT fit, and only for the shortfall that decided it,
// so a caller can state how far short the machine falls without this
// package writing that sentence. Reason is ReasonOK exactly when Fits.
type Verdict struct {
	Fits   bool
	Reason string
	NeedMB int
	HaveMB int
}

// OllamaVRAMOverheadMB is the fit-time overhead reservation: the small
// flat UMA constant on unified-memory hosts, the weight-scaled discrete
// model otherwise (falling back to the conservative flat reservation
// when the weight is unknown). Keyed on unified memory — the same axis
// EffectiveVRAMMB uses — so the overhead matches the budget it is
// compared against (#424).
//
// Exported because the serve-time context-length clamp (#621) has to
// subtract the same overhead this gate assumes; model scoring counts
// RAW weights precisely because the whole overhead lives in this
// subtraction. Never double-count it.
func OllamaVRAMOverheadMB(unifiedMemory bool, weightGB float64) int {
	if unifiedMemory {
		return OllamaVRAMOverheadUMAMB
	}
	if weightGB <= 0 {
		return OllamaVRAMOverheadUnknownWeightMB
	}
	return OllamaVRAMOverheadBaseDiscreteMB + int(float64(OllamaVRAMOverheadPerWeightGBMB)*weightGB)
}

// OllamaResidentMB is what a variant must hold in GPU-addressable
// memory to serve without spilling: weights, the reserved KV budget,
// and the engine overhead. Returns 0 for a variant with no weight
// annotation, which no caller may read as "it fits in nothing".
func OllamaResidentMB(v catalog.Variant, unifiedMemory bool) int {
	if v.EstimatedWeightGB <= 0 {
		return 0
	}
	// Weights are annotated in decimal GB; the budget is binary MiB.
	weightMiB := int(math.Ceil(v.EstimatedWeightGB * 1e9 / (1 << 20)))
	kvMiB := v.KVBytesPerTokenFP16 * OllamaKVBudgetTokens / (1 << 20)
	return weightMiB + kvMiB + OllamaVRAMOverheadMB(unifiedMemory, v.EstimatedWeightGB)
}

// OllamaResident is the GPU-residency half of the ollama fit: can this
// host hold the variant without spilling layers to the CPU?
//
// The system-RAM gate alone let multi-GB models "fit" hosts whose GPU
// could never hold them — ollama then silently spills and decode
// collapses to single-digit tok/s. This is the term the control plane
// was missing entirely (waired-ai/waired#942).
//
// A CPU-only host is reported as fitting: spilling to system RAM is how
// it is meant to run, and the RAM gate is its real bound. Note the
// asymmetry that creates — a host WITH a small GPU is judged more
// strictly than the same host without one, even though it would serve
// the model faster. That is a live question about the rule, tracked in
// waired-ai/waired-agent#229; it is preserved verbatim here rather than
// changed in passing, because unifying the implementation and changing
// the policy are two different reviews.
//
// Exposed separately from OllamaFit because a caller explaining WHY a
// model was rejected has to know which of the two gates bound: naming
// the RAM figure when the GPU was the wall sends the operator to buy
// the wrong hardware.
func OllamaResident(v catalog.Variant, h Host) Verdict {
	if !h.HasGPU() {
		return Verdict{Fits: true}
	}
	need := OllamaResidentMB(v, h.UnifiedMemory)
	if need <= 0 {
		return Verdict{Fits: true} // unannotated weight: nothing to compare
	}
	have := h.EffectiveVRAMMB()
	if have <= 0 {
		return Verdict{Fits: true} // budget unknown: don't reject the catalog
	}
	if need <= have {
		return Verdict{Fits: true}
	}
	return Verdict{Reason: ReasonInsufficientVRAM, NeedMB: need, HaveMB: have}
}

// OllamaFit decides whether ollama can serve v on this host: the system
// RAM gate, then GPU residency.
//
// The order decides which shortfall gets reported when both would fail,
// and RAM comes first because it is the one the operator can read off
// their own machine. The gate is skipped on unified-memory hosts: on a
// UMA carve-out box RAMTotalGB reports only what the OS keeps after the
// iGPU allocation (~31 GB of a 128 GB machine), so a MinRAMGB threshold
// authored for a host that loads into system RAM would wrongly reject
// every large MoE there — residency is the honest bound on UMA.
func OllamaFit(v catalog.Variant, h Host) Verdict {
	if !h.UnifiedMemory {
		// RAMTotalGB == 0 means detection failed (e.g. an OS whose probe
		// we do not have); skip the gate rather than reject everything.
		if v.MinRAMGB > 0 && h.RAMTotalGB > 0 && h.RAMTotalGB < v.MinRAMGB {
			return Verdict{
				Reason: ReasonInsufficientRAM,
				NeedMB: v.MinRAMGB * 1024,
				HaveMB: h.RAMTotalGB * 1024,
			}
		}
	}
	return OllamaResident(v, h)
}

// VLLMFit decides whether vLLM can serve v against budgetMB — the
// engine-aware VRAM budget, which is the caller's to compute: the agent
// aggregates across an identical multi-GPU tensor-parallel set (#678),
// the control plane has only the broadcast summary and passes
// Host.EffectiveVRAMMB.
//
// A host with no budget at all is reported as ReasonNoGPU rather than
// as a shortfall, including for a variant that declares no minimum:
// vLLM does not run without a GPU, so "it fits" would be a worse answer
// than "there is no card". HaveMB is left unset there — there is no
// figure to compare against, and 0 GB would read as a measurement.
func VLLMFit(v catalog.Variant, budgetMB int) Verdict {
	if budgetMB <= 0 {
		return Verdict{Reason: ReasonNoGPU, NeedMB: v.MinVRAMMB}
	}
	if v.MinVRAMMB <= 0 || budgetMB >= v.MinVRAMMB {
		return Verdict{Fits: true}
	}
	return Verdict{Reason: ReasonInsufficientVRAM, NeedMB: v.MinVRAMMB, HaveMB: budgetMB}
}
