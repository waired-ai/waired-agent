package hardware

import (
	"strings"
)

// Unified-memory bandwidth, keyed by the chip the detector already
// reports (waired-ai/waired-agent#251).
//
// WHY A TABLE AND NOT A PROBE: Apple Silicon exposes no memory-bandwidth
// figure through any API — not sysctl, not IOKit, not system_profiler —
// and on a unified host a benchmark cannot answer the question either. A
// CPU-side measurement is bounded by what the CPU cores can pull, which
// is well under what the GPU pulls from the same pool, so it is a LOWER
// bound; the fit rule needs an UPPER one before it may exclude anything
// (proto/hostfit.Estimate.UpperBound). The part's published peak is that
// upper bound, and it is a constant per part. Hence a table.
//
// WHY THESE FIGURES ARE SAFE TO BE WRONG UPWARD: the number is used only
// as a ceiling on decode speed. Crediting a host with more bandwidth than
// it has makes the rule exclude LESS, never more, so an over-estimate
// costs a "may be slow" annotation that should have been an exclusion.
// Under-estimating is the harmful direction: it withholds models the
// machine runs. That asymmetry decides the binning rule below.

// strixHaloBandwidthGBs is the Ryzen AI Max (Strix Halo) platform's
// unified pool: 256-bit LPDDR5X-8000. Unlike the Apple parts this is one
// figure for the platform rather than per SKU, which is why it does not
// live in the map below — IsStrixHaloAPU already recognises the family
// from the CPU string, and every member shares the bus.
const strixHaloBandwidthGBs = 256.0

// appleUnifiedBandwidthGBs maps Apple's own chip name to that part's
// published peak memory bandwidth in GB/s.
//
// BINNED PARTS TAKE THE HIGHER FIGURE. Several chips ship in two memory
// configurations that report an IDENTICAL chip name — the M3 Max is
// 300 GB/s with a 14-core CPU and 400 with a 16-core, the M4 Max is 410
// and 546 the same way — and nothing in the detected string separates
// them. Taking the higher one keeps the value an upper bound for both
// bins, which is the property the exclusion rests on. Taking the lower
// would make the figure a bound for neither and would withhold models
// from the larger part.
//
// Keys are matched EXACTLY after normalisation, not by prefix. "Apple M4
// Max" containing "Apple M4" is the obvious way to get this wrong, and a
// prefix table would need its entries ordered longest-first forever to
// stay correct; exact match has no such ordering hazard, and a string
// that does not match simply falls through to "unknown", which is the
// safe answer anyway.
var appleUnifiedBandwidthGBs = map[string]float64{
	"apple m1":       68.25,
	"apple m1 pro":   200,
	"apple m1 max":   400,
	"apple m1 ultra": 800,

	"apple m2":       100,
	"apple m2 pro":   200,
	"apple m2 max":   400,
	"apple m2 ultra": 800,

	"apple m3":     100,
	"apple m3 pro": 150,
	// 300 (14-core CPU) | 400 (16-core) — see the binning note above.
	"apple m3 max":   400,
	"apple m3 ultra": 819,

	"apple m4":     120,
	"apple m4 pro": 273,
	// 410 (14-core CPU) | 546 (16-core).
	"apple m4 max": 546,

	"apple m5": 153,

	// Deliberately absent: M4 Ultra and the M5 Pro / Max / Ultra parts.
	// Every entry above is a figure the vendor published for a shipping
	// part; guessing one by extrapolating the previous generation's
	// scaling would put a WRONG upper bound in the one place the rule is
	// allowed to refuse a model. An absent part costs nothing but the
	// annotation it already had before #251 — see the fallback in
	// hostfit.EstimateOllamaDecode — so the table is safe to grow lazily
	// and must never be grown speculatively.
}

// UnifiedMemoryBandwidthGBs returns the published peak memory bandwidth
// of a unified-memory part, in GB/s, or 0 when the part is not
// recognised.
//
// 0 is a normal answer, not a failure: proto/hostfit falls back to its
// population constant and stays annotate-only there, exactly as it
// behaved before this table existed. That is what lets the table ship
// incomplete and grow one verified part at a time.
//
// cpuModel is the DETECTOR's own CPU string — Profile.CPU.Model, which
// comes from `sysctl machdep.cpu.brand_string` on macOS ("Apple M4") and
// from /proc/cpuinfo or the CentralProcessor registry key elsewhere.
// Deliberately NOT the GPU's Model field: that one is documented
// free-form and "do not parse" for exactly this class of decision. The
// rule binds CONSUMERS of the published summary, and this is the
// producer side turning a string into the number it then publishes,
// which is what keeps consumers honest.
func UnifiedMemoryBandwidthGBs(cpuModel string) float64 {
	// Strix Halo first: it is a family match on a long marketing string
	// ("AMD Ryzen AI Max+ PRO 395 w/ Radeon 8060S"), so it can never
	// collide with the exact Apple keys.
	if IsStrixHaloAPU(cpuModel) {
		return strixHaloBandwidthGBs
	}
	return appleUnifiedBandwidthGBs[normalizeChipName(cpuModel)]
}

// unifiedBandwidthFor is the profiler's rule for populating
// Profile.MemoryBandwidthSpecGBs: the part's peak, but only on a host the
// UMA hook actually classified as unified.
//
// The gate matters. A Strix Halo whose iGPU was never enumerated (Linux
// without rocm-smi) leaves UnifiedMemory false, and hostfit then judges
// it CPU-only or discrete — classes that read a different bandwidth term
// entirely. Publishing a unified pool figure for such a host would
// describe memory the fit rule is not reasoning about.
//
// Split out from the profiler so the (unified, chip) -> figure decision
// is testable without building a Profile, per the "put the seam below the
// behaviour" discipline.
func unifiedBandwidthFor(unifiedMemory bool, cpuModel string) float64 {
	if !unifiedMemory {
		return 0
	}
	return UnifiedMemoryBandwidthGBs(cpuModel)
}

// normalizeChipName lowercases and collapses runs of whitespace so the
// table is insensitive to the spacing and casing differences between the
// several places a chip name can come from (sysctl, system_profiler's
// chip_type, /proc/cpuinfo). It does not strip anything else: a string
// carrying extra tokens is a string this table does not know, and
// guessing past the difference is how a table starts answering for parts
// it was never given.
func normalizeChipName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
