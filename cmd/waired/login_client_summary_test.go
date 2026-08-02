package main

import (
	"errors"
	"strings"
	"testing"
)

// The closing box is the last thing `waired init` says, and until #310 it
// was chosen by asking only whether the engine could be INSTALLED. On the
// rc7 host the install worked, the engine then would not run, and the run
// ended on "everything completed successfully!" over a device with no
// local AI at all.
//
// Product contract. The negative cases are the point as much as the
// positive one: not-ready is the honest answer on plenty of hosts where
// nothing is wrong, so only a STATED engine fault may change the box.
func TestPrintDaemonSummaryBoxPicksTheOutcomeItCanDefend(t *testing.T) {
	// Substrings, not whole lines: box() pads and frames its content, and
	// emoji are dropped when the terminal cannot render them.
	const (
		celebration  = "everything completed successfully"
		needsInstall = "local AI still needs installing"
		notRunning   = "local AI isn't running"
	)

	cases := []struct {
		name    string
		summary daemonSummary
		want    string
		absent  []string
	}{
		{
			name:    "everything landed",
			summary: daemonSummary{accountEmail: "someone@example.test"},
			want:    celebration,
			absent:  []string{needsInstall, notRunning},
		},
		{
			name:    "the engine could not be installed (#188)",
			summary: daemonSummary{engineErr: errors.New("download: 403")},
			want:    needsInstall,
			absent:  []string{celebration},
		},
		{
			// #310, the case that had no box of its own. The install box
			// would be wrong here: it points at the command that installs
			// an engine, and this host already has one.
			name:    "the engine installed and would not stay up (#310)",
			summary: daemonSummary{engineFailure: "ollama: process exited during startup: signal: killed"},
			want:    notRunning,
			absent:  []string{celebration, needsInstall},
		},
		{
			// Both true: the install never produced an engine, so there is
			// nothing for the #310 wording to describe.
			name: "an install failure outranks a wait that saw no engine",
			summary: daemonSummary{
				engineErr:     errors.New("download: 403"),
				engineFailure: "ollama: not reachable",
			},
			want:   needsInstall,
			absent: []string{celebration, notRunning},
		},
		{
			// NEGATIVE CONTROL. A gateway-only host answers `disabled` and
			// the wait returns not-ready by design. Keying the box on
			// "the wait did not reach ready" instead of on a stated fault
			// would hand these operators a warning about a machine that is
			// doing exactly what they configured.
			name:    "inference is simply switched off",
			summary: daemonSummary{accountEmail: "someone@example.test"},
			want:    celebration,
			absent:  []string{notRunning},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			printDaemonSummaryBox(&out, tc.summary)
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected %q in the summary, got: %q", tc.want, got)
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("did not expect %q in the summary, got: %q", a, got)
				}
			}
		})
	}
}
