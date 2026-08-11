package main

import (
	"errors"
	"io"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
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
		name                      string
		nonInteractive, noBrowser bool
	}{
		{"non-interactive", true, false},
		{"no-browser", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Active: this daemon is serving a leftover instruction, which is
			// the shape that made the daemon report `driver: browser` over a
			// terminal-driven run on a reused device row (waired-agent#645).
			d := &fakeSetupDaemon{state: management.SetupStateResponse{Active: true}}
			srv := d.server(t)
			s := attachSetupExecutor(srv.URL, true)
			t.Cleanup(s.Release)

			awaitBrowserSetup(s, nil, io.Discard, tc.nonInteractive, tc.noBrowser)

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

	awaitBrowserSetup(s, nil, io.Discard, true, false)

	if got := d.noted(); len(got) != 0 {
		t.Fatalf("posted %d lease updates to a daemon without the routes, want none", len(got))
	}
}

// --- the coding-tool row the terminal now reports (waired-agent#645) ---

func TestReportTerminalIntegrations(t *testing.T) {
	tests := []struct {
		name      string
		consented bool
		err       error
		want      []string // nil = nothing reported
	}{
		{
			// The #645 case: the operator said yes, `waired link all`
			// configured both agents, and until now nobody told the daemon.
			name:      "consented and clean reports the row",
			consented: true,
			want:      []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
		},
		{
			// Declined writes nothing, so there is nothing to claim. The row
			// stays with whatever the projection already says about it.
			name:      "declined reports nothing",
			consented: false,
		},
		{
			// A half-configured machine is what applySetupIntegrations
			// already refuses to call done; the terminal must not be laxer
			// than the wizard about the same files.
			name:      "a failed apply reports nothing",
			consented: true,
			err:       errors.New("claude-code: permission denied"),
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
			if tc.want == nil {
				if got != nil {
					t.Fatalf("reported %+v, want no integration row", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("no integration row reported")
			}
			if got.Phase != management.SetupExecutorPhaseDone {
				t.Errorf("phase = %q, want done", got.Phase)
			}
			if len(got.IntegrationTargets) != len(tc.want) {
				t.Fatalf("targets = %v, want %v", got.IntegrationTargets, tc.want)
			}
			for i, w := range tc.want {
				if got.IntegrationTargets[i] != w {
					t.Fatalf("targets = %v, want %v", got.IntegrationTargets, tc.want)
				}
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
	want := []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw}
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
}
