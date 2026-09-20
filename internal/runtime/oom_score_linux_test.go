//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestApplyEngineOOMScore writes the score on a real child and reads it
// back from /proc, because the whole function is that one write and a fake
// would be testing the fake.
//
// A child rather than this process: the kernel lets an unprivileged process
// RAISE its own oom_score_adj and not lower it again, so a test that set its
// own would leave the test binary marked for the rest of the run.
func TestApplyEngineOOMScore(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a child to write the score on: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if err := applyEngineOOMScore(cmd.Process.Pid); err != nil {
		t.Fatalf("applyEngineOOMScore: %v", err)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/oom_score_adj")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("read back %q: %v", b, err)
	}
	if got != engineOOMScoreAdj {
		t.Errorf("oom_score_adj = %d, want %d", got, engineOOMScoreAdj)
	}
}

// TestApplyEngineOOMScore_MissingProcessIsAnError pins that the failure is
// reported rather than swallowed here. The CALLER swallows it — a container
// without write access to /proc leaves the engine at the default, which is
// the behaviour before this existed — but it logs what it swallowed, and it
// can only do that if this returns something.
func TestApplyEngineOOMScore_MissingProcessIsAnError(t *testing.T) {
	// A pid that cannot exist: the kernel's maximum is well under this.
	if err := applyEngineOOMScore(1 << 30); err == nil {
		t.Error("writing the score for a pid that does not exist reported success")
	}
}

// TestEngineOOMScoreAdjIsAPreferenceNotASentence records why the constant is
// not 1000.
//
// At 1000 the engine is the first thing killed by any memory spike anywhere
// on the machine, and a needlessly dead engine costs a multi-minute reload.
// At 0 the kernel is as likely to take the operator's editor or the ssh
// session they would use to fix the problem. This is a record of today's
// choice rather than a ratified contract.
func TestEngineOOMScoreAdjIsAPreferenceNotASentence(t *testing.T) {
	if engineOOMScoreAdj <= 0 || engineOOMScoreAdj >= 1000 {
		t.Errorf("engineOOMScoreAdj = %d, want a preference strictly between 0 and 1000",
			engineOOMScoreAdj)
	}
}
