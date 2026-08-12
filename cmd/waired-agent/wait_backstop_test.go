package main

import (
	"testing"
	"time"
)

// waitBackstop bounds every wait in this package's tests. It is a BACKSTOP,
// not a budget: reaching it is a failure, and a healthy run ends in
// milliseconds when the signal the test is actually waiting for fires.
//
// One figure, where this package used to carry five (1 s / 2 s / 3 s / 5 s /
// 10 s) across roughly thirty sites and four near-identical helpers. None of
// those numbers was ever part of an assertion. awaitFlag's comment states the
// general case: the regression these waits guard is an intent being DROPPED,
// and a dropped intent never becomes true, so the test still fails — at the
// deadline instead of instantly. The deadline bounds the failure; it does not
// detect it. A figure that is too small therefore has exactly one possible
// effect, a false red, which is what waired-agent#720 was: the whole
// cmd/waired-agent package takes 53.5 s on the Windows runner against ~20 s
// on linux, and under that much contention 5 s is thin for "a goroutine gets
// scheduled and sends on a channel".
//
// Deliberately NOT scaled by GOOS and NOT read from a CI environment
// variable. Neither has any precedent in this tree, both make the local run
// differ from the one that fails, and a wait that is generous everywhere
// costs nothing on the runs that pass.
//
// This is a record of today's behaviour, not a product contract: no issue or
// decision fixes the number, and a test that genuinely needs to assert
// something happens WITHIN a duration should say so with its own figure and
// a comment explaining what the figure is part of.
const waitBackstop = 30 * time.Second

// waitPoll paces the polling helpers. Small enough to be invisible beside a
// test's real work, and it costs nothing on a condition that already holds:
// waitUntil checks before it first sleeps.
const waitPoll = 2 * time.Millisecond

// waitUntil blocks until cond holds, then returns; on waitBackstop it fails
// the test. `what` completes "timed out waiting for ...".
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitBackstop)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s after %s — the backstop, not a budget: something is wrong with the subject, not with the runner", what, waitBackstop)
		}
		time.Sleep(waitPoll)
	}
}
