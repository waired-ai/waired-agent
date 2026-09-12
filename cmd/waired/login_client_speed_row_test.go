package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// PRODUCT CONTRACT (waired-agent#1299): the closing box's `Speed` row says
// what the figure was taken on.
//
// It is the host-cutoff probe's measurement — a fixed 0.8 B stand-in,
// timed before the chosen model was downloaded — and on a wizard-driven
// install it is the only number in the box. Without the clause, the row
// reads as the speed of the model this computer is about to serve. NAVI
// has carried the same clause on the same figure
// (web/admin/src/i18n/dict/setup.ts, setup_host_speed: "This computer:
// {secs} s per request, measured with a small model.").
func TestHostSpeedTurnLine_SaysWhatItWasMeasuredOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		hs   *management.HostSpeedStatus
		want string
	}{
		{
			name: "a full measurement",
			hs:   &management.HostSpeedStatus{TurnSeconds: 13.7, BudgetSeconds: 45},
			want: "13.7 s per request, measured with a small model (target: 45 s or less)",
		},
		{
			// waired-agent#579's lower bound: a host too slow to finish
			// the measurement inside the install window. Same clause, same
			// reason — it is the same probe.
			name: "the lower bound a screened host publishes",
			hs:   &management.HostSpeedStatus{TurnFloorSeconds: 210.4, BudgetSeconds: 45},
			want: "210.4 s or more per request, measured with a small model (target: 45 s or less)",
		},
		{
			name: "nothing measured says nothing",
			hs:   nil,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostSpeedTurnLine(tc.hs); got != tc.want {
				t.Errorf("hostSpeedTurnLine =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#1299): the `Claude` row carries the way
// back when it reports a computer that is not routed.
//
// It is the one row in a box titled "setup is complete" that reports
// something the operator has not got. On a wizard-driven install whose
// browser toggle was left off, the run ends on that box over a computer
// whose Claude Code still talks to the Anthropic API, and until now the
// row stated the fact and stopped there.
func TestClaudeSummaryLine_UnroutedRowNamesTheWayBack(t *testing.T) {
	unrouted := claudeSummaryLine(false)
	if !strings.Contains(unrouted, "still using the Anthropic API") {
		t.Errorf("unrouted row = %q, want the fact it has always stated", unrouted)
	}
	if !strings.Contains(unrouted, "waired claude enable") {
		t.Errorf("unrouted row = %q, want the command that routes it", unrouted)
	}
	// The write is machine-wide and needs elevation, which is spelled
	// differently per OS — a wrong `sudo` on Windows was waired#752.
	if runtime.GOOS == "windows" {
		if strings.Contains(unrouted, "sudo") {
			t.Errorf("unrouted row = %q, want no sudo on Windows", unrouted)
		}
	} else if !strings.Contains(unrouted, "sudo waired claude enable") {
		t.Errorf("unrouted row = %q, want the elevated spelling", unrouted)
	}

	// The routed row is untouched: there is nothing to do about it.
	routed := claudeSummaryLine(true)
	if !strings.Contains(routed, "routed through Waired") {
		t.Errorf("routed row = %q", routed)
	}
	if strings.Contains(routed, "claude enable") {
		t.Errorf("routed row = %q, want no remedy on a computer that has it", routed)
	}
}
