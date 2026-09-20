package catalog

import "math"

// ExtendedContextLength is the context window a model reaches with the rope
// scaling its publisher documents: Factor times OriginalContextLength. It
// is 0 for a model that carries no scaling, and 0 when the scaling does not
// actually reach past the model's own window — so a caller can treat 0 as
// "this model serves its own ContextLength and nothing longer" without
// looking at RopeScaling itself.
//
// This is a fact about the MODEL. Whether any computer can hold that window,
// and whether anybody asked for it, are separate questions and answered
// elsewhere: nothing here implies a host will be given the longer window.
func ExtendedContextLength(m Manifest) int {
	r := m.RopeScaling
	if r == nil || r.Factor <= 1 || r.OriginalContextLength <= 0 {
		return 0
	}
	reach := r.Factor * float64(r.OriginalContextLength)
	if reach > math.MaxInt32 {
		return 0
	}
	n := int(reach)
	if n <= m.ContextLength {
		return 0
	}
	return n
}

// PublisherContextLimit is the longest context the model's publisher says
// its scaling is good for, or 0 where they stated none. It is prose to
// quote, not a number to serve or compare a host against: the window the
// product asks an engine for is decided by hostfit, and a publisher's
// figure can sit anywhere relative to it. Qwen states 1,010,000 tokens for
// a scaling whose factor reaches 1,048,576.
func PublisherContextLimit(m Manifest) int {
	if m.RopeScaling == nil {
		return 0
	}
	return m.RopeScaling.PublisherMaxContextLength
}

// HasRopeScaling reports whether m documents a way past its own window.
func HasRopeScaling(m Manifest) bool { return ExtendedContextLength(m) > 0 }
