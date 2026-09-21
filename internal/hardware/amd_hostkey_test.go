package hardware

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// waired-agent#1485: an AMD discrete card is named by its ISA target, an
// APU by its CPU. Before this change every AMD row below took the CPU
// string, so the first three all keyed as the CPU and the fourth named
// an AMD card after an Intel processor.
func TestChipSlug_AMD(t *testing.T) {
	known := func(g GPU, yes bool) GPU { g.Integrated, g.IntegratedKnown = yes, true; return g }
	for _, tc := range []struct {
		name string
		gpu  GPU
		cpu  string
		want string
	}{
		{"RX 7900 XTX", known(GPU{Vendor: "amd", GFXTarget: "gfx1100"}, false), "AMD Ryzen 9 7950X 16-Core Processor", "gfx1100"},
		{"RX 9070 XT, 1.5x less bandwidth, a different key", known(GPU{Vendor: "amd", GFXTarget: "gfx1201"}, false), "AMD Ryzen 9 7950X 16-Core Processor", "gfx1201"},
		{"an AMD card in an Intel box never names Intel", known(GPU{Vendor: "amd", GFXTarget: "gfx1100"}, false), "Intel(R) Core(TM) i9-14900K", "gfx1100"},
		// Windows never reads "discrete": the carve-out arithmetic only
		// ever says integrated. A discrete target is enough.
		{"windows dGPU, integration unread, discrete target", GPU{Vendor: "amd", GFXTarget: "gfx1100"}, "AMD Ryzen 9 7950X 16-Core Processor", "gfx1100"},
		{"known discrete, no target read", known(GPU{Vendor: "amd"}, false), "AMD Ryzen 9 7950X 16-Core Processor", "unknown"},

		// The reference host keeps its key in every state its readings can be in.
		{"strix halo, known integrated", known(GPU{Vendor: "amd", GFXTarget: "gfx1151"}, true), "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", "ryzen-ai-max-395"},
		{"strix halo, integration unread, APU target", GPU{Vendor: "amd", GFXTarget: "gfx1151"}, "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", "ryzen-ai-max-395"},
		{"strix halo, nothing read at all", GPU{Vendor: "amd"}, "AMD Ryzen AI Max+ 395", "ryzen-ai-max-395"},
		{"phoenix, known integrated", known(GPU{Vendor: "amd", GFXTarget: "gfx1103"}, true), "AMD Ryzen 9 7940HS w/ Radeon 780M Graphics", "ryzen-9-7940hs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChipSlug(tc.gpu, tc.cpu); got != tc.want {
				t.Errorf("ChipSlug = %q, want %q", got, tc.want)
			}
		})
	}
}

// The key end to end: the sysfs reading of a discrete card through the
// profiler, with the PCI pair beside it.
func TestHostKey_AMDDiscrete(t *testing.T) {
	prof := profileWith("linux", "AMD Ryzen 9 7950X 16-Core Processor", 64,
		[]GPU{{Vendor: "amd", Model: "AMD Radeon RX 7900 XTX", VRAMTotalMB: 24576, PCIID: "1002:744c", GFXTarget: "gfx1100", IntegratedKnown: true}},
		func(*Profile, int) integration { return integrationUnknown() })
	if got := HostKey(&prof); got != "discrete-amd-gfx1100" {
		t.Errorf("HostKey = %q, want discrete-amd-gfx1100", got)
	}
}

// On Windows the target comes from the PCI pair, so the same card keys
// the same way there.
func TestHostKey_AMDDiscreteOnWindowsNamesTheTargetFromThePCIPair(t *testing.T) {
	prof := profileWith("windows", "AMD Ryzen 9 7950X 16-Core Processor", 64,
		[]GPU{{Vendor: "amd", Model: "AMD Radeon RX 7900 XTX", VRAMTotalMB: 24576, PCIID: "1002:744c"}},
		func(*Profile, int) integration { return integrationUnknown() })
	if prof.GPUs[0].GFXTarget != "gfx1100" {
		t.Fatalf("GFXTarget = %q, want gfx1100 from the PCI pair", prof.GPUs[0].GFXTarget)
	}
	if got := HostKey(&prof); got != "discrete-amd-gfx1100" {
		t.Errorf("HostKey = %q, want discrete-amd-gfx1100", got)
	}
}

// A Strix Halo recognised by its ISA target alone — a SKU whose CPU
// string does not say "Ryzen AI Max" — gets the unified budget and the
// published bandwidth.
func TestStrixHaloByTargetWithoutTheFamilyName(t *testing.T) {
	prof := profileWith("windows", "AMD Engineering Sample", 128,
		[]GPU{{Vendor: "amd", Model: "AMD Radeon(TM) Graphics", VRAMTotalMB: 512, PCIID: "1002:1586"}},
		func(*Profile, int) integration { return integratedKnown(true) })
	if len(prof.GPUs) != 1 {
		t.Fatalf("GPUs = %+v, UnusedGPUs = %+v; gfx1151 is one the engine uses", prof.GPUs, prof.UnusedGPUs)
	}
	if !prof.UnifiedMemory || prof.UsableVRAMMB != 96*1024 {
		t.Errorf("UnifiedMemory=%v UsableVRAMMB=%d, want the Strix Halo budget", prof.UnifiedMemory, prof.UsableVRAMMB)
	}
	if prof.MemoryBandwidthSpecGBs != 256 {
		t.Errorf("MemoryBandwidthSpecGBs = %v, want 256 from the gfx table", prof.MemoryBandwidthSpecGBs)
	}
}

// The other direction: a CPU string that says "Ryzen AI Max" does not
// override a target reading that says otherwise. The engine keys on the
// target, and so does every rule that must agree with it.
//
// On both operating systems, and Windows is the one that bites: its
// budget branch needs no VRAM reading, so a CPU-name fallback that only
// looked at the GPUs in use — the gfx1150 is set aside — would publish a
// unified budget for a GPU nothing runs on. It did, until StrixHaloHost
// was made to ask every detected device.
func TestAReadTargetOutranksTheFamilyName(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			prof := profileWith(goos, "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", 128,
				[]GPU{{Vendor: "amd", Model: "x", VRAMTotalMB: 512, GFXTarget: "gfx1150", Integrated: true, IntegratedKnown: true}},
				func(*Profile, int) integration { return integrationUnknown() })
			if prof.UnifiedMemory {
				t.Errorf("UnifiedMemory = true for a gfx1150; the family name must not override the target")
			}
			if len(prof.UnusedGPUs) != 1 {
				t.Errorf("UnusedGPUs = %+v, want the gfx1150 set aside", prof.UnusedGPUs)
			}
		})
	}
}

// Linux Strix Halo in AMD's recommended configuration: a 512 MB
// carve-out and a GTT the kernel puts compute allocations in, which KFD
// reports as the pool. The carve-out alone was the budget before
// waired-agent#1485 and would have published 512 MB.
func TestLinuxStrixHaloBudgetIsTheKFDPool(t *testing.T) {
	prof := profileWith("linux", "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", 124,
		[]GPU{{Vendor: "amd", Model: "AMD Radeon 8060S Graphics", VRAMTotalMB: 512, KFDMemMB: 63 * 1024,
			GTTTotalMB: 63 * 1024, GFXTarget: "gfx1151", PCIID: "1002:1586", Integrated: true, IntegratedKnown: true}},
		func(*Profile, int) integration { return integrationUnknown() })
	if !prof.UnifiedMemory || prof.UsableVRAMMB != 63*1024 || prof.CarveOutVRAMMB != 512 {
		t.Errorf("UnifiedMemory=%v UsableVRAMMB=%d CarveOutVRAMMB=%d, want the KFD pool as the budget and only the carve-out additive",
			prof.UnifiedMemory, prof.UsableVRAMMB, prof.CarveOutVRAMMB)
	}
	if got := prof.HostFit().Class(); got != hostfit.ClassUnified {
		t.Errorf("Class = %v, want Unified", got)
	}
}
