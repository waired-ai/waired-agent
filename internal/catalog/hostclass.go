package catalog

import (
	"slices"
	"strings"
)

// HostClasses is the vocabulary a measurement may name as the hardware it
// ran on.
//
// Declared rather than recognised by shape. The risk this closes is not a
// malformed string: this repository is public, and the field's own doc
// (VariantAgentGrade.Host) already says "a hardware CLASS, never an
// identifier". A pattern that tried to tell a class from a machine name
// would have to guess — "sv-mag" and "apple-unified-64gb" are both
// lower-case words joined by dashes. A list cannot guess, and it makes
// adding a class a reviewed line of diff rather than a typed string that
// nobody sees again.
//
// Both stores read this one list, for the reason RequestShapeGaps reuses
// the agent-grade "unmeasurable" map: two spellings of the same
// vocabulary is how the two stores would start disagreeing about where a
// model was measured.
var HostClasses = []string{
	// The GPU lane's machine, and every record in either store today.
	"nvidia-24gb-discrete",
	// Named as legal by VariantAgentGrade.Host's own doc comment since
	// the field was introduced; no measurement has used it yet.
	"apple-unified-64gb",
}

// ValidHostClass reports whether h is one of HostClasses.
func ValidHostClass(h string) bool { return slices.Contains(HostClasses, h) }

// HostClassList renders the vocabulary for an error message, in the order
// HostClasses declares it.
func HostClassList() string { return strings.Join(HostClasses, ", ") }
