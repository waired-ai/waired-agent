package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestTotalMemoryMB is the table the capacity gate divides by, and the
// row that matters most is the one the issue that asked for this gate
// got wrong.
//
// waired-ai/waired-agent#464 says "Apple Silicon is the one exception:
// its VRAM figure is synthesized FROM RAM, so it must NOT be added". It
// is not the one exception. internal/hardware's Windows detector flips
// UnifiedMemory off the CPU model alone, so a Strix Halo whose registry
// value is unreadable lands on the same 75 %-of-RAM heuristic and reports
// a figure that is equally a view into system RAM. That is why the
// producer publishes the carve-out QUANTITY rather than a platform: the
// discriminator is where the number came from, and one of the two
// platforms that can synthesize it is not Apple.
func TestTotalMemoryMB(t *testing.T) {
	const gb = 1024
	for _, tc := range []struct {
		name string
		host hostfit.Host
		want int
	}{
		{
			"cpu-only: RAM less the OS allowance",
			hostfit.Host{RAMTotalGB: 16},
			(16 - hostfit.OSMemoryAllowanceGB) * gb,
		},
		{
			// Disjoint by construction: the card's memory is its own
			// silicon, and the OS does not live there.
			"discrete: RAM plus the card",
			hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24564},
			(64-hostfit.OSMemoryAllowanceGB)*gb + 24564,
		},
		{
			// The pool ollama spreads layers over, not whichever card
			// enumerated first (waired-ai/waired-agent#264).
			"discrete: RAM plus the pooled cards",
			hostfit.Host{RAMTotalGB: 64, GPUCount: 2, VRAM0MB: 24564, VRAMPoolMB: 48104},
			(64-hostfit.OSMemoryAllowanceGB)*gb + 48104,
		},
		{
			// iogpu.wired_limit_mb, or 75 % of RAM. Either way a cap on
			// how much of the one pool the GPU may pin, not memory the OS
			// withheld — so the pool alone is the ceiling.
			"apple silicon: the synthesized figure is not added",
			hostfit.Host{RAMTotalGB: 16, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 12288},
			(16 - hostfit.OSMemoryAllowanceGB) * gb,
		},
		{
			// sysfs mem_info_vram_total on Linux, qwMemorySize on
			// Windows: memory the firmware took before the OS counted,
			// so RAMTotalGB is the leftover and the sum is the machine.
			"strix halo with a real carve-out: added",
			hostfit.Host{RAMTotalGB: 31, GPUCount: 1, UnifiedMemory: true,
				UsableVRAMMB: 96 * gb, CarveOutVRAMMB: 96 * gb},
			(31-hostfit.OSMemoryAllowanceGB)*gb + 96*gb,
		},
		{
			// The row #464 misses: Windows, registry unreadable, so
			// UsableVRAMMB is 75 % of a RAMTotalGB that already reports
			// the whole pool. Adding it would inflate this host by 75 %.
			"strix halo on the windows heuristic: not added",
			hostfit.Host{RAMTotalGB: 128, GPUCount: 1, UnifiedMemory: true,
				UsableVRAMMB: 96 * gb},
			(128 - hostfit.OSMemoryAllowanceGB) * gb,
		},
		{
			// Detection failure, not a 0 GB machine. Callers read
			// RAMTotalGB to tell this apart from the row below.
			"unknown RAM reports nothing",
			hostfit.Host{GPUCount: 1, VRAM0MB: 24564},
			0,
		},
		{
			// A real machine with nothing left once the OS is served.
			// Same zero, opposite meaning.
			"a machine smaller than the OS allowance has nothing",
			hostfit.Host{RAMTotalGB: 2},
			0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.host.TotalMemoryMB(); got != tc.want {
				t.Errorf("TotalMemoryMB() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBundledCatalog_TheSymptomHostsKeepLocalInference walks the four
// hosts waired-ai/waired#1056 opens with — the ones a small graphics card
// or an 8 GB Mac left with no local model at all — and asserts each one
// is admitted a model it can hold.
//
// Ratifying source: the owner decision of 2026-08-03 on that issue,
// decision 1 (refusal is reserved for certain OOM) and the symptom table
// in its body. The old gate refused three of these four on a
// hand-authored min_ram_gb or a carve-out comparison; none of them was
// out of memory.
//
// AMENDED 2026-08-08 (#552). The two 8 GB rows keep local inference on
// qwen3.5-2b instead of qwen3.5-4b. What the test promises is unchanged
// — none of these four hosts is left with no local model — but "a model
// it can hold" now means holdable at a window the product serves, and
// 8 GB holds neither the 4b's 200,704-token cache (7403 MiB against a
// 6144 MiB budget) nor anything near it. The 2b holds its FULL native
// 262,144 there with room to spare, so the window goes up, not down.
// See docs/decisions/20260808/1907-price-capacity-at-the-served-window.md.
func TestBundledCatalog_TheSymptomHostsKeepLocalInference(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	// qwen3.5-4b is the model every one of these hosts should be able to
	// hold: 3.4 GB of weights, and the smallest 262144-native variant
	// above the install quality floor.
	v := variantOf(t, manifests, "qwen3.5-4b")
	m := manifestOf(t, manifests, "qwen3.5-4b")

	for _, tc := range []struct {
		name string
		host hostfit.Host
		// declares200k is what the host can promise the mesh. It is a
		// recommendation input, never a reason to withhold the model.
		declares200k bool
		// wantRec is the recommendation verdict's reason, "" when it is
		// recommended. Three clauses can decline it and they are not the
		// same claim: the model's own window, the weights' residency, and
		// the window this host would serve.
		wantRec string
		// keepsItWith is the model this host is asserted to be able to
		// hold. Empty means qwen3.5-4b itself.
		keepsItWith string
	}{
		{
			"8 GB RAM, no card",
			hostfit.Host{RAMTotalGB: 8}, false, hostfit.ReasonWindowExceedsMemory, "qwen3.5-2b",
		},
		{
			// A 2 GB card holds none of the weights, and a host WITH an
			// accelerator is asked to keep them in it — the CPU-only host
			// above is exempt because it has nothing to be resident in.
			// That asymmetry is real, known, and waits on a measured
			// speed for the CPU-only arm (waired-ai/waired-agent#466).
			"8 GB RAM + 2 GB card",
			hostfit.Host{RAMTotalGB: 8, GPUCount: 1, VRAM0MB: 2048}, false, hostfit.ReasonWeightsSpill, "",
		},
		{
			// This host CAN declare the coding window, and only because
			// of OllamaPlannedRung's rule 3: the 2 GB card holds almost
			// nothing, but the same machine with the card removed sizes
			// its window from 12 GB of system RAM and reaches the coding
			// window, so the carded host must too. The residency clause
			// still declines to preselect it.
			"16 GB RAM + 2 GB card",
			hostfit.Host{RAMTotalGB: 16, GPUCount: 1, VRAM0MB: 2048}, true, hostfit.ReasonWeightsSpill, "",
		},
		{
			// 3.4 GB of weights fit the 6 GB wired limit, and ~120k of
			// window is all that fits beside them — which is the problem,
			// not the answer. 200,704 of the 4b's KV needs 7403 MiB here.
			// A 7 GiB machine of exactly this shape was measured taking
			// the model and returning HTTP 500 on the first generation
			// (run 31164150206).
			"8 GB Mac",
			hostfit.Host{RAMTotalGB: 8, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 6144},
			false, hostfit.ReasonWindowExceedsMemory, "qwen3.5-2b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keeper := tc.keepsItWith
			if keeper == "" {
				keeper = "qwen3.5-4b"
			}
			if got := hostfit.OllamaCapacityFit(
				manifestOf(t, manifests, keeper), variantOf(t, manifests, keeper), tc.host,
			); !got.Fits {
				t.Errorf("%s is refused (%s: needs %d MiB, host has %d MiB); this host is "+
					"left with no local model at all, which is the symptom waired#1056 opens with",
					keeper, got.Reason, got.NeedMB, got.HaveMB)
			}
			fit := hostfit.OllamaCapacityFit(m, v, tc.host)
			if fit.Fits != (tc.keepsItWith == "") {
				t.Errorf("qwen3.5-4b fits = %v, want %v (%s: needs %d MiB, host has %d MiB)",
					fit.Fits, tc.keepsItWith == "", fit.Reason, fit.NeedMB, fit.HaveMB)
			}
			if got := hostfit.OllamaDeclaresWindow(m, v, tc.host, hostfit.ServingWindow200k); got != tc.declares200k {
				t.Errorf("declares the coding window = %v, want %v", got, tc.declares200k)
			}
			rec := hostfit.OllamaRecommendModel(m, v, tc.host)
			if rec.Reason != tc.wantRec {
				t.Errorf("recommendation reason = %q, want %q (verdict %+v)",
					rec.Reason, tc.wantRec, rec)
			}
			if rec.Fits != (tc.wantRec == "") {
				t.Errorf("recommendation Fits = %v with reason %q; the two disagree",
					rec.Fits, rec.Reason)
			}
		})
	}
}

// TestOllamaCapacityFit_PricesAWindowTheProductWouldServe pins the rule
// the gate now runs on, and it is the REVERSE of what this test used to
// assert.
//
// It used to say: a 5 GB host asked to hold a 1 GB model at the coding
// window needs a 200k KV cache it will never be given, the sizing lands
// on the engine's own default there, and pricing the refusal at 200k
// would turn "this machine would serve a small window" into "this
// machine is out of memory".
//
// That reasoning rested on a premise the product does not hold: that a
// window between the rungs is a smaller version of the thing. It is not.
// A node declares 200,704 or 1,048,576 or nothing (waired#1031), and a
// coding session is sized for the 200k rung (#624), so a machine served
// at 32,768 has a model it cannot route to and cannot code with. Pricing
// the gate at the shrunken window also made it unable to refuse
// ANYTHING — the sizing picks the largest window that fits, so
// re-checking it is a question already answered yes, and that is how a
// 7 GiB Mac was handed a model that returned HTTP 500 on its first
// generation (waired-ai/waired-agent#552).
//
// Owner decision 2026-08-08, recorded in
// docs/decisions/20260808/1907-price-capacity-at-the-served-window.md.
func TestOllamaCapacityFit_PricesAWindowTheProductWouldServe(t *testing.T) {
	tiny := catalog.Manifest{
		ModelID: "tiny", ContextLength: 262144,
		Variants: []catalog.Variant{{
			RuntimeSupport: []string{catalog.RuntimeOllama},
			// 1.0 GB of weights; a 200k q8 KV cache costs another
			// ~1.2 GiB on top of the engine overhead.
			EstimatedWeightGB: 1.0, KVBytesPerTokenFP16: 12288,
		}},
	}
	v := tiny.Variants[0]
	host := hostfit.Host{RAMTotalGB: 5}

	// The fixture is only interesting while the host cannot prove the
	// rung — that gap is the whole subject.
	plan := hostfit.OllamaPlannedRung(tiny, v, host, hostfit.OllamaKVFactorQ8_0, 0)
	if plan.Fits {
		t.Fatalf("fixture: this host reaches the rung (plan %+v), so it does not exercise "+
			"the gap between the served window and the coding one", plan)
	}
	got := hostfit.OllamaCapacityFit(tiny, v, host)
	if got.Fits {
		t.Errorf("capacity = %+v, want a refusal: this host cannot hold the model at "+
			"%d tokens, and %d is not a window this product serves",
			got, hostfit.ServingWindow200k, plan.ContextLength)
	}
	if got.NeedMB <= got.HaveMB {
		t.Errorf("refusal carries need %d ≤ have %d; the figures must name the shortfall",
			got.NeedMB, got.HaveMB)
	}

	// OllamaFit, the variant-only entry point, has no manifest and so
	// always assumes the coding rung. For a 262144-native model the two
	// now AGREE — that is the change. They still part company on a model
	// whose own window is below the rung, which is the case the doc
	// comment on OllamaFit is about.
	if got := hostfit.OllamaFit(v, host); got.Fits {
		t.Errorf("OllamaFit = %+v, want the same coding-window refusal", got)
	}
	short := catalog.Manifest{
		ModelID: "short", ContextLength: 131072,
		Variants: []catalog.Variant{{
			RuntimeSupport:    []string{catalog.RuntimeOllama},
			EstimatedWeightGB: 1.0, KVBytesPerTokenFP16: 12288,
		}},
	}
	sv := short.Variants[0]
	if got := hostfit.OllamaCapacityFit(short, sv, host); !got.Fits {
		t.Errorf("a 131072-native model = %+v, want a fitting verdict: 131072 is the "+
			"whole of what it offers, so there is no rung to trim it to", got)
	}
	if got := hostfit.OllamaFit(sv, host); got.Fits {
		t.Errorf("OllamaFit on the same variant = %+v, want the coding-window refusal — "+
			"this is the gap the manifest-aware entry point exists to close", got)
	}
}
