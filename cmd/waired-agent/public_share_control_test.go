package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// waired#1297. Public sharing is the control plane's setting; this
// controller holds the live value it was told and stops what is running
// when the machine stops lending itself out. PRODUCT CONTRACT from the
// owner ruling on that issue.

// Serving strangers stays strictly opt-in: a computer that comes up
// before it has heard from the control plane serves nobody. Nothing is
// persisted, so there is no remembered yes to act on.
func TestPublicShareController_BootsOff(t *testing.T) {
	pc := newPublicShareController(nil)
	if pc.IsPublic() {
		t.Error("a freshly built controller is serving guests")
	}
	if !pc.IsPublicShareDenied() {
		t.Error("IsPublicShareDenied disagrees with IsPublic")
	}
	if pc.State() != state.SharingOff {
		t.Errorf("State = %q, want %q", pc.State(), state.SharingOff)
	}
}

// The echo is adopted in both directions and unconditionally. The
// pending window and the echo-true latch that used to guard a local
// writer went with the writer: a device that never asserts a value has
// nothing to protect from the control plane's.
func TestPublicShareController_AdoptsTheEchoBothWays(t *testing.T) {
	pc := newPublicShareController(nil)
	var aborts int
	pc.SetOnDisable(func() { aborts++ })

	pc.ReconcileRemote(true, 3)
	if !pc.IsPublic() || pc.MaxClients() != 3 {
		t.Fatalf("enable not adopted: public=%v max=%d", pc.IsPublic(), pc.MaxClients())
	}
	if aborts != 0 {
		t.Errorf("turning sharing ON aborted %d requests", aborts)
	}

	// A repeat is a no-op: the frame carries the same value every time,
	// and re-firing the abort would cut guests on every poll.
	pc.ReconcileRemote(true, 3)
	if aborts != 0 {
		t.Errorf("a repeated frame aborted %d requests", aborts)
	}

	pc.ReconcileRemote(false, 3)
	if pc.IsPublic() {
		t.Error("the OFF echo was not adopted")
	}
	if aborts != 1 {
		t.Errorf("the kill switch fired %d times, want 1", aborts)
	}
}

// The ceiling arrives with the setting. A negative value is ignored
// rather than stored: it is not a smaller ceiling, it is no answer.
func TestPublicShareController_MaxClients(t *testing.T) {
	pc := newPublicShareController(nil)
	pc.ReconcileRemote(true, 5)
	pc.ReconcileRemote(true, -1)
	if pc.MaxClients() != 5 {
		t.Errorf("MaxClients = %d, want the last real value 5", pc.MaxClients())
	}
}

// StopServing cuts the guests but leaves the setting alone. Clearing it
// here would leave public serving off after the computer came back,
// because the console's value did not change and so nothing would ever
// re-assert it.
func TestPublicShareController_StopServingKeepsTheSetting(t *testing.T) {
	pc := newPublicShareController(nil)
	var aborts int
	pc.SetOnDisable(func() { aborts++ })
	pc.ReconcileRemote(true, 2)

	pc.StopServing()
	if aborts != 1 {
		t.Errorf("StopServing aborted %d times, want 1", aborts)
	}
	if !pc.IsPublic() {
		t.Error("StopServing cleared the control plane's setting")
	}

	// And it is quiet when there was nothing to stop.
	pc2 := newPublicShareController(nil)
	pc2.SetOnDisable(func() { t.Error("aborted with nothing being served") })
	pc2.StopServing()
}
