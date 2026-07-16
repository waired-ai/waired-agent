package tray

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// waired#811: the tray icon lights to IconBusy while the local engine is
// serving requests (Observability.Agent.Inflight > 0), but only ever as a
// promotion of a plain IconConnected — it must never mask an
// error/degraded/disconnected icon.

func obsWithInflight(n int) *management.ObservabilityState {
	return &management.ObservabilityState{Agent: management.AgentState{Inflight: n}}
}

func TestUpdate_InferenceActivity_Promotes(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		obs  *management.ObservabilityState
		want IconState
	}{
		{"inflight>0 promotes to busy", obsWithInflight(1), IconBusy},
		{"multiple inflight still busy", obsWithInflight(5), IconBusy},
		{"inflight==0 stays connected", obsWithInflight(0), IconConnected},
		{"nil observability stays connected", nil, IconConnected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Update(connectedSnapshot(now, nil, tc.obs))
			if got.Icon != tc.want {
				t.Errorf("Icon=%d, want %d", got.Icon, tc.want)
			}
		})
	}
}

// Busy is the lowest-priority overlay: a recent fallback promotes the
// connected icon to IconDegraded, and that must survive even when the
// engine is simultaneously busy.
func TestUpdate_InferenceActivity_DoesNotMaskDegraded(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := []FallbackEntry{{
		TS: now.Add(-1 * time.Minute), From: "a", To: "b", Reason: "engine_not_ready",
	}}
	got := Update(connectedSnapshot(now, fbs, obsWithInflight(3)))
	if got.Icon != IconDegraded {
		t.Errorf("Icon=%d, want IconDegraded (degraded must win over busy)", got.Icon)
	}
}

// A non-connected network state (here: paused → IconDisconnected) must not
// be flipped to busy even if the engine reports in-flight work.
func TestUpdate_InferenceActivity_DoesNotOverrideDisconnected(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Health:        HealthOnline,
		Identity:      &management.IdentityView{Enrolled: true, AccountEmail: "a@b.c", DeviceID: "d"},
		Status:        &management.Status{Phase: "paused"},
		Observability: obsWithInflight(2),
		Now:           now,
	}
	got := Update(snap)
	if got.Icon != IconDisconnected {
		t.Errorf("Icon=%d, want IconDisconnected (busy must not override a paused/disconnected state)", got.Icon)
	}
}

// The daemon-down branch returns IconError before any inference-activity
// consideration; a stray Inflight must not turn the down state green.
func TestUpdate_InferenceActivity_DoesNotOverrideError(t *testing.T) {
	got := Update(Snapshot{Health: HealthOffline, Observability: obsWithInflight(9)})
	if got.Icon != IconError {
		t.Errorf("Icon=%d, want IconError (busy must not override daemon-down)", got.Icon)
	}
}
