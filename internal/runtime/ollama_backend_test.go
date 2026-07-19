package runtime

import (
	"reflect"
	"testing"
)

func TestResolveOllamaBackend(t *testing.T) {
	cases := []struct {
		name      string
		in        BackendInputs
		wantSteps []BackendStep
	}{
		{
			name: "strix halo linux: rocm then vulkan",
			in:   BackendInputs{GOOS: "linux", StrixHaloAPU: true},
			wantSteps: []BackendStep{
				{Backend: BackendROCm, Env: []string{"HSA_OVERRIDE_GFX_VERSION=11.5.1", "OLLAMA_IGPU_ENABLE=1"}},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "strix halo linux: identified by CPU even when iGPU undetected",
			// No GPU vendor (rocm-smi absent) but CPU says Strix Halo —
			// the whole point of keying off the CPU model (#290).
			in: BackendInputs{GOOS: "linux", PrimaryGPUVendor: "", StrixHaloAPU: true},
			wantSteps: []BackendStep{
				{Backend: BackendROCm, Env: []string{"HSA_OVERRIDE_GFX_VERSION=11.5.1", "OLLAMA_IGPU_ENABLE=1"}},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "strix halo linux: APU wins even if amd GPU also detected",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", StrixHaloAPU: true},
			wantSteps: []BackendStep{
				{Backend: BackendROCm, Env: []string{"HSA_OVERRIDE_GFX_VERSION=11.5.1", "OLLAMA_IGPU_ENABLE=1"}},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "strix halo windows: vulkan only (no ROCm on Win APU)",
			in:   BackendInputs{GOOS: "windows", StrixHaloAPU: true},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name:      "apple silicon: metal, no override",
			in:        BackendInputs{GOOS: "darwin", PrimaryGPUVendor: "apple"},
			wantSteps: []BackendStep{{Backend: BackendMetal}},
		},
		{
			// macOS has only Metal (Apple Silicon) or CPU in ollama's build —
			// no ROCm/CUDA/Vulkan. A non-apple vendor on darwin (an Intel
			// Mac's iGPU, or a future detectIntel wiring) must fall to CPU,
			// never the Linux/Windows Vulkan env. Guards the parity trap.
			name:      "macos non-apple gpu: cpu, never vulkan",
			in:        BackendInputs{GOOS: "darwin", PrimaryGPUVendor: "intel"},
			wantSteps: []BackendStep{{Backend: BackendCPU}},
		},
		{
			name:      "macos no gpu: cpu",
			in:        BackendInputs{GOOS: "darwin", PrimaryGPUVendor: ""},
			wantSteps: []BackendStep{{Backend: BackendCPU}},
		},
		{
			name:      "nvidia: cuda, no override",
			in:        BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia"},
			wantSteps: []BackendStep{{Backend: BackendCUDA}},
		},
		{
			name: "amd integrated 780M windows: vulkan + igpu (not rocm)",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon 780M Graphics"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "amd integrated 780M linux: vulkan + igpu (not rocm)",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon 780M Graphics"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "amd rocm-supported RX 7900 windows: rocm then vulkan",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 7900 XTX"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "amd discrete RX 7900 linux: rocm then vulkan",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 7900 XTX"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			// Discrete AMD outside Ollama's Windows ROCm overlay set: no
			// ROCm runtime is installed, so it must use Vulkan (#40).
			name: "amd unsupported discrete RX 6600 windows: vulkan (no overlay)",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 6600"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			// Same card on Linux: ROCm is bundled and covers a broader set,
			// so try ROCm with the Vulkan probe fallback.
			name: "amd discrete RX 6600 linux: rocm then vulkan (rocm bundled)",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 6600"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			// Unknown model (amd detected, no name): safe default — Windows
			// can't confirm the overlay, Linux has bundled ROCm to try.
			name: "amd vendor, empty model, windows: vulkan",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "amd vendor, empty model, linux: rocm then vulkan",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name:      "intel igpu: vulkan",
			in:        BackendInputs{GOOS: "linux", PrimaryGPUVendor: "intel"},
			wantSteps: []BackendStep{{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}}},
		},
		{
			name:      "no gpu: cpu, no override",
			in:        BackendInputs{GOOS: "linux", PrimaryGPUVendor: ""},
			wantSteps: []BackendStep{{Backend: BackendCPU}},
		},
		{
			// Linux mobile APU whose iGPU is invisible without rocm-smi:
			// engage it via Vulkan by CPU-model signal instead of CPU (#68).
			name: "undetected amd mobile apu linux: vulkan + igpu",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "", AMDMobileAPU: true},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name: "undetected amd mobile apu windows: vulkan + igpu",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "", AMDMobileAPU: true},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1", "OLLAMA_IGPU_ENABLE=1"}},
			},
		},
		{
			name:      "unrecognised vendor: auto",
			in:        BackendInputs{GOOS: "linux", PrimaryGPUVendor: "moore-threads"},
			wantSteps: []BackendStep{{Backend: BackendAuto}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := ResolveOllamaBackend(c.in)
			if !reflect.DeepEqual(plan.Steps, c.wantSteps) {
				t.Fatalf("Steps = %+v, want %+v", plan.Steps, c.wantSteps)
			}
			if plan.Reason == "" {
				t.Errorf("Reason is empty; every plan should explain itself")
			}
			// Probes() is true iff there is a fallback step.
			if got, want := plan.Probes(), len(c.wantSteps) > 1; got != want {
				t.Errorf("Probes() = %v, want %v", got, want)
			}
			// Preferred() must equal Steps[0].
			if !reflect.DeepEqual(plan.Preferred(), c.wantSteps[0]) {
				t.Errorf("Preferred() = %+v, want %+v", plan.Preferred(), c.wantSteps[0])
			}
		})
	}
}

func TestAMDROCmSupported(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"AMD Radeon RX 7900 XTX", true},
		{"AMD Radeon RX 7600", true},
		{"AMD Radeon RX 6800 XT", true},
		{"AMD Radeon RX 6950 XT", true},
		{"AMD Radeon PRO W7900", true},
		{"AMD Radeon (TM) PRO W6800", true},
		{"AMD Radeon PRO V620", true},
		// Below Ollama's Windows overlay cut / not discrete.
		{"AMD Radeon RX 6700 XT", false},
		{"AMD Radeon RX 6600", false},
		{"AMD Radeon RX 5700 XT", false},
		{"AMD Radeon 780M Graphics", false},
		{"AMD Radeon Graphics", false},
		{"", false},
	}
	for _, c := range cases {
		if got := amdROCmSupported(c.model); got != c.want {
			t.Errorf("amdROCmSupported(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestAMDIsIntegratedModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"AMD Radeon 780M Graphics", true},
		{"AMD Radeon 760M Graphics", true},
		{"AMD Radeon 890M", true},
		{"AMD Radeon Graphics", true},            // bare Vega/Cezanne iGPU
		{"AMD Radeon(TM) Vega 8 Graphics", true}, // Vega iGPU
		// Discrete markers win, including mobile discrete.
		{"AMD Radeon RX 7900 XTX", false},
		{"AMD Radeon RX 7600M XT", false},
		{"AMD Radeon PRO W7900", false},
		{"AMD Instinct MI300X", false},
		{"", false}, // unknown -> treated as discrete/unknown, not integrated
	}
	for _, c := range cases {
		if got := amdIsIntegratedModel(c.model); got != c.want {
			t.Errorf("amdIsIntegratedModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}
