//go:build linux

package runtime

import (
	"strings"
	"testing"
)

// The vLLM half of the SetStartFailureReason contract; see
// start_failure_reason_test.go for why it is split by OS.

func TestSetStartFailureReason_VLLMPrependsWithoutReplacing(t *testing.T) {
	const raw = "vllm: process exited during startup: exit status 1"
	const why = "another program is already listening on 127.0.0.1:9479"

	a := &VLLMAdapter{}
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

func TestSetStartFailureReason_VLLMNoOpUnlessFailed(t *testing.T) {
	a := &VLLMAdapter{}
	a.state = Health{State: StateReady}
	a.SetStartFailureReason("a cause")
	if a.state.LastErr != "" {
		t.Errorf("wrote onto a healthy engine: %q", a.state.LastErr)
	}
}

// TestSetStartFailureReason_VLLMNoOpOnceLatched is the race waired-agent#1069
// closed, on the engine that actually hit it.
func TestSetStartFailureReason_VLLMNoOpOnceLatched(t *testing.T) {
	const why = "another program is already listening on 127.0.0.1:9479"
	a := &VLLMAdapter{}
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
