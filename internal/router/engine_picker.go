package router

import (
	"errors"
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// MinVLLMVRAMMB is the smallest VRAM size for which the auto-picker
// will choose vLLM over Ollama. Below this, even GPU-equipped hosts
// fall through to Ollama because vLLM's overhead (CUDA context,
// engine workers, KV cache) eats most of a tiny GPU before any model
// loads. 8 GB matches the smallest reasonable model card we ship.
//
// The value lives in proto/hostfit because the control plane's
// onboarding decides the same thing about the same hosts and used to
// carry its own copy of the number.
const MinVLLMVRAMMB = hostfit.MinVLLMVRAMMB

// VLLMAutoSelectable gates whether the hardware auto-picker (and the CLI's
// recommendEngine) may choose vLLM. It is true now that vLLM local serving is
// wired (#557 COMPLETED): the Linux adapter is registered
// (cmd/waired-agent/inference_vllm_linux.go) and bootstrapVLLM serves against a
// real venv, so a large NVIDIA host can actually run what the picker advertises.
// A qualifying host (NVIDIA GPU, VRAM >= MinVLLMVRAMMB, Linux) auto-picks vLLM;
// smaller GPUs, non-NVIDIA vendors, and non-Linux hosts still fall to ollama —
// the OS half of that promise is enforced by VLLMSupportedOS below, which the
// picker went without until waired-agent#319. The
// picker only advertises vLLM — the agent's own serving path
// (chooseEngine/engineViable) still declines it without an installed venv, so a
// host that auto-picks vLLM without one keeps serving on ollama until the venv
// is installed. An explicit `--prefer vllm` (Preference) forces vLLM regardless.
//
// A var, not a const, so an operator/build can still gate it off and tests can
// exercise both states.
var VLLMAutoSelectable = true

// VLLMSupportedOS reports whether vLLM serving exists on goos at all.
//
// Linux only: the Windows and darwin installers are stubs whose Active()
// always answers "no install" (internal/runtime/vllm_stub_windows.go), and
// engineViable therefore declines vllm on those hosts however the picker
// votes. An unknown/empty goos fails closed to false — an unpopulated
// hardware.Profile.OS must never unlock an engine the host cannot run.
func VLLMSupportedOS(goos string) bool { return goos == "linux" }

// VLLMAutoEligible is the single rule behind auto-selecting vLLM: the #557
// gate, an NVIDIA GPU carrying at least MinVLLMVRAMMB, and an OS that can
// serve it.
//
// PickEngine and the CLI's recommendEngine both answer through it so the two
// cannot drift again. They had: the CLI kept its own copy of the vendor/VRAM
// test, and neither carried the OS term at all, so a Windows host with a large
// NVIDIA card auto-picked an engine it can never run — every catalog surface
// then judged models against vllm while the daemon served ollama
// (waired-agent#319).
func VLLMAutoEligible(goos, vendor string, vramMB int) bool {
	return VLLMAutoSelectable &&
		strings.EqualFold(vendor, "nvidia") &&
		vramMB >= MinVLLMVRAMMB &&
		VLLMSupportedOS(goos)
}

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

	// Catalog is the manifest set this pick will be judged against. When
	// it is non-empty the auto path additionally requires that the engine
	// it is about to name can actually be fed here — see PickEngine's
	// fourth term.
	//
	// Empty means the caller has no catalog in hand, and the engine is
	// decided on hardware alone. Every production caller passes one; the
	// tests that leave it empty are exercising the hardware rule on its
	// own.
	Catalog []catalog.Manifest
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
//     ≥ MinVLLMVRAMMB AND Hardware.OS can serve vLLM
//     (VLLMSupportedOS — Linux) AND — when Catalog is supplied — that
//     engine has at least one variant fitting this host, pick "vllm".
//   - Else pick "ollama".
//
// The OS term reads Hardware.OS rather than runtime.GOOS so the rule stays a
// pure (facts) -> plan function that table-tests across all three OSes;
// production profiles carry runtime.GOOS there (hardware.defaultOSArch).
//
// The fourth term exists because every consumer of this pick judges
// models against the engine it names — the install pick, the catalog
// endpoint, computeAvailableUpdate — and none of them can revisit the
// engine once it is chosen. Naming one the catalog cannot feed hands
// SelectInstallModel an empty candidate set, and the host reports below
// the recommended spec on hardware that would serve a model perfectly
// well on the other engine. Found while scoping waired-agent#522: the
// only vLLM variants under 24 GB of VRAM are in the model generation
// that issue retires, so an 8-23 GB NVIDIA Linux host would have lost
// local inference entirely the moment those manifests were deleted.
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
		if !VLLMSupportedOS(in.Hardware.OS) {
			// vLLM serving is Linux-only. Advertising it anywhere else
			// hands every consumer of this pick — the catalog endpoint,
			// computeAvailableUpdate, install-time model selection — an
			// engine the host will never run, so they judge models against
			// vllm while the daemon serves ollama (waired-agent#319). An
			// explicit `--prefer vllm` (Preference, above) is unaffected.
			reasons = append(reasons,
				fmt.Sprintf("auto: ollama (vllm serving is linux-only; host os=%s)", osLabel(in.Hardware.OS)))
			return EnginePick{Engine: catalog.RuntimeOllama, Source: EngineSourceAuto, Reasons: reasons}, nil
		}
		if !engineServesAModelHere(in.Catalog, catalog.RuntimeVLLM, in.Hardware) {
			reasons = append(reasons,
				"auto: ollama (no vllm variant in the catalog fits this host)")
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

// engineServesAModelHere reports whether manifests hold at least one
// variant that engine could serve on hw.
//
// It answers through RankModels rather than re-deriving the fit, so the
// engine picker and the install pick a moment later ask the same
// question about the same host and cannot drift — the shape
// VLLMAutoEligible already uses for the hardware half of the rule.
//
// An empty catalog answers yes: the caller has nothing to judge
// against, so the hardware terms alone decide, which is what every
// caller got before this term existed.
//
// EngineVersion is deliberately left unset. It floors ollama's
// mtp-class variants and nothing on the vllm side declares
// MinEngineVersion today, so an unknown version excludes nothing this
// call would have counted. Were that to change, the effect is a more
// conservative answer — falling back to ollama — not a wrong engine.
func engineServesAModelHere(manifests []catalog.Manifest, engine string, hw hardware.Profile) bool {
	if len(manifests) == 0 {
		return true
	}
	ranked, err := RankModels(PickInput{Catalog: manifests, Hardware: hw, Engine: engine})
	return err == nil && len(ranked) > 0
}

// osLabel renders a Profile.OS value for a reason string, naming the
// empty case rather than leaving a blank in the audit trail.
func osLabel(goos string) string {
	if goos == "" {
		return "unknown"
	}
	return goos
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
