package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The vision tower's load-time reservation, and the band of hosts it
// changes the answer for (waired-ai/waired-agent#552).
//
// These tests exist because the defect was not visible from any single
// host. The capacity gate's margin on a unified machine is a function of
// RAM with a discontinuity in it, and reading one row of that function
// tells you nothing: 6 GB is refused, 7 and 8 GB pass with 5 MiB, 9 GB
// has a gigabyte. So every assertion here is a SWEEP.

// appleSiliconHost is the Host a Mac with R GB of RAM reports, matching
// internal/hardware/profiler_darwin.go's defaultUMA: unified memory, and
// a wired limit of 75 % of RAM when iogpu.wired_limit_mb is unset — which
// it is by default. The integer division is deliberate and load-bearing;
// see TestVisionTower_TheBandWhereTheTwoBudgetsCoincide.
func appleSiliconHost(ramGB int) hostfit.Host {
	return hostfit.Host{
		RAMTotalGB:    ramGB,
		GPUCount:      1,
		UnifiedMemory: true,
		UsableVRAMMB:  ramGB * 3 / 4 * 1024,
	}
}

// ramSweep is every RAM size the assertions below are made over. It
// spans the real Apple Silicon configurations (8/16/24/36/48/64/96/128)
// plus the sizes between them, because the defect lives in a two-entry
// band and a table that only sampled shipping configurations would have
// found one of its two rows.
var ramSweep = []int{4, 5, 6, 7, 8, 9, 10, 12, 16, 18, 24, 32, 36, 48, 64, 96, 128}

// withoutVisionTerm is v with the new annotation cleared — the same
// variant as this build shipped before #552. Comparing against it is how
// "unchanged elsewhere" is asserted rather than asserted-by-eyeball.
func withoutVisionTerm(v catalog.Variant) catalog.Variant {
	v.VisionWorkingSetGB = 0
	return v
}

// TestVisionTower_TheBandWhereTheTwoBudgetsCoincide is the defect itself.
//
// On a unified host the window sizing spends the whole accelerator
// budget — OllamaVRAMBudgetMB less OllamaVRAMOverheadUMAMB — so the
// window it picks satisfies weights + KV == that. The capacity gate then
// prices weights + overhead + that same KV, which comes back to
// OllamaVRAMBudgetMB exactly, and compares it against TotalMemoryMB().
//
// So the gate's whole verdict on a unified host reduces to
//
//	floor(3R/4)·1024  ≤  (R−2)·1024
//
// and those two are the SAME NUMBER for 5 ≤ R ≤ 8. In that band the gate
// cannot refuse what the sizing just sized, whatever the model, and
// everything the arithmetic does not model comes out of a margin of a
// few MiB.
//
// A 7 GiB host of exactly this shape was measured failing: ollama fell
// back to partial offload and the first generation returned HTTP 500
// (run 31164150206). The term charged here is that run's own
// reserve_compute_meta figure for the CLIP graph.
func TestVisionTower_TheBandWhereTheTwoBudgetsCoincide(t *testing.T) {
	for _, ramGB := range ramSweep {
		h := appleSiliconHost(ramGB)
		sizingCeiling := h.OllamaVRAMBudgetMB()
		capacity := h.TotalMemoryMB()
		coincide := sizingCeiling == capacity
		if want := ramGB >= 5 && ramGB <= 8; coincide != want {
			t.Errorf("%d GB: what the sizing spends up to = %d MiB, capacity budget = %d MiB, "+
				"coincide = %v, want %v", ramGB, sizingCeiling, capacity, coincide, want)
		}
		if capacity > sizingCeiling && ramGB < 9 {
			t.Errorf("%d GB: capacity budget %d MiB exceeds the sizing ceiling %d MiB below 9 GB",
				ramGB, capacity, sizingCeiling)
		}
	}
}

// TestVisionTower_ASmallMacIsRefusedTheModelItCannotServe pins the
// outcome the term exists for, and the outcome that makes it acceptable:
// the host is refused the multimodal 4b and keeps local inference on the
// 2b, at a WIDER window than the 4b was being sized for.
func TestVisionTower_ASmallMacIsRefusedTheModelItCannotServe(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	m4 := manifestOf(t, manifests, "qwen3.5-4b")
	v4 := variantOf(t, manifests, "qwen3.5-4b")
	m2 := manifestOf(t, manifests, "qwen3.5-2b")
	v2 := variantOf(t, manifests, "qwen3.5-2b")

	if v4.VisionWorkingSetGB <= 0 {
		t.Fatal("qwen3.5-4b carries no vision_working_set_gb; the annotation is what this test is about")
	}
	if v2.VisionWorkingSetGB != 0 {
		t.Error("qwen3.5-2b is text-only and must carry no vision term")
	}

	for _, ramGB := range ramSweep {
		h := appleSiliconHost(ramGB)
		got4 := hostfit.OllamaCapacityFit(m4, v4, h).Fits
		// 4b needs its 3.4 GB of weights, its window's KV and now its
		// vision tower. 9 GB is where that first fits.
		if want := ramGB >= 9; got4 != want {
			t.Errorf("%d GB Mac: qwen3.5-4b fits = %v, want %v", ramGB, got4, want)
		}
		// 2b is what a refused host keeps local inference with, and it
		// must be there for every host that lost the 4b.
		//
		// The boundary is 5 GB rather than something rounder because
		// that host clears the gate by 1 MiB — the same coincidence
		// TestVisionTower_TheBandWhereTheTwoBudgetsCoincide is about,
		// which this change narrows but does not remove. Charging a
		// text-only variant something it does not reserve would be a
		// refusal without evidence, and refusal is reserved for certain
		// OOM (waired-ai/waired#1056). No Apple machine ships at 5 GB;
		// the row is here to keep the band visible, not because it is
		// load-bearing.
		got2 := hostfit.OllamaCapacityFit(m2, v2, h).Fits
		if want := ramGB >= 5; got2 != want {
			t.Errorf("%d GB Mac: qwen3.5-2b fits = %v, want %v", ramGB, got2, want)
		}
	}

	// The window is not the price of the smaller model — it is the
	// reward. 8 GB is the shipping configuration this decides.
	h8 := appleSiliconHost(8)
	win4 := hostfit.OllamaPlannedWindow(m4, v4, h8, hostfit.OllamaKVFactorQ8_0, true).ContextLength
	win2 := hostfit.OllamaPlannedWindow(m2, v2, h8, hostfit.OllamaKVFactorQ8_0, true).ContextLength
	if win2 <= win4 {
		t.Errorf("8 GB Mac: qwen3.5-2b window %d is not wider than qwen3.5-4b's %d; "+
			"the whole argument for refusing the 4b here is that it costs no context",
			win2, win4)
	}
}

// TestVisionTower_ChangesNothingItWasNotAimedAt sweeps every bundled
// ollama variant over every host size and asserts the verdict moved for
// exactly one thing: a multimodal variant in the band above. A text-only
// variant carries no term and cannot move at all; a multimodal one may
// only move from fits to refused, never the other way.
func TestVisionTower_ChangesNothingItWasNotAimedAt(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			for _, ramGB := range ramSweep {
				for _, h := range []hostfit.Host{
					appleSiliconHost(ramGB),
					{RAMTotalGB: ramGB},                              // CPU-only
					{RAMTotalGB: ramGB, GPUCount: 1, VRAM0MB: 8192},  // discrete
					{RAMTotalGB: ramGB, GPUCount: 1, VRAM0MB: 24564}, // big card
				} {
					now := hostfit.OllamaCapacityFit(m, v, h).Fits
					before := hostfit.OllamaCapacityFit(m, withoutVisionTerm(v), h).Fits
					switch {
					case v.VisionWorkingSetGB == 0 && now != before:
						t.Errorf("%s/%s on %d GB %v: text-only variant moved %v -> %v",
							m.ModelID, v.VariantID, ramGB, h.Class(), before, now)
					case now && !before:
						t.Errorf("%s/%s on %d GB %v: charging a reservation made it FIT (%v -> %v)",
							m.ModelID, v.VariantID, ramGB, h.Class(), before, now)
					}
				}
			}
		}
	}
}

// TestVisionTower_CapacityStaysMonotoneInRAM is the property a gate
// ladder is good at hiding: more memory must never turn a fitting host
// into a refused one. It is asserted by sweeping rather than by reading
// the rules, because the rules are three clauses deep and the
// coincidence this file is about was invisible in all of them.
func TestVisionTower_CapacityStaysMonotoneInRAM(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			fittedAt := 0
			for _, ramGB := range ramSweep {
				h := appleSiliconHost(ramGB)
				fits := hostfit.OllamaCapacityFit(m, v, h).Fits
				switch {
				case fits && fittedAt == 0:
					fittedAt = ramGB
				case !fits && fittedAt > 0:
					t.Errorf("%s/%s: fits at %d GB but not at %d GB — capacity is not monotone in RAM",
						m.ModelID, v.VariantID, fittedAt, ramGB)
				}
			}
		}
	}
}
