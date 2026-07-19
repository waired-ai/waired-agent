package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestPublicShareController_DefaultOff pins the opt-in contract: with
// no persisted choice (empty initial) the controller boots OFF and the
// gate-facing IsPublicShareDenied reads true.
func TestPublicShareController_DefaultOff(t *testing.T) {
	pc := newPublicShareController(t.TempDir(), "", nil)
	if pc.IsPublic() || !pc.IsPublicShareDenied() {
		t.Fatalf("default state: IsPublic=%v IsPublicShareDenied=%v, want false/true", pc.IsPublic(), pc.IsPublicShareDenied())
	}
	current, desired := pc.State()
	if current != state.PublicShareOff || desired != state.PublicShareOff {
		t.Fatalf("State() = (%q, %q), want both %q", current, desired, state.PublicShareOff)
	}
}

// TestPublicShareController_EnablePersistsAndReboots: Enable flips the
// live flag and persists, so a controller rebuilt from the same state
// dir boots ON.
func TestPublicShareController_EnablePersistsAndReboots(t *testing.T) {
	dir := t.TempDir()
	pc := newPublicShareController(dir, "", nil)
	if err := pc.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !pc.IsPublic() {
		t.Fatal("IsPublic after Enable = false")
	}
	persisted, err := state.ReadDesiredPublicShare(dir)
	if err != nil || persisted != state.PublicShareOn {
		t.Fatalf("persisted = (%q, %v), want %q", persisted, err, state.PublicShareOn)
	}
	reboot := newPublicShareController(dir, persisted, nil)
	if !reboot.IsPublic() {
		t.Fatal("rebooted controller: IsPublic = false, want true")
	}
}

// TestPublicShareController_DisableFiresKillSwitch: the OFF transition
// runs the registered onDisable hook (wired to AbortPublicInFlight)
// and flips the deny flag before returning.
func TestPublicShareController_DisableFiresKillSwitch(t *testing.T) {
	pc := newPublicShareController(t.TempDir(), state.PublicShareOn, nil)
	fired := 0
	pc.SetOnDisable(func() {
		fired++
		if !pc.IsPublicShareDenied() {
			t.Error("onDisable ran before the deny flag flipped")
		}
	})
	if err := pc.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if fired != 1 {
		t.Fatalf("onDisable fired %d times, want 1", fired)
	}
	// Enable does not fire the hook.
	if err := pc.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if fired != 1 {
		t.Fatalf("onDisable fired %d times after Enable, want still 1", fired)
	}
}
