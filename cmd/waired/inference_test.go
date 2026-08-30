package main

import (
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestRunInference_Errors(t *testing.T) {
	// Under cobra, a namespace command with no subverb (e.g. `inference` or
	// `inference share`) prints help and exits 0 — so only genuinely-unknown
	// subcommands are errors here.
	cases := []struct {
		name string
		args []string
	}{
		{"unknown subverb", []string{"share", "what"}},
		{"unknown top sub", []string{"unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Redirect stderr to swallow the usage output.
			stderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stderr = w
			defer func() { os.Stderr = stderr }()
			err := runInference(tc.args)
			w.Close()
			_ = readAll(t, r)
			if err == nil {
				t.Errorf("expected error for args=%v, got nil", tc.args)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#589). `waired inference status` shows
// the install-time memory measurement and dates it.
//
// The figure is fixed for the life of the install and reads exactly like
// a live reading, so an operator looking at a model-fit verdict has no
// other way to see what it rests on or how stale it is.
func TestHostMemoryLine(t *testing.T) {
	cases := []struct {
		name string
		in   *management.HostMemoryMeasurement
		want string
	}{
		{
			name: "measured, with a total to compare against",
			in: &management.HostMemoryMeasurement{
				AvailableGB: 22, TotalGB: 32, MeasuredAt: "2026-08-10T04:12:03Z",
			},
			want: "Free memory measured at startup: 22 GB of 32 GB (measured 2026-08-10).",
		},
		{
			// A record written before the date field existed still reads.
			name: "no date recorded",
			in:   &management.HostMemoryMeasurement{AvailableGB: 22, TotalGB: 32},
			want: "Free memory measured at startup: 22 GB of 32 GB.",
		},
		{
			name: "nothing measured is not a zero",
			in:   &management.HostMemoryMeasurement{AvailableGB: 0, TotalGB: 32},
			want: "",
		},
		{name: "absent", in: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostMemoryLine(tc.in); got != tc.want {
				t.Errorf("hostMemoryLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The date is rendered as a DATE. The measurement describes an install,
// so a wall-clock time would claim a precision the figure does not have.
func TestHostMemoryLine_RendersADateNotATimestamp(t *testing.T) {
	got := hostMemoryLine(&management.HostMemoryMeasurement{
		AvailableGB: 22, MeasuredAt: "2026-08-10T04:12:03Z",
	})
	if strings.Contains(got, "04:12") || strings.Contains(got, "T") {
		t.Errorf("line = %q, want the date only", got)
	}
	if !strings.Contains(got, "2026-08-10") {
		t.Errorf("line = %q, want it to carry the measurement date", got)
	}
}
