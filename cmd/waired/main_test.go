package main

import (
	"strings"
	"testing"
)

// TestResidencyLine pins how the status line reports a model in memory.
// The product default holds it with no expiry, and the engine says so by
// naming a date centuries out; printing that produced
// "until 2318-11-30T12:52:47Z" on a real host, which reads as
// corruption rather than as the setting the operator chose
// (waired-agent#910).
func TestResidencyLine(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resident   bool
		model      string
		until      string
		indefinite bool
		want       string
		notWant    string
	}{
		{
			name: "not resident says what happens next",
			want: "no (the next request reloads it)",
		},
		{
			name:     "a finite hold names the time",
			resident: true, model: "m:q4", until: "2026-08-20T13:11:43Z",
			want: "m:q4 (until 2026-08-20T13:11:43Z)",
		},
		{
			name:     "an indefinite hold names no time",
			resident: true, model: "m:q4", indefinite: true,
			want: "m:q4 (kept until unloaded)", notWant: "until 2",
		},
		{
			name:     "resident with no expiry reported",
			resident: true, model: "m:q4",
			want: "m:q4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := residencyLine(tc.resident, tc.model, tc.until, tc.indefinite)
			if got != tc.want {
				t.Errorf("residencyLine = %q, want %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("residencyLine = %q, must not contain %q", got, tc.notWant)
			}
		})
	}
}
