//go:build integration

package integration

import (
	"context"
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

	for _, leg := range legs() {
		leg := leg
		if e.Only != nil && !e.Only[leg.Name] {
			continue
		}
		t.Run(leg.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

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
			// warms. Stop on the first 2xx.
			var lastStatus int
			var lastBody []byte
			driveDeadline := time.Now().Add(3 * time.Minute)
			for {
				status, body, derr := leg.Drive(ctx, e)
				if derr != nil {
					t.Fatalf("drive: %v", derr)
				}
				lastStatus, lastBody = status, body
				if status >= 200 && status < 300 {
					break
				}
				if time.Now().After(driveDeadline) {
					t.Fatalf("drive never returned 2xx (last HTTP %d): %s", lastStatus, truncate(lastBody))
				}
				time.Sleep(2 * time.Second)
			}

			// Sentinel: the event ring must show a LOCALLY-served 2xx request of
			// the expected kind since the cursor. Its existence excludes a
			// fail-open passthrough (which the recorder never sees).
			ev, err := awaitLocalRequest(ctx, e, cursor, leg.ExpectKind, 30*time.Second)
			if err != nil {
				t.Fatalf("served-locally sentinel: %v\n(drive returned HTTP %d: %s)", err, lastStatus, truncate(lastBody))
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
