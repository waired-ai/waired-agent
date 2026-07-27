//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestIntegration is the coding-agent routing sentinel (#496). For each leg it
// configures the tool's real provider config, drives one inference request at
// the gateway surface that config targets, and asserts the daemon's event ring
// recorded a LOCALLY-served 2xx response of the expected kind — proving the
// tool → provider → gateway → local-model path works and did not fail open to
// real Anthropic.
//
// Requires a live enrolled daemon (WAIRED_MGMT_URL et al.); skips otherwise.
func TestIntegration(t *testing.T) {
	e := LoadEnv()
	if !daemonReachable(e) {
		t.Skipf("enrolled daemon not reachable at %s (set WAIRED_MGMT_URL to a live agent)", e.MgmtURL)
	}

	// Make the routing model ready (idempotent; the shell hook normally
	// pre-pulls it, but keep the Go test self-sufficient for local runs).
	pullCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := pullTinyModel(pullCtx, e); err != nil {
		t.Logf("warn: models/pull %s: %v (continuing; the model may already be ready)", e.TinyAlias, err)
	}

	selected := make([]Leg, 0, 4)
	for _, leg := range legs() {
		if includedLeg(leg.Name, e.Only) {
			selected = append(selected, leg)
		}
	}
	deadline, hasDeadline := t.Deadline()

	for i, leg := range selected {
		leg := leg
		// Derive each leg's retry budget from the time the TEST BINARY has
		// left, so the legs structurally cannot outlive -timeout. Before
		// waired-agent#29 this was a flat 3 minutes: 4 legs x 3min exceeded
		// go test's 10min default, the last leg was killed mid-request, and
		// the t.Fatalf naming the real HTTP status never printed.
		budget := maxLegDriveBudget
		if hasDeadline {
			budget = legDriveBudget(time.Until(deadline), len(selected)-i)
		}
		t.Run(leg.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), budget+legOverhead+15*time.Second)
			defer cancel()
			progressf("%s: drive budget %s, attempt timeout %s", leg.Name, budget, driveAttemptTimeout)

			if leg.Configure != nil {
				teardown, err := leg.Configure(ctx, e)
				if err != nil {
					t.Fatalf("configure: %v", err)
				}
				if teardown != nil {
					t.Cleanup(teardown)
				}
			}

			cursor, err := ringCursor(ctx, e)
			if err != nil {
				t.Fatalf("event-ring cursor: %v", err)
			}

			// Drive with retries: the first request triggers a cold model load,
			// so a transient 425/503 (not-ready) is expected before the engine
			// warms. Stop on the first 2xx — or the moment the response proves
			// no amount of waiting can produce one (classifyDrive), so the
			// failure lands while the engine's logs still explain it.
			var last driveResponse
			started := time.Now()
			driveDeadline := started.Add(budget)
			for attempt := 1; ; attempt++ {
				attemptTimeout := driveAttemptTimeout
				if remaining := time.Until(driveDeadline); remaining > 0 && remaining < attemptTimeout {
					attemptTimeout = remaining
				}
				attemptCtx, acancel := context.WithTimeout(ctx, attemptTimeout)
				stopWatch := watchLeg(leg.Name, fmt.Sprintf("drive attempt %d", attempt))
				resp, derr := leg.Drive(attemptCtx, e)
				stopWatch()
				acancel()
				if derr != nil {
					t.Fatalf("drive attempt %d after %s: %v", attempt, time.Since(started).Round(time.Second), derr)
				}
				last = resp
				verdict, why := classifyDrive(resp.Status, resp.Header, resp.Body, e.AnthropicBlackholed)
				progressf("%s attempt %d: HTTP %d after %s", leg.Name, attempt, resp.Status,
					time.Since(started).Round(time.Millisecond))
				if verdict == driveOK {
					break
				}
				if verdict == driveTerminal {
					t.Fatalf("drive gave up after %d attempt(s) in %s: %s\n%s",
						attempt, time.Since(started).Round(time.Second), why,
						driveFailureDetail(e, cursor, leg.ExpectKind, last))
				}
				if time.Now().After(driveDeadline) {
					t.Fatalf("drive never returned 2xx within %s (%d attempts, last HTTP %d):\n%s",
						budget, attempt, last.Status,
						driveFailureDetail(e, cursor, leg.ExpectKind, last))
				}
				time.Sleep(driveRetryDelay)
			}

			// Sentinel: the event ring must show a LOCALLY-served 2xx request of
			// the expected kind since the cursor. Its existence excludes a
			// fail-open passthrough (which the recorder never sees).
			ev, err := awaitLocalRequest(ctx, e, cursor, leg.ExpectKind, sentinelTimeout)
			if err != nil {
				t.Fatalf("served-locally sentinel: %v\n%s", err,
					driveFailureDetail(e, cursor, leg.ExpectKind, last))
			}
			t.Logf("served locally: kind=%s model=%s decision=%s status=%d latency=%dms",
				ev.Kind, ev.Model, ev.Decision, ev.Status, ev.LatencyMs)
		})
	}
}

func truncate(b []byte) string {
	const max = 400
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
