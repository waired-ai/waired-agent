//go:build !linux && !windows && !darwin

package hardware

// Every other build target answers "unknown" to the question in
// integrated.go.
//
// Unknown is the correct answer here rather than a stub that needs
// filling in: this repo ships agents for three operating systems, and a
// fourth would need its own reading of the same question before it could
// say anything. Answering "not integrated" would be a claim nobody
// measured, and would be the one direction the merge rule refuses to
// accept from a source that does not know.
func integratedFromOS(_ *Profile, _ int) integration { return integrationUnknown() }
