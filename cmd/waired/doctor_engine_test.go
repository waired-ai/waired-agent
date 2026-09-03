package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// TestEngineRepair is the gate on `waired doctor --fix`'s engine arm.
//
// PRODUCT CONTRACT (waired-agent#1170) for the repairable case: the browser
// wizard sends an operator whose engine will not start to `sudo waired doctor
// --fix`, and until this existed that command ran `waired link all`, printed
// Done, and left the engine exactly as it was.
//
// The rest are the states a repair must keep its hands off. Two of them look
// identical on EngineReady alone, which is why the gate is the REASON: a hard
// stop and a boot in progress both report an engine that is not running and
// nothing wrong with it.
func TestEngineRepair(t *testing.T) {
	tests := []struct {
		name  string
		state management.AgentState
		want  engineDoctor
	}{
		{
			name:  "a running engine needs nothing",
			state: management.AgentState{EngineReady: true, LocalInferenceState: "enabled"},
			want:  engineDoctor{},
		},
		{
			name: "an engine that will not start, with the daemon's reason",
			state: management.AgentState{
				LocalInferenceState: "enabled",
				EngineFailureReason: "no vLLM-capable model selected",
			},
			want: engineDoctor{Repair: true, Reason: "no vLLM-capable model selected"},
		},
		{
			// `waired inference engine stop` frees the memory and records no
			// fault. Starting it again would undo an operator action nobody
			// asked us to undo — and a boot still in progress reads the same
			// and wants the same answer: wait.
			name:  "an engine somebody stopped is not a fault",
			state: management.AgentState{LocalInferenceState: "enabled"},
			want:  engineDoctor{},
		},
		{
			// A setting, not a fault (#465). engineFinding says so at
			// StatusOK, and an engine start would fight the setting.
			name: "local inference turned off is a setting",
			state: management.AgentState{
				LocalInferenceState: localInferenceDisabled,
				EngineFailureReason: "engine stopped: local inference is off",
			},
			want: engineDoctor{},
		},
		{
			// `waired resume` is the finding's own advice and the only thing
			// that helps; starting the engine would not lift the pause.
			name: "a paused device wants resume, not a start",
			state: management.AgentState{
				Paused:              true,
				LocalInferenceState: "enabled",
				EngineFailureReason: "engine failed to start 4 times",
			},
			want: engineDoctor{},
		},
		{
			// Older daemons send no reason at all (the field postdates
			// waired-agent#1069). Offering a repair we cannot explain, on a
			// build that cannot tell us what is wrong, is a worse answer than
			// the warning line.
			name:  "a daemon too old to say why is left to the warning line",
			state: management.AgentState{LocalInferenceState: "enabled", EngineName: "ollama"},
			want:  engineDoctor{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineRepair(tc.state); got != tc.want {
				t.Errorf("engineRepair(%+v) = %+v, want %+v", tc.state, got, tc.want)
			}
		})
	}
}

// TestRepairEngine_ReportsWhatHappened: the repair prints the outcome rather
// than claiming success, which is the failure mode it was written to end.
func TestRepairEngine_ReportsWhatHappened(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"engine_power":"starting","engine_managed":true}`))
		}))
		defer srv.Close()

		var sb strings.Builder
		if err := repairEngine(srv.URL, "no vLLM-capable model selected", &sb); err != nil {
			t.Fatalf("repairEngine: %v", err)
		}
		if path != "/waired/v1/inference/engine/start" {
			t.Errorf("posted to %q, want the engine start endpoint", path)
		}
		out := sb.String()
		if !strings.Contains(out, "no vLLM-capable model selected") {
			t.Errorf("output = %q, want it to name why the engine is not running", out)
		}
		// "Asked", not "started": a cold vLLM engine takes minutes to load,
		// and the daemon's 200 means the request was accepted.
		if !strings.Contains(out, "Asked.") {
			t.Errorf("output = %q, want it to say the engine was asked, not that it started", out)
		}
	})

	t.Run("declined", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error_code": "engine_start_refused",
				"message":    "local inference is off on this computer",
			})
		}))
		defer srv.Close()

		var sb strings.Builder
		err := repairEngine(srv.URL, "", &sb)
		if err == nil {
			t.Fatal("repairEngine reported success on a refusal")
		}
		if !strings.Contains(err.Error(), "local inference is off on this computer") {
			t.Errorf("err = %v, want the daemon's own sentence", err)
		}
	})
}
