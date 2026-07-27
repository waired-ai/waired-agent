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

// Inputs of the decode-speed estimate (waired-ai/waired-agent#229).
//
// Fitting a model is not the same as running it usefully, and capacity
// alone cannot tell the two apart: on a 24 GB Mac a dense 27B fits and
// decodes at ~7 tok/s, while a 35B mixture-of-experts does NOT fit by
// weight but — where it does fit — decodes at ~50, because only ~3B of
// its parameters are read per token. Ranking those two by size gets the
// order exactly backwards. So the rule carries a speed term, and it is
// a roofline: autoregressive decode is memory-bandwidth-bound, and the
// bytes it must read per token are the ACTIVE weights.
//
// The two bandwidth constants below look like the same kind of number
// and are NOT. They are asked for opposite things, because only one of
// the classes they serve is allowed to exclude a model, and each
// constant has to be wrong in the direction that class can afford. Read
// them together before changing either.
const (
	// BandwidthSystemRAMGBs is the assumed ACHIEVABLE system-memory read
	// bandwidth — what a streaming read sustains, not the spec-sheet
	// product of clock x width x channels. Sustained throughput runs
	// around 60 % of spec: a DDR5-4800 dual-channel host (76.8 GB/s on
	// paper) measures ~48 GB/s, and that ratio puts DDR4-3200 near 32
	// and DDR5-5600 near 56.
	//
	// It has to be an UPPER bound on that figure. An earlier revision of
	// this comment claimed the opposite — "guessing low only adds 'this
	// may be slow'" — and that is exactly backwards for the branch that
	// matters. This constant enters the one case permitted to EXCLUDE a
	// model (ClassDiscrete spilled, the only place Estimate.UpperBound is
	// set), and there the estimate is directly proportional to it: lower
	// the constant, the predicted rate falls, it breaches
	// DecodeFloorTokps sooner, and the wizard starts refusing models the
	// machine runs perfectly well. "Guess low, stay safe" holds only for
	// the annotate-only classes.
	//
	// 60 therefore sits ABOVE the achievable figure of the mainstream
	// population rather than at its floor. Two residuals follow, both
	// accepted until waired-ai/waired-agent#252 measures the host:
	//
	//   - Below the line: a DDR4 laptop sustaining ~32 GB/s is credited
	//     with 60, so "may be slow" under-triggers. Costs a sentence.
	//   - Above the line: DDR5-6400 and multi-channel workstation memory
	//     exceed 60 achievable, so for them it is not a bound at all, and
	//     a model they would run can be wrongly excluded.
	//
	// Do NOT lower this to match a measured effective figure. A
	// measurement describes one host; this constant stands in for a
	// population, and moving it toward that population's middle moves the
	// second residual out of the tail and into the bulk.
	BandwidthSystemRAMGBs = 60.0

	// BandwidthUnifiedGBs is the same quantity for a unified-memory pool:
	// an Apple M-series base chip is ~120 GB/s and every larger part is
	// above it (M4 Pro 273, AMD Strix Halo 256, M4 Max 546, M3 Ultra
	// 819).
	//
	// Unlike its neighbour this one is a FLOOR, and is allowed to be,
	// because ClassUnified never excludes: EstimateOllamaDecode sets no
	// UpperBound there, so the figure only decides whether the wizard
	// adds "may be slow". A floor over-warns on the large parts — an M4
	// Max is judged as if it were an M4 base — and refuses nothing.
	//
	// Which also means it cannot simply be promoted. Letting UMA exclude
	// (the change that would stop a 24 GB Mac being handed a 7 tok/s
	// dense model in preference to a 50 tok/s MoE) needs the per-chip
	// spec figure from waired-ai/waired-agent#251, not a retuned
	// constant: nothing single-valued is an upper bound across a
	// 120..819 GB/s span.
	BandwidthUnifiedGBs = 120.0

	// DecodeFloorTokps is the decode rate below which a model should not
	// be OFFERED as a reasonable choice for this machine. It is the
	// lower of the two agentic-coding SLOs NVIDIA evaluates at (20 and
	// 60) — the same pair CodingAgentSelectionFloorTokps in the agent's
	// router is anchored on, which takes the upper one. The two are
	// different questions: 60 is "fast enough to work in", 20 is "fast
	// enough to be worth downloading at all".
	DecodeFloorTokps = 20.0
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

// Class is the kind of machine a fit decision is about. The three
// differ in where the weights live and what happens when they do not
// fit, so they cannot share one rule — which is what this package
// originally did, and what made a discrete-GPU host judged more
// strictly than the same host with the card removed
// (waired-ai/waired-agent#229).
type Class int

const (
	// ClassCPUOnly has no GPU-addressable memory. The weights are read
	// from system RAM, which is also the only bound on their size.
	ClassCPUOnly Class = iota

	// ClassDiscrete has dedicated VRAM with system RAM behind it. What
	// does not fit spills back over PCIe and executes on the CPU: slower
	// than resident, never slower than having no card at all.
	ClassDiscrete

	// ClassUnified shares one physical pool between CPU and GPU (Apple
	// Silicon, AMD Strix Halo). There is nowhere to spill TO — oversubs-
	// cribing the carve-out stalls the whole machine — so residency is a
	// hard bound here in a way it is not on a discrete card.
	ClassUnified
)

// Class reports which of the three kinds of machine this host is.
func (h Host) Class() Class {
	switch {
	case h.UnifiedMemory:
		return ClassUnified
	case h.GPUCount > 0:
		return ClassDiscrete
	default:
		return ClassCPUOnly
	}
}

// Verdict is one fit decision. NeedMB / HaveMB are populated only when
// the variant does NOT fit, and only for the shortfall that decided it,
// so a caller can state how far short the machine falls without this
// package writing that sentence. Reason is ReasonOK exactly when Fits.
//
// Fits answers CAPACITY only — can the machine hold and run this at all.
// How fast it would run is a separate, weaker claim, carried in the
// Estimate below, because a speed rule that could veto Fits would newly
// mark working hosts as unable to run anything, which is the failure
// mode the agent's context floor (#624) is carefully built to avoid.
type Verdict struct {
	Fits   bool
	Reason string
	NeedMB int
	HaveMB int

	// Estimate is how fast this variant is expected to decode here, and
	// how much of it the GPU holds. Zero value when the variant carries
	// no sizing annotations to reason from.
	//
	// json:"-" because Verdict is a decision, not a payload: nothing
	// marshals it, and each consumer projects the parts it needs onto its
	// own wire shape. The tag says so explicitly, which is also what the
	// additive-only guard asks of a field added to a published struct.
	Estimate Estimate `json:"-"`
}

// Estimate is the roofline decode prediction for one (variant, host)
// pair: bytes read per token over memory bandwidth.
//
// It is deliberately partial. On a discrete GPU holding the whole model
// the card's bandwidth decides the answer and this package does not know
// it, so TokpsEstimate is left at zero and MeetsSpeedFloor is true — a
// resident discrete GPU is not the wall.
//
// Everywhere else the estimate is INFORMATIONAL unless UpperBound says
// otherwise. Only the spilled-discrete case carries a margin no unknown
// hardware can eat — the card's own reads are priced at zero — so only
// there does MeetsSpeedFloor being false mean "slow even under
// favourable assumptions" rather than a constant that happened to land
// low. See UpperBound, and the constants above for which direction each
// of them is deliberately wrong in.
type Estimate struct {
	// TokpsEstimate is the predicted decode rate in tokens/second, or 0
	// when no claim is made (resident on a discrete GPU, or a variant
	// with no parameter annotations).
	TokpsEstimate float64

	// Resident reports whether the weights fit entirely in
	// GPU-addressable memory. False on CPU-only hosts by definition.
	Resident bool

	// ResidentShare is the fraction of the weights the GPU holds, in
	// [0,1]. 0 on a CPU-only host, 1 when Resident.
	ResidentShare float64

	// MeetsSpeedFloor reports whether TokpsEstimate clears
	// DecodeFloorTokps, and is true when no claim is made. INFORMATIONAL
	// on its own — see UpperBound for whether it may be acted on.
	MeetsSpeedFloor bool

	// UpperBound reports that the real machine cannot be FASTER than
	// TokpsEstimate, which is the only condition under which a caller
	// may exclude a model for being slow.
	//
	// It holds exactly for the spilled-discrete case, where the GPU's
	// contribution is priced at zero precisely so the unknown card
	// cannot make the answer wrong. That over-estimate is STRUCTURAL: it
	// survives whatever the bandwidth constants happen to be, and no
	// other class has anything like it.
	//
	// ClassUnified rests entirely on BandwidthUnifiedGBs, which is the
	// floor of its population, so the real machine is usually faster —
	// a lower bound, and rejecting on one would withhold from an M4 Max
	// models an M4 base runs. ClassCPUOnly rests entirely on
	// BandwidthSystemRAMGBs, which IS meant as an upper bound, but with
	// no structural margin behind it: a host whose memory beats the
	// constant would be excluded on the strength of the constant alone.
	// Both classes may still SAY "this may be slow", because that costs
	// the user a sentence rather than a choice.
	//
	// Measured per-device bandwidth (waired-ai/waired-agent#251) is what
	// turns the other classes into decisions rather than annotations.
	//
	// json:"-" for the same reason Verdict.Estimate carries it: nothing
	// marshals a decision, each consumer projects what it needs onto its
	// own wire shape, and the additive-only guard asks an addition to a
	// published struct to say so. The neighbouring fields predate the
	// guard's view of this struct and cannot be retagged now.
	UpperBound bool `json:"-"`
}

// ActiveBytesPerToken is how many bytes of weights a decode step must
// read, in decimal GB.
//
// Derived from the manifest's own measurements rather than from a
// quantization-bits ladder: estimated_weight_gb / param_count IS the
// variant's effective bytes per parameter, so scaling it by the active
// share needs no table and cannot drift from the published weight. A
// dense model (active_params unset) reads all of its weights.
//
// This is the term capacity math cannot see. qwen3.6-27b and
// qwen3.6-35b-a3b sit within 6 GB of each other on disk and differ by
// SEVEN TIMES here (14.85 GB/token vs 2.13), because the second one is a
// mixture of experts.
func ActiveBytesPerToken(v catalog.Variant) float64 {
	w := v.EstimatedWeightGB
	if w <= 0 {
		return 0
	}
	if v.ParamCount <= 0 || v.ActiveParams <= 0 || v.ActiveParams >= v.ParamCount {
		return w
	}
	return w * float64(v.ActiveParams) / float64(v.ParamCount)
}

// EstimateOllamaDecode predicts how fast v would decode on h, per class.
//
//   - ClassCPUOnly — every byte comes from system RAM.
//   - ClassUnified — one pool, so the same single-domain arithmetic at
//     the unified bandwidth. Residency is enforced by OllamaFit here, so
//     there is no spilled case to model.
//   - ClassDiscrete, resident — no claim (see Estimate).
//   - ClassDiscrete, spilled — the resident share is priced at ZERO
//     read time, leaving only the share the CPU must fetch. That is an
//     upper bound on speed for any card, which is what lets this decide
//     without knowing the card's bandwidth: if a model is too slow even
//     when the GPU's contribution is free, no GPU makes it fast enough.
//     It also makes the estimate monotone in hardware by construction —
//     the bound is >= the CPU-only rate for every resident share — which
//     is the invariant #229 exists because the old rule broke.
func EstimateOllamaDecode(v catalog.Variant, h Host) Estimate {
	pass := Estimate{MeetsSpeedFloor: true}
	b := ActiveBytesPerToken(v)
	if b <= 0 {
		return pass // nothing to reason from; do not invent a verdict
	}
	rate := func(bandwidthGBs, share float64) Estimate {
		tokps := bandwidthGBs / b
		return Estimate{
			TokpsEstimate:   tokps,
			Resident:        share >= 1,
			ResidentShare:   share,
			MeetsSpeedFloor: tokps >= DecodeFloorTokps,
		}
	}
	switch h.Class() {
	case ClassCPUOnly:
		return rate(BandwidthSystemRAMGBs, 0)
	case ClassUnified:
		e := rate(BandwidthUnifiedGBs, 1)
		e.Resident = true
		return e
	}

	// Discrete: how much of the weights the card can hold, after the
	// same overhead and KV reservation the residency gate assumes.
	budgetMB := h.EffectiveVRAMMB() -
		OllamaVRAMOverheadMB(false, v.EstimatedWeightGB) -
		v.KVBytesPerTokenFP16*OllamaKVBudgetTokens/(1<<20)
	weightMB := v.EstimatedWeightGB * 1e9 / (1 << 20)
	if weightMB <= 0 || h.EffectiveVRAMMB() <= 0 {
		return pass
	}
	share := float64(budgetMB) / weightMB
	switch {
	case share >= 1:
		// The card holds all of it. Its bandwidth decides the rate and
		// this package does not know it; the slowest shipping card that
		// could hold the weights at all is still far above the floor.
		return Estimate{Resident: true, ResidentShare: 1, MeetsSpeedFloor: true}
	case share < 0:
		share = 0
	}
	e := rate(BandwidthSystemRAMGBs/(1-share), share)
	e.Resident = false
	// The only figure here that no unknown hardware can improve on: the
	// card's own reads were priced at zero.
	e.UpperBound = true
	return e
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
// it is meant to run, and the RAM gate is its real bound.
//
// This is NO LONGER the capacity gate on discrete GPUs — see OllamaFit
// for why — but it remains the honest answer to "can the card hold all
// of this", which is what the agent's deficit labels and the control
// plane's shortfall figures are explaining. It IS still the capacity
// gate on unified memory, where there is nowhere to spill.
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
// RAM gate, then GPU residency, with the decode estimate attached.
//
// The order decides which shortfall gets reported when both would fail,
// and RAM comes first because it is the one the operator can read off
// their own machine. The gate is skipped on unified-memory hosts: on a
// UMA carve-out box RAMTotalGB reports only what the OS keeps after the
// iGPU allocation (~31 GB of a 128 GB machine), so a MinRAMGB threshold
// authored for a host that loads into system RAM would wrongly reject
// every large MoE there — residency is the honest bound on UMA.
//
// Residency is required only where there is nowhere to spill TO:
//
//	ClassCPUOnly   system RAM >= min_ram_gb
//	ClassDiscrete  system RAM >= min_ram_gb  (residency NOT required)
//	ClassUnified   the weights fit the usable pool (min_ram_gb ignored)
//
// The discrete row is the correction waired-ai/waired-agent#229 exists
// for. Requiring full residency there made adding a graphics card REMOVE
// models: a 128 GB host served a 62 GB model, and the same host with a
// 24 GB card did not — even though ollama offloads what fits and runs
// the remainder from the same system RAM the card-less host was using,
// which is strictly faster. A capacity rule has to be monotone in
// hardware, and this one now is by construction: the discrete gate IS
// the CPU-only gate, and Estimate's spill bound is >= the CPU-only rate
// for every resident share.
//
// Fits is capacity only. Whether the result would be fast enough to want
// rides on Verdict.Estimate, so a caller can offer a slow-but-working
// model with a warning rather than silently withholding it — which is
// what withholding the 62 GB model above amounted to.
func OllamaFit(v catalog.Variant, h Host) Verdict {
	var out Verdict
	if h.Class() == ClassUnified {
		out = OllamaResident(v, h)
	} else if v.MinRAMGB > 0 && h.RAMTotalGB > 0 && h.RAMTotalGB < v.MinRAMGB {
		// RAMTotalGB == 0 means detection failed (e.g. an OS whose probe
		// we do not have); skip the gate rather than reject everything.
		out = Verdict{
			Reason: ReasonInsufficientRAM,
			NeedMB: v.MinRAMGB * 1024,
			HaveMB: h.RAMTotalGB * 1024,
		}
	} else {
		out = Verdict{Fits: true}
	}
	out.Estimate = EstimateOllamaDecode(v, h)
	return out
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
