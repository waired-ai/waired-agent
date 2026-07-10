package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestUpdate_ShareShared_Connected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		DesiredState:   "enabled",
		ShareWithMesh:  "shared",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.ShareToggleAction != "Stop sharing engine to mesh" {
		t.Errorf("ShareToggleAction=%q, want Stop sharing engine to mesh", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "Sharing: enabled" {
		t.Errorf("ShareStateLabel=%q, want Sharing: enabled", got.ShareStateLabel)
	}
}

func TestUpdate_ShareNotShared_Connected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		DesiredState:   "enabled",
		ShareWithMesh:  "not_shared",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.ShareToggleAction != "Share engine to mesh" {
		t.Errorf("ShareToggleAction=%q, want Share engine to mesh", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "Sharing: disabled" {
		t.Errorf("ShareStateLabel=%q, want Sharing: disabled", got.ShareStateLabel)
	}
}

// Daemon predates the share API: share_with_mesh is empty. The toggle
// must stay hidden so the menu doesn't bait clicks on an endpoint that
// doesn't exist. Engine toggle still renders normally.
func TestUpdate_ShareHiddenWhenDaemonDoesntSupportIt(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		DesiredState:   "enabled",
		// No ShareWithMesh set.
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.ShareToggleAction != "" {
		t.Errorf("ShareToggleAction=%q, want empty when daemon predates share API", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "" {
		t.Errorf("ShareStateLabel=%q, want empty when daemon predates share API", got.ShareStateLabel)
	}
	// Engine toggle must still render.
	if got.InferenceToggleAction != "Disable inference engine" {
		t.Errorf("inference toggle must still render: %q", got.InferenceToggleAction)
	}
}

// SubsystemState=no_engine means there's no engine to share, so both
// the inference toggle AND the share toggle must hide.
func TestUpdate_ShareHiddenWhenNoEngine(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "no_engine",
		DesiredState:   "enabled",
		ShareWithMesh:  "shared", // daemon technically supports it
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.ShareToggleAction != "" {
		t.Errorf("ShareToggleAction=%q, want empty when no_engine (nothing to share)", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "" {
		t.Errorf("ShareStateLabel=%q, want empty when no_engine", got.ShareStateLabel)
	}
}

// Share remains visible even when the engine is soft-disabled by the
// operator. The user may want to flip share before re-enabling.
func TestUpdate_ShareVisibleWhenInferenceDisabled(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "disabled",
		DesiredState:   "disabled",
		ShareWithMesh:  "shared",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.ShareToggleAction != "Stop sharing engine to mesh" {
		t.Errorf("ShareToggleAction=%q, want Stop sharing engine to mesh", got.ShareToggleAction)
	}
	if got.InferenceToggleAction != "Enable inference engine" {
		t.Errorf("InferenceToggleAction=%q, want Enable inference engine", got.InferenceToggleAction)
	}
}

// Mid-transition / not-signed-in / daemon down all hide the share
// toggle alongside the inference toggle (same gating logic in Update).
func TestUpdate_ShareHiddenWhenNotConnectedOrDisconnected(t *testing.T) {
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		DesiredState:   "enabled",
		ShareWithMesh:  "shared",
	}
	cases := []struct {
		name string
		in   Snapshot
	}{
		{"daemon down", Snapshot{Health: HealthOffline, Inference: inf}},
		{"not signed in", Snapshot{Health: HealthOnline, Inference: inf}},
		{
			"connecting",
			Snapshot{
				Health:    HealthOnline,
				Identity:  &management.IdentityView{Enrolled: true},
				Status:    &management.Status{Phase: "starting"},
				Inference: inf,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Update(c.in)
			if got.ShareToggleAction != "" || got.ShareStateLabel != "" {
				t.Errorf("share fields must be empty for %s, got %+v", c.name, got)
			}
		})
	}
}
