package hardware

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The table carries both sides; an ID in neither is unknown, not guessed.
func TestIntelPartFor(t *testing.T) {
	for id, want := range map[string]struct {
		platform string
		discrete bool
		ok       bool
	}{
		"8086:e20b": {"bmg", true, true},   // Arc B580
		"8086:56a0": {"dg2", true, true},   // Arc A770
		"8086:4905": {"dg1", true, true},   // Iris Xe MAX
		"8086:64a0": {"lnl", false, true},  // Arc 140V (Lunar Lake)
		"8086:7d51": {"arl", false, true},  // Arc 140T (Arrow Lake H)
		"8086:7d55": {"mtl", false, true},  // Meteor Lake
		"8086:a780": {"rpls", false, true}, // UHD 770 (Raptor Lake-S)
		"8086:b080": {"ptl", false, true},  // Panther Lake
		"8086:ffff": {"", false, false},
		"10de:e20b": {"", false, false}, // not Intel
	} {
		p, ok := intelPartFor(id)
		if ok != want.ok || p.platform != want.platform || p.discrete != want.discrete {
			t.Errorf("intelPartFor(%q) = %+v,%v, want %+v", id, p, ok, want)
		}
	}
}

func TestChipSlug_Intel(t *testing.T) {
	known := func(g GPU, yes bool) GPU { g.Integrated, g.IntegratedKnown = yes, true; return g }
	for _, tc := range []struct {
		name string
		gpu  GPU
		want string
	}{
		{"Arc B580", known(GPU{Vendor: "intel", PCIID: "8086:e20b"}, false), "bmg"},
		{"Arc A770", known(GPU{Vendor: "intel", PCIID: "8086:56a0"}, false), "dg2"},
		{"a card newer than the table", GPU{Vendor: "intel", PCIID: "8086:ffff"}, "unknown"},
		{"an iGPU, were it ever keyed, names the CPU", known(GPU{Vendor: "intel", PCIID: "8086:64a0"}, true), "core-ultra-7-258v"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChipSlug(tc.gpu, "Core Ultra 7 258V"); got != tc.want {
				t.Errorf("ChipSlug = %q, want %q", got, tc.want)
			}
		})
	}
}

// End to end through the profiler, the four Intel shapes.
func TestProfile_Intel(t *testing.T) {
	silent := func(*Profile, int) integration { return integrationUnknown() }
	withPart := func(gs ...GPU) []GPU {
		for i := range gs {
			applyIntelPart(&gs[i])
		}
		return gs
	}

	t.Run("an Arc iGPU laptop is a CPU host", func(t *testing.T) {
		prof := profileWith("windows", "Intel(R) Core(TM) Ultra 7 258V", 32,
			withPart(GPU{Vendor: "intel", Model: "Intel(R) Arc(TM) 140V GPU (16GB)", PCIID: "8086:64a0", VRAMTotalMB: 128}), silent)
		if len(prof.GPUs) != 0 || len(prof.UnusedGPUs) != 1 || HostKey(&prof) != "cpu-none" {
			t.Errorf("GPUs=%+v Unused=%+v key=%s, want the iGPU set aside and cpu-none", prof.GPUs, prof.UnusedGPUs, HostKey(&prof))
		}
	})

	t.Run("a B580 with its memory read is a discrete host", func(t *testing.T) {
		prof := profileWith("linux", "AMD Ryzen 7 7700X 8-Core Processor", 64,
			withPart(GPU{Vendor: "intel", Model: "Intel GPU 8086:e20b", PCIID: "8086:e20b", VRAMTotalMB: 12288}), silent)
		h := prof.HostFit()
		if len(prof.GPUs) != 1 || h.Class() != hostfit.ClassDiscrete || h.VRAM0MB != 12288 {
			t.Fatalf("HostFit = %+v, want one discrete 12 GB card", h)
		}
		if got := HostKey(&prof); got != "discrete-intel-bmg" {
			t.Errorf("HostKey = %q, want discrete-intel-bmg", got)
		}
	})

	t.Run("a B580 whose memory was not read is left out", func(t *testing.T) {
		prof := profileWith("linux", "AMD Ryzen 7 7700X 8-Core Processor", 64,
			withPart(GPU{Vendor: "intel", Model: "Intel GPU 8086:e20b", PCIID: "8086:e20b"}), silent)
		if len(prof.GPUs) != 0 || len(prof.UnusedGPUs) != 1 || !strings.Contains(prof.UnusedGPUs[0].Reason, "sudo waired init") {
			t.Errorf("GPUs=%+v Unused=%+v, want the card left out with a reason that says how to read it", prof.GPUs, prof.UnusedGPUs)
		}
	})

	t.Run("the elevated run's reading brings it back", func(t *testing.T) {
		prof := NewProfiler("",
			WithOSArch(func() (string, string) { return "linux", "amd64" }),
			WithRAM(func(context.Context) (int, int, error) { return 64, 32, nil }),
			WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
				return withPart(GPU{Vendor: "intel", PCIID: "8086:e20b"}), Accelerators{}, nil
			}),
			WithUMA(func(context.Context, *Profile) {}),
			WithIntegratedDetector(silent),
			WithPersistedVRAM(func(id string) (int, bool) {
				if id == "8086:e20b" {
					return 12288, true
				}
				return 0, false
			}),
		).Profile(context.Background())
		if len(prof.GPUs) != 1 || prof.GPUs[0].VRAMTotalMB != 12288 {
			t.Errorf("GPUs = %+v, want the card in use with the persisted 12288 MB", prof.GPUs)
		}
	})
}

// The shape of a Windows laptop with an NVIDIA card beside an Intel iGPU
// (the fleet's Windows laptop): the iGPU is set aside, the NVIDIA card
// alone describes the host, and the NVIDIA card stays first.
func TestProfile_NVIDIABesideAnIntelIGPU(t *testing.T) {
	gpus := []GPU{
		{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4060 Laptop GPU", VRAMTotalMB: 8188, ComputeCap: "8.9"},
		{Vendor: "intel", Model: "Intel(R) Iris(R) Xe Graphics", PCIID: "8086:a7a0", VRAMTotalMB: 128},
	}
	applyIntelPart(&gpus[1])
	prof := profileWith("windows", "13th Gen Intel(R) Core(TM) i7-13700H", 32, gpus,
		func(*Profile, int) integration { return integrationUnknown() })
	if len(prof.GPUs) != 1 || prof.GPUs[0].Vendor != "nvidia" {
		t.Fatalf("GPUs = %+v, want the NVIDIA card only", prof.GPUs)
	}
	if got := HostKey(&prof); got != "discrete-nvidia-sm89" {
		t.Errorf("HostKey = %q, want discrete-nvidia-sm89", got)
	}
}
