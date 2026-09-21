package hardware

import "strings"

// Which of the detected GPUs the inference engine will actually run on
// (waired-agent#1484).
//
// THE RULE IS OLLAMA'S, NOT OURS. The engine decides for itself which
// devices to use, and at OllamaPinnedVersion it drops every integrated
// GPU by default except two kinds (discover/runner.go at v0.34.2,
// integratedGPUAllowedByDefault):
//
//	switch device.Library {
//	case "CUDA": return true                       // e.g. NVIDIA GB10
//	case "ROCm": return defaultIntegratedROCmGFXTargets[device.GFXTarget] // {"gfx1151"}
//	default:     return false                      // every Vulkan iGPU
//	}
//
// An Intel iGPU has only the Vulkan path in that build, so every one of
// them is dropped — including the large Arc parts, which also carry open
// wrong-output reports (ollama/ollama#13964, ggml-org/llama.cpp#28648).
// The 880M/890M were proposed for the allowlist and declined after two
// test systems failed outright (ollama/ollama#16701).
//
// waired used to be MORE eager than this, routing 780M-class iGPUs onto
// Vulkan with OLLAMA_IGPU_ENABLE=1. The owner ruled on 2026-09-21 to
// follow the engine's default instead: a GPU the engine does not use by
// default is not used, and the host is described as the CPU host it then
// is. An operator who wants the iGPU anyway sets OLLAMA_IGPU_ENABLE=1 in
// the agent's environment; it reaches the engine untouched because the
// backend plan no longer sets that key.
//
// WHY THE HOST IS DESCRIBED BY THE USED LIST ONLY. Every rule downstream
// — hostfit's class, the budget, the host key, the backend plan, the
// summary the control plane classifies the host from — reads
// Profile.GPUs. A detected iGPU left in that list makes the host
// ClassDiscrete with the carve-out as its VRAM, which is the one
// classification that can EXCLUDE models, on a machine whose engine is
// running on the CPU (docs/knowledges/20260805/1610 §4). Moving the
// device out of the list before any of those rules run is what makes
// better detection unable to make the description worse.
//
// MAINTENANCE: re-read integratedGPUAllowedByDefault and
// defaultIntegratedROCmGFXTargets at every OllamaPinnedVersion bump
// (internal/runtime/ollama_version.go carries the reminder).

// ollamaDefaultIntegratedROCmGFXTargets is ollama's own allowlist of
// integrated ROCm devices, copied from defaultIntegratedROCmGFXTargets
// (discover/runner.go:31-34 at v0.34.2).
var ollamaDefaultIntegratedROCmGFXTargets = map[string]struct{}{
	"gfx1151": {}, // AMD Radeon 8060S / Ryzen AI Max+ 395 (Strix Halo)
}

// unusedIntegratedReason is the Reason every integrated GPU the engine
// leaves off carries. It says what the engine does, and how to change it.
const unusedIntegratedReason = "the engine uses an integrated GPU by default only when it is a CUDA device or a ROCm gfx1151 device; set OLLAMA_IGPU_ENABLE=1 to override"

// UnusedGPU is a GPU that was detected but that the inference engine
// will not run on, with the reason. It is a report: nothing sizes a
// model against it.
type UnusedGPU struct {
	GPU
	Reason string `json:"reason"`
}

// DetectedGPUs returns every GPU the profiler found — the ones the engine
// uses first, then the ones it does not. For the readers that want the
// hardware rather than the engine's view of it: the persisted topology
// reading, diagnostics.
func (p Profile) DetectedGPUs() []GPU {
	out := make([]GPU, 0, len(p.GPUs)+len(p.UnusedGPUs))
	out = append(out, p.GPUs...)
	for _, u := range p.UnusedGPUs {
		out = append(out, u.GPU)
	}
	return out
}

// engineUsesByDefault reports whether the engine runs on g without being
// told to, and why not when it does not.
//
// Only a device KNOWN to be integrated can be left off. Silence about a
// device keeps it in use, which is today's behaviour: dropping a GPU on
// no evidence is how a GPU host comes to be profiled as CPU-only (#67),
// and the integration answer is deliberately three-state so that silence
// is never read as a fact (integrated.go).
func engineUsesByDefault(g GPU, cpuModel string) (bool, string) {
	if !g.IntegratedKnown || !g.Integrated {
		return true, ""
	}
	switch strings.ToLower(strings.TrimSpace(g.Vendor)) {
	case "apple":
		// Metal. The allowlist above is ollama's Linux/Windows discovery;
		// on macOS the single Apple Silicon device is the engine's GPU.
		return true, ""
	case "nvidia":
		return true, ""
	case "amd":
		if g.GFXTarget != "" {
			if _, ok := ollamaDefaultIntegratedROCmGFXTargets[g.GFXTarget]; ok {
				return true, ""
			}
			return false, unusedIntegratedReason
		}
		// No architecture reading. Every Strix Halo part is gfx1151, and
		// the CPU name is what names one where the GPU side is silent.
		if IsStrixHaloAPU(cpuModel) {
			return true, ""
		}
		return false, unusedIntegratedReason
	default:
		return false, unusedIntegratedReason
	}
}

// partitionForEngine splits the detected GPUs into the ones the engine
// uses and the ones it does not, keeping each list in detection order.
func partitionForEngine(prof *Profile) (used []GPU, unused []UnusedGPU) {
	used = make([]GPU, 0, len(prof.GPUs))
	for _, g := range prof.GPUs {
		if ok, why := engineUsesByDefault(g, prof.CPU.Model); ok {
			used = append(used, g)
		} else {
			unused = append(unused, UnusedGPU{GPU: g, Reason: why})
		}
	}
	return used, unused
}
