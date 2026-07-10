package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestUpdate_InferenceEnabled_Connected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		Active:         &management.ActiveSelection{Runtime: "ollama", ModelID: "llama3.2:3b"},
		DesiredState:   "enabled",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.InferenceToggleAction != "Disable inference engine" {
		t.Errorf("InferenceToggleAction=%q, want Disable inference engine", got.InferenceToggleAction)
	}
	if got.InferenceStateLabel != "Engine: ready" {
		t.Errorf("InferenceStateLabel=%q, want Engine: ready", got.InferenceStateLabel)
	}
	if got.ActiveModelLabel != "Model: llama3.2:3b" {
		t.Errorf("ActiveModelLabel=%q, want Model: llama3.2:3b", got.ActiveModelLabel)
	}
}

func TestUpdate_InferenceDisabled_Connected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "disabled",
		Active:         &management.ActiveSelection{Runtime: "ollama", ModelID: "llama3.2:3b"},
		DesiredState:   "disabled",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.InferenceToggleAction != "Enable inference engine" {
		t.Errorf("InferenceToggleAction=%q, want Enable inference engine", got.InferenceToggleAction)
	}
	if got.InferenceStateLabel != "Engine: disabled" {
		t.Errorf("InferenceStateLabel=%q, want Engine: disabled", got.InferenceStateLabel)
	}
	if got.ActiveModelLabel != "Model: llama3.2:3b" {
		t.Errorf("ActiveModelLabel=%q, want Model: llama3.2:3b (still visible while disabled)", got.ActiveModelLabel)
	}
}

func TestUpdate_InferenceNoEngine(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "no_engine",
		DesiredState:   "enabled",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.InferenceToggleAction != "" {
		t.Errorf("InferenceToggleAction=%q, want empty (no engine = no toggle)", got.InferenceToggleAction)
	}
	if got.InferenceStateLabel != "Engine: no engine" {
		t.Errorf("InferenceStateLabel=%q, want Engine: no engine", got.InferenceStateLabel)
	}
	if got.ActiveModelLabel != "" {
		t.Errorf("ActiveModelLabel=%q, want empty (no active model)", got.ActiveModelLabel)
	}
	if got.InstallEngineAction != "Install Ollama…" {
		t.Errorf("InstallEngineAction=%q, want Install Ollama… (#188)", got.InstallEngineAction)
	}
}

// TestUpdate_InstallEngineActionOnlyOnNoEngine ensures the "Install
// Ollama…" item is exclusive to the no_engine state — it must not leak
// into ready/disabled/loading menus where an engine already exists.
func TestUpdate_InstallEngineActionOnlyOnNoEngine(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	for _, state := range []string{"ready", "disabled", "loading", "awaiting_model", "pull_failed"} {
		inf := &management.InferenceStatus{
			SubsystemState: state,
			Active:         &management.ActiveSelection{ModelID: "llama3.2:3b"},
			DesiredState:   "enabled",
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if got.InstallEngineAction != "" {
			t.Errorf("state %q: InstallEngineAction=%q, want empty", state, got.InstallEngineAction)
		}
	}
}

// --- #186 hard engine power axis rendering ---

func TestUpdate_EnginePower_RunningShowsStop(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready", DesiredState: "enabled",
		Active:      &management.ActiveSelection{ModelID: "llama3.2:3b"},
		EnginePower: "running", EngineManaged: true,
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if got.EngineToggleAction != "Stop inference engine" {
		t.Errorf("EngineToggleAction=%q, want Stop inference engine", got.EngineToggleAction)
	}
	if !got.EngineToggleEnabled {
		t.Error("EngineToggleEnabled=false, want true for managed running engine")
	}
}

func TestUpdate_EnginePower_StoppedShowsStart(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "stopped", DesiredState: "enabled",
		Active:      &management.ActiveSelection{ModelID: "llama3.2:3b"},
		EnginePower: "stopped", EngineManaged: true,
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if got.EngineToggleAction != "Start inference engine" {
		t.Errorf("EngineToggleAction=%q, want Start inference engine", got.EngineToggleAction)
	}
	if got.InferenceStateLabel != "Engine: stopped (memory freed)" {
		t.Errorf("InferenceStateLabel=%q, want Engine: stopped (memory freed)", got.InferenceStateLabel)
	}
}

func TestUpdate_EnginePower_ReusedDisabledRow(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready", DesiredState: "enabled",
		Active:      &management.ActiveSelection{ModelID: "llama3.2:3b"},
		EnginePower: "running", EngineManaged: false,
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if got.EngineToggleAction != "Engine reused — not managed" {
		t.Errorf("EngineToggleAction=%q, want reused-not-managed label", got.EngineToggleAction)
	}
	if got.EngineToggleEnabled {
		t.Error("EngineToggleEnabled=true for reused engine, want false (greyed out)")
	}
}

func TestUpdate_EnginePower_EmptyHidesRow(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready", DesiredState: "enabled",
		Active: &management.ActiveSelection{ModelID: "llama3.2:3b"},
		// EnginePower empty: daemon predates engine control.
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if got.EngineToggleAction != "" {
		t.Errorf("EngineToggleAction=%q, want empty (hidden) when daemon lacks engine control", got.EngineToggleAction)
	}
}

func TestUpdate_InferenceNil_NoFields(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: nil})

	if got.InferenceToggleAction != "" || got.InferenceStateLabel != "" || got.ActiveModelLabel != "" {
		t.Errorf("Inference=nil should leave all inference fields empty, got %+v", got)
	}
}

// Inference UI must hide outside of Connected / Disconnected states so the
// menu doesn't bait the user into clicking a toggle while the daemon is
// unreachable, mid-transition, or not signed in.
func TestUpdate_InferenceHiddenWhenNotConnectedOrDisconnected(t *testing.T) {
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		Active:         &management.ActiveSelection{ModelID: "llama3.2:3b"},
		DesiredState:   "enabled",
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
		{
			"error",
			Snapshot{
				Health:    HealthOnline,
				Identity:  &management.IdentityView{Enrolled: true},
				Status:    &management.Status{Phase: "error"},
				Inference: inf,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Update(c.in)
			if got.InferenceToggleAction != "" || got.InferenceStateLabel != "" || got.ActiveModelLabel != "" {
				t.Errorf("inference fields must be empty for %s, got %+v", c.name, got)
			}
		})
	}
}

func TestUpdate_InferenceEngineProvenance(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}

	t.Run("borrowed mode suffixes the state label", func(t *testing.T) {
		inf := &management.InferenceStatus{
			SubsystemState: "ready",
			DesiredState:   "enabled",
			Runtimes: map[string]management.RuntimeStatus{
				"ollama": {Name: "ollama", Installed: true, Mode: "borrowed", LiveVersion: "0.24.0"},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if got.InferenceStateLabel != "Engine: ready (borrowed)" {
			t.Errorf("InferenceStateLabel=%q, want Engine: ready (borrowed)", got.InferenceStateLabel)
		}
		if got.EngineWarningLabel != "" {
			t.Errorf("EngineWarningLabel=%q, want empty (no warning)", got.EngineWarningLabel)
		}
	})

	t.Run("spawned mode keeps the plain label", func(t *testing.T) {
		inf := &management.InferenceStatus{
			SubsystemState: "ready",
			DesiredState:   "enabled",
			Runtimes: map[string]management.RuntimeStatus{
				"ollama": {Name: "ollama", Installed: true, Mode: "spawned", LiveVersion: "0.30.7"},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if got.InferenceStateLabel != "Engine: ready" {
			t.Errorf("InferenceStateLabel=%q, want Engine: ready", got.InferenceStateLabel)
		}
	})

	t.Run("version warning renders the warning row", func(t *testing.T) {
		inf := &management.InferenceStatus{
			SubsystemState: "ready",
			DesiredState:   "enabled",
			Runtimes: map[string]management.RuntimeStatus{
				"ollama": {Name: "ollama", Installed: true, Mode: "adopted",
					VersionWarning: "engine version 0.24.0 does not match the bundled pin 0.30.7"},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if want := "⚠ engine version 0.24.0 does not match the bundled pin 0.30.7"; got.EngineWarningLabel != want {
			t.Errorf("EngineWarningLabel=%q, want %q", got.EngineWarningLabel, want)
		}
	})

	t.Run("old daemon without provenance fields renders the pre-feature menu", func(t *testing.T) {
		inf := &management.InferenceStatus{
			SubsystemState: "ready",
			DesiredState:   "enabled",
			Runtimes: map[string]management.RuntimeStatus{
				"ollama": {Name: "ollama", Installed: true},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if got.InferenceStateLabel != "Engine: ready" {
			t.Errorf("InferenceStateLabel=%q, want Engine: ready", got.InferenceStateLabel)
		}
		if got.EngineWarningLabel != "" {
			t.Errorf("EngineWarningLabel=%q, want empty", got.EngineWarningLabel)
		}
	})
}
