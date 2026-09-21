package runtime

import (
	"reflect"
	"testing"
)

// The plan is the engine's default everywhere but one host. Rewritten for
// waired-agent#1492: the plan used to be a list of steps (ROCm, then
// Vulkan) with OLLAMA_VULKAN, the gfx1151 HSA override and a hand-kept
// Windows SKU table deciding between them. Ollama 0.34.2 does all of
// that itself, so every row here now expects no environment except the
// Windows Strix Halo, and the label says what the engine is expected to
// choose.
func TestResolveOllamaBackend(t *testing.T) {
	cases := []struct {
		name    string
		in      BackendInputs
		want    OllamaBackend
		wantEnv []string
	}{
		{
			// The one override, measured (#1233): Vulkan with the iGPU
			// un-gated. No OLLAMA_VULKAN — Vulkan is on by default.
			name:    "strix halo windows: vulkan + igpu-enable",
			in:      BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", StrixHaloAPU: true, AMDGPU: true},
			want:    BackendVulkan,
			wantEnv: []string{"OLLAMA_IGPU_ENABLE=1"},
		},
		{
			// INVERTED by #1492 (owner ruling: follow the engine, no
			// special rule on Linux). Was ROCm with the HSA override and
			// OLLAMA_IGPU_ENABLE, then Vulkan. The engine admits gfx1151
			// through ROCm by default and its ROCm build carries the
			// kernels natively.
			name: "strix halo linux: the engine's choice",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", StrixHaloAPU: true, AMDGPU: true},
			want: BackendAuto,
		},
		{
			name: "amd discrete linux: the engine's choice",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", AMDGPU: true},
			want: BackendAuto,
		},
		{
			// INVERTED by #1492. An RX 6600 on Windows used to be forced
			// onto Vulkan because it was outside a hand-kept copy of
			// upstream's SKU table. The engine drops what the overlay's
			// rocBLAS cannot serve and keeps it on Vulkan by itself.
			name: "amd discrete windows: the engine's choice",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", AMDGPU: true},
			want: BackendAuto,
		},
		{
			name: "nvidia: cuda",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia"},
			want: BackendCUDA,
		},
		{
			name: "intel discrete: vulkan, no env",
			in:   BackendInputs{GOOS: "windows", PrimaryGPUVendor: "intel"},
			want: BackendVulkan,
		},
		{
			name: "apple silicon: metal",
			in:   BackendInputs{GOOS: "darwin", PrimaryGPUVendor: "apple"},
			want: BackendMetal,
		},
		{
			name: "macos non-apple gpu: cpu, never a linux/windows backend",
			in:   BackendInputs{GOOS: "darwin", PrimaryGPUVendor: "intel"},
			want: BackendCPU,
		},
		{
			name: "no gpu in use: cpu",
			in:   BackendInputs{GOOS: "linux"},
			want: BackendCPU,
		},
		{
			// A Windows Strix Halo whose adapter the registry walk missed
			// is still recognised by its CPU (hardware.StrixHaloHost), and
			// still gets the measured arm.
			name:    "strix halo by cpu name, adapter unseen, windows",
			in:      BackendInputs{GOOS: "windows", StrixHaloAPU: true},
			want:    BackendVulkan,
			wantEnv: []string{"OLLAMA_IGPU_ENABLE=1"},
		},
		{
			name: "unrecognised vendor: auto",
			in:   BackendInputs{GOOS: "linux", PrimaryGPUVendor: "moore-threads"},
			want: BackendAuto,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := ResolveOllamaBackend(c.in)
			if plan.Backend != c.want || !reflect.DeepEqual(plan.Env, c.wantEnv) {
				t.Fatalf("plan = %+v, want backend %s env %v", plan, c.want, c.wantEnv)
			}
			if plan.Reason == "" {
				t.Errorf("Reason is empty; every plan should explain itself")
			}
		})
	}
}

// TestOnlyStrixHaloWindowsSetsEnv pins the rule #1484 and #1492 set: the
// engine's own default decides, and the one place waired overrides it is
// the measured Windows Strix Halo arm. Any other plan with environment
// would override the engine without a measurement behind it — and an
// OLLAMA_IGPU_ENABLE would switch on an iGPU the engine leaves off.
func TestOnlyStrixHaloWindowsSetsEnv(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "darwin"} {
		for _, vendor := range []string{"", "amd", "intel", "nvidia", "apple", "moore-threads"} {
			for _, strix := range []bool{false, true} {
				in := BackendInputs{GOOS: goos, PrimaryGPUVendor: vendor, StrixHaloAPU: strix, AMDGPU: vendor == "amd"}
				plan := ResolveOllamaBackend(in)
				if len(plan.Env) > 0 && (goos != "windows" || !strix) {
					t.Errorf("%+v: plan sets %v", in, plan.Env)
				}
			}
		}
	}
}

func TestWantsROCmOverlay(t *testing.T) {
	for _, c := range []struct {
		name string
		in   BackendInputs
		want bool
	}{
		{"amd discrete linux", BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", AMDGPU: true}, true},
		{"amd discrete windows, any card", BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", AMDGPU: true}, true},
		// Owner ruling: no special rule on Linux, so the engine gets ROCm.
		{"strix halo linux", BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", StrixHaloAPU: true, AMDGPU: true}, true},
		// The measured Windows arm names Vulkan; the overlay would make
		// the engine prefer ROCm.
		{"strix halo windows", BackendInputs{GOOS: "windows", PrimaryGPUVendor: "amd", StrixHaloAPU: true, AMDGPU: true}, false},
		{"amd card second, nvidia first", BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia", AMDGPU: true}, true},
		{"no amd gpu in use", BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia"}, false},
		{"macos", BackendInputs{GOOS: "darwin", AMDGPU: true}, false},
	} {
		if got := WantsROCmOverlay(c.in); got != c.want {
			t.Errorf("%s: WantsROCmOverlay = %v, want %v", c.name, got, c.want)
		}
	}
}
