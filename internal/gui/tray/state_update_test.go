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

func TestUpdateBanner_ShownWhenConnected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: availUpdate()})
	if !got.ShowUpdate {
		t.Fatal("expected ShowUpdate=true when connected and an update is available")
	}
	if got.UpdateVersion != "1.4.0" || got.UpdateMethod != "apt" {
		t.Fatalf("banner fields not projected: %+v", got)
	}
	if !strings.Contains(got.UpdateLabel, "1.4.0") {
		t.Fatalf("label missing version: %q", got.UpdateLabel)
	}
}

func TestUpdateBanner_ShownWhenDisconnected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "paused"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Update: availUpdate()})
	if !got.ShowUpdate {
		t.Fatal("update banner should remain visible while paused/disconnected")
	}
}

func TestUpdateBanner_ShownWhenNotSignedIn(t *testing.T) {
	// The check is identity-independent — offer it before sign-in too.
	got := Update(Snapshot{Health: HealthOnline, Identity: nil, Update: availUpdate()})
	if got.Kind != MenuNotSignedIn {
		t.Fatalf("expected not-signed-in, got kind %d", got.Kind)
	}
	if !got.ShowUpdate {
		t.Fatal("update banner should show even before sign-in")
	}
}

func TestUpdateBanner_HiddenWhenDaemonDown(t *testing.T) {
	// Daemon-down returns early with its own model — the daemon is the
	// source of the check, so no banner is possible there.
	got := Update(Snapshot{Health: HealthOffline, Update: availUpdate()})
	if got.ShowUpdate {
		t.Fatal("banner must be hidden when the daemon is down")
	}
}

func TestUpdateBanner_HiddenWhenCurrentOrAbsent(t *testing.T) {
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
			if got.ShowUpdate {
				t.Fatalf("banner should be hidden for %q", name)
			}
			// No banner ⇒ no toggle row either.
			if got.UpdateNotifyAction != "" {
				t.Fatalf("notify toggle should be hidden for %q, got %q", name, got.UpdateNotifyAction)
			}
		})
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
