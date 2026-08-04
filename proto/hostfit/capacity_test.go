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
	}{
		{
			"8 GB RAM, no card",
			hostfit.Host{RAMTotalGB: 8}, false, hostfit.ReasonWindowExceedsMemory,
		},
		{
			// A 2 GB card holds none of the weights, and a host WITH an
			// accelerator is asked to keep them in it — the CPU-only host
			// above is exempt because it has nothing to be resident in.
			// That asymmetry is real, known, and waits on a measured
			// speed for the CPU-only arm (waired-ai/waired-agent#466).
			"8 GB RAM + 2 GB card",
			hostfit.Host{RAMTotalGB: 8, GPUCount: 1, VRAM0MB: 2048}, false, hostfit.ReasonWeightsSpill,
		},
		{
			// This host CAN declare the coding window, and only because
			// of OllamaPlannedWindow's rule 3: the 2 GB card holds almost
			// nothing, but the same machine with the card removed sizes
			// its window from 12 GB of system RAM and reaches the coding
			// window, so the carded host must too. The residency clause
			// still declines to preselect it.
			"16 GB RAM + 2 GB card",
			hostfit.Host{RAMTotalGB: 16, GPUCount: 1, VRAM0MB: 2048}, true, hostfit.ReasonWeightsSpill,
		},
		{
			// 3.4 GB of weights fit the 6 GB wired limit; ~120k of window
			// is all that fits beside them, and a unified host has
			// nowhere to spill the rest TO.
			"8 GB Mac",
			hostfit.Host{RAMTotalGB: 8, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 6144},
			false, hostfit.ReasonWindowExceedsMemory,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fit := hostfit.OllamaCapacityFit(m, v, tc.host)
			if !fit.Fits {
				t.Errorf("qwen3.5-4b is refused (%s: needs %d MiB, host has %d MiB). "+
					"Refusal is reserved for certain OOM, and 3.4 GB of weights is not "+
					"out of memory here", fit.Reason, fit.NeedMB, fit.HaveMB)
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

// TestOllamaCapacityFit_PricesTheWindowTheHostWouldServe pins the
// distinction that keeps the gate honest on small machines.
//
// A 2 GB host asked to hold a 1 GB model at the coding window needs a
// 200k KV cache it will never be given: the sizing lands on the engine's
// own default there, and pricing the refusal at 200k would turn "this
// machine would serve a small window" into "this machine is out of
// memory". The window-inclusive figure a SURFACE shows is the coding one
// either way — see Presentation.RequiredWindowResidentMB.
func TestOllamaCapacityFit_PricesTheWindowTheHostWouldServe(t *testing.T) {
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

	plan := hostfit.OllamaPlannedWindow(tiny, v, host, hostfit.OllamaKVFactorQ8_0, true)
	if plan.ContextLength >= hostfit.ServingWindow200k {
		t.Fatalf("fixture: this host plans a %d-token window, so it does not exercise "+
			"the gap between the served window and the coding one", plan.ContextLength)
	}
	if got := hostfit.OllamaCapacityFit(tiny, v, host); !got.Fits {
		t.Errorf("capacity = %+v, want a fitting verdict: this host serves the model at "+
			"%d tokens, and refusing it for a %d-token cache it is never given would be "+
			"a refusal on a window question", got, plan.ContextLength, hostfit.ServingWindow200k)
	}
	// The legacy variant-only entry point has no manifest to size from
	// and therefore assumes the coding window. That is exactly why every
	// in-tree caller moved off it.
	if got := hostfit.OllamaFit(v, host); got.Fits {
		t.Errorf("OllamaFit = %+v, want the coding-window refusal — if this starts "+
			"agreeing with OllamaCapacityFit the doc comment on it is stale", got)
	}
}
