package runtime

import (
	"regexp"
	"strconv"
	"strings"
)

// LlamaPlacement is where llama.cpp put one model load, as its own log
// says: the witness for "how much of this model is on the GPU"
// (waired-ai/waired-agent#1337, #1330).
//
// It exists because the figure the agent used to read, ollama's /api/ps
// size / size_vram, is ollama's own parse of these buffer lines and is
// wrong in both directions that matter. It left out the MTP draft
// context and reported a dense 27B at 14.97 GB against 21.2 GB really on
// the card, and it leaves out CPU_Mapped weights entirely, so a model with
// a third of its weights in system RAM read as "100% GPU"
// (docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md).
type LlamaPlacement struct {
	// ModelPath is the weights file the load read (the blob path, whose
	// name carries its digest).
	ModelPath string
	// ContextCells is the main context's n_ctx: the window times the
	// request slots.
	ContextCells int
	// OffloadedLayers / TotalLayers are "offloaded N/M layers to GPU".
	OffloadedLayers int
	TotalLayers     int
	// DeviceWeightsMiB is the model buffers on an accelerator (CUDA*,
	// ROCm*, Vulkan*, Metal / MTL*). HostWeightsMiB is the model buffers in
	// system RAM (CPU, CPU_Mapped, CPU_REPACK, *_Host).
	DeviceWeightsMiB float64
	HostWeightsMiB   float64
	// KVCacheType is the main KV cache's K type ("f16", "q8_0", "q4_0").
	KVCacheType string
	// FitProjectedMiB / FitFreeMiB are fit's "projected to use A MiB of
	// device memory vs. B MiB of free device memory"; FitShortMiB is its
	// "need to reduce device memory by R MiB", 0 when it needed nothing.
	// All 0 when fit did not run (an explicit -ngl) or on a build that
	// does not log it.
	FitProjectedMiB int
	FitFreeMiB      int
	FitShortMiB     int
}

// CPULayers is how many of TotalLayers llama.cpp kept in system RAM.
func (p LlamaPlacement) CPULayers() int { return max(p.TotalLayers-p.OffloadedLayers, 0) }

var (
	llamaStartRe     = regexp.MustCompile(`msg="starting llama-server"`)
	llamaLoaderRe    = regexp.MustCompile(`llama_model_loader: loaded meta data with .* from (\S+)`)
	llamaOffloadRe   = regexp.MustCompile(`load_tensors: offloaded (\d+)/(\d+) layers to GPU`)
	llamaModelBufRe  = regexp.MustCompile(`load_tensors:\s+(\S+) model buffer size =\s+([0-9.]+) MiB`)
	llamaNCtxRe      = regexp.MustCompile(`llama_context: n_ctx\s+=\s+(\d+)`)
	llamaKVTypeRe    = regexp.MustCompile(`llama_kv_cache: size =.*K \(([a-z0-9_]+)\)`)
	llamaFitProjRe   = regexp.MustCompile(`common_params_fit_impl: projected to use (\d+) MiB of device memory vs\. (\d+) MiB of free device memory`)
	llamaFitReduceRe = regexp.MustCompile(`need to reduce device memory by (\d+) MiB`)
)

// ParseLlamaPlacement reads the last model load in an ollama engine.log
// tail. ok is false when the tail holds no complete load — no "offloaded
// N/M" line after the last runner start — which is what an adopted engine
// (its runner writes to a log this agent does not hold), a capped log, and
// a build that logs nothing all look like. Callers must treat that as "no
// evidence", never as "nothing offloaded".
//
// Scoped to the LAST runner start: one ollama serve log holds every load
// since the engine started (keep-alive unloads and model switches reload
// the runner), and an earlier block describes a configuration that is not
// the one running. Within the block the first n_ctx and KV cache lines are
// the main model's; an MTP draft context logs its own after them.
func ParseLlamaPlacement(tail string) (LlamaPlacement, bool) {
	if tail == "" {
		return LlamaPlacement{}, false
	}
	lines := strings.Split(tail, "\n")
	start := 0
	for i, l := range lines {
		if llamaStartRe.MatchString(l) {
			start = i
		}
	}
	var p LlamaPlacement
	loaded := false
	for _, l := range lines[start:] {
		switch {
		case p.ModelPath == "" && llamaLoaderRe.MatchString(l):
			p.ModelPath = llamaLoaderRe.FindStringSubmatch(l)[1]
		case llamaFitProjRe.MatchString(l):
			m := llamaFitProjRe.FindStringSubmatch(l)
			p.FitProjectedMiB, _ = strconv.Atoi(m[1])
			p.FitFreeMiB, _ = strconv.Atoi(m[2])
		case p.FitShortMiB == 0 && llamaFitReduceRe.MatchString(l):
			p.FitShortMiB, _ = strconv.Atoi(llamaFitReduceRe.FindStringSubmatch(l)[1])
		case llamaOffloadRe.MatchString(l):
			m := llamaOffloadRe.FindStringSubmatch(l)
			p.OffloadedLayers, _ = strconv.Atoi(m[1])
			p.TotalLayers, _ = strconv.Atoi(m[2])
			loaded = true
		case loaded && llamaModelBufRe.MatchString(l):
			m := llamaModelBufRe.FindStringSubmatch(l)
			mib, _ := strconv.ParseFloat(m[2], 64)
			if llamaHostBuffer(m[1]) {
				p.HostWeightsMiB += mib
			} else {
				p.DeviceWeightsMiB += mib
			}
		case loaded && p.ContextCells == 0 && llamaNCtxRe.MatchString(l):
			p.ContextCells, _ = strconv.Atoi(llamaNCtxRe.FindStringSubmatch(l)[1])
		case loaded && p.KVCacheType == "" && llamaKVTypeRe.MatchString(l):
			p.KVCacheType = llamaKVTypeRe.FindStringSubmatch(l)[1]
		}
	}
	if !loaded || p.TotalLayers <= 0 {
		return LlamaPlacement{}, false
	}
	return p, true
}

// llamaHostBuffer reports whether a llama.cpp buffer name is system RAM.
// Device buffers are named after their backend device (CUDA0, ROCm0,
// Vulkan0, MTL0, Metal); host ones are CPU, CPU_Mapped, CPU_REPACK, and
// the pinned "<backend>_Host" buffers.
func llamaHostBuffer(name string) bool {
	return strings.HasPrefix(name, "CPU") || strings.HasSuffix(name, "_Host")
}
