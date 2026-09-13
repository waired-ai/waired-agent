package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Team Share (team share spec §6.2, waired#1374): the team switch is the
// control plane's setting, and this controller holds the live value and
// stops running team requests when it goes off or the machine stops
// lending itself out. PRODUCT CONTRACT from the spec and waired#1297
// (sharing settings live in the console; the node keeps one hard kill).

type fakeSharing struct{ on bool }

func (f fakeSharing) IsSharing() bool { return f.on }

// A computer that has not heard from the control plane serves no
// teammate.
func TestTeamShareController_BootsOff(t *testing.T) {
	tc := newTeamShareController(nil)
	if tc.IsTeamShared() {
		t.Error("a freshly built controller is serving teammates")
	}
	if tc.State() != state.SharingOff {
		t.Errorf("State = %q, want %q", tc.State(), state.SharingOff)
	}
}

// The map value is adopted both ways; only a real ON→OFF transition fires
// the kill switch, and a repeated frame fires nothing.
func TestTeamShareController_AdoptsTheMapBothWays(t *testing.T) {
	tc := newTeamShareController(nil)
	var aborts int
	tc.SetOnDisable(func() { aborts++ })

	tc.ReconcileRemote(true)
	tc.ReconcileRemote(true)
	if !tc.IsTeamShared() || aborts != 0 {
		t.Fatalf("enable: shared=%v aborts=%d, want true 0", tc.IsTeamShared(), aborts)
	}
	tc.ReconcileRemote(false)
	tc.ReconcileRemote(false)
	if tc.IsTeamShared() {
		t.Error("the OFF value was not adopted")
	}
	if aborts != 1 {
		t.Errorf("the kill switch fired %d times, want 1", aborts)
	}
}

// StopServing cuts running team requests but keeps the setting, so the
// computer resumes serving its team when it lends itself out again.
func TestTeamShareController_StopServingKeepsTheSetting(t *testing.T) {
	tc := newTeamShareController(nil)
	var aborts int
	tc.SetOnDisable(func() { aborts++ })
	tc.ReconcileRemote(true)
	tc.StopServing()
	if aborts != 1 {
		t.Errorf("StopServing aborted %d times, want 1", aborts)
	}
	if !tc.IsTeamShared() {
		t.Error("StopServing cleared the control plane's setting")
	}

	idle := newTeamShareController(nil)
	idle.SetOnDisable(func() { t.Error("aborted with nothing being served") })
	idle.StopServing()
}

// The gate's denial composes the console's switch with the machine's own
// hard kill: either one refuses a teammate, and neither erases the other.
func TestTeamShareDenied_ComposesTheTwoSwitches(t *testing.T) {
	for _, tc := range []struct {
		name          string
		team, sharing bool
		want          bool
	}{
		{"team on, machine sharing", true, true, false},
		{"team on, machine hard-killed", true, false, true},
		{"team off, machine sharing", false, true, true},
		{"team off, machine hard-killed", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := newTeamShareController(nil)
			ctl.ReconcileRemote(tc.team)
			if got := teamShareDenied(ctl, fakeSharing{on: tc.sharing})(); got != tc.want {
				t.Errorf("denied = %v, want %v", got, tc.want)
			}
		})
	}
	if teamShareDenied(nil, fakeSharing{on: true}) != nil {
		t.Error("no controller must leave the gate unwired (the server fails closed on nil)")
	}
	ctl := newTeamShareController(nil)
	ctl.ReconcileRemote(true)
	if teamShareDenied(ctl, nil)() {
		t.Error("with no sharing controller the console's ON must admit")
	}
}
