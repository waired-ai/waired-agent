package main

import "testing"

// Sign-out leaves managed settings alone on purpose — the file needs elevation
// and sign-out asks for none (docs/decisions/20260907/0230-sign-out-is-the-daemons-job.md).
// So ANTHROPIC_BASE_URL is still there afterwards, and the SessionStart hook
// would cheerfully rewrite the /model rows that `waired logout` just removed.
// The hook has to ask whether this computer is still signed in.
//
// PIN: product contract — a signed-out computer offers no Waired rows in
// /model (waired-ai/waired-agent#1310, owner ruling 2026-09-12).
func TestPickerHookClearsOnlyOnAnExplicitSignOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    daemonEnrolment
		want bool
	}{
		{"signed out: take the rows away", enrolmentSignedOut, true},
		{"signed in: keep refreshing them", enrolmentSignedIn, false},
		// The important one. No answer is what a machine looks like for a
		// moment at boot, and permanently on a per-user install with nothing
		// running; clearing then would take a signed-in user's rows for a race.
		{"no answer: leave them alone", enrolmentUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickerHookClears(tc.e); got != tc.want {
				t.Errorf("pickerHookClears(%v) = %v, want %v", tc.e, got, tc.want)
			}
		})
	}
}
