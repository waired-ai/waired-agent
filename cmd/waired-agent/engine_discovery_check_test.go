package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The engine's report against the prediction (waired-agent#1513), over the
// host shapes the fleet has and the ones #1495–#1499 measure.
func TestCompareEngineDiscovery(t *testing.T) {
	nvidia := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", ComputeCap: "12.0"}
	amdIGPU := hardware.UnusedGPU{GPU: hardware.GPU{Vendor: "amd", Model: "AMD GPU 1002:13c0", GFXTarget: "gfx1036",
		Integrated: true, IntegratedKnown: true}}
	strix := hardware.GPU{Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics", GFXTarget: "gfx1151",
		Integrated: true, IntegratedKnown: true}
	cuda := func(compute string) infruntime.EngineDevice {
		return infruntime.EngineDevice{Library: "CUDA", Compute: compute, Description: "NVIDIA RTX PRO 4000 Blackwell", Type: "discrete"}
	}
	vulkanAMD := infruntime.EngineDevice{Library: "Vulkan", Compute: "0.0", Description: "AMD Radeon(TM) 8060S Graphics", Type: "iGPU"}
	rocmAMD := func(gfx string) infruntime.EngineDevice {
		return infruntime.EngineDevice{Library: "ROCm", Compute: gfx, Description: "AMD Radeon(TM) 8060S Graphics", Type: "iGPU"}
	}

	for _, tc := range []struct {
		name  string
		goos  string
		pred  enginePrediction
		got   infruntime.EngineDiscovery
		wants []string // substrings, one per expected difference; empty = agree
	}{
		{
			name: "the Linux fleet host: CUDA only, the AMD iGPU set aside",
			goos: "linux",
			pred: enginePrediction{gpus: []hardware.GPU{nvidia}, setAside: []hardware.UnusedGPU{amdIGPU}},
			got:  infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{cuda("12.0")}},
		},
		{
			name:  "a compute capability waired read differently",
			goos:  "linux",
			pred:  enginePrediction{gpus: []hardware.GPU{nvidia}},
			got:   infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{cuda("8.9")}},
			wants: []string{"CUDA compute capability is 8.9 on the engine; waired read 12.0"},
		},
		{
			// The case a stale copy of the engine's rules would produce: an
			// upstream release starts keeping an iGPU waired set aside.
			name:  "the engine keeps an iGPU waired set aside",
			goos:  "linux",
			pred:  enginePrediction{gpus: []hardware.GPU{nvidia}, setAside: []hardware.UnusedGPU{amdIGPU}},
			got:   infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{cuda("12.0"), {Library: "Vulkan", Compute: "0.0", Description: "AMD Radeon Graphics", Type: "iGPU"}}},
			wants: []string{"the engine uses 1 amd GPU(s); waired expected 0"},
		},
		{
			name:  "the engine fell back to the CPU",
			goos:  "linux",
			pred:  enginePrediction{gpus: []hardware.GPU{nvidia}},
			got:   infruntime.EngineDiscovery{CPUOnly: true},
			wants: []string{"the engine uses 0 nvidia GPU(s); waired expected 1"},
		},
		{
			name: "the Windows Strix Halo arm: Vulkan with OLLAMA_IGPU_ENABLE, no overlay",
			goos: "windows",
			pred: enginePrediction{gpus: []hardware.GPU{strix}, igpuEnable: "1"},
			got:  infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{vulkanAMD}},
		},
		{
			name: "Linux Strix Halo on ROCm",
			goos: "linux",
			pred: enginePrediction{gpus: []hardware.GPU{strix}, rocmOverlay: true},
			got:  infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{rocmAMD("gfx1151")}},
		},
		{
			// #1511's symptom seen from here.
			name:  "the overlay should be there and nothing came up on ROCm",
			goos:  "linux",
			pred:  enginePrediction{gpus: []hardware.GPU{strix}, rocmOverlay: true},
			got:   infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{vulkanAMD}},
			wants: []string{"ROCm overlay should be installed; it may be missing"},
		},
		{
			name:  "a gfx target waired read differently",
			goos:  "linux",
			pred:  enginePrediction{gpus: []hardware.GPU{strix}, rocmOverlay: true},
			got:   infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{rocmAMD("gfx1150")}},
			wants: []string{"ROCm target is gfx1150 on the engine; waired read gfx1151"},
		},
		{
			name: "the operator turned integrated GPUs on",
			goos: "linux",
			pred: enginePrediction{setAside: []hardware.UnusedGPU{amdIGPU}, igpuEnable: "true"},
			got:  infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{{Library: "Vulkan", Compute: "0.0", Description: "AMD Radeon Graphics", Type: "iGPU"}}},
		},
		{
			name: "the operator turned integrated GPUs off",
			goos: "linux",
			pred: enginePrediction{gpus: []hardware.GPU{strix}, igpuEnable: "0"},
			got:  infruntime.EngineDiscovery{CPUOnly: true},
		},
		{
			// An Intel card whose memory was not read is left out of the
			// budget, not out of the engine: either answer is expected.
			name: "an unread Intel card, used or not",
			goos: "linux",
			pred: enginePrediction{setAside: []hardware.UnusedGPU{{GPU: hardware.GPU{Vendor: "intel", Model: "Intel Arc B580"}, MemoryUnread: true}}},
			got:  infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{{Library: "Vulkan", Compute: "0.0", Description: "Intel(R) Arc(TM) B580 Graphics", Type: "discrete"}}},
		},
		{
			name: "Apple",
			goos: "darwin",
			pred: enginePrediction{gpus: []hardware.GPU{{Vendor: "apple", Model: "Apple M4"}}},
			got:  infruntime.EngineDiscovery{Devices: []infruntime.EngineDevice{{Library: "Metal", Compute: "0.0", Description: "Apple M4", Type: "iGPU"}}},
		},
		{
			name: "a CPU host",
			goos: "linux",
			got:  infruntime.EngineDiscovery{CPUOnly: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diffs := compareEngineDiscovery(tc.goos, tc.pred, tc.got)
			if len(diffs) != len(tc.wants) {
				t.Fatalf("differences = %q, want %d: %q", diffs, len(tc.wants), tc.wants)
			}
			for i, w := range tc.wants {
				if !strings.Contains(diffs[i], w) {
					t.Errorf("difference %d = %q, want it to contain %q", i, diffs[i], w)
				}
			}
		})
	}
}

func TestIGPUEnableFor(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if got := igpuEnableFor([]string{"OLLAMA_IGPU_ENABLE=1"}, env("0")); got != "1" {
		t.Errorf("plan env = %q, want the plan's 1 over the inherited 0", got)
	}
	if got := igpuEnableFor(nil, env("0")); got != "0" {
		t.Errorf("inherited = %q, want 0", got)
	}
}

// End to end through a real adapter's engine.log: agreement is an INFO
// line, a difference a WARN that names it, and an adopted engine — whose
// output is not in this agent's log — is left alone.
func TestReportEngineDiscovery(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "runtime", "testdata", "ollama", "discovery", "cuda-rtx-pro-4000.log"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, pred enginePrediction) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "engine.log"), fixture, 0o644); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		p := &agentInferenceProvider{
			logger:   slog.New(slog.NewTextHandler(&buf, nil)),
			ollama:   infruntime.NewOllamaAdapter(infruntime.OllamaConfig{LogDir: dir}),
			bootPlan: engineBootstrapPlan{prediction: pred},
		}
		p.reportEngineDiscovery()
		return buf.String()
	}
	nvidia := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", ComputeCap: "12.0"}

	if out := run(t, enginePrediction{gpus: []hardware.GPU{nvidia}}); !strings.Contains(out, "level=INFO") ||
		!strings.Contains(out, "matches waired's prediction") {
		t.Errorf("agreement logged %q", out)
	}
	if out := run(t, enginePrediction{}); !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, "the engine uses 1 nvidia GPU(s); waired expected 0") {
		t.Errorf("difference logged %q", out)
	}
}
