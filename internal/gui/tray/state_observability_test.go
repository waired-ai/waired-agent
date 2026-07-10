package tray

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// connectedSnapshot returns a Snapshot.Now-stamped baseline that
// lands in the MenuConnected branch so the icon-override path is
// exercised cleanly by the observability tests.
func connectedSnapshot(now time.Time, fallbacks []FallbackEntry, obs *management.ObservabilityState) Snapshot {
	id := &management.IdentityView{
		Enrolled:     true,
		AccountEmail: "alice@example.com",
		DeviceID:     "dev-a",
		DeviceName:   "alice-laptop",
		OverlayIP:    "100.96.0.10",
	}
	return Snapshot{
		Health:          HealthOnline,
		Identity:        id,
		Status:          &management.Status{Phase: "active", PeerCount: 2},
		Observability:   obs,
		RecentFallbacks: fallbacks,
		Now:             now,
	}
}

func TestUpdate_Observability_NoData_HidesSubmenu(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	got := Update(connectedSnapshot(now, nil, nil))
	if got.ShowRecentActivity {
		t.Errorf("ShowRecentActivity=true with no Phase 9 inputs")
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon=%d, want IconConnected (no override)", got.Icon)
	}
	if got.HasRecentFallbackBadge {
		t.Errorf("HasRecentFallbackBadge=true with no fallbacks")
	}
}

func TestUpdate_Observability_OneRecentFallback_PromotesIcon(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := []FallbackEntry{{
		TS:     now.Add(-2 * time.Minute),
		From:   "peer_a",
		To:     "peer_b",
		Reason: "engine_not_ready",
		Model:  "qwen3:8b",
	}}
	got := Update(connectedSnapshot(now, fbs, &management.ObservabilityState{}))

	if !got.ShowRecentActivity {
		t.Fatalf("ShowRecentActivity=false, want true")
	}
	if got.Icon != IconDegraded {
		t.Errorf("Icon=%d, want IconDegraded", got.Icon)
	}
	if !got.HasRecentFallbackBadge {
		t.Errorf("HasRecentFallbackBadge=false, want true")
	}
	if len(got.RecentActivityEntries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got.RecentActivityEntries), got.RecentActivityEntries)
	}
	label := got.RecentActivityEntries[0].Label
	for _, want := range []string{"qwen3:8b", "peer_a", "peer_b", "engine_not_ready", "2m ago"} {
		if !strings.Contains(label, want) {
			t.Errorf("row label %q missing %q", label, want)
		}
	}
}

func TestUpdate_Observability_OldFallback_Dropped(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := []FallbackEntry{
		// Outside the 10-min window — must be dropped.
		{TS: now.Add(-11 * time.Minute), From: "peer_a", To: "peer_b", Reason: "x"},
		// Inside the window — must survive.
		{TS: now.Add(-3 * time.Minute), From: "peer_c", To: "peer_d", Reason: "y"},
	}
	got := Update(connectedSnapshot(now, fbs, nil))

	if len(got.RecentActivityEntries) != 1 {
		t.Fatalf("got %d entries, want 1 (old should be dropped): %+v",
			len(got.RecentActivityEntries), got.RecentActivityEntries)
	}
	if !strings.Contains(got.RecentActivityEntries[0].Label, "peer_c") {
		t.Errorf("surviving row should be the newer fallback (peer_c → peer_d); got %q",
			got.RecentActivityEntries[0].Label)
	}
}

func TestUpdate_Observability_RowCap(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := make([]FallbackEntry, 8) // more than MaxRecentActivity
	for i := range fbs {
		fbs[i] = FallbackEntry{
			TS:     now.Add(-time.Duration(i+1) * time.Minute),
			From:   "peer_a",
			To:     "peer_b",
			Reason: "engine_not_ready",
			Model:  "qwen3:8b",
		}
	}
	got := Update(connectedSnapshot(now, fbs, nil))

	if len(got.RecentActivityEntries) != MaxRecentActivity {
		t.Errorf("entries=%d, want %d", len(got.RecentActivityEntries), MaxRecentActivity)
	}
}

func TestUpdate_Observability_AllOld_NoOverride(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := []FallbackEntry{
		{TS: now.Add(-30 * time.Minute), From: "peer_a", To: "peer_b", Reason: "x"},
		{TS: now.Add(-45 * time.Minute), From: "peer_c", To: "peer_d", Reason: "y"},
	}
	got := Update(connectedSnapshot(now, fbs, nil))

	if got.ShowRecentActivity {
		t.Errorf("ShowRecentActivity=true with only out-of-window entries")
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon=%d, want IconConnected (no override when all entries are old)", got.Icon)
	}
}

func TestUpdate_Observability_DoesNotOverrideNonConnectedIcon(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	// Daemon up but identity unknown → MenuNotSignedIn / IconDisconnected.
	// Even with a recent fallback, the icon should NOT flip to degraded
	// because we never promote anything outside IconConnected.
	snap := Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: false},
		RecentFallbacks: []FallbackEntry{{
			TS: now.Add(-2 * time.Minute), From: "a", To: "b", Reason: "r",
		}},
		Now: now,
	}
	got := Update(snap)
	if got.Icon != IconDisconnected {
		t.Errorf("Icon=%d, want IconDisconnected (no override outside IconConnected)", got.Icon)
	}
}

func TestUpdate_Observability_ShortModel_StripsRegistryPrefix(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := []FallbackEntry{{
		TS: now.Add(-30 * time.Second), From: "a", To: "b",
		Reason: "r", Model: "ollama.io/qwen3:8b",
	}}
	got := Update(connectedSnapshot(now, fbs, nil))
	if len(got.RecentActivityEntries) != 1 {
		t.Fatalf("entries=%d, want 1", len(got.RecentActivityEntries))
	}
	label := got.RecentActivityEntries[0].Label
	if strings.Contains(label, "ollama.io/") {
		t.Errorf("registry prefix should be stripped: %q", label)
	}
	if !strings.Contains(label, "qwen3:8b") {
		t.Errorf("model name missing: %q", label)
	}
	// sub-minute is rendered as "<1m"
	if !strings.Contains(label, "<1m") {
		t.Errorf("age should render as <1m for sub-minute: %q", label)
	}
}
