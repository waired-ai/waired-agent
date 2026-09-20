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
//     side, and the only structured field there is ComputeCap. That is
//     an ARCHITECTURE, not a part: an RTX PRO 4000 Blackwell and an RTX
//     5090 both report 12.0 while their memory bandwidth differs by
//     nearly 3x.
//
// GPU.Model would name the NVIDIA part, and is deliberately not used:
// its own doc says "free-form; do not parse", and #251 recorded choosing
// CPU.Model over it for this exact class of decision. Encoding capacity
// in the name to compensate would reintroduce what this key exists to
// remove. The cover for the gap belongs at import instead, where the
// record's own numbers — VRAM, bandwidth — can be held against the ones
// already in the store; a label cannot check itself.

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
// vendor is the lowercase GPU vendor token (hardware.GPU.Vendor).
func ChipSlug(vendor, cpuModel, computeCap string) string {
	switch strings.ToLower(strings.TrimSpace(vendor)) {
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
		// "12.1" -> "sm121". The "sm_" prefix is NVIDIA's own spelling
		// for a compute capability as a target (sm_121), which is what
		// makes it readable as an architecture rather than a version.
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
		ChipSlug(prof.GPUs[0].Vendor, prof.CPU.Model, prof.GPUs[0].ComputeCap)
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
