package router

import (
	"errors"
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// MinVLLMVRAMMB is the smallest VRAM size for which the auto-picker
// will choose vLLM over Ollama. Below this, even GPU-equipped hosts
// fall through to Ollama because vLLM's overhead (CUDA context,
// engine workers, KV cache) eats most of a tiny GPU before any model
// loads. 8 GB matches the smallest reasonable model card we ship.
const MinVLLMVRAMMB = 8 * 1024

// VLLMAutoSelectable gates whether the hardware auto-picker (and the CLI's
// recommendEngine) may choose vLLM. It is true now that vLLM local serving is
// wired (#557 COMPLETED): the Linux adapter is registered
// (cmd/waired-agent/inference_vllm_linux.go) and bootstrapVLLM serves against a
// real venv, so a large NVIDIA host can actually run what the picker advertises.
// A qualifying host (NVIDIA GPU, VRAM >= MinVLLMVRAMMB) auto-picks vLLM; smaller
// GPUs, non-NVIDIA vendors, and non-Linux hosts still fall to ollama. The
// picker only advertises vLLM — the agent's own serving path
// (chooseEngine/engineViable) still declines it without an installed venv, so a
// host that auto-picks vLLM without one keeps serving on ollama until the venv
// is installed. An explicit `--prefer vllm` (Preference) forces vLLM regardless.
//
// A var, not a const, so an operator/build can still gate it off and tests can
// exercise both states.
var VLLMAutoSelectable = true

// EngineSource describes where the engine choice came from. Surfaces
// in the decision trace so refresh prompts can say "preference" vs
// "auto" and the operator knows whether the chosen engine was
// implied by hardware or explicitly demanded.
type EngineSource string

const (
	EngineSourceAuto       EngineSource = "auto"
	EngineSourcePreference EngineSource = "preference"
)

// ErrInvalidEnginePreference is returned when EnginePickInput.Preference
// is set to a value that isn't a known engine name.
var ErrInvalidEnginePreference = errors.New("router: preference must be \"\", \"ollama\", or \"vllm\"")

// EnginePickInput is the world the engine picker sees.
type EnginePickInput struct {
	Hardware hardware.Profile

	// Preference, when non-empty, forces the engine choice and bypasses
	// the hardware-based heuristic. Accepts "ollama" or "vllm".
	// Honoured even when the host lacks the resources the chosen
	// engine wants — operators using --prefer vllm on a CPU host are
	// telling us they have an external reason for that decision.
	Preference string
}

// EnginePick is the picker's verdict.
type EnginePick struct {
	Engine  string
	Source  EngineSource
	Reasons []string
}

// PickEngine implements the Step 2.4 decision rule:
//
//   - If Preference is "ollama" or "vllm", honour it.
//   - Else if Hardware has at least one NVIDIA GPU with VRAMTotalMB
//     ≥ MinVLLMVRAMMB, pick "vllm".
//   - Else pick "ollama".
//
// Returns ErrInvalidEnginePreference when Preference is set to an
// unknown value.
func PickEngine(in EnginePickInput) (EnginePick, error) {
	if in.Preference != "" {
		switch in.Preference {
		case catalog.RuntimeOllama, catalog.RuntimeVLLM:
			return EnginePick{
				Engine: in.Preference,
				Source: EngineSourcePreference,
				Reasons: []string{
					fmt.Sprintf("preference=%q honoured (auto-detection bypassed)", in.Preference),
				},
			}, nil
		default:
			return EnginePick{}, fmt.Errorf("%w: got %q", ErrInvalidEnginePreference, in.Preference)
		}
	}

	reasons := []string{}
	gpu := primaryGPU(in.Hardware)
	if gpu == nil {
		reasons = append(reasons, fmt.Sprintf("auto: ollama (RAM-only host, %d GB total)", in.Hardware.RAMTotalGB))
		return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil
	}
	vendor := strings.ToLower(gpu.Vendor)
	// VRAM budget — UMA hosts (Apple Silicon, AMD Strix Halo) substitute
	// the operator-controlled UsableVRAMMB for the raw VRAMTotalMB so the
	// 8 GB threshold compares against the real GPU-addressable budget.
	vramMB := gpu.VRAMTotalMB
	if eff := in.Hardware.EffectiveVRAMMB(); in.Hardware.UnifiedMemory && eff > 0 {
		vramMB = eff
	}

	switch vendor {
	case "nvidia":
		reasons = append(reasons, fmt.Sprintf("NVIDIA GPU detected: %s (%d MB VRAM)", gpu.Model, vramMB))
		if vramMB < MinVLLMVRAMMB {
			reasons = append(reasons,
				fmt.Sprintf("auto: ollama (VRAM %d MB < threshold %d MB for vllm)", vramMB, MinVLLMVRAMMB))
			return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil
		}
		if !VLLMAutoSelectable {
			// vLLM serving is not yet wired (#557): selecting it would advertise
			// an engine this host can't pull or serve, so the auto path stays on
			// ollama. Explicit `--prefer vllm` (Preference, above) is unaffected.
			reasons = append(reasons,
				fmt.Sprintf("auto: ollama (VRAM %d MB ≥ %d MB, but vllm serving not yet wired (#557))", vramMB, MinVLLMVRAMMB))
			return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil
		}
		reasons = append(reasons,
			fmt.Sprintf("auto: vllm (VRAM %d MB ≥ threshold %d MB)", vramMB, MinVLLMVRAMMB))
		return EnginePick{Engine: catalog.RuntimeVLLM, Source: EngineSourceAuto, Reasons: reasons}, nil

	case "amd":
		// AMD is an Ollama path by design (#290): Ollama drives the AMD
		// GPU via ROCm (discrete cards, and Strix Halo with the gfx1151
		// HSA override) or Vulkan (APUs / fallback), selected at engine
		// start in internal/runtime/ollama_backend.go. The vLLM-ROCm
		// adapter (#130) was closed as superseded — it shares the same
		// ROCm substrate and gains nothing over Ollama outside GA
		// multi-tenant batching.
		reasons = append(reasons, fmt.Sprintf("AMD GPU detected: %s (%d MB VRAM)", gpu.Model, vramMB))
		reasons = append(reasons, "auto: ollama (canonical AMD path; ROCm/Vulkan backend chosen at engine start)")
		return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil

	case "apple":
		// Apple Silicon is an Ollama path by design (#290): Ollama runs on
		// Metal and auto-engages its MLX backend on ≥32 GB hosts (0.19+),
		// so the standalone MLX-LM adapter (#131) was closed as superseded.
		reasons = append(reasons, fmt.Sprintf("Apple GPU detected: %s (%d MB UMA budget)", gpu.Model, vramMB))
		reasons = append(reasons, "auto: ollama (canonical Apple path; Metal/MLX handled by the engine)")
		return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil

	default:
		reasons = append(reasons, fmt.Sprintf("GPU vendor %q is not recognised by the engine picker", gpu.Vendor))
		reasons = append(reasons, "auto: ollama")
		return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil
	}
}

// primaryGPU returns a pointer to the first GPU on hw, or nil for
// CPU-only hosts. Vendor-specific routing decisions are made by the
// caller; this helper is vendor-agnostic.
func primaryGPU(hw hardware.Profile) *hardware.GPU {
	if len(hw.GPUs) == 0 {
		return nil
	}
	return &hw.GPUs[0]
}
