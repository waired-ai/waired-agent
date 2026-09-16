package hostfit

import (
	"math"

	"github.com/waired-ai/waired-agent/proto/catalog"
)

// Memory estimate for an ollama load, term by term, the way llama.cpp's
// own fit (common/fit.cpp) adds it up (waired-ai/waired-agent#1337).
//
// ollama 0.31.1 and later pass no -ngl: llama.cpp's --fit measures every
// buffer of a dry-run load and keeps whole layers on the device while
//
//	model + KV + recurrent state + compute + draft context  <=  free − target
//
// with target = 1 GiB, or projector + 1 GiB when ollama offloads a
// projector (llm/llama_server.go mmprojFitTargetMiB, ollama v0.33.3). The
// estimate below mirrors those terms so the product predicts the same
// placement the engine will choose. It replaces a single-point
// calibration (OllamaSpillCalibration, #625) that stood in for the terms
// the older arithmetic did not have: the fit target, the recurrent state
// of hybrid models, the MTP draft context, and the KV block size.
//
// Terms that are engineering facts are exported constants. Terms that are
// measured — the compute buffers and the per-process device context — are
// unexported, so a recalibration is a new proto tag rather than a const
// value change the additive-only guard refuses.

// KV-cache block factors relative to f16, from ggml's block layout: a
// q8_0 block stores 32 values in 34 bytes and a q4_0 block in 18, against
// 64 bytes at f16. They match the engine's own allocation to the byte:
// 200,704 cells × 65,536 B/token is 12,544 MiB at f16, 6,664 MiB at q8_0
// and 3,528 MiB at q4_0 on a 24 GB card
// (docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md,
// decision 10).
const (
	OllamaKVBlockFactorQ8_0 = 34.0 / 64
	OllamaKVBlockFactorQ4_0 = 18.0 / 64
)

// OllamaFitTargetMB is llama.cpp's default fit target: the device memory
// --fit leaves free beyond everything it measured. ollama raises it by the
// projector's size when it offloads one, and the projector is already
// inside catalog.Variant.EstimatedWeightGB, so pricing the whole weight
// plus this target is the same inequality.
const OllamaFitTargetMB = 1024

// Measured terms (see the file comment for why they are unexported).
// Sources: llama.cpp fit and buffer lines on a 24 GB CUDA card, a 48 GB
// Apple M5 Pro, a 16 GB Apple M4 and a 128 GB Radeon 8060S (Vulkan), all
// ollama 0.33.3 at its default ubatch of 512 (waired-ai/waired#1357,
// waired-ai/waired-agent#1337).
const (
	// ollamaUBatchTokens is the ubatch ollama passes (-b and -ub) when device
	// memory is tight. Its scheduler starts at 2048 for a window above
	// 32,768 tokens and steps down to 1024 and then 512 unless its own
	// prediction (file size plus an f16 cache) stays under 60 % and 75 % of
	// free memory (server/sched.go automaticGenerationBatch, ollama v0.33.3).
	// A larger ubatch therefore only happens with that much memory to spare,
	// where it cannot change whether a load fits, so the estimate prices 512.
	ollamaUBatchTokens = 512

	// The main context's compute buffer is a base that scales with the
	// ubatch, the attention mask (ubatch × cells × 2 B), and — when the KV
	// cache is quantized — one full-attention layer's K and V dequantized
	// to f16 for the whole window. On the dense 27B at 200,704 cells and
	// ubatch 512 that is 80 + 196 + 784 = 1,060 MiB, the logged figure. The
	// base logged 52 to 80 MiB at ubatch 512, 117 at 1024 and 96 to 233 at
	// 2048 across CUDA, Metal and Vulkan, so it is charged 80 MiB per 512
	// tokens of ubatch on every backend (an f16 cache logged 43 MiB more).
	ollamaComputeBasePer512MB = 80

	// ollamaSpillComputeMB is how much the compute buffer grows once a
	// full-attention layer's KV cache is in system RAM: 1,060 MiB fully
	// offloaded against 1,500 and 1,532 MiB with 5 and 13 layers on the
	// CPU, same window; 1,116 with only linear layers there.
	ollamaSpillComputeMB = 472

	// ollamaDeviceContextMB is what one llama-server process holds on a
	// CUDA device before fit measures free memory (driver context and
	// allocator reservations): the reading the product takes before the
	// engine starts, less the free figure fit reports.
	ollamaDeviceContextMB = 395

	// ollamaUnifiedContextMB is the same on a unified-memory host.
	ollamaUnifiedContextMB = 256

	// ollamaProjectorExtraMB is what ollama's fit target adds beyond the
	// projector's own weights: 24 MiB on the CUDA and Vulkan hosts. On Metal
	// the target was 385, 961 and 1,109 MiB above 192, 637 and 861 MiB
	// projectors — the projector a second time plus 193 to 324 MiB, which
	// is llama-server adding its own projector estimate to ollama's pad — so
	// Metal is charged the projector again plus ollamaProjectorExtraMetalMB.
	ollamaProjectorExtraMB      = 25
	ollamaProjectorExtraMetalMB = 330

	// The MTP draft head's second context. On the CUDA card its compute
	// buffer was 104 MiB + 837 B per cell and its setup held another
	// 600 MiB of device memory; on Metal its compute buffer matched the
	// main context's and the setup held ~190 MiB.
	ollamaDraftComputeBaseMB       = 104
	ollamaDraftComputeBytesPerCell = 837
	ollamaDraftContextMB           = 600
	ollamaDraftContextUnifiedMB    = 200
)

// OllamaKVCacheFactor is the KV-cache size of kvType relative to f16.
// An unknown type is priced at f16, the largest, so an unrecognised value
// can only over-reserve.
func OllamaKVCacheFactor(kvType string) float64 {
	switch kvType {
	case catalog.KVCacheQ8_0:
		return OllamaKVBlockFactorQ8_0
	case catalog.KVCacheQ4_0:
		return OllamaKVBlockFactorQ4_0
	}
	return 1
}

// kvCacheTypeForFactor maps the older float parameter onto a type, for the
// entry points whose published signature still takes a factor.
func kvCacheTypeForFactor(kvFactor float64) string {
	switch {
	case kvFactor >= 0.75:
		return catalog.KVCacheF16
	case kvFactor >= 0.4:
		return catalog.KVCacheQ8_0
	}
	return catalog.KVCacheQ4_0
}

// OllamaMemory is one ollama load's memory, term by term, in binary MiB.
type OllamaMemory struct {
	// DeviceWeightsMB is the weights llama.cpp may place on the device,
	// projector included (it loads inside the fit target), less
	// HostWeightsMB. From the GGUF layout's loaded bytes when the variant
	// has one, from EstimatedWeightGB otherwise.
	DeviceWeightsMB int
	// HostWeightsMB is the input-layer tensors llama.cpp keeps in system
	// RAM however much device memory is free
	// (catalog.Variant.HostResidentWeightGB).
	HostWeightsMB int
	// KVCacheMB is the attention KV cache for every cell of the window
	// across all request slots.
	KVCacheMB int
	// RecurrentStateMB is the fixed-size state of a hybrid model's linear
	// layers, one copy per slot and one more per drafted token.
	RecurrentStateMB int
	// ComputeMB is the main context's compute buffer.
	ComputeMB int
	// DraftMB is everything the MTP draft head adds: its f16 KV cache, its
	// compute buffer and its setup. 0 when the tag drafts nothing.
	DraftMB int
	// DraftKVCacheMB is the part of DraftMB that is the draft head's own
	// KV cache, so a surface can count it with KVCacheMB.
	DraftKVCacheMB int
	// FixedMB is the per-process device context, per device. On a variant
	// without a GGUF layout it is the older overhead term
	// (OllamaVRAMOverheadMB) and ComputeMB, RecurrentStateMB, DraftMB and
	// FitTargetMB are 0.
	FixedMB int
	// FitTargetMB is the device memory llama.cpp's fit leaves free per
	// device (OllamaFitTargetMB). It is headroom rather than an allocation:
	// DeviceMB counts it, because the fit does, and TotalMB does not.
	FitTargetMB int
	// TotalLayers is llama.cpp's M in "offloaded N/M layers": the blocks
	// plus the output layer. 0 when the layout is unknown.
	TotalLayers int
}

// DeviceMB is what the load needs in GPU-addressable memory to place
// every layer there.
func (e OllamaMemory) DeviceMB() int {
	return e.DeviceWeightsMB + e.KVCacheMB + e.RecurrentStateMB + e.ComputeMB + e.DraftMB + e.FixedMB + e.FitTargetMB
}

// TotalMB is what the load allocates, wherever it lands: DeviceMB less the
// fit target, plus the weights that stay in system RAM. It is the
// capacity question's figure.
func (e OllamaMemory) TotalMB() int { return e.DeviceMB() - e.FitTargetMB + e.HostWeightsMB }

// OllamaEstimateMemory prices loading v on h with a kvType KV cache,
// window tokens per request slot and parallel slots. window 0 prices the
// load with no context at all (weights, fixed terms and base buffers),
// which is the "do the weights fit" question.
//
// The zero value is returned for a variant with no weight annotation;
// no caller may read it as "needs nothing".
func OllamaEstimateMemory(v catalog.Variant, h Host, kvType string, window, parallel int) OllamaMemory {
	return ollamaEstimateMemoryAt(v, h, kvType, window, parallel, ollamaUBatchTokens)
}

// ollamaEstimateMemoryAt is OllamaEstimateMemory at a given ubatch, so the
// estimate can be held to logged loads that ran with a larger one.
func ollamaEstimateMemoryAt(v catalog.Variant, h Host, kvType string, window, parallel, ubatch int) OllamaMemory {
	if v.EstimatedWeightGB <= 0 {
		return OllamaMemory{}
	}
	parallel = max(parallel, 1)
	cells := int64(max(window, 0)) * int64(parallel)
	weightMB := int(math.Ceil(v.EstimatedWeightGB * 1e9 / (1 << 20)))
	hostMB := 0
	if v.HostResidentWeightGB > 0 {
		hostMB = int(math.Round(v.HostResidentWeightGB * 1e9 / (1 << 20)))
	}
	out := OllamaMemory{
		DeviceWeightsMB: max(weightMB-hostMB, 0),
		HostWeightsMB:   hostMB,
		KVCacheMB:       bytesToMiB(float64(v.KVBytesPerTokenFP16) * OllamaKVCacheFactor(kvType) * float64(cells)),
	}
	g := v.GGUF
	if g == nil || g.BlockCount <= 0 || g.TensorBytes <= 0 {
		out.FixedMB = OllamaVRAMOverheadMB(h.UnifiedMemory, v.EstimatedWeightGB)
		return out
	}
	// With a layout the weights are the bytes llama.cpp actually loads,
	// not the catalog's download size: the whole tensor table, less the
	// next-token blocks when nothing drafts with them, plus the projector
	// ollama offloads beside the model and the device copy of a tied
	// embedding.
	loaded := g.TensorBytes + g.ProjectorBytes + g.TiedOutputBytes
	draft := catalog.MTPDraftTokens(v)
	if draft <= 0 {
		loaded -= g.NextNBytes
	}
	out.DeviceWeightsMB = max(bytesToMiB(float64(loaded))-hostMB, 0)
	out.TotalLayers = g.BlockCount + 1
	unified := h.Class() == ClassUnified
	copies := int64(parallel) * int64(1+max(draft, 0))
	out.RecurrentStateMB = bytesToMiB(float64(g.RecurrentStateBytes * copies))
	layerKVPerCell := 0.0
	if g.FullAttentionLayers > 0 {
		layerKVPerCell = float64(v.KVBytesPerTokenFP16) / float64(g.FullAttentionLayers)
	}
	metal := h.GPUVendor == "apple"
	base := (ollamaComputeBasePer512MB*ubatch + 511) / 512
	compute := float64(ubatch*2) * float64(cells)
	if OllamaKVCacheFactor(kvType) < 1 {
		compute += layerKVPerCell * float64(cells)
	}
	out.ComputeMB = base + bytesToMiB(compute)
	if draft > 0 && layerKVPerCell > 0 {
		draftKV := bytesToMiB(layerKVPerCell * float64(cells))
		out.DraftKVCacheMB = draftKV
		if unified {
			out.DraftMB = draftKV + out.ComputeMB + ollamaDraftContextUnifiedMB
		} else {
			out.DraftMB = draftKV + ollamaDraftComputeBaseMB +
				bytesToMiB(float64(ollamaDraftComputeBytesPerCell)*float64(cells)) + ollamaDraftContextMB
		}
	}
	switch h.Class() {
	case ClassDiscrete:
		n := ollamaDevices(h)
		out.FixedMB, out.FitTargetMB = n*ollamaDeviceContextMB, n*OllamaFitTargetMB
	case ClassUnified:
		out.FixedMB, out.FitTargetMB = ollamaUnifiedContextMB, OllamaFitTargetMB
	}
	if projector := g.ProjectorBytes + g.InlineProjectorBytes; out.FitTargetMB > 0 && projector > 0 {
		if metal {
			out.FitTargetMB += bytesToMiB(float64(projector)) + ollamaProjectorExtraMetalMB
		} else {
			out.FitTargetMB += ollamaProjectorExtraMB
		}
	}
	return out
}

// ollamaDevices is how many devices the ollama budget spans: every NVIDIA
// device pooled into VRAMPoolMB, one otherwise. Each device holds its own
// process context and its own fit target.
func ollamaDevices(h Host) int {
	if h.UnifiedMemory || h.VRAMPoolMB <= h.ollamaSingleDeviceMB() || h.GPUCount < 2 {
		return 1
	}
	return h.GPUCount
}

func bytesToMiB(b float64) int {
	if b <= 0 {
		return 0
	}
	return int(math.Ceil(b / (1 << 20)))
}

// OllamaPlacement is where llama.cpp's fit is predicted to put a load.
type OllamaPlacement struct {
	// GPULayers / TotalLayers predict llama.cpp's "offloaded N/M layers":
	// layers held entirely in GPU-addressable memory. On a mixture of
	// experts a layer whose experts were moved to system RAM does not
	// count. Both are 0 when the layout is unknown.
	GPULayers   int
	TotalLayers int
	// CPUWeightMB is how much of the device-placeable weights the fit
	// moves to system RAM. HostWeightsMB is not part of it: those never
	// were placeable.
	CPUWeightMB int
	// CPUWeightShare is CPUWeightMB over the device-placeable weights, in
	// [0, 1].
	CPUWeightShare float64
	// ShortMB is how far the full-offload requirement exceeds the budget;
	// 0 when everything fits.
	ShortMB int
}

// OllamaPredictPlacement predicts the fit's placement of v on h. A host
// with no GPU-addressable memory places everything in system RAM.
//
// llama.cpp fills the device from the LAST block backwards, so the blocks
// that stay in system RAM are the first ones. What each of them frees is
// its weights plus, on a hybrid model, its recurrent state or — every
// full_attention_interval-th block — its KV cache. On a mixture of experts
// the fit first moves expert tensors, block by block from the front, and
// only then whole blocks. Once a full-attention block's KV cache is in
// system RAM the device compute buffer grows (ollamaSpillComputeMB), which
// the walk charges when it reaches the first such block.
func OllamaPredictPlacement(v catalog.Variant, h Host, kvType string, window, parallel int) OllamaPlacement {
	mem := OllamaEstimateMemory(v, h, kvType, window, parallel)
	out := OllamaPlacement{TotalLayers: mem.TotalLayers, GPULayers: mem.TotalLayers}
	if mem.DeviceWeightsMB <= 0 {
		return out
	}
	budget := h.OllamaVRAMBudgetMB()
	if !h.HasGPU() || budget <= 0 {
		out.GPULayers = 0
		out.CPUWeightMB = mem.DeviceWeightsMB
		out.CPUWeightShare = 1
		return out
	}
	short := mem.DeviceMB() - budget
	if short <= 0 {
		return out
	}
	out.ShortMB = short
	g := v.GGUF
	blocks := 0
	if g != nil {
		blocks = g.BlockCount - g.NextNLayers
	}
	if mem.TotalLayers == 0 || blocks <= 0 || g.RepeatingBytes <= 0 {
		// No layout: layers move with their share of the KV cache, so the
		// weights leaving the device are the shortfall's weight share.
		movable := mem.DeviceWeightsMB + mem.KVCacheMB
		out.CPUWeightMB = min(short*mem.DeviceWeightsMB/max(movable, 1), mem.DeviceWeightsMB)
		out.CPUWeightShare = float64(out.CPUWeightMB) / float64(mem.DeviceWeightsMB)
		return out
	}

	perBlock := float64(g.RepeatingBytes) / (1 << 20) / float64(blocks)
	perBlockExperts := float64(g.ExpertBytes) / (1 << 20) / float64(blocks)
	interval, kvPerFull, rsPerLinear := 0, 0.0, 0.0
	if g.FullAttentionLayers > 0 {
		interval = max(blocks/g.FullAttentionLayers, 1)
		kvPerFull = float64(mem.KVCacheMB) / float64(g.FullAttentionLayers)
	}
	if linear := blocks - g.FullAttentionLayers; linear > 0 {
		rsPerLinear = float64(mem.RecurrentStateMB) / float64(linear)
	}
	isFull := func(i int) bool { return interval > 0 && (i+1)%interval == 0 }

	need, freed, cpuWeight := float64(short), 0.0, 0.0
	expertBlocks := 0
	if perBlockExperts > 0 {
		for ; expertBlocks < blocks && freed < need; expertBlocks++ {
			freed += perBlockExperts
			cpuWeight += perBlockExperts
		}
	}
	cpuBlocks, bumped := 0, false
	for ; cpuBlocks < blocks && freed < need; cpuBlocks++ {
		c := perBlock - perBlockExperts
		cpuWeight += c
		if isFull(cpuBlocks) {
			c += kvPerFull
			if !bumped {
				need += ollamaSpillComputeMB
				bumped = true
			}
		} else {
			c += rsPerLinear
		}
		freed += c
	}
	switch {
	case freed < need:
		// Not even every repeating block in system RAM is enough: the
		// output and next-token blocks go too.
		out.GPULayers = 0
		cpuWeight = float64(mem.DeviceWeightsMB)
	default:
		out.GPULayers = out.TotalLayers - max(cpuBlocks, expertBlocks)
	}
	out.CPUWeightMB = min(int(math.Ceil(cpuWeight)), mem.DeviceWeightsMB)
	out.CPUWeightShare = float64(out.CPUWeightMB) / float64(mem.DeviceWeightsMB)
	return out
}

// OllamaDeviceCapacityTokens is the largest window, rounded down to a
// multiple of 1024, whose full-offload requirement at one request slot
// fits h's GPU-addressable budget — or, on a host with none, its system
// RAM budget. 0 when the inputs are unknown or the weights alone do not
// fit.
func OllamaDeviceCapacityTokens(v catalog.Variant, h Host, kvType string) int {
	if v.EstimatedWeightGB <= 0 || v.KVBytesPerTokenFP16 <= 0 {
		return 0
	}
	budget, total := h.OllamaVRAMBudgetMB(), false
	if budget <= 0 {
		budget, total = max((h.RAMTotalGB-h.OSMemoryDeductionGB())*1024, 0), true
	}
	if budget <= 0 {
		return 0
	}
	need := func(window int) int {
		e := OllamaEstimateMemory(v, h, kvType, window, 1)
		if total {
			return e.TotalMB()
		}
		return e.DeviceMB()
	}
	base := need(0)
	if base > budget {
		return 0
	}
	// The requirement is affine in the window; measure its slope over a
	// large span so per-MiB rounding does not bias it.
	const span = 1 << 20
	slope := float64(need(span)-base) / span
	if slope <= 0 {
		return 0
	}
	tokens := int(float64(budget-base) / slope)
	tokens = tokens / 1024 * 1024
	// Rounding in the terms can leave the solved window a MiB over.
	for tokens > 0 && need(tokens) > budget {
		tokens -= 1024
	}
	return max(tokens, 0)
}
