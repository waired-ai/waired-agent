package tray

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

func availUpdate() *management.UpdateStatus {
	return &management.UpdateStatus{
		Phase:          management.UpdatePhaseAvailable,
		Available:      true,
		CurrentVersion: "1.2.3",
		LatestVersion:  "1.4.0",
		ApplyMethod:    "apt",
		NotifyEnabled:  true,
	}
}

// The update rows moved in waired-agent#1229: the banner is published by
// the daemon and rendered from MenuModel.Notices (see state_notices_test.go
// and internal/notice), so what these pin now is the click data the tray
// still resolves locally, and the toggle that used to hang beneath the
// banner.

func TestUpdateAvailable_ProjectedWhenConnected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: availUpdate()})
	if !got.UpdateAvailable {
		t.Fatal("expected UpdateAvailable=true when connected and an update is available")
	}
	if got.UpdateVersion != "1.4.0" || got.UpdateMethod != "apt" {
		t.Fatalf("click fields not projected: %+v", got)
	}
}

func TestUpdateAvailable_ProjectedWhenDisconnected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "paused"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: availUpdate()})
	if !got.UpdateAvailable {
		t.Fatal("an update stays available while paused/disconnected")
	}
}

func TestUpdateAvailable_ProjectedWhenNotSignedIn(t *testing.T) {
	// The check is identity-independent — offer it before sign-in too.
	got := Update(Snapshot{Health: HealthOnline, Identity: nil, Update: availUpdate()})
	if got.Kind != MenuNotSignedIn {
		t.Fatalf("expected not-signed-in, got kind %d", got.Kind)
	}
	if !got.UpdateAvailable {
		t.Fatal("an update should be offered even before sign-in")
	}
}

func TestUpdateAvailable_NotProjectedWhenDaemonDown(t *testing.T) {
	// Daemon-down returns early with its own model — the daemon is the
	// source of the check, and of the notice.
	got := Update(Snapshot{Health: HealthOffline, Update: availUpdate()})
	if got.UpdateAvailable {
		t.Fatal("no update is claimable when the daemon is down")
	}
}

func TestUpdateAvailable_NotProjectedWhenCurrentOrAbsent(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	cases := map[string]*management.UpdateStatus{
		"nil (old daemon)":         nil,
		"up to date":               {Phase: management.UpdatePhaseIdle, Available: false, CurrentVersion: "1.4.0", LatestVersion: "1.4.0"},
		"check errored":            {Phase: management.UpdatePhaseError, Error: "github unreachable", CurrentVersion: "1.2.3"},
		"available but no version": {Phase: management.UpdatePhaseAvailable, Available: true},
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: up})
			if got.UpdateAvailable {
				t.Fatalf("no update should be claimable for %q", name)
			}
		})
	}
}

// TestUpdateNotifyToggle_DoesNotWaitForAnUpdate
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05; waired-agent#1229). This
// INVERTS the assertion this file used to carry ("no banner ⇒ no toggle
// row either"): the toggle hung beneath the banner and appeared only
// while an update was pending, so the preference could be set only while
// the person was already being interrupted by it. It is in Settings now,
// and a daemon that reports its update status at all reports the
// preference.
func TestUpdateNotifyToggle_DoesNotWaitForAnUpdate(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	current := &management.UpdateStatus{
		Phase: management.UpdatePhaseIdle, Available: false,
		CurrentVersion: "1.4.0", LatestVersion: "1.4.0", NotifyEnabled: true,
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: current})
	if got.UpdateNotifyAction == "" {
		t.Fatal("an up-to-date host should still be able to set the preference")
	}

	// A daemon predating the settings API sends no status, and then there
	// is no preference to show.
	got = Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: nil})
	if got.UpdateNotifyAction != "" {
		t.Fatalf("old daemon should render no toggle, got %q", got.UpdateNotifyAction)
	}
}

func TestUpdateNotifyToggle_Projection(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}

	on := availUpdate() // NotifyEnabled = true
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: on})
	if !got.UpdateNotifyEnabled || !strings.HasPrefix(got.UpdateNotifyAction, "✓") {
		t.Fatalf("prompts on should render a checked toggle, got %q (enabled=%v)", got.UpdateNotifyAction, got.UpdateNotifyEnabled)
	}

	off := availUpdate()
	off.NotifyEnabled = false
	got = Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: off})
	if got.UpdateNotifyEnabled {
		t.Fatalf("prompts off should project UpdateNotifyEnabled=false, got %+v", got)
	}
	if got.UpdateNotifyAction == "" || strings.HasPrefix(got.UpdateNotifyAction, "✓") {
		t.Fatalf("prompts off should render an unchecked toggle, got %q", got.UpdateNotifyAction)
	}
}

func TestShouldNotifyUpdate(t *testing.T) {
	base := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	const renotify = 24 * time.Hour

	t.Run("new version fires", func(t *testing.T) {
		if !shouldNotifyUpdate(availUpdate(), "", time.Time{}, base, renotify) {
			t.Fatal("a newly-seen version should fire")
		}
	})
	t.Run("same version within interval is silent", func(t *testing.T) {
		if shouldNotifyUpdate(availUpdate(), "1.4.0", base, base.Add(time.Hour), renotify) {
			t.Fatal("same version within the re-reminder window must not fire")
		}
	})
	t.Run("same version after interval re-reminds", func(t *testing.T) {
		if !shouldNotifyUpdate(availUpdate(), "1.4.0", base, base.Add(25*time.Hour), renotify) {
			t.Fatal("same version past the re-reminder window should fire")
		}
	})
	t.Run("disabled prompt is silent", func(t *testing.T) {
		off := availUpdate()
		off.NotifyEnabled = false
		if shouldNotifyUpdate(off, "", time.Time{}, base, renotify) {
			t.Fatal("toast must be suppressed when prompts are disabled")
		}
	})
	t.Run("not available is silent", func(t *testing.T) {
		up := &management.UpdateStatus{Phase: management.UpdatePhaseIdle, Available: false, NotifyEnabled: true}
		if shouldNotifyUpdate(up, "", time.Time{}, base, renotify) {
			t.Fatal("no update ⇒ no toast")
		}
	})
	t.Run("nil status is silent", func(t *testing.T) {
		if shouldNotifyUpdate(nil, "", time.Time{}, base, renotify) {
			t.Fatal("nil status ⇒ no toast")
		}
	})
}
