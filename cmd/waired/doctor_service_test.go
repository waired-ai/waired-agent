package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
)

func TestServiceFindingFromResult(t *testing.T) {
	cases := map[string]struct {
		in     servicediag.Result
		want   integration.Status
		silent bool
	}{
		// A down agent is not a soft warning: nothing about Waired works, and
		// `waired doctor`'s exit code has to say so.
		"failed": {
			servicediag.Result{Status: servicediag.Failed, Cause: "blocked at startup", Hint: "start it"},
			integration.StatusFail, false,
		},
		// Deliberately stopped is not a fault to fail the run over — but the
		// user asking why nothing works still has to be told.
		"stopped": {
			servicediag.Result{Status: servicediag.Stopped, Cause: "stopped deliberately"},
			integration.StatusWarn, false,
		},
		// Healthy with history: up now, but the user should know it was
		// blocked at boot and will be again.
		"healthy after a past failure": {
			servicediag.Result{Status: servicediag.Healthy, Cause: "was blocked", Evidence: "SCM 7000"},
			integration.StatusWarn, false,
		},
		// Healthy and quiet: doctor already prints a ✓ for the management
		// endpoint, and a second ✓ for the same fact is padding.
		"healthy": {
			servicediag.Result{Status: servicediag.Healthy, Cause: "running"},
			integration.StatusUnknown, true,
		},
		// No evidence: the live probe already says "unreachable". A second
		// line admitting we cannot explain it is noise for someone stuck.
		"unknown": {servicediag.Result{}, integration.StatusUnknown, true},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := serviceFindingFromResult(c.in)
			if c.silent {
				if got.Subject != "" {
					t.Errorf("emitted a finding %+v, want silence", got)
				}
				return
			}
			if got.Subject == "" {
				t.Fatal("no finding emitted")
			}
			if got.Status != c.want {
				t.Errorf("Status=%v, want %v", got.Status, c.want)
			}
		})
	}
}

// The detail line carries cause, hint and evidence in that order: what
// happened, what to do, and what it rests on.
func TestServiceFindingFromResult_DetailCarriesEverything(t *testing.T) {
	got := serviceFindingFromResult(servicediag.Result{
		Status:   servicediag.Failed,
		Cause:    "Windows blocked the service.",
		Hint:     "Start it from the Waired menu.",
		Evidence: "Service Control Manager event 7000: ...",
	})
	for _, want := range []string{"Windows blocked", "Start it from", "event 7000"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("Detail=%q is missing %q", got.Detail, want)
		}
	}
}
