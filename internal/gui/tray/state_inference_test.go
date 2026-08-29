package tray

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// The soft-gate labels below are a PRODUCT CONTRACT, not a record of
// today's wording: the menu must not promise that this toggle turns the
// engine off. It only stops new requests — the process keeps running and
// the model stays in VRAM — and the previous "Disable inference engine"
// wording sent an rc7 tester looking for the memory it never freed
// (#316). Freeing memory is the separate power axis ("Stop inference
// engine"), asserted further down this file.
func TestUpdate_InferenceEnabled_Connected(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready",
		Active:         &management.ActiveSelection{Runtime: "ollama", ModelID: "llama3.2:3b"},
		DesiredState:   "enabled",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.InferenceToggleAction != "Pause local inference" {
		t.Errorf("InferenceToggleAction=%q, want the soft-gate pause label", got.InferenceToggleAction)
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

	if got.InferenceToggleAction != "Resume local inference" {
		t.Errorf("InferenceToggleAction=%q, want the soft-gate resume label", got.InferenceToggleAction)
	}
	if got.InferenceStateLabel != "Engine: disabled" {
		t.Errorf("InferenceStateLabel=%q, want Engine: disabled", got.InferenceStateLabel)
	}
	if got.ActiveModelLabel != "Model: llama3.2:3b" {
		t.Errorf("ActiveModelLabel=%q, want Model: llama3.2:3b (still visible while disabled)", got.ActiveModelLabel)
	}
}

// TestUpdate_InferenceOffWithNoEngineOffersTheWayIn is the app half of
// #465. A computer below the recommended spec starts with local
// inference off AND no engine installed — and the no_engine arm hid the
// toggle for every DesiredState, so the one surface a desktop user
// actually looks at offered no way to turn it on at all.
//
// The label is not "Resume": nothing ran here to resume.
func TestUpdate_InferenceOffWithNoEngineOffersTheWayIn(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "no_engine",
		DesiredState:   "disabled",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.InferenceToggleAction != "Run models on this computer" {
		t.Errorf("InferenceToggleAction=%q, want the never-set-up label", got.InferenceToggleAction)
	}
}

// The other no_engine case is unchanged: local inference is already on,
// so there is nothing for the toggle to do and the row would bait a
// click. Record of today's behaviour.
func TestUpdate_InferenceNoEngine(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "no_engine",
		DesiredState:   "enabled",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})

	if got.InferenceToggleAction != "" {
		t.Errorf("InferenceToggleAction=%q, want empty (already on, no engine to pause)", got.InferenceToggleAction)
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

func TestUpdate_EnginePower_UnmanagedDisabledRow(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "ready", DesiredState: "enabled",
		Active:      &management.ActiveSelection{ModelID: "llama3.2:3b"},
		EnginePower: "running", EngineManaged: false,
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if got.EngineToggleAction != "Engine not managed" {
		t.Errorf("EngineToggleAction=%q, want the not-managed label", got.EngineToggleAction)
	}
	if got.EngineToggleEnabled {
		t.Error("EngineToggleEnabled=true for an unmanaged engine, want false (greyed out)")
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

	t.Run("adopted mode suffixes the state label", func(t *testing.T) {
		inf := &management.InferenceStatus{
			SubsystemState: "ready",
			DesiredState:   "enabled",
			Runtimes: map[string]management.RuntimeStatus{
				"ollama": {Name: "ollama", Installed: true, Mode: "adopted", LiveVersion: "0.24.0"},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if got.InferenceStateLabel != "Engine: ready (adopted)" {
			t.Errorf("InferenceStateLabel=%q, want Engine: ready (adopted)", got.InferenceStateLabel)
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

// PRODUCT CONTRACT (waired-agent#1026): the tray reports the engine this
// host serves with, not the engine named ollama.
//
// The block that renders the engine warning read inf.Runtimes["ollama"]
// directly, so on a vLLM host it was dead code: the version warning and the
// reason the engine failed to start never reached the one surface a desktop
// user has. The active runtime is the authority because it moves when the
// host adopts an engine after boot (waired-agent#339).
func TestUpdate_InferenceWarningFollowsTheServingEngine(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	const busyPort = "another program is already listening on 127.0.0.1:9479, " +
		"the port the inference engine was told to use — " +
		"set inference.vllm_port in agent.json to a free port"

	t.Run("a serving vLLM host shows its own warning, not ollama's", func(t *testing.T) {
		inf := &management.InferenceStatus{
			SubsystemState: "ready",
			DesiredState:   "enabled",
			Active:         &management.ActiveSelection{Runtime: "vllm", ModelID: "gpt-oss-20b"},
			Runtimes: map[string]management.RuntimeStatus{
				"ollama": {Name: "ollama", Installed: true, VersionWarning: "an idle ollama's complaint"},
				"vllm":   {Name: "vllm", Installed: true, VersionWarning: "the venv is older than this build expects"},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if want := "⚠ the venv is older than this build expects"; got.EngineWarningLabel != want {
			t.Errorf("EngineWarningLabel=%q, want %q", got.EngineWarningLabel, want)
		}
	})

	t.Run("an engine that never started still names its reason", func(t *testing.T) {
		// No Active at all — the state a host whose engine cannot bind its
		// port never leaves, and the one the old read answered "" for.
		inf := &management.InferenceStatus{
			SubsystemState: "engine_failed",
			DesiredState:   "enabled",
			Runtimes: map[string]management.RuntimeStatus{
				"vllm": {Name: "vllm", Installed: true, State: "failed", LastError: busyPort},
			},
		}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
		if !strings.Contains(got.EngineWarningLabel, "inference.vllm_port") {
			t.Errorf("EngineWarningLabel=%q, want it to carry the engine's reason", got.EngineWarningLabel)
		}
	})
}

// PRODUCT CONTRACT (waired-agent#1111): a give-up latch that a Stop has
// since overwritten is still an engine reporting a failure.
//
// Stop() assigns the whole Health struct with no give-up guard
// (internal/runtime/ollama.go:1613-1633), so a model switch, a reconcile
// bounce or a park after the give-up puts the row on the wire as
// state:"stopped" + failure_latched + last_error. servingRuntime keyed on
// the state alone, so its "exactly one failed row" arm matched ZERO rows
// rather than one and fell through to Runtimes["ollama"] — which on a vLLM
// host is a registered, never-started adapter with an empty LastError. The
// top-level row still drew the fault glyph, so the desktop user got
// "⚠ Engine: engine failed" and no cause anywhere in the menu.
func TestUpdate_InferenceWarningReadsTheLatchAStopOutlives(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	const gaveUp = "engine repeatedly crashed; not retrying — " +
		"another program is already listening on 127.0.0.1:9479"

	// The shape a latched-then-stopped vLLM host puts on the wire: no
	// Active, no row reading "failed", and the reason on the stopped row.
	inf := &management.InferenceStatus{
		SubsystemState: "engine_failed",
		DesiredState:   "enabled",
		Runtimes: map[string]management.RuntimeStatus{
			"ollama": {Name: "ollama", Installed: true, State: "not_started"},
			"vllm": {
				Name: "vllm", Installed: true, State: "stopped",
				FailureLatched: true, LastError: gaveUp,
			},
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if !strings.Contains(got.EngineWarningLabel, "not retrying") {
		t.Fatalf("EngineWarningLabel=%q, want the latched engine's reason;"+
			" a fault glyph with no cause is the whole defect", got.EngineWarningLabel)
	}
}

// PRODUCT CONTRACT (waired-agent#1111): the reason the engine is not
// running outranks the note that it is the wrong version.
//
// Not a fresh judgement — `waired status` already collects them this way
// round (cmd/waired/main.go:588-590 before :626-631), as does
// `waired runtimes ls`. Those surfaces append both; the tray has one row,
// and it was the only place picking the less urgent of the two.
func TestUpdate_TheReasonOutranksTheVersionNote(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	inf := &management.InferenceStatus{
		SubsystemState: "engine_failed",
		DesiredState:   "enabled",
		Runtimes: map[string]management.RuntimeStatus{
			"vllm": {
				Name: "vllm", Installed: true, State: "failed",
				VersionWarning: "the venv is older than this build expects",
				LastError:      "the engine could not bind 127.0.0.1:9479",
			},
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if want := "⚠ the engine could not bind 127.0.0.1:9479"; got.EngineWarningLabel != want {
		t.Errorf("EngineWarningLabel=%q, want %q — a wrong-version venv that then\n"+
			"failed to start showed the version nag and swallowed the start failure",
			got.EngineWarningLabel, want)
	}
}

// A menu row is a one-line surface, and last_error is not one line: it
// carries up to 4 KiB of engine.log tail (waired-agent#1137). Unclamped it
// went through escapeMenuLabel, which doubles every underscore in it.
func TestUpdate_TheEngineWarningIsOneMenuRow(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "active"}
	tail := "engine gave up: could not bind :9479\n" + strings.Repeat("llama_model_load: x\n", 300)
	inf := &management.InferenceStatus{
		SubsystemState: "engine_failed",
		DesiredState:   "enabled",
		Runtimes: map[string]management.RuntimeStatus{
			"vllm": {Name: "vllm", Installed: true, State: "failed", LastError: tail},
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, Inference: inf})
	if strings.ContainsRune(got.EngineWarningLabel, '\n') {
		t.Errorf("EngineWarningLabel spans lines: %q", got.EngineWarningLabel)
	}
	if n := len([]rune(got.EngineWarningLabel)); n > menuLabelMax+2 { // +2 for the glyph and its space
		t.Errorf("EngineWarningLabel is %d runes, want at most %d", n, menuLabelMax+2)
	}
	if !strings.Contains(got.EngineWarningLabel, "could not bind") {
		t.Errorf("EngineWarningLabel=%q, want the first line kept", got.EngineWarningLabel)
	}
}

// firstLine keeps a reason that already fits exactly as it is — an ellipsis
// on a short sentence would say the row is hiding something when it is not.
func TestFirstLine_LeavesAShortReasonAlone(t *testing.T) {
	const short = "the engine could not bind 127.0.0.1:9479"
	if got := firstLine(short); got != short {
		t.Errorf("firstLine(%q) = %q", short, got)
	}
	if got := firstLine(strings.Repeat("x", menuLabelMax+50)); !strings.HasSuffix(got, "…") {
		t.Errorf("a clamped line should say so: %q", got)
	}
}

// The tests above build management.RuntimeStatus directly, which cannot
// see a JSON tag that does not match what the daemon writes. This one
// starts from the bytes.
//
// The payload is the shape cmd/waired-agent/inference.go's runtimeStatusFor
// produces for a latched-then-stopped vLLM host: state "stopped",
// failure_latched true, last_error back-filled from the latch
// (inference.go:3112-3120). Decoding it is the half a fixture built in Go
// freezes out (waired-agent#1111).
func TestUpdate_TheLatchedReasonSurvivesTheWire(t *testing.T) {
	const body = `{
	  "subsystem_state": "engine_failed",
	  "desired_state": "enabled",
	  "runtimes": {
	    "ollama": {"name":"ollama","installed":true,"state":"not_started"},
	    "vllm": {
	      "name":"vllm","installed":true,"state":"stopped",
	      "failure_latched":true,
	      "last_error":"engine repeatedly crashed; not retrying — could not bind 127.0.0.1:9479"
	    }
	  }
	}`
	var inf management.InferenceStatus
	if err := json.Unmarshal([]byte(body), &inf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r := inf.Runtimes["vllm"]; !r.FailureLatched {
		t.Fatalf("failure_latched did not decode; the wire name and the struct tag disagree")
	}
	got := Update(Snapshot{
		Health:    HealthOnline,
		Identity:  &management.IdentityView{Enrolled: true, AccountEmail: "a@b"},
		Status:    &management.Status{Phase: "active"},
		Inference: &inf,
	})
	if !strings.Contains(got.EngineWarningLabel, "could not bind") {
		t.Errorf("EngineWarningLabel=%q, want the latched engine's reason", got.EngineWarningLabel)
	}
}

// The clamp is calibrated on the longest reason the product writes for this
// row, so tightening it would cut the remediation — the half a person acts
// on — while still measuring well on the cause. This pins the whole
// sentence (waired-agent#1137).
func TestUpdate_TheBusyPortRemediationFitsOnTheRow(t *testing.T) {
	const busyPort = "engine repeatedly crashed; not retrying — another program is " +
		"already listening on 127.0.0.1:9479, the port the inference engine was " +
		"told to use — set inference.vllm_port in agent.json to a free port"
	inf := &management.InferenceStatus{
		SubsystemState: "engine_failed",
		DesiredState:   "enabled",
		Runtimes: map[string]management.RuntimeStatus{
			"vllm": {Name: "vllm", Installed: true, State: "stopped",
				FailureLatched: true, LastError: busyPort},
		},
	}
	got := Update(Snapshot{
		Health:    HealthOnline,
		Identity:  &management.IdentityView{Enrolled: true, AccountEmail: "a@b"},
		Status:    &management.Status{Phase: "active"},
		Inference: inf,
	})
	if want := "⚠ " + busyPort; got.EngineWarningLabel != want {
		t.Errorf("the remediation did not survive the clamp:\ngot  %q\nwant %q",
			got.EngineWarningLabel, want)
	}
}
