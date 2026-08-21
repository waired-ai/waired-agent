package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
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
			got := residencyLine(residencyView{
				InMemory: tc.resident, Model: tc.model, Until: tc.until, Indefinite: tc.indefinite,
			})
			if got != tc.want {
				t.Errorf("residencyLine = %q, want %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("residencyLine = %q, must not contain %q", got, tc.notWant)
			}
		})
	}
}

// TestResidencyLine_MismatchAndStaleness covers what waired-agent#837 added.
//
// The mismatch clause is not decoration: under one-model-resident
// (docs/decisions/20260811/2340-one-model-resident-at-a-time.md) a request
// for another model evicts the one the router points at, so "yes" alone is
// true about memory and false about the next request. And nil — the agent
// could not resolve which tag this computer serves — must render as no
// claim, because a wrong mismatch would appear on a warm machine.
func TestResidencyLine_MismatchAndStaleness(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name    string
		view    residencyView
		want    string
		notWant string
	}{
		{
			name: "the wrong model is loaded",
			view: residencyView{InMemory: true, Model: "other:q4", Indefinite: true, IsActive: &no},
			want: "other:q4 (kept until unloaded; not the model this computer serves)",
		},
		{
			name: "the right model is loaded",
			view: residencyView{InMemory: true, Model: "m:q4", Indefinite: true, IsActive: &yes},
			want: "m:q4 (kept until unloaded)",
		},
		{
			name:    "no claim about which model this computer serves",
			view:    residencyView{InMemory: true, Model: "m:q4", Indefinite: true},
			want:    "m:q4 (kept until unloaded)",
			notWant: "not the model",
		},
		{
			name: "the wrong model, with an expiry",
			view: residencyView{InMemory: true, Model: "other:q4", Until: "2026-08-20T13:11:43Z", IsActive: &no},
			want: "other:q4 (until 2026-08-20T13:11:43Z; not the model this computer serves)",
		},
		{
			name: "a reading whose probe missed a tick says so",
			view: residencyView{StaleFor: 47 * time.Second},
			want: "no (the next request reloads it) — last checked 47s ago",
		},
		{
			name:    "a fresh reading says nothing about its age",
			view:    residencyView{InMemory: true, Model: "m:q4"},
			want:    "m:q4",
			notWant: "last checked",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := residencyLine(tc.view)
			if got != tc.want {
				t.Errorf("residencyLine = %q, want %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("residencyLine = %q, must not contain %q", got, tc.notWant)
			}
		})
	}
}

// TestResidencyStaleFor: an age is shown only once the probe that produces
// the reading has missed a tick. Anything shorter is just time passing, and
// putting a number on every healthy status is noise.
func TestResidencyStaleFor(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	if got := residencyStaleFor("", now); got != 0 {
		t.Errorf("no timestamp (an agent that predates the field) = %v, want 0", got)
	}
	if got := residencyStaleFor("not a time", now); got != 0 {
		t.Errorf("unparseable = %v, want 0", got)
	}
	if got := residencyStaleFor(stamp(state.HeartbeatInterval), now); got != 0 {
		t.Errorf("one cadence old = %v, want 0 (that is a normal reading)", got)
	}
	if got := residencyStaleFor(stamp(47*time.Second), now); got != 47*time.Second {
		t.Errorf("47s old = %v, want 47s", got)
	}
}

func TestRequestCount(t *testing.T) {
	for n, want := range map[int]string{0: "0 requests", 1: "1 request", 2: "2 requests"} {
		if got := requestCount(n); got != want {
			t.Errorf("requestCount(%d) = %q, want %q", n, got, want)
		}
	}
}
