package runtime

import (
	"strings"
	"testing"
)

// SetStartFailureReason had no test on either adapter until
// waired-agent#1069 — it shipped with one caller and zero coverage, which
// is how the interaction below went unnoticed.
//
// The ollama half lives here and the vLLM half in the _linux file:
// VLLMAdapter does not exist on darwin or windows at all, so a shared
// file would break those unit-test legs (CLAUDE.md §Cross-OS parity).

// TestSetStartFailureReason_OllamaPrependsWithoutReplacing pins the shape
// the doc promises: the diagnosis is an interpretation, the error is what
// actually happened, and both survive.
func TestSetStartFailureReason_OllamaPrependsWithoutReplacing(t *testing.T) {
	const raw = "ollama: process exited during startup: exit status 1"
	const why = "the inference engine was started without a home directory"

	a := &OllamaAdapter{}
	a.state = Health{State: StateFailed, LastErr: raw}
	a.SetStartFailureReason(why)

	got := a.state.LastErr
	if !strings.HasPrefix(got, why+"\n") {
		t.Errorf("diagnosis not prepended: %q", got)
	}
	if !strings.Contains(got, raw) {
		t.Errorf("raw error replaced rather than kept: %q", got)
	}
}

// TestSetStartFailureReason_OllamaNoOpUnlessFailed pins the guard the vLLM
// twin has carried since #1026.
func TestSetStartFailureReason_OllamaNoOpUnlessFailed(t *testing.T) {
	a := &OllamaAdapter{}
	a.state = Health{State: StateReady}
	a.SetStartFailureReason("a cause")
	if a.state.LastErr != "" {
		t.Errorf("wrote onto a healthy engine: %q", a.state.LastErr)
	}
	a.state = Health{State: StateFailed}
	a.SetStartFailureReason("")
	if a.state.LastErr != "" {
		t.Errorf("an empty reason must stay empty: %q", a.state.LastErr)
	}
}

// TestSetStartFailureReason_OllamaNoOpOnceLatched is the guard
// waired-agent#1069 added, and the reason it had to exist.
//
// The two writers of LastErr had no order between them: OnStartFailed
// fires from runStart's defer on a fresh goroutine, while the bootstrap
// calls this one after EnsureRunning returns. LatchFailed now composes the
// diagnosis into its own message, so a prepend landing afterwards printed
// the same sentence twice — on the surface a person reads first.
func TestSetStartFailureReason_OllamaNoOpOnceLatched(t *testing.T) {
	const why = "the inference engine was started without a home directory"
	a := &OllamaAdapter{}
	a.LatchFailed(why + "\nengine failed to start 4 times within 5m0s")
	before := a.state.LastErr
	a.SetStartFailureReason(why)
	if a.state.LastErr != before {
		t.Errorf("prepended onto a latched engine:\n%s", a.state.LastErr)
	}
	if strings.Count(a.state.LastErr, why) != 1 {
		t.Errorf("the named cause appears %d times:\n%s",
			strings.Count(a.state.LastErr, why), a.state.LastErr)
	}
}
