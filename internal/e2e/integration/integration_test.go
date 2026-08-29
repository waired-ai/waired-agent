//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
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
// Requires a live enrolled daemon (WAIRED_MGMT_URL et al.); skips only when
// the caller named none.
func TestIntegration(t *testing.T) {
	e := LoadEnv()

	// Check what the CALLER asked for before consulting the world. A
	// filter naming a leg that does not exist is a mistyped request, and
	// it needs no daemon to notice; left alone it produces an empty
	// selection, zero subtests and exit 0.
	all := legs()
	names := make([]string, 0, len(all))
	for _, leg := range all {
		names = append(names, leg.Name)
	}
	if bad := unknownLegs(e.Only, names); len(bad) > 0 {
		t.Fatalf("%s names %v, which match no leg; the legs are %v", legsEnv, bad, names)
	}

	if !daemonReachable(e) {
		// #1118: this was an unconditional skip, so the three installtest
		// wrappers printed "every leg served locally (no fail-open)" over
		// a run that had contacted nothing. The signal telling the two
		// cases apart was already here: whether the caller named a
		// daemon. All three wrappers set WAIRED_MGMT_URL, and a daemon
		// they named and cannot reach is a failure of what they asked
		// for, not an absence of work.
		if e.MgmtNamed {
			t.Fatalf("%s=%s named a daemon and it is not reachable — a sentinel run that "+
				"contacted nothing has proved nothing, and must not report success (#1118)",
				mgmtURLEnv, e.MgmtURL)
		}
		t.Skipf("enrolled daemon not reachable at %s and no %s was named", e.MgmtURL, mgmtURLEnv)
	}

	// Make the routing model ready (idempotent; the shell hook normally
	// pre-pulls it, but keep the Go test self-sufficient for local runs).
	pullCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := pullTinyModel(pullCtx, e); err != nil {
		t.Logf("warn: models/pull %s: %v (continuing; the model may already be ready)", e.TinyAlias, err)
	}

	selected := make([]Leg, 0, len(all))
	for _, leg := range all {
		if includedLeg(leg.Name, e.Only) {
			selected = append(selected, leg)
		}
	}
	// Belt to unknownLegs' braces: any future way of arriving here with
	// nothing to drive is a run that asserts nothing, and the loop below
	// would report it as a pass.
	if len(selected) == 0 {
		t.Fatalf("no legs selected out of %v — this run would assert nothing", names)
	}

	// What actually ran, for the wrapper to read. Appended from inside the
	// subtests, which t.Run executes synchronously here (no t.Parallel).
	//
	// Every leg that STARTED, with what became of it — not the legs that
	// were served locally. Recording only the successes would put this
	// file back in the hole it was written to close: a run where four
	// legs each went upstream and a run where none started would both
	// produce an empty file, which is the empty-selection pass moved from
	// the exit code to the artifact. It does not arise today (a leg that
	// is not served locally fails its own assertion, and the wrapper
	// never reads the file on a non-zero exit) and it would arise the
	// moment a leg is legitimately expected upstream — waired-agent#1141
	// is that change.
	var ran []legOutcome
	t.Cleanup(func() { writeRunSummary(t, e, ran) })

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
			ran = append(ran, legOutcome{Name: leg.Name, Outcome: outcomeRan})
			this := len(ran) - 1
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
			ran[this].Outcome = outcomeLocal
		})
	}
}

// legOutcome is one leg the run started, and what became of it.
type legOutcome struct {
	Name    string
	Outcome string
}

const (
	// outcomeRan: the leg started and did not reach the served-locally
	// assertion. On a clean run nothing ends here.
	outcomeRan = "ran"
	// outcomeLocal: the event ring showed a locally-served 2xx of the
	// expected kind, which is the whole claim the sentinel makes.
	outcomeLocal = "local"
)

// writeRunSummary records what the run did, one "<leg> <outcome>" per line.
//
// The wrapper scripts asserted "every leg served locally (no fail-open)"
// from the exit status of `go test` and nothing else. That status is
// satisfied by this package's untagged budget tests on their own, so it
// did not even imply this function's caller ran. A file naming what
// happened is something the shell can read and repeat, and it cannot go
// stale against a wording change the way grepping `go test` output would
// — which is the coupling scripts/ci/harness-failure-strings-guard.sh
// exists to police.
//
// Plain text, whitespace-separated: three shells read it (bash, zsh on
// macOS, PowerShell) and none of them is guaranteed a jq.
func writeRunSummary(t *testing.T, e Env, ran []legOutcome) {
	t.Helper()
	if e.SummaryPath == "" {
		return
	}
	body := ""
	for _, r := range ran {
		body += r.Name + " " + r.Outcome + "\n"
	}
	if err := os.WriteFile(e.SummaryPath, []byte(body), 0o644); err != nil {
		t.Errorf("write %s=%s: %v", summaryEnv, e.SummaryPath, err)
	}
}

func truncate(b []byte) string {
	const max = 400
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
