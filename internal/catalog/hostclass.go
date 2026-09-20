package catalog

import (
	"slices"
	"strings"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// The vocabulary a measurement may name as the hardware it ran on.
//
// It used to be a hand-written list spelled vendor-plus-memory-size, and
// the list was on its way to being a roster of machines: admitting a DGX
// Spark meant adding "nvidia-unified-128gb" beside "amd-unified-128gb",
// two hosts that share a capacity and differ about 2x on prefill
// (waired-agent#1455). Capacity was also folding parts together that
// should be apart — one "apple-unified-64gb" covers chips whose
// bandwidth differs more than fourfold.
//
// A key is now DERIVED from the measuring host's own profile
// (hardware.HostKey) and checked against a grammar, so a new machine
// produces a new key without anyone editing anything here. That is not a
// reversal of decision 20260829/1100 §2 but an application of its §1:
// "what can be observed is derived, never typed". §2 bound `--host` to a
// list precisely because it could not be observed, and declined to tell
// a class from an identifier BY PATTERN — "sv-mag" and
// "apple-unified-64gb" are both lowercase words joined by hyphens, so a
// pattern would have to guess. A derived key is not in that bind: it is
// not typed at all, it carries a topology as its first component, and
// the profile it comes from cannot produce a hostname.

// LegacyHostClasses is the vocabulary as it was spelled before keys were
// derived. It is FROZEN and must not grow.
//
// It exists so the records already in the stores stay readable. Decision
// 20260829/1100 settled that shipped records are not rewritten — the 18
// verdicts with no run_url were left as they are, because that is what
// actually happened — and the same applies here: re-spelling a record
// would claim a provenance nobody re-measured.
//
// A new measurement never gets one of these. That is what makes the list
// frozen rather than merely short.
var LegacyHostClasses = []string{
	// The GPU lane's machine — "a g2-standard-4 with one L4", as
	// installtest-inference.yml says where it names this very class.
	// An L4 reports compute capability 8.9, so today it would derive
	// discrete-nvidia-sm89.
	//
	// This entry is also the clearest evidence for why the vocabulary
	// was re-keyed, and it is our own hardware: this repo's Linux fleet
	// host carries an RTX PRO 4000 Blackwell, ALSO 24 GB and also an
	// NVIDIA discrete card, which this spelling would call by the same
	// name. They are not interchangeable — 300 GB/s of GDDR6 against
	// 672 GB/s of GDDR7, and 8.9 against 12.0 — and seconds measured on
	// one say nothing about the other (waired-agent#1455).
	"nvidia-24gb-discrete",
	// Named as legal by VariantAgentGrade.Host's doc comment since the
	// field was introduced; no measurement ever used it.
	"apple-unified-64gb",
	// The 128 GB AMD unified-memory reference host (Strix Halo class).
	// Today: unified-amd-ryzen-ai-max-395.
	"amd-unified-128gb",
}

// ValidHostClass reports whether h may name the hardware a measurement
// ran on: either a well-formed derived key, or one of the frozen legacy
// spellings.
//
// Both stores read this one function, for the reason RequestShapeGaps
// reuses the agent-grade "unmeasurable" map: two spellings of one
// vocabulary is how two stores start disagreeing about where a model was
// measured.
func ValidHostClass(h string) bool {
	return hardware.ValidHostKey(h) || slices.Contains(LegacyHostClasses, h)
}

// IsLegacyHostClass reports whether h is one of the frozen spellings, so
// an importer can refuse to write a NEW record under one while a reader
// still accepts it.
func IsLegacyHostClass(h string) bool { return slices.Contains(LegacyHostClasses, h) }

// HostClassList renders what a caller may supply, for an error message.
func HostClassList() string {
	return "a derived key (" + strings.Join([]string{
		"unified-amd-ryzen-ai-max-395", "discrete-nvidia-sm120", "cpu-none",
	}, ", ") + ", …) or one of the frozen legacy spellings (" +
		strings.Join(LegacyHostClasses, ", ") + ")"
}
