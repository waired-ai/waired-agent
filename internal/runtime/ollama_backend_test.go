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
			name:      "amd discrete: rocm, no override",
			in:        BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd"},
			wantSteps: []BackendStep{{Backend: BackendROCm}},
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
