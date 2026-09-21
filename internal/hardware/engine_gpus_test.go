package hardware

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// Every vendor against every integration state. The rule copies the
// engine's default (discover/runner.go integratedGPUAllowedByDefault at
// v0.34.2), so each row names what the engine does with such a device.
func TestEngineUsesByDefault(t *testing.T) {
	known := func(g GPU, yes bool) GPU { g.Integrated, g.IntegratedKnown = yes, true; return g }
	evidenceOnly := func(g GPU) GPU { g.Integrated, g.IntegratedKnown = true, false; return g }

	for _, tc := range []struct {
		name string
		gpu  GPU
		cpu  string
		want bool
	}{
		// Silence keeps a device in use: dropping a GPU on no evidence is
		// how a GPU host gets profiled as CPU-only (#67).
		{"amd, nothing read", GPU{Vendor: "amd", Model: "AMD Radeon 780M Graphics"}, "", true},
		{"intel, nothing read, memory known", GPU{Vendor: "intel", VRAMTotalMB: 12288}, "", true},
		{"amd, evidence but no knowing source", evidenceOnly(GPU{Vendor: "amd"}), "", true},
		{"amd, known discrete", known(GPU{Vendor: "amd", GFXTarget: "gfx1100"}, false), "", true},
		{"intel, known discrete, memory known (B580)", known(GPU{Vendor: "intel", PCIID: "8086:e20b", VRAMTotalMB: 12288}, false), "", true},
		// An Intel card whose memory was not read is left out rather than
		// described as a GPU with no limit (waired-agent#1483). On Linux
		// that is the daemon before `sudo waired init` has read it.
		{"intel, known discrete, memory unread", known(GPU{Vendor: "intel", PCIID: "8086:e20b"}, false), "", false},
		{"intel, nothing read, memory unread", GPU{Vendor: "intel"}, "", false},

		// Integrated devices the engine uses by default.
		{"apple silicon", known(GPU{Vendor: "apple"}, true), "Apple M4 Max", true},
		{"nvidia integrated (GB10)", known(GPU{Vendor: "nvidia", Model: "NVIDIA GB10"}, true), "", true},
		{"strix halo by gfx target", known(GPU{Vendor: "amd", GFXTarget: "gfx1151"}, true), "", true},
		{"strix halo by cpu name, gfx unread", known(GPU{Vendor: "amd"}, true), "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", true},

		// Integrated devices the engine drops by default.
		{"780M (gfx1103)", known(GPU{Vendor: "amd", GFXTarget: "gfx1103"}, true), "AMD Ryzen 7 8845HS w/ Radeon 780M Graphics", false},
		{"890M (gfx1150), declined upstream in ollama#16701", known(GPU{Vendor: "amd", GFXTarget: "gfx1150"}, true), "", false},
		{"desktop 2-CU (gfx1036)", known(GPU{Vendor: "amd", GFXTarget: "gfx1036"}, true), "AMD Ryzen 9 9950X 16-Core Processor", false},
		{"amd integrated, gfx unread, not strix halo", known(GPU{Vendor: "amd"}, true), "AMD Ryzen 7 8845HS w/ Radeon 780M Graphics", false},
		// A gfx reading outranks the CPU name: the engine keys on the gfx.
		{"gfx says not gfx1151 whatever the cpu says", known(GPU{Vendor: "amd", GFXTarget: "gfx1150"}, true), "AMD RYZEN AI MAX+ 395", false},
		{"intel arc igpu", known(GPU{Vendor: "intel", Model: "Intel(R) Arc(TM) 140T GPU"}, true), "", false},
		{"unknown vendor, integrated", known(GPU{Vendor: "moore-threads"}, true), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := engineUsesByDefault(tc.gpu, tc.cpu)
			if got != tc.want {
				t.Fatalf("engineUsesByDefault = %v, want %v", got, tc.want)
			}
			if got && why != "" {
				t.Errorf("a used device carries reason %q", why)
			}
			if !got && !strings.Contains(why, "OLLAMA_IGPU_ENABLE") && !strings.Contains(why, "sudo waired init") {
				t.Errorf("reason %q does not say how to change it", why)
			}
		})
	}
}

// profileWith builds a profile through the real pipeline — PCI pass,
// integration merge, partition, the real budget rule — so the assertions
// are about what a host is described as, not about one function.
func profileWith(goos, cpu string, ramGB int, gpus []GPU, integrated func(*Profile, int) integration) Profile {
	return NewProfiler("",
		WithOSArch(func() (string, string) { return goos, "amd64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: cpu, Cores: 16} }),
		WithRAM(func(context.Context) (int, int, error) { return ramGB, ramGB / 2, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1 << 40, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) { return gpus, Accelerators{}, nil }),
		WithUMA(func(_ context.Context, p *Profile) { applyUnifiedBudget(goos, p) }),
		WithIntegratedDetector(integrated),
	).Profile(context.Background())
}

func answers(yes bool) func(*Profile, int) integration {
	return func(*Profile, int) integration { return integratedKnown(yes) }
}

// THE DEFECT waired-agent#1484 closes, end to end. A Windows laptop with
// a 780M: the registry walk finds the adapter, the carve-out arithmetic
// knows it is integrated, and before this change the host was
// ClassDiscrete with the 512 MB carve-out as its VRAM — the one class
// that can exclude models — while the engine was about to be told to run
// on the iGPU through Vulkan. Now the engine runs it on the CPU, and the
// description agrees.
func TestProfile_A780MHostIsACPUHost(t *testing.T) {
	prof := profileWith("windows", "AMD Ryzen 7 8845HS w/ Radeon 780M Graphics", 32,
		[]GPU{{Vendor: "amd", Model: "AMD Radeon 780M Graphics", VRAMTotalMB: 512, PCIID: "1002:1900"}},
		answers(true))

	if len(prof.GPUs) != 0 {
		t.Fatalf("GPUs = %+v, want none: the engine does not use this iGPU by default", prof.GPUs)
	}
	if len(prof.UnusedGPUs) != 1 || prof.UnusedGPUs[0].Model != "AMD Radeon 780M Graphics" {
		t.Fatalf("UnusedGPUs = %+v, want the 780M", prof.UnusedGPUs)
	}
	if got := prof.HostFit().Class(); got != hostfit.ClassCPUOnly {
		t.Errorf("Class = %v, want CPU-only (was Discrete with a 512 MB budget)", got)
	}
	if got := HostKey(&prof); got != "cpu-none" {
		t.Errorf("HostKey = %q, want cpu-none: its measurements are CPU measurements", got)
	}
	if prof.UnifiedMemory || prof.UsableVRAMMB != 0 {
		t.Errorf("UnifiedMemory=%v UsableVRAMMB=%d, want no budget", prof.UnifiedMemory, prof.UsableVRAMMB)
	}
	// The report survives on the JSON endpoint.
	raw, err := json.Marshal(prof)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"unused_gpus":[{"vendor":"amd","model":"AMD Radeon 780M Graphics"`) ||
		!strings.Contains(string(raw), `"reason":"the engine uses an integrated GPU by default only when`) {
		t.Errorf("JSON does not carry the unused device and its reason: %s", raw)
	}
}

// The reference host must come through unchanged: the chip the engine
// admits, its budget, its class and its key.
func TestProfile_StrixHaloStaysInUse(t *testing.T) {
	prof := profileWith("windows", "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", 128,
		[]GPU{{Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics", VRAMTotalMB: 98304, PCIID: "1002:1586"}},
		answers(true))

	if len(prof.GPUs) != 1 || len(prof.UnusedGPUs) != 0 {
		t.Fatalf("GPUs=%+v UnusedGPUs=%+v, want the 8060S in use", prof.GPUs, prof.UnusedGPUs)
	}
	if got := prof.HostFit().Class(); got != hostfit.ClassUnified {
		t.Errorf("Class = %v, want Unified", got)
	}
	if got := HostKey(&prof); got != "unified-amd-ryzen-ai-max-395" {
		t.Errorf("HostKey = %q, want the reference host's key unchanged", got)
	}
	if prof.MemoryBandwidthSpecGBs != 256 {
		t.Errorf("MemoryBandwidthSpecGBs = %v, want 256", prof.MemoryBandwidthSpecGBs)
	}
}

// The Linux fleet host's shape: a discrete NVIDIA card and a 2-CU AMD
// iGPU. The iGPU must not change anything the host is described by.
func TestProfile_ADiscreteCardBesideAnUnusedIGPU(t *testing.T) {
	gpus := []GPU{
		{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", VRAMTotalMB: 24467, ComputeCap: "12.0", PCIID: "10de:2c34"},
		{Vendor: "amd", Model: "AMD Radeon Graphics", VRAMTotalMB: 2048, GFXTarget: "gfx1036", PCIID: "1002:13c0"},
	}
	integrated := func(p *Profile, i int) integration {
		if p.GPUs[i].Vendor == "amd" {
			return integratedKnown(true)
		}
		return integrationUnknown()
	}
	prof := profileWith("linux", "AMD Ryzen 9 9950X 16-Core Processor", 121, gpus, integrated)

	if len(prof.GPUs) != 1 || prof.GPUs[0].Vendor != "nvidia" {
		t.Fatalf("GPUs = %+v, want the NVIDIA card only", prof.GPUs)
	}
	if len(prof.UnusedGPUs) != 1 || prof.UnusedGPUs[0].GFXTarget != "gfx1036" {
		t.Fatalf("UnusedGPUs = %+v, want the gfx1036 iGPU", prof.UnusedGPUs)
	}
	h := prof.HostFit()
	if h.GPUCount != 1 || h.Class() != hostfit.ClassDiscrete || h.VRAM0MB != 24467 {
		t.Errorf("HostFit = %+v, want one discrete 24 GB card", h)
	}
	if got := HostKey(&prof); got != "discrete-nvidia-sm120" {
		t.Errorf("HostKey = %q, want discrete-nvidia-sm120", got)
	}
	if got := len(prof.DetectedGPUs()); got != 2 {
		t.Errorf("DetectedGPUs = %d, want both devices", got)
	}
}
