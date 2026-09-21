package hardware

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// dgxSparkProfile is a DGX Spark as the detectors actually leave it, and
// every field here is a reading somebody published rather than a number
// chosen to make a test pass:
//
//	nvidia-smi --query-gpu=name,memory.total,compute_cap --format=csv,noheader
//	NVIDIA GB10, [N/A], 12.1
//
// The "[N/A]" is NVIDIA's documented behaviour, not a fault: "On iGPU
// platforms, nvidia-smi will display 'Memory-Usage: Not Supported' …
// because iGPUs do not have dedicated framebuffer memory" (DGX Spark
// known issues). waired-agent#1468 is why the device survives that with
// VRAMTotalMB 0 instead of failing the whole query.
//
// CPU.Model is empty because the machine is aarch64 and defaultCPU reads
// only /proc/cpuinfo's "model name", which ARM does not have. RAM is
// ~121.7 GiB, which is both what MemTotal reports and what the CUDA
// driver offers as the pool — one pool, two readings, which is the
// question integrated.go asks.
func dgxSparkProfile() Profile {
	return Profile{
		OS: "linux", Arch: "arm64",
		CPU:        CPUInfo{Model: "", Cores: 20},
		RAMTotalGB: 122,
		GPUs: []GPU{{
			Vendor: "nvidia", Model: "NVIDIA GB10",
			VRAMTotalMB: 0, ComputeCap: "12.1",
		}},
		Accelerators: Accelerators{CUDA: true},
	}
}

// TestIntegratedFromVendor is the detection table. Rows that must NOT
// classify matter more than the one that must, because an over-eager
// answer credits a discrete card with memory it does not have, which is
// the direction integrated.go's merge doc calls a regression rather than
// the status quo.
func TestIntegratedFromVendor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gpu           GPU
		wantIntegrat  bool
		wantKnown     bool
		whyNotIfUnset string
	}{
		{
			name:         "a GB10 is named by the vendor as one pool",
			gpu:          GPU{Vendor: "nvidia", Model: "NVIDIA GB10"},
			wantIntegrat: true, wantKnown: true,
		},
		{
			name: "casing and spacing do not matter",
			gpu:  GPU{Vendor: "NVIDIA", Model: "  nvidia   gb10 "},
			// normalizeChipName lowercases and collapses whitespace,
			// which is all it does — see its doc for why it strips
			// nothing else.
			wantIntegrat: true, wantKnown: true,
		},

		// THE COLLISION ROWS. In pci.ids "GB10" is a prefix of three
		// other codenames, and two of them are discrete HBM parts:
		//
		//	2901  GB100 [B200]
		//	29bc  GB102 [B100]
		//	2b00  GB10B [Jetson AGX Thor]
		//
		// A prefix or substring table would credit a B200 with one
		// pool. These rows fail if the match ever stops being exact.
		{
			name:          "GB100 is a B200, which is discrete HBM3e",
			gpu:           GPU{Vendor: "nvidia", Model: "NVIDIA GB100"},
			whyNotIfUnset: "substring matching would call a B200 one pool",
		},
		{
			name:          "GB102 is a B100, also discrete",
			gpu:           GPU{Vendor: "nvidia", Model: "NVIDIA GB102"},
			whyNotIfUnset: "substring matching would call a B100 one pool",
		},
		{
			name:          "GB10B is Jetson Thor, which is not in the table",
			gpu:           GPU{Vendor: "nvidia", Model: "NVIDIA GB10B"},
			whyNotIfUnset: "an unlisted part keeps today's behaviour",
		},

		// A MIG parent and some vGPU guests also decline to report a
		// memory total, and the bracketed sentinel this repo parses
		// covers "[Insufficient Permissions]" too. None of them is one
		// pool. The name is what separates them, which is why the
		// absence of a memory figure is NOT consulted here.
		{
			name:          "a MIG parent that reports no memory total",
			gpu:           GPU{Vendor: "nvidia", Model: "NVIDIA A100-SXM4-40GB", VRAMTotalMB: 0},
			whyNotIfUnset: "no memory figure is not evidence of one pool",
		},
		{
			name:          "a plain discrete card",
			gpu:           GPU{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 5090", VRAMTotalMB: 32768},
			whyNotIfUnset: "unlisted",
		},
		{
			name:          "a GH200: coherent, but two pools (CUDA integrated == 0)",
			gpu:           GPU{Vendor: "nvidia", Model: "NVIDIA GH200 480GB", VRAMTotalMB: 97871},
			whyNotIfUnset: "coherence is not topology; see integrated.go",
		},
		{
			name:          "another vendor's part is not this axis's business",
			gpu:           GPU{Vendor: "amd", Model: "NVIDIA GB10"},
			whyNotIfUnset: "the vendor gate runs first",
		},
		{
			name:          "an empty device name",
			gpu:           GPU{Vendor: "nvidia", Model: ""},
			whyNotIfUnset: "nothing to match",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof := Profile{GPUs: []GPU{tc.gpu}}
			got := integratedFromVendor(&prof, 0)
			if got.integrated != tc.wantIntegrat || got.known != tc.wantKnown {
				t.Errorf("integratedFromVendor(%q/%q) = {integrated:%v known:%v}, "+
					"want {integrated:%v known:%v} — %s",
					tc.gpu.Vendor, tc.gpu.Model,
					got.integrated, got.known, tc.wantIntegrat, tc.wantKnown,
					tc.whyNotIfUnset)
			}
		})
	}
}

// An index outside the slice is a caller bug, not a hardware fact.
func TestIntegratedFromVendor_OutOfRange(t *testing.T) {
	prof := dgxSparkProfile()
	for _, i := range []int{-1, 1, 99} {
		if got := integratedFromVendor(&prof, i); got != (integration{}) {
			t.Errorf("index %d: got %+v, want the zero (unknown) answer", i, got)
		}
	}
	if got := integratedFromVendor(nil, 0); got != (integration{}) {
		t.Errorf("nil profile: got %+v, want unknown", got)
	}
}

// sparkProfiler builds a profiler that sees a DGX Spark, with the UMA
// hook pinned to the REAL rule rather than to the host's own.
//
// WHY THE HOOK IS INJECTED, AND WHY IT IS NOT A STUB. What has to be
// asserted is that the budget rule consults the detected fact, so
// replacing that rule with a fake would test nothing. But defaultUMA is
// build-tagged, and the macOS one answers from a sysctl and returns on
// arm64 before this rule is reached — so on a Mac the assertions below
// would be measuring Apple Silicon's 75 %-of-RAM fallback while passing
// or failing for reasons that have nothing to do with the subject. (It
// failed exactly that way in CI first: 122 GB * 3/4 = 93696 MB, under
// the range this test expects.) applyUnifiedBudget is the real,
// untagged body both non-darwin hooks run, so pinning goos here runs
// the production rule identically on every host.
func sparkProfiler(goos string) *Profiler {
	spark := dgxSparkProfile()
	return NewProfiler("",
		WithOSArch(func() (string, string) { return goos, "arm64" }),
		WithCPU(func(context.Context) CPUInfo { return spark.CPU }),
		WithRAM(func(context.Context) (int, int, error) { return spark.RAMTotalGB, 100, nil }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return spark.GPUs, spark.Accelerators, nil
		}),
		WithUMA(func(_ context.Context, p *Profile) { applyUnifiedBudget(goos, p) }),
		// The per-OS reading is silenced so the vendor axis is the ONLY
		// source of the classification. Without this the test would pass
		// on a darwin runner whatever the vendor axis did, because
		// integratedFromOS used to answer "integrated" there off GOARCH
		// alone, for any device — a pass that would survive deleting the
		// subject. It now answers only for an Apple device; the silence
		// stays so the test does not depend on that.
		WithIntegratedDetector(func(*Profile, int) integration { return integrationUnknown() }),
	)
}

// TestProfile_DGXSparkIsClassifiedEndToEnd drives the whole pipeline —
// PCI pass, integration merge, UMA hook, bandwidth — through the real
// budget rule, asserting that the hook now CONSULTS the detected fact.
func TestProfile_DGXSparkIsClassifiedEndToEnd(t *testing.T) {
	spark := dgxSparkProfile()
	got := sparkProfiler("linux").Profile(context.Background())

	if !got.GPUs[0].IntegratedKnown || !got.GPUs[0].Integrated {
		t.Fatalf("GPU integration = {%v,%v}, want a known single-pool reading",
			got.GPUs[0].Integrated, got.GPUs[0].IntegratedKnown)
	}
	if !got.UnifiedMemory {
		t.Error("UnifiedMemory is false: the reported fact did not reach the policy flag")
	}
	if got.CarveOutVRAMMB != 0 {
		t.Errorf("CarveOutVRAMMB = %d, want 0: there is no separate pool to add, "+
			"and hostfit.TotalMemoryMB adds this one", got.CarveOutVRAMMB)
	}
	// 122 GB less whatever the OS keeps. The point of the assertion is
	// that the budget is the POOL and not the 0 MB nvidia-smi reported,
	// so it is checked as a range rather than pinned to the arithmetic
	// of OSMemoryDeductionGB, which belongs to hostfit's own tests.
	if got.UsableVRAMMB < 100*1024 || got.UsableVRAMMB >= spark.RAMTotalGB*1024 {
		t.Errorf("UsableVRAMMB = %d, want most of the %d GB pool",
			got.UsableVRAMMB, spark.RAMTotalGB)
	}
	if got.MemoryBandwidthSpecGBs != 273.0 {
		t.Errorf("MemoryBandwidthSpecGBs = %v, want 273 (NVIDIA's published peak)",
			got.MemoryBandwidthSpecGBs)
	}
	if key := HostKey(&got); key != "unified-nvidia-gb10" {
		t.Errorf("HostKey() = %q, want unified-nvidia-gb10", key)
	}
}

// TestDGXSparkThroughHostfit is the reason the change exists: four
// separate consequences in proto/hostfit, all of which read wrong today.
// Every expectation is about a DIRECTION, so it keeps its meaning if the
// constants behind hostfit move.
func TestDGXSparkThroughHostfit(t *testing.T) {
	spark := dgxSparkProfile()
	h := sparkProfiler("linux").Profile(context.Background()).HostFit()

	// 1. The class itself. ClassDiscrete's doc says what does not fit
	//    "spills back over PCIe and executes on the CPU"; there is no
	//    PCIe here and nowhere to spill to.
	if h.Class() != hostfit.ClassUnified {
		t.Fatalf("Class() = %v, want ClassUnified", h.Class())
	}

	// 2. No double counting. TotalMemoryMB's ClassDiscrete arm adds the
	//    VRAM budget to RAM; on one pool those are the same bytes, and
	//    the shipped copy already promises they are not added
	//    (docs-site/TRANSLATION.md, owner ruling 20260822).
	ramMB := (spark.RAMTotalGB - h.OSMemoryDeductionGB()) * 1024
	if total := h.TotalMemoryMB(); total != ramMB {
		t.Errorf("TotalMemoryMB() = %d, want %d (RAM less the OS reserve, with "+
			"nothing added): one pool must not be counted twice", total, ramMB)
	}

	// 3. There is a VRAM budget at all. This is what made every shipped
	//    variant recommendable: OllamaRecommend's discrete arm is
	//    `need > 0 && have > 0 && need > have`, and with have == 0 it
	//    imposed no constraint whatsoever.
	if h.EffectiveVRAMMB() <= 0 {
		t.Error("EffectiveVRAMMB() is 0, so no residency rule can bind")
	}

	// 4. The decode estimate can now speak. A published peak is an upper
	//    bound on THIS machine, which is what licenses exclusion (#251).
	v := catalog.Variant{EstimatedWeightGB: 40, KVBytesPerTokenFP16: 100000}
	est := hostfit.EstimateOllamaDecode(v, h)
	if !est.UpperBound {
		t.Error("Estimate.UpperBound is false: with a published peak the " +
			"unified arm is allowed to exclude, and without one nothing " +
			"ever fires — neither an exclusion nor a 'may be slow' note")
	}
	if est.TokpsEstimate <= 0 {
		t.Errorf("TokpsEstimate = %v, want a positive rate", est.TokpsEstimate)
	}
}

// TestNvidiaUnifiedBudgetHasNoStrixHaloCeiling pins the one number that
// separates the NVIDIA rule from the AMD one it was factored out of.
//
// This is a record of today's behaviour, not a product contract: nobody
// has run this repository on a GB10. It bites if someone reuses
// strixHaloUMA's Windows branch wholesale, which is the obvious
// shortcut and would silently cap a 128 GB machine at 96 GiB.
func TestNvidiaUnifiedBudgetHasNoStrixHaloCeiling(t *testing.T) {
	p := &Profile{
		RAMTotalGB: 122,
		GPUs: []GPU{{
			Vendor: "nvidia", Model: "NVIDIA GB10",
			Integrated: true, IntegratedKnown: true,
		}},
	}
	for _, goos := range []string{"linux", "windows", "darwin"} {
		usable, carveOut, ok := unifiedBudgetFor(goos, p)
		if !ok {
			t.Fatalf("%s: no budget rule applied to a detected single-pool NVIDIA part", goos)
		}
		if usable <= strixHaloUMACapMB {
			t.Errorf("%s: usable = %d MB, which is at or under the Strix Halo BIOS "+
				"ceiling (%d MB). CUDA addresses the whole pool on these parts; "+
				"that ceiling is an AMD platform fact and does not apply",
				goos, usable, strixHaloUMACapMB)
		}
		if carveOut != 0 {
			t.Errorf("%s: carveOut = %d, want 0 (hostfit.TotalMemoryMB ADDS this)",
				goos, carveOut)
		}
	}
}

// TestUnifiedBudgetFor_StrixHaloUnchanged is the regression half. The
// reference host's numbers are the ones catalog_admission_test.go holds
// the bundled catalog against, so a drift here moves what ships.
func TestUnifiedBudgetFor_StrixHaloUnchanged(t *testing.T) {
	strixHalo := func() *Profile {
		return &Profile{
			CPU:        CPUInfo{Model: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"},
			RAMTotalGB: 127,
			GPUs:       []GPU{{Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics", VRAMTotalMB: 512}},
		}
	}
	t.Run("windows is the pool less the OS reserve, capped at the BIOS ceiling", func(t *testing.T) {
		usable, carveOut, ok := unifiedBudgetFor("windows", strixHalo())
		if !ok || usable != strixHaloUMACapMB || carveOut != 0 {
			t.Errorf("got (%d, %d, %v), want (%d, 0, true)",
				usable, carveOut, ok, strixHaloUMACapMB)
		}
	})
	t.Run("linux takes the carve-out reading, additively", func(t *testing.T) {
		usable, carveOut, ok := unifiedBudgetFor("linux", strixHalo())
		if !ok || usable != 512 || carveOut != 512 {
			t.Errorf("got (%d, %d, %v), want (512, 512, true)", usable, carveOut, ok)
		}
	})
	t.Run("linux declines when the igpu was never enumerated", func(t *testing.T) {
		// No rocm-smi: the iGPU is invisible and there is no reading to
		// build a budget from. Windows deliberately differs — it flips
		// the flag off the CPU string alone — and that asymmetry is what
		// unifiedBudgetFor takes goos for.
		p := strixHalo()
		p.GPUs = nil
		if usable, carveOut, ok := unifiedBudgetFor("linux", p); ok {
			t.Errorf("got (%d, %d, true), want ok=false", usable, carveOut)
		}
		if _, _, ok := unifiedBudgetFor("windows", p); !ok {
			t.Error("windows should still classify from the CPU string alone")
		}
	})
	t.Run("a host no rule covers is left alone", func(t *testing.T) {
		p := &Profile{
			CPU:        CPUInfo{Model: "AMD Ryzen 9 7950X 16-Core Processor"},
			RAMTotalGB: 64,
			GPUs:       []GPU{{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564}},
		}
		for _, goos := range []string{"linux", "windows", "darwin"} {
			if _, _, ok := unifiedBudgetFor(goos, p); ok {
				t.Errorf("%s: a discrete host was given a unified budget", goos)
			}
		}
	})
}

// A part this build has never heard of must not be given a bandwidth
// figure, and must not be refused a classification it earned some other
// way. That combination is what lets the table ship incomplete: the
// Windows arithmetic classifies an RTX Spark N1X with no table entry at
// all, and the estimate then falls back to hostfit's population constant
// and stays annotate-only (#251, #273).
func TestUnifiedNVIDIAWithNoPublishedBandwidth(t *testing.T) {
	prof := &Profile{
		UnifiedMemory: true,
		RAMTotalGB:    54,
		GPUs: []GPU{{
			Vendor: "nvidia", Model: "NVIDIA N1X",
			VRAMTotalMB: 8128, Integrated: true, IntegratedKnown: true,
		}},
	}
	if bw := unifiedBandwidthFor(prof); bw != 0 {
		t.Errorf("MemoryBandwidthSpecGBs = %v, want 0: no figure has been "+
			"published for this part and guessing one would put a wrong "+
			"upper bound where the rule may refuse a model", bw)
	}
	usable, _, ok := unifiedBudgetFor("windows", prof)
	if !ok {
		t.Fatal("a detected single-pool NVIDIA part got no budget rule")
	}
	if usable <= prof.GPUs[0].VRAMTotalMB {
		t.Errorf("usable = %d MB, which is no better than the %d MB carve-out "+
			"nvidia-smi reports. The carve-out is a slice of the pool, not the "+
			"budget — measured 5.7x understated on this class of part",
			usable, prof.GPUs[0].VRAMTotalMB)
	}
}
