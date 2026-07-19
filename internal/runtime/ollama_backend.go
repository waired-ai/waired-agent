package runtime

import (
	"fmt"
	"regexp"
)

// OllamaBackend names the GPU compute backend waired steers Ollama
// toward via process environment. It is informational (surfaced in the
// doctor / inference status) plus the key the probe state-cache is
// stored under; the actual steering is done by the env in BackendStep.
type OllamaBackend string

const (
	// BackendAuto leaves backend selection entirely to Ollama (no env
	// override). Used for hosts where Ollama's own auto-detection is
	// trusted and unambiguous.
	BackendAuto OllamaBackend = "auto"
	// BackendCUDA is NVIDIA. Ollama detects it automatically; we set no
	// override and only label it for diagnostics.
	BackendCUDA OllamaBackend = "cuda"
	// BackendROCm is the AMD HIP/ROCm path. On Linux Ollama bundles the
	// HIP runtime; on Windows the base package ships no ROCm and the
	// installer adds it as a ~350 MiB overlay only for the discrete SKUs
	// in Ollama's supported set (see amdROCmSupported). For Strix Halo it
	// requires the gfx1151 HSA override; for supported discrete AMD cards
	// Ollama engages it with no override.
	BackendROCm OllamaBackend = "rocm"
	// BackendVulkan is Ollama's experimental Vulkan path (Mesa RADV on
	// Linux, the AMD/Intel ICD on Windows). The only GPU route for AMD
	// APUs on Windows and for Intel iGPUs, and the Strix Halo Linux
	// fallback when bundled ROCm doesn't engage gfx1151.
	BackendVulkan OllamaBackend = "vulkan"
	// BackendMetal is Apple Silicon. Ollama auto-engages Metal (and its
	// MLX backend on ≥32 GB hosts as of 0.19+); we set no override.
	BackendMetal OllamaBackend = "metal"
	// BackendCPU means no GPU acceleration is expected on this host.
	BackendCPU OllamaBackend = "cpu"
)

// strixHaloHSAOverride is the HSA_OVERRIDE_GFX_VERSION value that points
// Ollama's bundled ROCm runtime at the Strix Halo iGPU (gfx1151).
// Without it, Ollama 0.18+ silently fails to discover gfx1151 and runs
// on CPU (ollama/ollama #15336, #13589). 11.5.1 maps to gfx1151.
const strixHaloHSAOverride = "HSA_OVERRIDE_GFX_VERSION=11.5.1"

// envOllamaVulkan opts Ollama into its (experimental) Vulkan backend.
const envOllamaVulkan = "OLLAMA_VULKAN=1"

// envOllamaIGPUEnable un-gates integrated GPUs. As of Ollama 0.30.x the
// runner DROPS any integrated GPU by default — even one it discovered
// via Vulkan — logging "dropping integrated GPU; to enable, set
// OLLAMA_IGPU_ENABLE=1" and falling back to CPU (total_vram=0). Verified
// live on a Ryzen AI Max+ 395: with only OLLAMA_VULKAN=1 the Radeon
// 8060S iGPU was detected then dropped; adding OLLAMA_IGPU_ENABLE=1 made
// it engage (type=iGPU, total≈112 GiB). So every step that targets an
// integrated GPU (Strix Halo on either OS, Intel iGPU) must set this
// alongside the backend flag, or the machine silently runs on CPU.
const envOllamaIGPUEnable = "OLLAMA_IGPU_ENABLE=1"

// BackendInputs are the host facts that drive the backend choice. They
// are extracted from a hardware.Profile by the caller (cmd/waired-agent)
// so this package stays decoupled from internal/hardware.
//
// StrixHaloAPU is sourced from the *CPU model* (hardware.IsStrixHaloAPU),
// deliberately NOT from GPU detection: on Linux the Strix Halo iGPU is
// invisible to the profiler unless rocm-smi is installed (Ollama ships
// its own HIP runtime, so most users never install the ROCm SDK), so
// the CPU string is the only reliable Strix Halo signal (#290).
type BackendInputs struct {
	GOOS             string // host runtime.GOOS: "linux" / "windows" / "darwin"
	PrimaryGPUVendor string // lower-case vendor of the first detected GPU; "" if none
	PrimaryGPUModel  string // model string of the first detected GPU (GPU.Model); "" if none
	StrixHaloAPU     bool   // CPU model matched hardware.IsStrixHaloAPU
	// AMDMobileAPU is true when the CPU model names a numbered AMD mobile
	// iGPU (hardware.IsAMDMobileAPU). Consulted only when no GPU was
	// detected (PrimaryGPUVendor == "") to still engage an iGPU that is
	// invisible to the profiler on Linux without rocm-smi (#68).
	AMDMobileAPU bool
}

// BackendStep is one spawn attempt: a labelled backend plus the env
// overrides `ollama serve` is launched with. Env is nil when no override
// is needed (Ollama auto-detects).
type BackendStep struct {
	Backend OllamaBackend
	Env     []string
}

// BackendPlan is the ordered set of backend attempts for a host.
// Steps[0] is the preferred backend. A second step is present only for
// hosts where the preferred backend can silently fail and a runtime
// fallback is warranted — Strix Halo on Linux, where bundled ROCm may
// not actually engage gfx1151, so the caller verifies GPU engagement and
// advances to the Vulkan step on CPU fallback (#290).
type BackendPlan struct {
	Steps  []BackendStep
	Reason string
}

// Preferred returns the first (best-guess) backend step.
func (p BackendPlan) Preferred() BackendStep { return p.Steps[0] }

// Probes reports whether the plan has a fallback step that the caller
// should activate when the preferred backend does not engage the GPU.
func (p BackendPlan) Probes() bool { return len(p.Steps) > 1 }

// amdROCmSupportedRes mirror scripts/install/ollama-windows.ps1's
// Test-AMDRocmSupported: the AMD GPUs that Ollama's bundled *Windows*
// ROCm overlay (v6.1) supports, matched against the GPU model string.
// This gates whether the agent may prefer ROCm on Windows, because the
// overlay is installed only for these SKUs — anything else has no ROCm
// runtime on Windows and must use Vulkan.
//
// !!! MAINTENANCE: keep in sync with Test-AMDRocmSupported in
// !!! scripts/install/ollama-windows.ps1 (that comment carries the
// !!! per-Ollama-bump review checklist). Supported today (Ollama 0.31.x,
// !!! ROCm v6.1 overlay): RX 7000 series, RX 6800/6900/6950, Radeon PRO
// !!! W6xxx/W7xxx, Radeon PRO V620.
var amdROCmSupportedRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)radeon\s+rx\s+7\d{3}`),                  // RX 7000 series
	regexp.MustCompile(`(?i)radeon\s+rx\s+6[89]\d{2}`),              // RX 6800/6900/6950
	regexp.MustCompile(`(?i)radeon\s+(\(tm\)\s+)?pro\s+w[67]\d{3}`), // PRO W6xxx/W7xxx
	regexp.MustCompile(`(?i)radeon\s+(\(tm\)\s+)?pro\s+v620`),       // PRO V620
}

// amdROCmSupported reports whether an AMD GPU model is in Ollama's
// Windows ROCm overlay support set (see amdROCmSupportedRes).
func amdROCmSupported(model string) bool {
	for _, re := range amdROCmSupportedRes {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

// amdDiscreteRe matches discrete-AMD name markers (RX / PRO / FirePro /
// Instinct). Checked first in amdIsIntegratedModel so a discrete mobile
// card ("Radeon RX 7600M") is never mistaken for an iGPU.
var amdDiscreteRe = regexp.MustCompile(`(?i)\b(rx|pro|firepro|instinct)\b`)

// amdIntegratedRe matches integrated-AMD iGPU name markers: a three-digit
// "…M" token (780M/760M/880M …), bare "Radeon Graphics" (Vega/Cezanne
// APUs), or a "Vega" iGPU.
var amdIntegratedRe = regexp.MustCompile(`(?i)(\b\d{3}m\b|radeon\s+graphics|\bvega\b)`)

// amdIsIntegratedModel reports whether an AMD GPU model names an
// integrated APU iGPU, which is not in Ollama's ROCm set and must use the
// Vulkan + OLLAMA_IGPU_ENABLE path. Discrete markers win, so a mobile
// discrete card ("Radeon RX 7600M") is not treated as integrated. An
// empty/unknown model returns false: it is treated as discrete/unknown
// and gets a ROCm attempt with a Vulkan probe fallback where ROCm exists.
func amdIsIntegratedModel(model string) bool {
	if amdDiscreteRe.MatchString(model) {
		return false
	}
	return amdIntegratedRe.MatchString(model)
}

// ResolveOllamaBackend maps host facts to an ordered backend plan.
//
// The Strix Halo APU is checked first and by CPU model, so the decision
// holds even when the iGPU was never detected (the common Linux case).
func ResolveOllamaBackend(in BackendInputs) BackendPlan {
	// darwin has exactly two backends in Ollama's macOS build: Metal (Apple
	// Silicon) or CPU. There is no ROCm / CUDA / Vulkan path on macOS, so
	// darwin is guarded up front — the vendor switch below emits Linux/
	// Windows-only GPU env (OLLAMA_VULKAN, OLLAMA_IGPU_ENABLE, the HSA
	// override), which would be meaningless-to-harmful if a future
	// detectIntel/detectAmd ever reported a non-apple vendor on a Mac.
	// Mirrors the Windows special-case inside the StrixHalo block.
	if in.GOOS == "darwin" {
		if in.PrimaryGPUVendor == "apple" {
			return BackendPlan{
				Steps:  []BackendStep{{Backend: BackendMetal}},
				Reason: "apple silicon: metal/mlx (ollama default, no override)",
			}
		}
		return BackendPlan{
			Steps:  []BackendStep{{Backend: BackendCPU}},
			Reason: "macOS non-apple gpu: cpu (ollama macOS has only metal or cpu)",
		}
	}

	if in.StrixHaloAPU {
		if in.GOOS == "windows" {
			// ROCm has no Windows APU support; Vulkan is the only GPU path.
			// OLLAMA_IGPU_ENABLE is mandatory or 0.30.x drops the iGPU.
			return BackendPlan{
				Steps:  []BackendStep{{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}}},
				Reason: "strix halo (windows): vulkan + igpu-enable — ROCm has no Windows APU support",
			}
		}
		// Linux (and any non-Windows): try ROCm with the gfx1151 HSA
		// override first (faster at long context); fall back to Vulkan if
		// the bundled ROCm runtime doesn't actually engage the iGPU. Both
		// steps carry OLLAMA_IGPU_ENABLE — the Strix Halo GPU is integrated
		// regardless of backend, so 0.30.x would otherwise drop it.
		return BackendPlan{
			Steps: []BackendStep{
				{Backend: BackendROCm, Env: []string{strixHaloHSAOverride, envOllamaIGPUEnable}},
				{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}},
			},
			Reason: "strix halo (linux): try rocm (gfx1151 HSA override), fall back to vulkan if CPU-bound",
		}
	}

	switch in.PrimaryGPUVendor {
	case "apple":
		// Metal is automatic; Ollama auto-engages MLX on ≥32 GB Apple
		// Silicon (0.19+). No override — forcing the preview MLX flag
		// would risk silent breakage, so we defer to Ollama's default.
		return BackendPlan{
			Steps:  []BackendStep{{Backend: BackendMetal}},
			Reason: "apple silicon: metal/mlx (ollama default, no override)",
		}
	case "intel":
		// Intel iGPUs have no ROCm/CUDA path; Vulkan is the GPU route, and
		// being integrated they also need OLLAMA_IGPU_ENABLE on 0.30.x.
		// (Intel detection is not wired into the profiler yet — this
		// branch is future-proofing for when detectIntel lands.)
		return BackendPlan{
			Steps:  []BackendStep{{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}}},
			Reason: "intel gpu: vulkan + igpu-enable",
		}
	case "nvidia":
		return BackendPlan{
			Steps:  []BackendStep{{Backend: BackendCUDA}},
			Reason: "nvidia gpu: cuda (ollama default, no override)",
		}
	case "amd":
		// Integrated AMD iGPUs (Radeon 780M / "Radeon Graphics" / Vega …)
		// are not in Ollama's ROCm set, and 0.30.x drops any integrated GPU
		// unless OLLAMA_IGPU_ENABLE is set — so route them to Vulkan + igpu.
		// The env is set here so engagement does not depend on the
		// installer's machine-scope OLLAMA_* flags having been written
		// (bundled Ollama / non-installer deploy / cleared env) — the #40
		// silent-CPU-plus-"rocm"-label case.
		if amdIsIntegratedModel(in.PrimaryGPUModel) {
			return BackendPlan{
				Steps:  []BackendStep{{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}}},
				Reason: "amd integrated gpu: vulkan + igpu-enable (not in ollama rocm set)",
			}
		}
		// On Windows the base package ships no ROCm; the installer adds the
		// ROCm overlay only for the SKUs in amdROCmSupported. A discrete AMD
		// outside that set therefore has no ROCm runtime and must use Vulkan
		// (matches ollama-windows.ps1 Resolve-GpuMode's vulkan path).
		if in.GOOS == "windows" && !amdROCmSupported(in.PrimaryGPUModel) {
			return BackendPlan{
				Steps:  []BackendStep{{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}}},
				Reason: "amd discrete (windows, outside ollama rocm overlay set): vulkan + igpu-enable",
			}
		}
		// Discrete AMD with ROCm available (Linux bundles the HIP runtime;
		// Windows has the overlay for supported SKUs): prefer ROCm and let
		// the engagement probe fall back to Vulkan + igpu if the model does
		// not actually land on the GPU. size_vram>0 on ROCm keeps ROCm with
		// no restart; a CPU-bound ROCm load switches to Vulkan (#290 probe).
		return BackendPlan{
			Steps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}},
			},
			Reason: "amd discrete gpu: try rocm, fall back to vulkan + igpu if CPU-bound",
		}
	case "":
		// No GPU was detected. On Linux a non-Strix AMD mobile-APU iGPU
		// (Radeon 780M/760M/880M …) is invisible to the profiler without
		// rocm-smi, so fall back to the CPU model: if it names a numbered
		// mobile iGPU, try to engage it via Vulkan (Ollama drops to CPU on
		// its own if no Vulkan device turns up). Vestigial desktop iGPUs
		// (bare "Radeon Graphics", ~2 CU) do not match IsAMDMobileAPU and
		// correctly stay on CPU (#68).
		if in.AMDMobileAPU {
			return BackendPlan{
				Steps:  []BackendStep{{Backend: BackendVulkan, Env: []string{envOllamaVulkan, envOllamaIGPUEnable}}},
				Reason: "amd mobile apu igpu (undetected, by cpu model): vulkan + igpu-enable",
			}
		}
		return BackendPlan{
			Steps:  []BackendStep{{Backend: BackendCPU}},
			Reason: "no gpu detected: cpu",
		}
	default:
		return BackendPlan{
			Steps:  []BackendStep{{Backend: BackendAuto}},
			Reason: fmt.Sprintf("unrecognised gpu vendor %q: ollama auto-detect", in.PrimaryGPUVendor),
		}
	}
}
