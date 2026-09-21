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
			// The parenthetical used to read "no ROCm on Win APU". ROCm is
			// present and does engage gfx1151 there; Vulkan wins on
			// correctness and on the numbers (#1233). See the arm itself.
			name: "strix halo windows: vulkan, not the ROCm that is also there",
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
			// INVERTED by waired-agent#1484. This row used to expect
			// Vulkan + OLLAMA_IGPU_ENABLE, chosen from the NAME. A 780M
			// the profiler knows is integrated never reaches the plan (it
			// is in Profile.UnusedGPUs); one whose integration nothing
			// read is left to the engine, which drops it by default. The
			// name decides nothing, and nothing here switches an iGPU on.
			name: "amd 780M by name, windows: no igpu-enable",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon 780M Graphics"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			name: "amd 780M by name, linux: no igpu-enable",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon 780M Graphics"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			name: "amd rocm-supported RX 7900 windows: rocm then vulkan",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 7900 XTX"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			name: "amd discrete RX 7900 linux: rocm then vulkan",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 7900 XTX"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			// Discrete AMD outside Ollama's Windows ROCm overlay set: no
			// ROCm runtime is installed, so it must use Vulkan (#40).
			name: "amd unsupported discrete RX 6600 windows: vulkan (no overlay)",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 6600"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			// Same card on Linux: ROCm is bundled and covers a broader set,
			// so try ROCm with the Vulkan probe fallback.
			name: "amd discrete RX 6600 linux: rocm then vulkan (rocm bundled)",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 6600"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			// Unknown model (amd detected, no name): safe default — Windows
			// can't confirm the overlay, Linux has bundled ROCm to try.
			name: "amd vendor, empty model, windows: vulkan",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd"},
			wantSteps: []BackendStep{
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			name: "amd vendor, empty model, linux: rocm then vulkan",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd"},
			wantSteps: []BackendStep{
				{Backend: BackendROCm},
				{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}},
			},
		},
		{
			// Only a discrete Intel card reaches the plan (an integrated
			// one is set aside by the profiler), so no igpu-enable.
			name:      "intel discrete: vulkan",
			in:        BackendInputs{GOOS: "linux", PrimaryGPUVendor: "intel"},
			wantSteps: []BackendStep{{Backend: BackendVulkan, Env: []string{"OLLAMA_VULKAN=1"}}},
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

// TestOnlyStrixHaloSetsIGPUEnable pins the rule waired-agent#1484 set:
// the engine's own default decides which integrated GPUs run, and the
// one place waired overrides it is the chip the engine itself admits
// (gfx1151) where waired routes it through Vulkan. Any other plan that
// set OLLAMA_IGPU_ENABLE would switch on an iGPU the engine leaves off —
// for a desktop Ryzen with a discrete card, the 2-CU iGPU beside it.
func TestOnlyStrixHaloSetsIGPUEnable(t *testing.T) {
	models := []string{"", "AMD Radeon 780M Graphics", "AMD Radeon(TM) Graphics", "AMD Radeon RX 7900 XTX", "AMD Radeon RX 6600", "NVIDIA GeForce RTX 4090", "Intel(R) Arc(TM) B580 Graphics"}
	for _, goos := range []string{"linux", "windows", "darwin"} {
		for _, vendor := range []string{"", "amd", "intel", "nvidia", "apple", "moore-threads"} {
			for _, model := range models {
				for _, strix := range []bool{false, true} {
					in := BackendInputs{GOOS: goos, PrimaryGPUVendor: vendor, PrimaryGPUModel: model, StrixHaloAPU: strix}
					for _, step := range ResolveOllamaBackend(in).Steps {
						for _, kv := range step.Env {
							if kv == envOllamaIGPUEnable && !strix {
								t.Errorf("%+v: step %s sets %s", in, step.Backend, kv)
							}
						}
					}
				}
			}
		}
	}
}
