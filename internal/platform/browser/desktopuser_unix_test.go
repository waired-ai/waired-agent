//go:build linux || darwin

package browser

import (
	"os/user"
	"testing"
	"time"
)

// runHop's contract is what makes the de-escalation safe to attempt: a real
// failure must be visible so Open can fall back to the direct launch, while a
// slow-but-live launcher must NOT be, or the fallback opens a second window.
func TestRunHop(t *testing.T) {
	if err := runHop([]string{"/bin/sh", "-c", "exit 0"}); err != nil {
		t.Errorf("runHop(success) = %v, want nil", err)
	}
	if err := runHop([]string{"/bin/sh", "-c", "exit 3"}); err == nil {
		t.Error("runHop(non-zero exit) = nil, want an error so Open falls back")
	}
	if err := runHop([]string{"/nonexistent/waired-hop-binary"}); err == nil {
		t.Error("runHop(missing binary) = nil, want an error so Open falls back")
	}
	if err := runHop(nil); err == nil {
		t.Error("runHop(nil) = nil, want an error")
	}
}

func TestRunHopTimeoutIsNotAFailure(t *testing.T) {
	orig := hopTimeout
	hopTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hopTimeout = orig })

	// /bin/sleep directly, not `sh -c sleep`: killing the shell would leave
	// the grandchild holding the inherited stderr open for its full duration,
	// and `go test` would then wait on that pipe.
	if err := runHop([]string{"/bin/sleep", "5"}); err != nil {
		t.Errorf("runHop(timeout) = %v, want nil (the handler was most likely launched)", err)
	}
}

func TestLookupUID(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("user.Current: %v", err)
	}
	if got := lookupUID(me.Username); got != me.Uid {
		t.Errorf("lookupUID(%q) = %q, want %q", me.Username, got, me.Uid)
	}
	if got := lookupUID("waired-test-no-such-user"); got != "" {
		t.Errorf("lookupUID(unknown) = %q, want \"\"", got)
	}
}
