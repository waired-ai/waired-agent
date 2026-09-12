//go:build linux || darwin

package logdump

import (
	"context"
	"io"
	"testing"
	"time"
)

// `waired logs` is the command a person runs when something is already wrong,
// so it is the worst one to hang. It did not bound its log-reading child in
// either direction: no process group, so a cancel reached only the direct
// child, and no WaitDelay, so CombinedOutput went on waiting for every
// inherited pipe writer to close — one surviving grandchild holds the command
// open for as long as it lives (waired-agent#1308).
//
// PIN: product contract — a cancelled service-log read returns without waiting
// on what the child left behind (waired-ai/waired-agent#1308).
//
// Unix-only: the fixture needs a shell that backgrounds a grandchild. The
// Windows half of the same fix is the WaitDelay, which is portable and covered
// by the same code path.
func TestRunServiceLogDoesNotWaitOnASurvivingGrandchild(t *testing.T) {
	// Absolute slack, not a ratio: the bound is WaitDelay (2s) plus room for a
	// loaded CI runner, and it is an order of magnitude below the grandchild's
	// own lifetime, so a pass cannot be a fast machine and a fail cannot be a
	// slow one.
	const grandchildLife = 120 * time.Second
	const bound = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The grandchild inherits the pipe and outlives the shell.
		_ = runServiceLog(ctx, io.Discard, "sh", []string{"-c", "sleep 120 & sleep 120"})
	}()

	select {
	case <-done:
	case <-time.After(bound):
		t.Fatalf("runServiceLog was still waiting %s after its context was cancelled; "+
			"the child it left behind lives %s", bound, grandchildLife)
	}
}
