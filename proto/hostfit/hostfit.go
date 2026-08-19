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

	// ReasonInsufficientMemory is the capacity refusal: the model does not
	// fit the machine's TOTAL memory, so loading it would run out. It
	// replaces the two above for the ollama path, where the gate is now a
	// computation over RAM + dedicated VRAM rather than a comparison
	// against one of them (owner decision 2026-08-03, waired-ai/waired#1056).
	//
	// A separate code rather than a reuse, because the two it replaces
	// name which of two pools fell short and this one names their sum.
	// Telling an operator "not enough RAM" when the sum was the wall
	// sends them to buy the wrong hardware, which is the same argument
	// Verdict's NeedMB/HaveMB doc makes one level down.
	ReasonInsufficientMemory = "insufficient_memory"

	// The two below are RECOMMENDATION reasons, not capacity reasons:
	// they name why a model that this host CAN run should not be the one
	// it is pointed at by default. A consumer that hides a model on
	// either of them has misread the contract — see OllamaRecommend.
	ReasonWeightsSpill = "weights_spill"
	ReasonTooSlow      = "too_slow"
)

// MinVLLMVRAMMB is the smallest VRAM size for which vLLM is worth
// choosing over Ollama. Below this, even GPU-equipped hosts fall
// through to Ollama because vLLM's overhead (CUDA context, engine
// workers, KV cache) eats most of a tiny GPU before any model loads.
// 8 GB matches the smallest reasonable model card we ship.
const MinVLLMVRAMMB = 8 * 1024

// Deprecated: nothing reads this. The install-time coding-quality floor
// it named was abolished in waired-agent#522 (owner decision
// 2026-08-08), and refusal is now capacity (OllamaCapacityFit — certain
// OOM) plus the #624 native window, both of which RankModels already
// applies.
//
// It was "the installer picks the largest catalog model that fits the
// host AND clears this quality_tier", anchored at 30 ==
// qwen2.5-coder-3b-instruct. That anchor retired with the 2025
// generation, and the deeper problem was that a tier threshold could
// not say what it was being asked to say: within the one generation the
// catalog carries, quality_tier is 10*log10(params) -
// 5*log10(footprint) (#518), so the floor was a size cutoff written the
// long way round — and the agent-grade harness, the only measurement
// that could have ranked those models, is not monotone in size across
// them.
//
// The declaration stays because scripts/ci/protoguard fails on both
// `const removed` and `const value changed`, with no exemption
// mechanism: this module is a published contract and the control plane
// compiles against whatever tag it has pinned. Consumers migrate first;
// the declaration can go in a later release once nothing on either side
// reads it.
//
// quality_tier itself is unaffected. It remains the internal ranking
// order, the only record of a tier_override, and a published field.
const InstallQualityFloorTier = 30

// NativeContextFloorTokens gates manifest membership in the
// coding-agent auto-selection pool: the model's OWN advertised window,
// before any host or serving consideration.
//
// Real coding-agent sessions peak at 75k–200k input tokens with 35–50k
// of fixed overhead before any conversation (#624), so a model that
// cannot hold ~200k truncates or compacts constantly. 200000 rather
// than the 200704 serve-time window because this is a manifest
// comparison and the two catalog classes sit far apart: exactly the
// 262144-native manifests pass, and the 131072 class does not.
//
// It lives here, next to the fit rules, because BOTH recommendation
// sites need it and only one of them had it. The agent's picker applied
// it from internal/router while the control plane's wizard could not —
// its doc comment said the #624 floor "would need the serve-time tuning
// inputs", which is true of the host half and NOT of this one: the
// manifest is all it takes, and the control plane holds the same
// manifests. That asymmetry is how a 131072-window model becomes the
// wizard's default on a host whose own agent would never serve it.
const NativeContextFloorTokens = 200000

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

	// BandwidthUnifiedGBs is the same quantity for a unified-memory pool,
	// and since waired-ai/waired-agent#251 it is only the FALLBACK: a host
	// whose part Host.MemoryBandwidthSpecGBs identifies uses that chip's
	// published peak instead, and only then is ClassUnified allowed to
	// exclude anything.
	//
	// Here, where the part is unknown, the figure stays annotate-only —
	// EstimateOllamaDecode leaves UpperBound unset — so it may be a rough
	// middle rather than a bound in either direction, which is what it
	// is. It is NOT the floor of the population, a claim this comment
	// carried until #251 checked it: the M1 base is 68.25 GB/s and the M2
	// and M3 bases are 100, all below 120. Nothing single-valued is an
	// upper bound across the 68..819 GB/s span the population actually
	// covers, which is the whole reason the per-chip table exists rather
	// than a retuned constant.
	//
	// Consequences of landing here are therefore bounded by construction:
	// an unrecognised part is never refused a model on speed, only told
	// that one "may be slow" — possibly wrongly in either direction. The
	// fix for a part that lands here often is to add it to the table in
	// internal/hardware, not to move this number.
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
// adapter in internal/hardware — and everything downstream sees the
// same small set of numbers. (Deliberately not spelled as a count: this
// sentence has said "five" through two additions.)
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

	// VRAMPoolMB is the VRAM ollama may pool ACROSS devices on a
	// multi-GPU host, 0 when there is nothing to pool (which is every
	// single-GPU host, so 0 is the common case rather than an error).
	// Computed by OllamaVRAMPoolMB from the device list each producer
	// already has; see that function for which devices may be summed
	// and why.
	//
	// It is read only through OllamaVRAMBudgetMB, never directly:
	// EffectiveVRAMMB stays the single-device figure that min_vram_mb,
	// engine selection and vLLM's TP=1 fallback were authored against,
	// and widening THAT would silently move all of them
	// (waired-ai/waired#678 makes the same argument in the other
	// direction).
	//
	// json:"-" for the reason MemoryBandwidthSpecGBs carries it: Host is
	// an INPUT to a decision, not a payload, and the additive-only guard
	// asks a field added to a published struct to say so explicitly.
	VRAMPoolMB int `json:"-"`

	// VRAMAvailable0MB is the first GPU's free VRAM — the single-device
	// mirror of VRAMPoolMB, and the only way the free reading reaches a
	// SINGLE-GPU host, which is the shape waired-agent#69 actually
	// reported (an 8 GB card also driving the display). A pooled host
	// gets its de-rate inside VRAMPoolMB; a one-card host has no pool,
	// so without this field it would keep sizing against the total.
	//
	// 0 means "no free reading" and leaves the total in place. Read only
	// through OllamaVRAMBudgetMB, never directly — EffectiveVRAMMB stays
	// the raw single-device figure that min_vram_mb, engine selection
	// and vLLM's TP=1 fallback were authored against, for the same
	// reason VRAMPoolMB may not widen it.
	//
	// json:"-" for the reason above: an input, not a payload.
	VRAMAvailable0MB int `json:"-"`

	// MemoryBandwidthSpecGBs is the published PEAK read bandwidth of the
	// pool the weights are read from, in GB/s. 0 means "unknown", and
	// that is a case this package must keep working for rather than an
	// error: the producer keys it off a chip table, and a part that is
	// not in the table reports nothing instead of guessing.
	//
	// A peak, therefore an UPPER bound on decode speed — which is exactly
	// what EstimateOllamaDecode needs before it may set Estimate.UpperBound
	// on a unified-memory host, and why a MEASURED figure cannot be
	// substituted here. On a unified host a CPU-side measurement is a
	// LOWER bound (it cannot reach what the GPU pulls from the same
	// pool), so feeding one in would license exclusions in the direction
	// the bound does not support. See signer.HardwareSummary's field doc:
	// spec and measured are separate fields on the wire for this reason.
	//
	// json:"-" for the reason Verdict.Estimate and Estimate.UpperBound
	// carry it: Host is an INPUT to a decision, not a payload — the wire
	// type is signer.HardwareSummary and each side adapts into this one.
	// The additive-only guard asks a field added to a published struct to
	// say so explicitly; the neighbouring fields predate the guard's view
	// of this struct and cannot be retagged now.
	MemoryBandwidthSpecGBs float64 `json:"-"`

	// CarveOutVRAMMB is GPU memory reserved at the firmware level that the
	// OS-reported RAM total EXCLUDES. It exists so TotalMemoryMB can add
	// the two figures without double-counting, on a host where they are
	// reads of one physical pool.
	//
	// Only Linux sets it, from the AMD GPU's reported VRAM total (via
	// rocm-smi, which reads sysfs mem_info_vram_total internally).
	//
	// Apple Silicon sets 0 because its "VRAM" figure is SYNTHESIZED from
	// RAM (the iogpu.wired_limit_mb sysctl, or 75 % of RAM). Windows sets
	// 0 for a different reason: the firmware carve-out there IS read, but
	// it is not memory a model may occupy in addition to RAM. Every
	// graphics allocation carries a system-memory backing store commit of
	// equal size, so a large carve-out lowers what the machine can load
	// rather than raising it, and the budget is sized from OS-visible RAM
	// instead (measured on a Ryzen AI Max+ 395,
	// waired-ai/waired-agent#863; internal/hardware/uma_common.go carries
	// the numbers).
	//
	// Those two are the whole reason this is a published quantity rather
	// than a check for "is it a Mac": what disqualifies a figure is its
	// provenance — synthesized, or read but not additive — not the
	// platform it came from.
	//
	// 0 therefore means "no separate pool", never "unknown, so guess".
	// The sum only ever grows on a host that proved its carve-out, which
	// is the direction a capacity gate can afford to be wrong in.
	//
	// json:"-" for the reason MemoryBandwidthSpecGBs carries it: Host is
	// an INPUT to a decision, not a payload, and the additive-only guard
	// asks a field added to a published struct to say so explicitly.
	CarveOutVRAMMB int `json:"-"`

	// RAMAvailableGB is how much of RAMTotalGB the operating system
	// reported as available, measured once per install/upgrade while no
	// engine or model was resident (waired-agent#568; the wire field is
	// signer.HardwareSummary.RAMAvailableGB, and the agent projects the
	// same persisted figure, so both adapters produce identical
	// verdicts).
	//
	// 0 means "measurement unavailable", never "the OS holds
	// everything" — OSMemoryDeductionGB answers with the
	// OSMemoryAllowanceGB constant then, which is also what every host
	// behind a pre-#568 agent gets. The value is clamped by the reader:
	// anything outside (0, RAMTotalGB] is treated as unavailable.
	//
	// json:"-" for the reason MemoryBandwidthSpecGBs carries it.
	RAMAvailableGB int `json:"-"`
}

// OSMemoryDeductionGB is what the operating system keeps of system RAM
// before any model is loaded: the larger of the OSMemoryAllowanceGB
// floor and this host's own install-time measurement
// (RAMTotalGB − RAMAvailableGB). One method so the capacity computation
// and the window sizing cannot subtract different figures
// (waired-agent#568, 2026-08-08 owner rulings on the issue).
//
// The measurement can only tighten: max() means a host that measured
// under the floor keeps the floor, and a host with no measurement
// (RAMAvailableGB == 0, or an implausible reading) keeps today's
// constant arithmetic. All three OS probes count reclaimable cache as
// available, so total − available never charges the OS for cache it
// would give back.
func (h Host) OSMemoryDeductionGB() int {
	if h.RAMAvailableGB <= 0 || h.RAMAvailableGB > h.RAMTotalGB {
		return OSMemoryAllowanceGB
	}
	return max(OSMemoryAllowanceGB, h.RAMTotalGB-h.RAMAvailableGB)
}

// Device is the per-device facts the pool rule reads. Like Host it is
// deliberately neither producer type: the agent's hardware.GPU carries
// driver versions and UUIDs no fit rule reads, and
// signer.HardwareGPUSummary is a wire shape fixed by compatibility.
// Both sides already hold everything below, so the rule can live here
// and be computed identically by each adapter — which is the whole
// point of this package, and why nothing new has to cross the wire.
type Device struct {
	// Vendor is lower-case, as both producers already spell it
	// ("nvidia" / "amd" / "apple").
	Vendor string

	// VRAMTotalMB is the device's raw total VRAM. 0 means "unknown" —
	// the AMD Windows registry fallback reports devices this way — and
	// such a device contributes nothing to a pool.
	VRAMTotalMB int

	// VRAMAvailableMB is how much of VRAMTotalMB the driver reported
	// free, measured once while no engine of ours held weights. 0 means
	// "no free reading" and falls back to the total, which is what every
	// producer sent before the field existed
	// (signer.HardwareGPUSummary.VRAMFreeMB carries the discipline and
	// the reason, waired-agent#69).
	//
	// Named "available" rather than "free" — matching Host.RAMAvailableGB
	// for the same quantity — partly to keep scripts/ci/protoconsumer
	// working. That guard matches producers by field NAME, so a Device
	// field spelled VRAMFreeMB and assigned right here in
	// FromHardwareSummary would read as a proto-internal producer for the
	// WIRE field of that name, and the real producer debt would never
	// become visible in its table. The same consideration named
	// LocalModelChoiceAt (waired-agent#647); the driver's own word stays
	// on the wire, where nvidia-smi's memory.free is what it reports.
	//
	// json:"-" for the reason Host.VRAMPoolMB carries it: Device is an
	// INPUT both adapters build, never a payload — the wire shape is
	// signer.HardwareGPUSummary — and the additive-only guard asks a
	// field added to a published struct to say so explicitly. Its
	// untagged siblings predate the guard's baseline.
	VRAMAvailableMB int `json:"-"`
}

// lendableMB is what this device can actually lend an engine: its free
// reading where the driver gave one, its total otherwise.
//
// The fallback is the whole safety argument. A device whose driver will
// not report free memory, and a producer that predates the field, both
// arrive here as 0 and get the total — so this rule can only ever
// de-rate a device the reader actually measured, never one it guessed
// at.
func (d Device) lendableMB() int {
	if d.VRAMAvailableMB > 0 && d.VRAMAvailableMB < d.VRAMTotalMB {
		return d.VRAMAvailableMB
	}
	return d.VRAMTotalMB
}

// OllamaVRAMPoolMB is the VRAM ollama may pool across devices, or 0
// when there is nothing to pool.
//
// Ollama DOES aggregate, which this package assumed it did not. Read
// the pinned engine's scheduler (ollama 0.31.1,
// server/sched.go:selectLlamaServerPlacement) rather than the folklore:
//
//   - Devices are grouped by backend library (ml.ByLibrary — "CUDA",
//     "ROCm", "Metal", "Vulkan"). A CUDA device and a ROCm device are
//     NEVER in one group, so any sum must be per-vendor at minimum.
//   - By default the scheduler first tries bestSingleGPUFit: a model
//     that fits in 80 % of ONE device's free memory runs on that one
//     device. Only when nothing fits alone does it take a whole group
//     via bestGPUGroupByAvailableMemory, and availableMemoryForLoad
//     then SUMS the group's free memory. So the pool is the capacity
//     bound, and the single device is merely the preferred placement
//     within it.
//   - There is no homogeneity requirement inside a group: a 4090 and a
//     3060 both under CUDA are pooled. This is why the rule cannot
//     simply mirror vLLM's tensor-parallel aggregate, which requires
//     identical devices because it shards each tensor rather than
//     splitting by layer.
//
// NVIDIA-only, and that is a scope decision rather than an oversight.
// discover/runner.go:filterIntegratedGPUs drops integrated devices —
// "dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1" —
// except integratedGPUAllowedByDefault, which admits EVERY "CUDA"
// device and admits "ROCm" only for an allowlist of GFX targets this
// repo does not detect. So for NVIDIA the integrated/discrete question
// does not arise: the engine pools those devices either way, and no
// integrated flag is needed to be right about them. Extending the sum
// to AMD needs that flag as a detected FACT — inferring it from a model
// name would put a second copy of internal/runtime's heuristic in the
// contract module, which is the drift this package exists to prevent
// (waired-ai/waired-agent#264 item 4).
//
// The deduction is per ADDITIONAL device, not per device: the base term
// is a per-card device context (see OllamaVRAMOverheadBaseDiscreteMB),
// so it repeats, while the per-weight slope is compute/scratch that
// splits with the layers and must not. OllamaVRAMOverheadMB still
// charges base + slope once against the resulting budget, which makes
// the total n*base + slope. Never double-count it elsewhere.
func OllamaVRAMPoolMB(devs []Device) int {
	var n, sum int
	for _, d := range devs {
		if d.Vendor != "nvidia" || d.VRAMTotalMB <= 0 {
			continue
		}
		n++
		// Free where it was measured, total otherwise. Summing free is
		// what the engine itself does — availableMemoryForLoad sums
		// gpu.FreeMemory — so the pool now answers the question the
		// scheduler asks rather than an optimistic neighbour of it
		// (waired-agent#69). The de-rate is applied per device, BEFORE
		// the sum, which is what #264's decision record asks for: the
		// total−free gap is per-device, so summing totals accumulates
		// it once per card.
		sum += d.lendableMB()
	}
	if n < 2 {
		// Nothing to pool. Reported as "unknown" rather than as the
		// single device's figure so every single-GPU host stays
		// bit-identical to the behaviour that predates this rule.
		return 0
	}
	return sum - (n-1)*OllamaVRAMOverheadBaseDiscreteMB
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
		RAMTotalGB:             hw.RAMTotalGB,
		RAMAvailableGB:         hw.RAMAvailableGB,
		GPUCount:               len(hw.GPUs),
		UnifiedMemory:          hw.UnifiedMemory,
		UsableVRAMMB:           hw.UsableVRAMMB,
		MemoryBandwidthSpecGBs: hw.MemoryBandwidthSpecGBs,
		CarveOutVRAMMB:         hw.CarveOutVRAMMB,
	}
	if len(hw.GPUs) > 0 {
		h.VRAM0MB = hw.GPUs[0].VRAMTotalMB
		h.VRAMAvailable0MB = hw.GPUs[0].VRAMFreeMB
	}
	// Every GPU has always been on the wire; only this adapter and its
	// agent-side twin threw the rest away. Nothing new had to be
	// published for the pool, so that fix reached every already-deployed
	// agent the moment the control plane bumped its proto tag. The free
	// reading is the exception: it is a new field, so it arrives only
	// from an agent new enough to measure it, and 0 keeps the total.
	devs := make([]Device, 0, len(hw.GPUs))
	for _, g := range hw.GPUs {
		devs = append(devs, Device{
			Vendor:          g.Vendor,
			VRAMTotalMB:     g.VRAMTotalMB,
			VRAMAvailableMB: g.VRAMFreeMB,
		})
	}
	h.VRAMPoolMB = OllamaVRAMPoolMB(devs)
	return h
}

// EffectiveVRAMMB is the VRAM budget a min_vram_mb or residency
// comparison may use. On unified-memory hosts the raw per-device figure
// overstates what the GPU can actually wire down, so the usable budget
// wins there; everyone else uses the first GPU's raw figure. Returns 0
// for CPU-only hosts, and for a unified-memory host that reports no
// usable figure it degrades to the raw one rather than to "no GPU".
//
// This is the SINGLE-DEVICE budget and stays that way. The ollama path
// asks OllamaVRAMBudgetMB instead, because ollama pools across devices
// and the callers of this one — min_vram_mb, engine selection, vLLM's
// TP=1 fallback — were all authored against a single card's figure.
func (h Host) EffectiveVRAMMB() int {
	if h.UnifiedMemory && h.UsableVRAMMB > 0 {
		return h.UsableVRAMMB
	}
	return h.VRAM0MB
}

// OllamaVRAMBudgetMB is the VRAM budget the OLLAMA path may use: the
// cross-device pool where there is one, the single-device figure
// otherwise.
//
// Two clamps, both load-bearing:
//
// A unified-memory host is one pool by construction — there is nothing
// to aggregate, and UsableVRAMMB is already the honest bound on what
// the GPU can wire down, so it keeps winning.
//
// The aggregate may never come in BELOW the single-device figure. That
// is the mirror of router.VLLMVRAMBudgetMB's own floor. It is also why
// no "floor at the largest device" clause is needed — a host whose
// GPUs[0] is its small card gets the pool, which already exceeds the
// large one. Whether EffectiveVRAMMB itself should rank devices rather
// than trust enumeration order is a separate question, deliberately not
// answered here (waired-ai/waired-agent#264 item 6).
//
// What the floor is measured AGAINST changed with waired-agent#69: it
// is the single device's FREE figure where one was measured, not its
// total. docs/decisions/20260813/1120-ollama-budget-sized-on-free-vram.md
// records why, and revises §4 of the pool decision that set the earlier
// floor. The short version is that a floor at the total made the budget
// structurally unable to shrink, which was the point while the only
// error being corrected was an UNDER-count across cards — and is
// exactly wrong once the error being corrected is an OVER-count on one.
func (h Host) OllamaVRAMBudgetMB() int {
	single := h.ollamaSingleDeviceMB()
	if h.UnifiedMemory || h.VRAMPoolMB <= single {
		return single
	}
	return h.VRAMPoolMB
}

// ollamaSingleDeviceMB is EffectiveVRAMMB as the OLLAMA path must read
// it: the same single-device budget, de-rated to what the driver
// reported free.
//
// Separate from EffectiveVRAMMB rather than folded into it, because the
// pool decision already settled that widening or narrowing THAT figure
// moves min_vram_mb, engine selection and vLLM's TP=1 fallback, all of
// which were authored against a whole card. This one moves only the
// ollama budget, which is the only consumer waired-agent#69 is about.
//
// A unified-memory host is left alone: UsableVRAMMB is already the
// honest bound on what its GPU can wire down from a shared pool, and no
// shipped detector reports a free figure for one, so there is nothing
// here to improve and a fallback to guess at.
func (h Host) ollamaSingleDeviceMB() int {
	eff := h.EffectiveVRAMMB()
	if h.UnifiedMemory {
		return eff
	}
	if h.VRAMAvailable0MB > 0 && h.VRAMAvailable0MB < eff {
		return h.VRAMAvailable0MB
	}
	return eff
}

// HasGPU reports whether the host has any GPU-addressable memory at
// all. A unified-memory host counts even when no discrete device was
// enumerated.
func (h Host) HasGPU() bool {
	return h.GPUCount > 0 || h.UnifiedMemory
}

// TotalMemoryMB is every byte a model may occupy on this machine: system
// RAM plus dedicated VRAM, counted once. It is the denominator of the
// capacity computation. Since the 2026-08-08 owner decision
// (waired-ai/waired#1067, superseding #1056's refusal rule) capacity no
// longer refuses anything: it decides which models are auto-selected
// and recommended, and what an explicit pick must be warned about and
// confirm — never whether the pick is allowed.
//
// The three classes differ only in what "dedicated VRAM" is worth here:
//
//   - ClassCPUOnly — there is none. RAM is the whole answer.
//   - ClassDiscrete — the card's memory is its own silicon, so the two
//     figures are disjoint and the sum is the machine. The pooled budget
//     is used rather than the single device's, because ollama spreads
//     layers across a group (see OllamaVRAMBudgetMB).
//   - ClassUnified — one physical pool, read twice by the OS. Adding
//     UsableVRAMMB would count the same bytes in both terms, so only a
//     PROVEN firmware carve-out is added: see CarveOutVRAMMB for why the
//     discriminator is the figure's provenance rather than the platform.
//
// The system-RAM term is net of OSMemoryDeductionGB — the
// OSMemoryAllowanceGB floor, raised by this host's own install-time
// measurement when one exists (waired-agent#568). A computation that
// reports "it fits" for a load that leaves the operating system nothing
// is not a capacity computation, and the hand-authored min_ram_gb this
// replaces had the allowance built into it (scoring.SuggestMinRAMGB:
// "VRAM at context plus a 2 GB OS/runtime headroom"). Dedicated VRAM
// takes no such deduction — the OS does not live there.
//
// It returns 0 for two different situations, and a caller must not treat
// them alike: RAMTotalGB == 0 is a FAILED PROBE (an OS whose detector we
// do not have) and means "skip the gate", while a machine with 2 GB of
// RAM and no accelerator genuinely has nothing left for a model once the
// OS is served. Read h.RAMTotalGB to tell them apart — a gate that reads
// the second as the first hands a 1 GB host the 290 GB flagship.
func (h Host) TotalMemoryMB() int {
	if h.RAMTotalGB <= 0 {
		return 0
	}
	ram := max((h.RAMTotalGB-h.OSMemoryDeductionGB())*1024, 0)
	switch h.Class() {
	case ClassDiscrete:
		if b := h.OllamaVRAMBudgetMB(); b > 0 {
			return ram + b
		}
	case ClassUnified:
		if h.CarveOutVRAMMB > 0 {
			return ram + h.CarveOutVRAMMB
		}
	}
	return ram
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
	// how much of it the GPU holds. Where there is nothing to reason
	// from — an unannotated variant, or an engine/host pair this package
	// declines to price — it carries the NO-CLAIM value
	// ({MeetsSpeedFloor: true}, no figure), never the zero value: a
	// consumer reading MeetsSpeedFloor alone would take a zero Estimate
	// for a confirmed-slow verdict (waired-agent#364).
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
// otherwise. Two cases earn it: the spilled-discrete case, which carries
// a margin no unknown hardware can eat because the card's own reads are
// priced at zero, and a unified host that published its part's peak
// bandwidth, where the bound is a fact about that machine rather than a
// constant. Only there does MeetsSpeedFloor being false mean "slow even
// under favourable assumptions". See UpperBound, and the constants above
// for which direction each of them is deliberately wrong in.
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
	// Two cases set it, for different reasons:
	//
	//   - ClassDiscrete spilled, where the GPU's contribution is priced at
	//     zero precisely so the unknown card cannot make the answer wrong.
	//     That over-estimate is STRUCTURAL: it survives whatever the
	//     bandwidth constants happen to be.
	//   - ClassUnified WHERE THE PART IS KNOWN, i.e. the host published
	//     Host.MemoryBandwidthSpecGBs. A published peak is an upper bound
	//     on that specific machine by definition, so "too slow even at
	//     peak" is a claim about the host rather than about a constant
	//     (waired-ai/waired-agent#251).
	//
	// ClassUnified where the part is NOT known falls back to
	// BandwidthUnifiedGBs, which is neither bound (see its doc), and stays
	// annotate-only. ClassCPUOnly rests entirely on
	// BandwidthSystemRAMGBs, which IS meant as an upper bound, but with
	// no structural margin behind it: a host whose memory beats the
	// constant would be excluded on the strength of the constant alone.
	// Both may still SAY "this may be slow", because that costs the user a
	// sentence rather than a choice.
	//
	// Per-chip SPEC bandwidth is what turned unified hosts into decisions
	// rather than annotations. Note "spec", not "measured": a measurement
	// on a unified host is a LOWER bound (a CPU-side benchmark cannot
	// reach what the GPU pulls from the same pool), so #252's measured
	// figure will NOT be usable here however precise it gets.
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
//     there is no spilled case to model. The bandwidth is the host's own
//     published peak when it reported one, and only then may the result
//     exclude (UpperBound); otherwise it falls back to the population
//     constant and stays an annotation (#251).
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
		// A published peak is an upper bound on THIS machine, so it may
		// exclude; the fallback constant is a population figure that is
		// neither bound, so it may only annotate (#251).
		bw, bounded := BandwidthUnifiedGBs, false
		if h.MemoryBandwidthSpecGBs > 0 {
			bw, bounded = h.MemoryBandwidthSpecGBs, true
		}
		e := rate(bw, 1)
		e.UpperBound = bounded
		e.Resident = true
		return e
	}

	// Discrete: how much of the weights the cards can hold, after the
	// same overhead and KV reservation the residency gate assumes. The
	// budget is the POOL where ollama has one — under-counting it here
	// is the costliest error this function can make, because this is
	// the branch that may exclude (#264).
	budget := h.OllamaVRAMBudgetMB()
	budgetMB := budget -
		OllamaVRAMOverheadMB(false, v.EstimatedWeightGB) -
		v.KVBytesPerTokenFP16*OllamaKVBudgetTokens/(1<<20)
	weightMB := v.EstimatedWeightGB * 1e9 / (1 << 20)
	if weightMB <= 0 || budget <= 0 {
		return pass
	}
	share := float64(budgetMB) / weightMB
	switch {
	case share >= 1:
		// The cards hold all of it. Their bandwidth decides the rate and
		// this package does not know it; the slowest shipping card that
		// could hold the weights at all is still far above the floor.
		//
		// That argument is per-DEVICE, and pooling weakens it: ollama
		// splits by layer, so two cards roughly double capacity while
		// leaving read bandwidth flat, and a pair of small cards can
		// reach a size their speed does not deserve. It is not wrong
		// enough to withhold the fix — this branch leaves UpperBound
		// unset, so it can only over-annotate, never exclude — but a
		// pooled host is exactly where a discrete per-card bandwidth
		// term would first pay for itself (#266, the discrete sibling
		// of #251's chip table).
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
	return OllamaWeightsResidentMB(v, unifiedMemory) +
		v.KVBytesPerTokenFP16*OllamaKVBudgetTokens/(1<<20)
}

// OllamaWeightsResidentMB is OllamaResidentMB without the KV term: what
// the WEIGHTS alone need in GPU-addressable memory, engine overhead
// included. Returns 0 for a variant with no weight annotation, which no
// caller may read as "it fits in nothing".
//
// The two differ by which spill the caller is asking about, and on a
// discrete card those are different questions. KV that does not fit is
// a window the serve tuning clamps (#621) or, past the clamp, pages
// that cost bandwidth per token. WEIGHTS that do not fit are re-read
// from system RAM on every prefill chunk and every decode step, and
// that is the one the operator feels: on a 16 GB card holding a 22.6 GB
// mixture of experts, 37.7 % of the weights landed in system RAM and a
// ~30k-token coding prompt prefilled at 388 tok/s — a minute and a half
// before the first output token.
//
// So this is the term the RECOMMENDATION gate compares, and
// OllamaResidentMB stays the capacity term on unified memory where
// there is nowhere to spill either of them to.
func OllamaWeightsResidentMB(v catalog.Variant, unifiedMemory bool) int {
	if v.EstimatedWeightGB <= 0 {
		return 0
	}
	// Weights are annotated in decimal GB; the budget is binary MiB.
	weightMiB := int(math.Ceil(v.EstimatedWeightGB * 1e9 / (1 << 20)))
	return weightMiB + OllamaVRAMOverheadMB(unifiedMemory, v.EstimatedWeightGB)
}

// MeetsNativeContextFloor reports whether the manifest's own advertised
// window qualifies it for the coding-agent auto-selection pool. See
// NativeContextFloorTokens for the number and for why it lives here.
//
// Auto-selection only. An explicit user choice bypasses it — with a
// visible warning, which is the caller's to word.
func MeetsNativeContextFloor(m catalog.Manifest) bool {
	return m.ContextLength >= NativeContextFloorTokens
}

// The two windows a node may declare it serves (waired#1031).
//
// There are two and not three because Claude Code resolves a session's
// window from the model id string alone: a "[1m]" suffix means 1M, a
// "claude-" prefix takes the 200k default, and only a non-"claude-" id
// consults CLAUDE_CODE_MAX_CONTEXT_TOKENS — which is a single global
// value shared by every such id. There is no way to express a third
// step, so a device that serves 140k cannot be routed traffic sized to
// it; it declares nothing and is reached by pinning instead.
//
// The catalog agrees with that shape rather than merely tolerating it.
// Manifest windows come in four classes — 32768, 131072, 262144 and
// 1048576 — and NOTHING sits between 262145 and 1048575, so an
// intermediate step would admit no model that 200k does not already
// admit. 262144 as a step would admit strictly FEWER hosts than 200704
// does, since the KV cache grows with the window while the model set
// stays identical.
const (
	// ServingWindow200k is the coding-agent window: the same 200704 the
	// serve tuning already aims for (router.CodingAgentContextFloorTokens),
	// pre-aligned to 1024. Reachable across the whole 262144-native class,
	// down to variants small enough for a laptop.
	ServingWindow200k = 200704

	// ServingWindow1M is the long-context window. Only the 1048576-native
	// class can declare it, and holding a 1M KV cache alongside those
	// models' weights is a datacenter-scale ask — this is a window for a
	// node that has one, not a target for a workstation.
	ServingWindow1M = 1048576
)

// ReasonWindowTooSmall is a fit reason code: the model's OWN advertised
// window cannot reach the serving window being asked about, so no
// hardware makes it servable. Distinct from ReasonInsufficientVRAM,
// which says the same model would fit a bigger machine — an operator
// can act on that one and cannot act on this one.
const ReasonWindowTooSmall = "window_too_small"

// ReasonWindowExceedsMemory is a RECOMMENDATION reason: the model runs
// here, but this host cannot hold the coding-agent window with it, so it
// is not what the host should be pointed at by default. NeedMB is the
// window-inclusive requirement and HaveMB the host's total memory.
//
// Distinct from ReasonWindowTooSmall, which says no hardware would help,
// and from ReasonInsufficientMemory, which is a refusal. A consumer that
// hides a model on this one has misread it — see OllamaRecommendModel.
const ReasonWindowExceedsMemory = "window_exceeds_memory"

// servingKVCacheDivisor converts the manifest's fp16 KV annotation to
// the cache the serve tuning actually exports. Both engines' coding
// path runs an 8-bit KV cache — ollama's OLLAMA_KV_CACHE_TYPE=q8_0 and
// vLLM's --kv-cache-dtype fp8 are 1 byte per element against fp16's 2 —
// so the annotated figure halves. q4_0 would quarter it and is
// deliberately not offered here: it degrades long-context recall, which
// is the entire thing a declared window is promising.
const servingKVCacheDivisor = 2

// MeetsServingWindow reports whether the manifest's own advertised
// window reaches the serving window. This is the manifest half of the
// question and the only half that can live here: whether a given HOST
// can hold the resulting KV cache depends on serve-time tuning inputs
// and a spill calibration that only the agent has, exactly as
// NativeContextFloorTokens' doc describes. Consumers without those
// inputs — the control-plane wizard — get this half, which is the half
// that was missing (waired-ai/waired#988).
func MeetsServingWindow(m catalog.Manifest, window int) bool {
	if window <= 0 {
		return true
	}
	return m.ContextLength >= window
}

// DeclarableNativeWindow is the largest serving window this MODEL could
// ever be declared at, ignoring hardware: ServingWindow1M,
// ServingWindow200k, or 0 for a model whose own window reaches neither.
//
// The 131072 class returns 0 and that is the intended answer, not an
// oversight: those models cannot hold a coding-agent session no matter
// how large the machine is.
func DeclarableNativeWindow(m catalog.Manifest) int {
	switch {
	case m.ContextLength >= ServingWindow1M:
		return ServingWindow1M
	case m.ContextLength >= ServingWindow200k:
		return ServingWindow200k
	default:
		return 0
	}
}

// ServingWindowKVMB is the KV-cache footprint of window input tokens
// for the variant, in binary MiB, at the 8-bit cache the serve tuning
// exports. Returns 0 when the variant carries no KV annotation, which
// no caller may read as "it costs nothing".
//
// The arithmetic runs in int64: a 196608 B/token variant at 1M tokens
// is 2.06e11 bytes, which overflows a 32-bit int.
func ServingWindowKVMB(v catalog.Variant, window int) int {
	if v.KVBytesPerTokenFP16 <= 0 || window <= 0 {
		return 0
	}
	b := int64(v.KVBytesPerTokenFP16) * int64(window) / servingKVCacheDivisor
	return int(b / (1 << 20))
}

// OllamaWindowResidentMB is what a variant must hold in GPU-addressable
// memory to serve `window` input tokens without spilling: weights,
// engine overhead, and the KV cache for that whole window. Returns 0
// for a variant with no weight annotation, matching OllamaResidentMB.
//
// It is NOT OllamaResidentMB with a bigger number. That one reserves a
// fixed OllamaKVBudgetTokens at fp16 — a small conservative floor for
// the question "can this host run the model at all". This one prices
// the window the node is proposing to STAND BEHIND, at the cache the
// tuner exports for it. Different questions, deliberately different
// arithmetic.
//
// Whether a shortfall is disqualifying is the caller's call, and on a
// discrete card it is not a plain comparison: the serve tuning accepts
// a bounded expected spill because a spilled flagship still beats the
// resident tier below it. That calibration lives in the agent, so this
// function returns the requirement and takes no verdict.
func OllamaWindowResidentMB(v catalog.Variant, window int, unifiedMemory bool) int {
	if v.EstimatedWeightGB <= 0 {
		return 0
	}
	return OllamaWeightsResidentMB(v, unifiedMemory) + ServingWindowKVMB(v, window)
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
// for why — but it remains the honest answer to "can the GPUs hold all
// of this", which is what the agent's deficit labels and the control
// plane's shortfall figures are explaining. Plural because on a
// multi-GPU host the answer is about the pool ollama would spread the
// layers over, not about whichever card enumerated first (#264). It IS
// still the capacity gate on unified memory, where there is nowhere to
// spill.
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
	have := h.OllamaVRAMBudgetMB()
	if have <= 0 {
		return Verdict{Fits: true} // budget unknown: don't reject the catalog
	}
	if need <= have {
		return Verdict{Fits: true}
	}
	return Verdict{Reason: ReasonInsufficientVRAM, NeedMB: need, HaveMB: have}
}

// OllamaCapacityFit decides whether ollama can serve v on this host at
// all: does the model, holding the window it is being asked about, fit
// the machine's total memory?
//
//	weights + KV(window) + engine overhead  <=  RAM + dedicated VRAM
//
// This is the ONLY rule permitted to refuse a model, grey it out, or
// exclude it from auto-selection. Everything else — quality tier, decode
// speed, whether the host can hold a 200k window — is a recommendation
// that warns and then honours an explicit choice (owner decision
// 2026-08-03, waired-ai/waired#1056; `waired`
// docs/decisions/20260803/1332-hard-vs-soft-model-limits.md).
//
// The window it is priced at is the one this host would actually SERVE
// it at (a rung of OllamaServedWindows — OllamaPlannedRung), and that is
// load-bearing rather than a detail. Refusal is reserved for certain OOM, and a machine that would
// run a 1 GB model at the engine's 32k default is not out of memory just
// because a 200k KV cache would not fit beside it. Pricing every host at
// the coding window would refuse small hosts a model they can load,
// which is the opposite of what this gate is for.
//
// The window-inclusive figure a surface SHOWS is a different quantity —
// Presentation.RequiredWindowResidentMB, always priced at the coding
// window, because "what would this need to do coding work here" is the
// question a user is asking when they read it.
//
// It replaces two rules that answered a different question. The old
// discrete/CPU-only arm compared RAMTotalGB against the hand-authored
// min_ram_gb — an opinion with ~2.3x of headroom baked into it
// (qwen3.5-4b declares 8 GB for 3.4 GB of weights) that never consulted
// the GPU at all; the old unified arm compared against the carve-out
// reading alone. Neither answered "does it fit". min_ram_gb survives as
// catalog data and as a sort key; it no longer gates.
//
// Two consequences worth stating, both intended:
//
//   - It is strictly more permissive on discrete hosts. A 6 GB-RAM host
//     with an 8 GB card stops being refused a 3.4 GB model, because the
//     hard gate exists only for certain OOM.
//   - It counts the WINDOW's KV cache, not the fixed OllamaKVBudgetTokens
//     reservation. Those differ by ~2.6 GB on qwen3.5-4b (4,915 MiB vs
//     7,539 MiB), which is how a host used to pass the gate, pull the
//     model, and then be unable to declare a window.
//
// min_ram_gb survives as the FALLBACK, for the case the computation
// cannot answer at all: a variant with no weight annotation. There the
// hand-authored threshold is the only figure there is, and keeping it is
// strictly better than admitting everything. It is no longer consulted
// for a variant the arithmetic can price, which is every variant the
// bundled catalog ships.
//
// Permissive on missing inputs otherwise, like every other rule here: a
// failed RAM probe yields a fitting verdict rather than a silent
// exclusion of the whole catalog.
//
// Fits is capacity only. Whether the result would be fast enough to want
// rides on Verdict.Estimate, so a caller can offer a slow-but-working
// model with a warning rather than silently withholding it.
func OllamaCapacityFit(m catalog.Manifest, v catalog.Variant, h Host) Verdict {
	// Priced at a window this product would SERVE (OllamaServedWindows),
	// highest rung first, not at the window the sizing shrank to.
	//
	// It used to be the latter, and that made the gate structurally
	// unable to refuse: the continuous sizing (the deprecated
	// OllamaPlannedWindow) returns the largest window that fits, so
	// asking whether that window fits is a question the sizing has
	// already answered yes. On a unified host the two even
	// come back to the same number — floor(3R/4)·1024 against
	// (R−2)·1024, equal for 5 ≤ R ≤ 8 — and a 7 GiB Mac cleared the gate
	// by 5 MiB for a model that needed 7403 MiB to serve the coding
	// window on a 4096 MiB budget (waired-ai/waired-agent#552).
	//
	// The reversal is deliberate and is the owner decision of
	// 2026-08-08: a window between the rungs is not a smaller version of
	// the product, so "this host can load it at 32k" stopped being a
	// reason to hand it over. Refusal is still reserved for a model this
	// host cannot serve — what changed is what serving means, not how
	// hard the gate presses (waired-ai/waired#1056 decision 1, amended;
	// docs/decisions/20260808/…-price-capacity-at-the-served-window.md).
	windows := OllamaServedWindows(m)
	if len(windows) == 0 {
		// No window annotation: no opinion, exactly as a missing weight
		// or a failed RAM probe yields one. Fall back to the variant-only
		// entry point rather than inventing a rung.
		return OllamaFit(v, h)
	}
	var last Verdict
	for _, w := range windows {
		last = ollamaCapacityAtWindow(v, h, w)
		if last.Fits {
			return last
		}
	}
	// Every rung refused; report the lowest one's shortfall, which is the
	// smallest ask this host still could not meet.
	return last
}

// ollamaCapacityAtWindow is OllamaCapacityFit's arithmetic against an
// explicit window, so the manifest-aware entry point and the legacy
// variant-only one share it.
func ollamaCapacityAtWindow(v catalog.Variant, h Host, window int) Verdict {
	out := Verdict{Fits: true}
	if h.RAMTotalGB <= 0 {
		// Failed probe. Skip the gate rather than reject the catalog —
		// and note this is NOT the same as TotalMemoryMB returning 0,
		// which a real machine with 2 GB of RAM also does.
		out.Estimate = EstimateOllamaDecode(v, h)
		return out
	}
	have := h.TotalMemoryMB()
	switch need := OllamaWindowResidentMB(v, window, h.UnifiedMemory); {
	case need > 0:
		if need > have {
			out = Verdict{Reason: ReasonInsufficientMemory, NeedMB: need, HaveMB: have}
		}
	case v.MinRAMGB > 0 && h.RAMTotalGB > 0 && h.RAMTotalGB < v.MinRAMGB:
		out = Verdict{
			Reason: ReasonInsufficientRAM,
			NeedMB: v.MinRAMGB * 1024,
			HaveMB: h.RAMTotalGB * 1024,
		}
	}
	out.Estimate = EstimateOllamaDecode(v, h)
	return out
}

// OllamaFit is the capacity gate for a caller that holds a variant and
// not its manifest, priced at the coding window.
//
// The signature is what forces the split: proto is additive-only across
// published tags, so the manifest could not be added here, and a variant
// alone says neither what window its model could serve nor what window
// this host would give it. Callers that have the manifest should use
// OllamaCapacityFit — this one over-prices a small host's KV cache and
// can refuse a model it would in fact load.
func OllamaFit(v catalog.Variant, h Host) Verdict {
	return ollamaCapacityAtWindow(v, h, ServingWindow200k)
}

// OllamaRecommend decides whether v should be the model this host is
// POINTED AT by default. It is policy, and OllamaFit stays capacity:
// a variant that fits but is not recommended must still be offered,
// greyed or annotated, never hidden. Hiding it is what #229 exists to
// undo.
//
// It exists because that question had two implementations, in the same
// shape as the disagreement this package was created for. The agent's
// picker ran the #624 coding-context gate — the model's native window,
// then a bounded-spill check against the host — and the control plane's
// wizard ran neither, because it holds no serve-time tuning inputs. So
// the wizard judged on the roofline decode estimate alone, and the
// roofline is structurally blind to the case that matters here: a
// mixture of experts reads only its ACTIVE weights per token, so a 35B
// MoE with 3.3B active is predicted at ~81 tok/s even with two thirds
// of its weights in system RAM. It cleared the floor, carried the
// highest quality tier, and became the default pick on a 16 GB card —
// a model the machine's own agent would never have served.
//
// The rule the two now share is the one an operator can compute in
// their head, which is why it was chosen over adding a spill cap to the
// roofline (waired-ai/waired#988):
//
//	ClassCPUOnly   no recommendation constraint — the RAM gate is the
//	               bound, and the roofline rests on a constant with no
//	               margin behind it (see BandwidthSystemRAMGBs), so it
//	               may annotate but never exclude here.
//	ClassDiscrete  the WEIGHTS are resident: weights + overhead fit
//	               OllamaVRAMBudgetMB. KV may spill — the serve tuning
//	               clamps the window (#621) and a clamped window costs
//	               tokens of context, while spilled weights cost every
//	               token of every prompt.
//	ClassUnified   the capacity rule already IS this rule plus KV, since
//	               there is nowhere to spill to; it is kept, and the
//	               #251 published-peak bound still excludes, because a
//	               pool large enough to hold a model says nothing about
//	               how fast that pool is read.
//
// On discrete hosts this can only ever shrink the recommended set: a
// variant whose weights are resident got no speed claim from the
// roofline either (EstimateOllamaDecode returns MeetsSpeedFloor for the
// resident case), so nothing newly becomes recommendable. The accepted
// regression is the other direction — a spilled MoE that really does
// decode acceptably is no longer preselected on a card that cannot hold
// it. The 24 GB anchor keeps its flagship; the 16 GB card no longer
// gets one.
//
// The margin is OllamaVRAMOverheadMB, not a fresh constant, and that is
// load-bearing rather than thrift: it is the SAME figure the #621
// serve-time clamp subtracts, so "recommended here" and "serves without
// spilling here" cannot drift apart into a model the wizard offers and
// the tuner then warns about.
//
// Permissive on missing inputs, like every other rule in this package:
// an unannotated weight or an unknown budget yields a recommendation
// rather than a silent exclusion.
func OllamaRecommend(v catalog.Variant, h Host) Verdict {
	out := Verdict{Fits: true}
	switch h.Class() {
	case ClassCPUOnly:
		// Nothing to require: there is no VRAM term to compare against.

	case ClassUnified:
		out = OllamaResident(v, h)
		if out.Fits {
			// A published peak is an upper bound on THIS machine, so
			// "too slow even at peak" is a fact about the host (#251).
			// The fallback population constant is neither bound and may
			// only annotate, which is what UpperBound encodes.
			if e := EstimateOllamaDecode(v, h); e.UpperBound && !e.MeetsSpeedFloor {
				out = Verdict{Reason: ReasonTooSlow}
			}
		}

	case ClassDiscrete:
		need := OllamaWeightsResidentMB(v, false)
		have := h.OllamaVRAMBudgetMB()
		if need > 0 && have > 0 && need > have {
			out = Verdict{Reason: ReasonWeightsSpill, NeedMB: need, HaveMB: have}
		}
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
//
// Every branch carries the no-claim Estimate. vLLM serves only from a
// discrete GPU, and that is precisely the case this package declines to
// price: the card's own bandwidth decides the answer and nothing here
// knows it (see EstimateOllamaDecode's discrete arm, and #266 for the
// table that would). "No claim" is spelled the same way it is there —
// a passing floor with no figure — because the zero value is NOT a
// neutral absence: a consumer that reads MeetsSpeedFloor alone reads it
// as "confirmed below the floor", which is how every vLLM entry came to
// be annotated "may be slow" on an H100 (waired-agent#364).
func VLLMFit(v catalog.Variant, budgetMB int) Verdict {
	var out Verdict
	switch {
	case budgetMB <= 0:
		out = Verdict{Reason: ReasonNoGPU, NeedMB: v.MinVRAMMB}
	case v.MinVRAMMB <= 0 || budgetMB >= v.MinVRAMMB:
		out = Verdict{Fits: true}
	default:
		out = Verdict{Reason: ReasonInsufficientVRAM, NeedMB: v.MinVRAMMB, HaveMB: budgetMB}
	}
	out.Estimate = Estimate{MeetsSpeedFloor: true}
	return out
}
