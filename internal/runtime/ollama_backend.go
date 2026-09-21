package runtime

import "fmt"

// OllamaBackend names the GPU compute backend Ollama is expected to run
// on. It is a LABEL: surfaced in the doctor and inference status, and
// corrected by the engagement check (cmd/waired-agent) when nothing
// actually landed on the GPU. Which backend runs is Ollama's own
// decision, except where a BackendPlan carries an override.
type OllamaBackend string

const (
	// BackendAuto: Ollama chooses between the backends it has. An AMD
	// card is the usual case — ROCm where the overlay's rocBLAS carries
	// its gfx target, Vulkan otherwise.
	BackendAuto OllamaBackend = "auto"
	// BackendCUDA is NVIDIA. Ollama detects it automatically.
	BackendCUDA OllamaBackend = "cuda"
	// BackendROCm is the AMD HIP/ROCm path. On both operating systems it
	// ships as a separate ~250 MB overlay (247 MB at 0.33.3, read off the
	// asset) that the installer fetches when WantsROCmOverlay says so.
	BackendROCm OllamaBackend = "rocm"
	// BackendVulkan is Ollama's Vulkan path (Mesa RADV / ANV on Linux, the
	// vendor ICD on Windows), enabled by default since 0.30. Intel's only
	// GPU route, and the one waired names for Windows Strix Halo.
	BackendVulkan OllamaBackend = "vulkan"
	// BackendMetal is Apple Silicon. Ollama auto-engages Metal (and its
	// MLX backend on >=32 GB hosts as of 0.19+).
	BackendMetal OllamaBackend = "metal"
	// BackendCPU means no GPU acceleration is expected on this host.
	BackendCPU OllamaBackend = "cpu"
)

// envOllamaIGPUEnable un-gates integrated GPUs. Since Ollama 0.30 the
// runner DROPS every integrated GPU it discovered through Vulkan, logging
// "dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1" and
// running on the CPU; only CUDA devices and ROCm gfx1151 are admitted by
// default (discover/runner.go integratedGPUAllowedByDefault at v0.34.2).
//
// It is set in exactly one place: the Windows Strix Halo arm, which names
// Vulkan on a measurement and so needs its iGPU un-gated. Everywhere else
// the engine's default stands (waired-agent#1484) — and because no plan
// sets the key, an operator's own OLLAMA_IGPU_ENABLE in the agent's
// environment reaches the engine untouched (processEnv drops only the
// keys a plan sets).
const envOllamaIGPUEnable = "OLLAMA_IGPU_ENABLE=1"

// BackendInputs are the host facts that drive the plan. They are
// extracted from a hardware.Profile's GPUs IN USE by the caller
// (internal/setup.OllamaBackendInputs) so this package stays decoupled
// from internal/hardware.
type BackendInputs struct {
	GOOS             string // host runtime.GOOS: "linux" / "windows" / "darwin"
	PrimaryGPUVendor string // lower-case vendor of the first GPU in use; "" if none
	// StrixHaloAPU is hardware.StrixHaloHost: the AMD GPU in use is a
	// gfx1151, or — with no GPU reading — the CPU names a Ryzen AI Max.
	StrixHaloAPU bool
	// AMDGPU reports that an AMD GPU is in use, whichever position it
	// holds: the ROCm overlay is fetched for it (WantsROCmOverlay).
	AMDGPU bool
}

// BackendPlan is what `ollama serve` is launched with: a label for the
// backend expected to engage, and the environment overrides — none, on
// every host but one.
//
// It used to be an ordered list of steps (ROCm, then Vulkan) that the
// engagement probe walked by restarting the engine. Ollama does that
// itself since 0.30: a device visible to both backends goes to ROCm
// (ml/device.go PreferredLibrary), and one ROCm drops for want of kernels
// (discover/amd.go filterUnsupportedROCmDevices) is served through
// Vulkan, which is on by default. The steps duplicated the engine's own
// fallback, so they are gone (waired-agent#1492).
type BackendPlan struct {
	Backend OllamaBackend
	Env     []string
	Reason  string
}

// WantsROCmOverlay reports whether the installer should fetch Ollama's
// ROCm overlay, which is the one backend decision left for waired to
// make: ROCm is a separate download on both operating systems, and the
// engine cannot choose a backend that is not on disk.
//
// Fetch it for any AMD GPU in use, and let the engine decide — it keeps
// the devices the overlay's rocBLAS carries kernels for and serves the
// rest through Vulkan. The one exception is the Windows Strix Halo, whose
// measured arm names Vulkan: with the overlay on disk the engine would
// prefer ROCm for it.
//
// This replaces a hand-kept copy of upstream's Windows SKU table
// (amdROCmSupportedRes), which disagreed with both upstream's own docs
// and the overlay's actual contents (waired-agent#1248, #1266).
func WantsROCmOverlay(in BackendInputs) bool {
	if in.GOOS == "darwin" || !in.AMDGPU {
		return false
	}
	return in.GOOS != "windows" || !in.StrixHaloAPU
}

// ResolveOllamaBackend maps host facts to the plan.
func ResolveOllamaBackend(in BackendInputs) BackendPlan {
	// macOS has exactly two backends in Ollama's build: Metal on Apple
	// Silicon, or the CPU.
	if in.GOOS == "darwin" {
		if in.PrimaryGPUVendor == "apple" {
			return BackendPlan{Backend: BackendMetal, Reason: "apple silicon: metal/mlx (ollama default)"}
		}
		return BackendPlan{Backend: BackendCPU, Reason: "macOS non-apple gpu: cpu (ollama macOS has only metal or cpu)"}
	}

	// THE ONE OVERRIDE. Every other host takes the engine's own choice.
	//
	// AT EVERY OllamaPinnedVersion BUMP, re-read the threads below before
	// assuming this arm still needs to name Vulkan; they are the reason it
	// does (full context: docs/knowledges/20260906/1700-what-to-recheck-
	// about-amd-backends.md). Checked open at 0.34.2 (2026-09-20):
	// ollama/ollama#17895 (ROCm on gfx1151 answers wrongly above ~4k
	// prompt tokens), #17847 (KV state bleeds between requests), #17498
	// (Gemma 4 output corrupted from ~1.2k tokens). The Vulkan-side
	// counterweight, #17870 (device lost on very long prefill, num_batch=128
	// works around it), was closed not_planned on 2026-09-07.
	if in.StrixHaloAPU && in.GOOS == "windows" {
		// Vulkan, because it is FASTER here — not because ROCm is
		// absent. This arm used to say "ROCm has no Windows APU
		// support", and that was never true of any engine this
		// product has pinned: on a Ryzen AI Max+ 395, ollama v0.31.1
		// — the version the stale stamp below named — already reports
		// `library=ROCm compute=gfx1151 ... type=iGPU total="76.8 GiB"`,
		// identically to 0.33.3, which serves a 21.8 GB model wholly
		// on the GPU. The Windows overlay has been rocm_v7_1 carrying
		// gfx1151 since at least v0.31.1 (kernel lists read off the
		// asset for 0.31.1, 0.32.13, 0.32.15, 0.33.2 and 0.33.3 — they
		// are the same). waired-agent#1233, measured 2026-09-06.
		//
		// The reason that decides it is CORRECTNESS, not speed.
		// ROCm on gfx1151 has open upstream defects that a coding
		// agent meets on every request: ollama/ollama#17895 has it
		// returning wrong output above ~4k prompt tokens — "fluent,
		// confident, wrong answers" with nothing logged, and past ~8k
		// byte-identical replies to different prompts — and
		// ollama/ollama#17847 has it bleeding KV state between
		// sequential requests at OLLAMA_NUM_PARALLEL=1. Both report
		// the same machine clean on Vulkan and on CPU. Vulkan has one
		// of its own (#17870, an amdgpu compute-ring timeout on very
		// long prefill) but it FAILS the request; ROCm answers wrongly,
		// which no gate here or in the catalog would catch.
		//
		// Speed agrees, and is the lesser reason. Ranges do not
		// overlap. qwen3.6:35b-a3b-q4_K_M, the two backends alternated
		// so drift cannot favour one, six turns each of a 30-36k-token
		// prompt at num_predict 512, the cold turn after each load
		// discarded:
		//
		//	prefill  Vulkan 876.8 (839.4-912.6)  ROCm 636.3 (580.0-654.1)
		//	decode   Vulkan  49.2 ( 47.9- 50.1)  ROCm  43.8 ( 43.1- 44.3)
		//
		// Vulkan by 37.8 % and 12.3 % at the medians — measured
		// against ollama's own bundled ROCm, which is a stock HIP
		// build. Tuned gfx1151 builds exist and report the split going
		// the other way on prefill, so this is a fact about the engine
		// we ship, not about ROCm. An earlier pass
		// measured one turn per backend at num_predict 64 — a
		// 1.2-second decode window — and read 4.9 % and 11.5 % off it;
		// a later session under the same conditions put ROCm ahead
		// instead, the same Vulkan configuration having moved 22 %
		// between the two. Size the window before believing a gap this
		// small, and alternate the backends inside one run so drift
		// cannot masquerade as a difference.
		//
		// Both genuinely run on the GPU — device buffers, not host
		// ones (`load_tensors: ROCm0 model buffer size = 21171.18 MiB`),
		// and peak GPU utilisation of 453 % and 112 %. The control that
		// settles it: with both backends moved out of lib/ollama the
		// same turn runs on the CPU at 158 tok/s prefill and 19.3
		// decode, with size_vram 0 and the GPU at 0 % — 4.0x and 2.3x
		// off the slower of the two GPU backends.
		//
		// The exposed figure matters as much as the rates: the engine
		// offers ROCm 78197 MiB of the unified pool where it offers
		// Vulkan 99437 MiB, so a model that fits under Vulkan may not
		// fit at all under ROCm on the same machine.
		//
		// A ROCm step here also changed nothing WHEN MEASURED: with
		// both backends on disk, four restarts — including one with
		// the HSA override set — all dispatched
		// `[{ID:0 Library:Vulkan}]`. Adding the step would only make
		// the installer fetch a ~250 MB overlay nothing then used
		// (WantsROCmOverlay says no for this arm). A user who wants to try
		// ROCm anyway has WAIRED_OLLAMA_GPU_MODE=rocm.
		//
		// Do not generalise that observation, and do not lean on it
		// here: the arm names Vulkan explicitly, so it does not
		// depend on ollama's choice, which is just as well because
		// the SOURCE PREDICTS THE OPPOSITE. ml/device.go's
		// PreferredLibrary returns true for CUDA and ROCm and false
		// for everything else, with no per-device case at all
		// (waired-agent#1248, read at v0.33.3). Four Vulkan dispatches
		// on gfx1151 are a fact; the mechanism that produced them is
		// not established, so on any other SKU assume ROCm wins once
		// the overlay is on disk.
		//
		// OLLAMA_IGPU_ENABLE is mandatory or the runner drops the iGPU.
		// OLLAMA_VULKAN is not set: Vulkan is on by default since 0.30,
		// and ROCm is kept off this host by not fetching its overlay.
		return BackendPlan{
			Backend: BackendVulkan,
			Env:     []string{envOllamaIGPUEnable},
			Reason:  "strix halo (windows): vulkan + igpu-enable — measured faster and correct where ROCm is not",
		}
	}

	switch in.PrimaryGPUVendor {
	case "apple":
		return BackendPlan{Backend: BackendMetal, Reason: "apple silicon: metal/mlx (ollama default)"}
	case "nvidia":
		return BackendPlan{Backend: BackendCUDA, Reason: "nvidia gpu: cuda (ollama default)"}
	case "amd":
		// ROCm where the overlay carries the card's gfx target — which on
		// Linux includes the Strix Halo's gfx1151, admitted by default —
		// and Vulkan for a card it does not. The engine decides.
		return BackendPlan{Backend: BackendAuto, Reason: "amd gpu: ollama chooses rocm or vulkan (ollama default)"}
	case "intel":
		// Only a discrete Intel card is in use (the engine drops Intel
		// iGPUs by default); Vulkan is Intel's only GPU route.
		return BackendPlan{Backend: BackendVulkan, Reason: "intel gpu: vulkan (ollama default)"}
	case "":
		return BackendPlan{Backend: BackendCPU, Reason: "no gpu the engine uses by default: cpu"}
	default:
		return BackendPlan{Backend: BackendAuto, Reason: fmt.Sprintf("unrecognised gpu vendor %q: ollama auto-detect", in.PrimaryGPUVendor)}
	}
}
