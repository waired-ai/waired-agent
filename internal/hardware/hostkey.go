package hardware

import (
	"regexp"
	"strings"
)

// HostKey names the KIND of machine a stored measurement was taken on,
// for the catalog's provenance stores (waired-agent#1455).
//
// WHAT WAS WRONG WITH THE OLD SPELLING. catalog.HostClasses spelled the
// vocabulary vendor-plus-memory-size — "nvidia-24gb-discrete",
// "apple-unified-64gb", "amd-unified-128gb" — so admitting a DGX Spark
// meant adding "nvidia-unified-128gb", and the list was on its way to
// being a roster of machines. Capacity is also the wrong axis to name
// things by: two 128 GB unified hosts measured about 2x apart on
// prefill, while one "apple-unified-64gb" would fold together parts
// whose bandwidth differs more than fourfold.
//
// The sharpest example is this project's own two NVIDIA machines, and
// the old spelling calls them the same thing:
//
//	CI GPU lane    NVIDIA L4                      24 GB GDDR6  cc 8.9   300 GB/s
//	Linux fleet    NVIDIA RTX PRO 4000 Blackwell  24 GB GDDR7  cc 12.0  672 GB/s
//
// Both are "nvidia-24gb-discrete". Their memory bandwidth differs by
// 2.24x, so seconds measured on one say nothing about the other — which
// is the single thing the name is supposed to convey. Derived, they are
// discrete-nvidia-sm89 and discrete-nvidia-sm120.
//
// THE SHAPE: <topology>-<vendor>-<chip>. Capacity is not in the name.
// It travels as a number in the record beside the seconds, where a
// reader can see that two machines differ instead of being told by a
// label that they are the same.
//
//	unified-apple-m4-max
//	unified-amd-ryzen-ai-max-395
//	discrete-nvidia-sm120
//	cpu-none
//
// The list stops growing because nothing enumerates it: every part is
// derived from something the profiler already holds, and the validator
// checks a grammar and three closed value sets rather than membership of
// a hand-written slice. A machine's own name cannot get in, because
// nothing here is typed — which is what the consequence note in
// docs/decisions/20260829/1100-measurement-provenance-is-derived-or-declared.md
// was actually protecting when it framed the hand-edit as "a review
// opportunity".
//
// HOW COARSE EACH VENDOR IS, HONESTLY. The chip part is only as precise
// as the structured facts allow, and they differ by vendor:
//
//   - Apple and AMD name the part in CPU.Model, which is the string
//     waired-agent#251 already keys the bandwidth table off. That is
//     chip-model granularity.
//   - NVIDIA does not. CPU.Model is EMPTY on aarch64 Linux — /proc/cpuinfo
//     has no "model name" line there at all, which is exactly the case a
//     Grace or GB10 host is — so the identity has to come from the GPU
//     side. For a DISCRETE part the only structured field there is
//     ComputeCap, and that is an ARCHITECTURE rather than a part: an RTX
//     PRO 4000 Blackwell and an RTX 5090 both report 12.0 while their
//     memory bandwidth differs by nearly 3x. That coarseness is known
//     and accepted; the cover for it belongs at import, where the
//     record's own numbers — VRAM, bandwidth — can be held against the
//     ones already in the store, because a label cannot check itself.
//   - NVIDIA's SINGLE-POOL parts are named from the device string
//     instead, through the table in nvidia_unified.go. This reverses,
//     for those parts only, the "GPU.Model is deliberately not used"
//     position this comment used to hold outright. Compute capability
//     genuinely cannot separate them — 12.1 is both the GB10 (128 GB at
//     273 GB/s) and the RTX Spark N1X (~45 GiB) — so keying on it would
//     reintroduce for NVIDIA exactly the two-machines-one-name defect
//     the L4 example above exists to condemn. The "free-form; do not
//     parse" rule binds consumers of the published summary; this is the
//     producer turning a string into the facts it publishes, which is
//     the carve-out #251 already relied on to key the bandwidth table
//     off CPU.Model.

// HostTopology values. Three, closed, and derived — never typed.
const (
	// TopologyUnified is a host whose accelerator memory and system RAM
	// are one physical pool.
	TopologyUnified = "unified"
	// TopologyDiscrete is a host whose accelerator has memory of its
	// own.
	TopologyDiscrete = "discrete"
	// TopologyCPU is a host with no accelerator to speak of.
	TopologyCPU = "cpu"
)

// HostTopologies is the closed set the first component may take.
var HostTopologies = []string{TopologyUnified, TopologyDiscrete, TopologyCPU}

// chipSlugUnknown is the chip component when nothing structured names
// the part. It is a legal key rather than an error because a machine
// with no readable chip identity can still be described by its topology
// and vendor, and refusing to name it at all would push the caller back
// towards typing something.
const chipSlugUnknown = "unknown"

// nonSlugChars is everything that is not a slug character. Applied after
// the vendor-specific trimming below, never instead of it: collapsing
// "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S" with this alone yields a key
// that names the GPU as well as the CPU and that differs between
// operating systems.
var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// amdGraphicsSuffix strips the iGPU that AMD's CPU strings carry —
// "AMD Ryzen AI Max+ 395 w/ Radeon 8060S" on Windows against
// "AMD Ryzen AI Max 395" on Linux for the SAME part. Without this the
// two operating systems would file measurements of one machine under two
// keys, which is the failure the single shared vocabulary existed to
// prevent.
var amdGraphicsSuffix = regexp.MustCompile(`\s+w/\s+.*$`)

// ChipSlug names the part, at whatever granularity the vendor's
// structured facts support. Empty is never returned; see
// chipSlugUnknown.
//
// It takes the device rather than three loose strings because it now
// reads three of its fields, and four positional strings of the same
// type is an argument order waiting to be transposed silently.
func ChipSlug(gpu GPU, cpuModel string) string {
	computeCap, gpuModel := gpu.ComputeCap, gpu.Model
	switch strings.ToLower(strings.TrimSpace(gpu.Vendor)) {
	case "apple":
		// "Apple M4 Max" -> "m4-max". The vendor is already the second
		// component of the key, so repeating it in the third would spell
		// "unified-apple-apple-m4-max".
		return slugOrUnknown(strings.TrimPrefix(normalizeChipName(cpuModel), "apple "))
	case "amd":
		// "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S" -> "ryzen-ai-max-395".
		// The "+" is a marketing distinction between bins of one part
		// and disappears with the other punctuation.
		m := amdGraphicsSuffix.ReplaceAllString(normalizeChipName(cpuModel), "")
		return slugOrUnknown(strings.TrimPrefix(m, "amd "))
	case "nvidia":
		// A named single-pool part first. Compute capability is an
		// ARCHITECTURE, not a part, and for these it is not even one
		// part per capability: 12.1 covers both the GB10 (DGX Spark,
		// 128 GB at 273 GB/s) and the RTX Spark N1X (a ~45 GiB pool).
		// Folding two machines under one provenance key is the defect
		// waired-agent#1455 removed for the discrete parts — an L4 and
		// an RTX PRO 4000 Blackwell were both "nvidia-24gb-discrete",
		// 2.24x apart on bandwidth — and it must not be reintroduced
		// here. nvidia_unified.go says why the name is the only thing
		// that separates them.
		if p, ok := nvidiaUnifiedPartFor(gpuModel); ok {
			return p.slug
		}
		// "12.1" -> "sm121". The "sm_" prefix is NVIDIA's own spelling
		// for a compute capability as a target (sm_121), which is what
		// makes it readable as an architecture rather than a version.
		// It remains the answer for discrete parts, where the same
		// coarseness is a known and accepted limitation (#1455).
		if digits := nonSlugChars.ReplaceAllString(computeCap, ""); digits != "" {
			return "sm" + digits
		}
		return chipSlugUnknown
	default:
		// Intel and anything a future detector adds: the CPU string is
		// the best structured name available, and for an integrated
		// part it is the right one.
		return slugOrUnknown(normalizeChipName(cpuModel))
	}
}

// slugOrUnknown reduces a already-normalised name to slug characters.
func slugOrUnknown(s string) string {
	out := strings.Trim(nonSlugChars.ReplaceAllString(s, "-"), "-")
	if out == "" {
		return chipSlugUnknown
	}
	return out
}

// HostTopologyOf answers the first component for a profile.
//
// It reads the REPORTED fact (GPU.Integrated) first and the policy flag
// (Profile.UnifiedMemory) only as a fallback, which is the right order
// for a provenance key: the key says what kind of machine took the
// measurement, and that is true of the hardware whether or not the
// budget rules have caught up with it. The fallback keeps today's two
// measured machines spelled correctly on hosts where the reading is not
// available — a Strix Halo still reads unified through IsStrixHaloAPU,
// and the GPU lane's discrete NVIDIA card still reads discrete.
//
// The device it describes is GPUs[0], the same entry EffectiveVRAMMB and
// PrimaryGPUVendor already read. That is enumeration order rather than a
// ranking (waired-agent#286), so on a host with two accelerators the key
// names whichever the detectors listed first. It is the same wrinkle
// those two callers have, and fixing it belongs with them.
func HostTopologyOf(prof *Profile) string {
	if len(prof.GPUs) == 0 {
		return TopologyCPU
	}
	if g := prof.GPUs[0]; g.IntegratedKnown {
		if g.Integrated {
			return TopologyUnified
		}
		return TopologyDiscrete
	}
	if prof.UnifiedMemory {
		return TopologyUnified
	}
	return TopologyDiscrete
}

// HostKey assembles <topology>-<vendor>-<chip> for a profile.
//
// A CPU-only host is "cpu-none": there is no vendor and no chip to name,
// and spelling the absence keeps every key three components wide so the
// grammar has one shape rather than two.
func HostKey(prof *Profile) string {
	topology := HostTopologyOf(prof)
	if topology == TopologyCPU {
		return TopologyCPU + "-none"
	}
	vendor := slugOrUnknown(normalizeChipName(prof.GPUs[0].Vendor))
	return topology + "-" + vendor + "-" +
		ChipSlug(prof.GPUs[0], prof.CPU.Model)
}

// hostKeyGrammar is what a derived key looks like: three or more
// lowercase alphanumeric words joined by single hyphens, the first of
// which is a topology.
//
// A grammar rather than a list is the point of waired-agent#1455: a new
// machine produces a new key without anybody editing a slice, and the
// keys still cannot be a machine's name, because nothing types them and
// "sv-mag" has no topology on the front.
var hostKeyGrammar = regexp.MustCompile(`^(unified|discrete|cpu)(-[a-z0-9]+)+$`)

// ValidHostKey reports whether s is well formed as a derived host key.
//
// It deliberately does NOT check that the vendor or chip are ones this
// build has seen. That check is what a roster does, and a roster is what
// this replaces: a measurement taken on a part released after this
// binary was built is still a measurement, and refusing to file it would
// send the operator back to editing a list.
func ValidHostKey(s string) bool { return hostKeyGrammar.MatchString(s) }
