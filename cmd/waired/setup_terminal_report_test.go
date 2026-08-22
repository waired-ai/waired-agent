package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// What a terminal-driven `waired init` tells the daemon about itself
// (waired-agent#646, #645). Until this, it told it nothing: the control
// plane's desired columns are written by the management API alone, so
// nothing on the device knew a setup had happened here.

// lastDriver is the most recent non-empty driver claim in the recorded
// lease traffic. Non-empty is what matters: a heartbeat repeats the claim
// but is allowed to omit it, and an omission must not read as a retraction.
func lastDriver(reqs []management.SetupExecutorRequest) string {
	out := ""
	for _, r := range reqs {
		if r.Driver != "" {
			out = r.Driver
		}
	}
	return out
}

// The #646 case in one test: an interactive init whose browser never
// arrives. Before this the grace simply expired and the run continued with
// the lease claiming nothing, so the daemon had neither desired state nor a
// driver and pushed no setup report at all.
func TestAwaitSetupBudgetClaimsTheTerminalWhenTheGraceExpires(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	budget, active := awaitSetupBudget(s, setupAwaitGrace, io.Discard, newTakeoverWatch(nil))
	if active || budget != benchPollDeadline {
		t.Fatalf("budget=%v active=%v, want the legacy terminal path", budget, active)
	}
	if got := lastDriver(d.noted()); got != signer.SetupDriverTerminal {
		t.Fatalf("driver = %q after the grace expired, want terminal", got)
	}
}

// A browser that DOES take the setup keeps its implicit claim: the desired
// state it wrote is the evidence, and a terminal claim here would have the
// wizard reporting a terminal over its own session.
func TestAwaitSetupBudgetLeavesTheClaimAloneWhenTheBrowserDrives(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{state: management.SetupStateResponse{Active: true}}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	_, active := awaitSetupBudget(s, setupAwaitGrace, io.Discard, newTakeoverWatch(nil))
	if !active {
		t.Fatal("active = false with a browser setup driving")
	}
	if got := lastDriver(d.noted()); got != "" {
		t.Fatalf("driver = %q while the browser drives, want no claim", got)
	}
}

// --non-interactive and --no-browser never offer a browser at all, so the
// terminal is the driver from the first frame. The rest of those paths is
// unchanged, which the existing skip test pins.
func TestAwaitBrowserSetupClaimsTheTerminalOnTheUnattendedPaths(t *testing.T) {
	shrinkSetupTimers(t)
	for _, tc := range []struct {
		name                                  string
		nonInteractive, noBrowser, authKeyRun bool
	}{
		{"non-interactive", true, false, false},
		{"no-browser", false, true, false},
		// waired-agent#797: an auth-key run has no browser either, and it
		// used to spend the whole grace unclaimed — which on this very
		// fixture is the window where the daemon derives `browser` from
		// the leftover instruction.
		{"auth key", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Active: this daemon is serving a leftover instruction, which is
			// the shape that made the daemon report `driver: browser` over a
			// terminal-driven run on a reused device row (waired-agent#645).
			d := &fakeSetupDaemon{state: management.SetupStateResponse{Active: true}}
			srv := d.server(t)
			s := attachSetupExecutor(srv.URL, true)
			t.Cleanup(s.Release)

			awaitBrowserSetup(s, nil, io.Discard, tc.nonInteractive, tc.noBrowser, tc.authKeyRun)

			if got := lastDriver(d.noted()); got != signer.SetupDriverTerminal {
				t.Fatalf("driver = %q, want terminal", got)
			}
		})
	}
}

// A daemon too old for the executor routes leaves the session inert, and an
// inert session must post nothing at all — that is what keeps the pre-#835
// flow byte-identical.
func TestAwaitBrowserSetupClaimsNothingOnAnOlderDaemon(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{notFound: true}
	srv := d.server(t)
	s := attachSetupExecutor(srv.URL, true)
	t.Cleanup(s.Release)

	awaitBrowserSetup(s, nil, io.Discard, true, false, false)

	if got := d.noted(); len(got) != 0 {
		t.Fatalf("posted %d lease updates to a daemon without the routes, want none", len(got))
	}
}

// --- the coding-tool row the terminal now reports (waired-agent#645) ---

func TestReportTerminalIntegrations(t *testing.T) {
	tests := []struct {
		name        string
		consented   bool
		err         error
		wantRow     bool
		wantPhase   string
		wantCode    string
		wantTargets []string
	}{
		{
			// The #645 case: the operator said yes, `waired link all`
			// configured both agents, and until now nobody told the daemon.
			name:        "consented and clean reports the row",
			consented:   true,
			wantRow:     true,
			wantPhase:   management.SetupExecutorPhaseDone,
			wantTargets: []string{signer.IntegrationClaudeCode, signer.IntegrationOpenCode, signer.IntegrationOpenClaw},
		},
		{
			// Declined writes nothing, so there is nothing to claim. The row
			// stays with whatever the projection already says about it.
			name:      "declined reports nothing",
			consented: false,
		},
		{
			// INVERTED by waired-agent#791: this case used to assert that a
			// failed apply reported NOTHING, which is how a failure left no
			// row at all — neither failed nor pending — and setup_complete
			// was reached over it. A half-configured machine still must not
			// be called done; it is called failed.
			//
			// No targets ride a failure: the list is a claim about files
			// this process wrote, the daemon reads it on the `done` edge,
			// and an apply that stopped at the first adapter wrote fewer
			// than it names.
			name:      "a failed apply reports the row as failed",
			consented: true,
			err:       errors.New("claude-code: permission denied"),
			wantRow:   true,
			wantPhase: management.SetupExecutorPhaseFailed,
		},
		{
			// The runuser flake #791 part 2 describes. The daemon only ever
			// sees the text, so `errors.Is` has to happen here or the code
			// is lost: classifySetupFailure would call a timeout `internal`.
			name:      "a deadline names itself",
			consented: true,
			err:       fmt.Errorf("/usr/sbin/runuser: %w", context.DeadlineExceeded),
			wantRow:   true,
			wantPhase: management.SetupExecutorPhaseFailed,
			wantCode:  signer.SetupErrorTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSetupTimers(t)
			d := &fakeSetupDaemon{}
			srv := d.server(t)
			s := attachSetupExecutor(srv.URL, true)
			t.Cleanup(s.Release)
			before := len(d.noted())

			reportTerminalIntegrations(s, tc.consented, tc.err)

			var got *management.SetupExecutorRequest
			for _, r := range d.noted()[before:] {
				if r.Step == management.SetupStepIntegration {
					c := r
					got = &c
				}
			}
			if !tc.wantRow {
				if got != nil {
					t.Fatalf("reported %+v, want no integration row", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("no integration row reported")
			}
			if got.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.ErrorCode != tc.wantCode {
				t.Errorf("error_code = %q, want %q", got.ErrorCode, tc.wantCode)
			}
			if tc.wantPhase == management.SetupExecutorPhaseFailed && got.Error == "" {
				t.Error("a failed row carried no detail; the daemon classifies from the text")
			}
			if len(got.IntegrationTargets) != len(tc.wantTargets) {
				t.Fatalf("targets = %v, want %v", got.IntegrationTargets, tc.wantTargets)
			}
			for i, w := range tc.wantTargets {
				if got.IntegrationTargets[i] != w {
					t.Fatalf("targets = %v, want %v", got.IntegrationTargets, tc.wantTargets)
				}
			}
		})
	}
}

// The CLI names exactly one code and leaves the rest to the daemon.
// classifyIntegrationFailure already owns the permission-denied reading,
// and a second implementation of it here is how the two would come to
// disagree about the same failure. A deadline is the exception because it
// is not in the text the daemon receives — only errors.Is can see it.
func TestTerminalIntegrationErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, ""},
		{"a bare deadline", context.DeadlineExceeded, signer.SetupErrorTimeout},
		{"a wrapped deadline", fmt.Errorf("runuser: %w", context.DeadlineExceeded), signer.SetupErrorTimeout},
		{"permission denied stays the daemon's to classify", errors.New("open /home/u/.claude: permission denied"), ""},
		{"anything else", errors.New("disk on fire"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalIntegrationErrorCode(tc.err); got != tc.want {
				t.Errorf("terminalIntegrationErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// The list has to be the one the applier actually writes, or the daemon
// records a claim about files nobody touched. Both terminal journeys run
// integration.Manager.ApplyAll over this adapter set and stop at the first
// failure, so a clean run wrote every one of them.
func TestTerminalIntegrationTargetsAreTheAdaptersTheApplierCovers(t *testing.T) {
	got := terminalIntegrationTargets()
	want := []string{signer.IntegrationClaudeCode, signer.IntegrationOpenCode, signer.IntegrationOpenClaw}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %v, want %v", got, want)
		}
	}
	// Every id has to survive the control plane's own validator, or the
	// daemon drops it on the way to the record and the row silently loses
	// its author.
	for _, id := range got {
		if !signer.IsValidIntegrationTarget(id) {
			t.Errorf("%q is not a target the control plane accepts", id)
		}
	}
	// And the other direction: every target the wire accepts has an
	// adapter here. The two lists live in different modules and move in
	// separate PRs (proto first, by rule); a wire-valid target with no
	// adapter is what the executor's "no adapter in this build → skip"
	// guard absorbs during that window (waired-agent#981), and this pin
	// is what says the window has closed (waired-agent#982).
	for _, id := range []string{signer.IntegrationClaudeCode, signer.IntegrationOpenCode, signer.IntegrationOpenClaw} {
		if !setup.HasAdapter(integration.AgentID(id)) {
			t.Errorf("%q is accepted on the wire but this build has no adapter for it", id)
		}
	}
}
