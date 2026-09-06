package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `waired inference engine start|stop|status` had no tests at all — including
// the line that printed "engine start ok." over a host where nothing changed
// (waired-agent#1170). These cover the two printers.

// engineMgmt is a fake Local Management API for the engine routes. It takes a
// per-path handler so a test says what the daemon answers and nothing else,
// and records what was posted (a fake that drops the request cannot fail the
// case where the CLI posts to the wrong endpoint — CLAUDE.md §Test discipline).
func engineMgmt(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (url string, posted *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			seen = append(seen, r.URL.Path)
		}
		h(w, r)
	})
	// A mux, not a bare handler: httptest listeners are not private to the
	// test that opened them, and a handler that answers every path makes a
	// stray request from another test indistinguishable from this one's.
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

// TestRunEngineTransition_AcceptedPrintsOK: the daemon accepted the request,
// so the CLI says so and shows the state it was handed.
func TestRunEngineTransition_AcceptedPrintsOK(t *testing.T) {
	url, posted := engineMgmt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"engine_power": "starting", "engine_managed": true,
		})
	})

	var err error
	out := captureStdout(t, func() { err = runEngineTransition(url, false, "engine start") })
	if err != nil {
		t.Fatalf("runEngineTransition: %v", err)
	}
	if !strings.Contains(out, "engine start ok.") {
		t.Errorf("stdout = %q, want the accepted line", out)
	}
	if len(*posted) != 1 || (*posted)[0] != "/waired/v1/inference/engine/start" {
		t.Errorf("posted %v, want one POST to the engine start endpoint", *posted)
	}
}

// TestRunEngineTransition_RefusalPrintsTheReason is the defect's CLI half.
//
// PRODUCT CONTRACT (waired-agent#1170): a start the daemon declined must
// print why, not "ok." The daemon reports these as 409 — a refusal it made on
// purpose is not a fault — and the sentence is the daemon's own.
//
// The parse was already here for the local-inference-off refusal
// (waired-agent#964); what changed is that the engine's own preconditions now
// reach it, so a host with no venv, no vLLM-capable model, or weights still
// arriving gets an answer instead of a claim.
func TestRunEngineTransition_RefusalPrintsTheReason(t *testing.T) {
	const reason = "no vLLM-capable model selected — set a preferred model that ships a" +
		" vllm/safetensors variant (e.g. gpt-oss-20b)"
	url, _ := engineMgmt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error_code": "engine_start_refused",
			"message":    "engine start refused: " + reason,
		})
	})

	var err error
	out := captureStdout(t, func() { err = runEngineTransition(url, false, "engine start") })
	if err == nil {
		t.Fatal("a refused start reported success")
	}
	if !strings.Contains(err.Error(), "no vLLM-capable model selected") {
		t.Errorf("err = %v, want the daemon's own sentence", err)
	}
	if strings.Contains(out, "ok.") {
		t.Errorf("stdout = %q — a refused start must not print an ok line", out)
	}
}

// TestRunEngineStatus_FailedCarriesTheRemediation pins the block
// waired-agent#964 added: "failed" on its own reads as a verdict with no next
// step. Here so the printer has a test at all — it had none.
func TestRunEngineStatus_FailedCarriesTheRemediation(t *testing.T) {
	url, _ := engineMgmt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subsystem_state": "engine_failed",
			"engine_power":    "failed",
			"engine_managed":  true,
		})
	})

	var err error
	out := captureStdout(t, func() { err = runEngineStatus(url) })
	if err != nil {
		t.Fatalf("runEngineStatus: %v", err)
	}
	for _, want := range []string{
		"Engine state: failed",
		"waired inference engine start",
		"Inference engine: engine_failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}
}

// TestRunEngineStatus_UnmanagedSaysStopStartIsUnavailable: an adopted ollama
// orphan has no process handle, so the power axis cannot act on it, and the
// remediation above would be a dead end.
func TestRunEngineStatus_UnmanagedSaysStopStartIsUnavailable(t *testing.T) {
	url, _ := engineMgmt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subsystem_state": "ready", "engine_power": "running", "engine_managed": false,
		})
	})

	var err error
	out := captureStdout(t, func() { err = runEngineStatus(url) })
	if err != nil {
		t.Fatalf("runEngineStatus: %v", err)
	}
	if !strings.Contains(out, "not managed by Waired") {
		t.Errorf("stdout = %q, want the unmanaged note", out)
	}
}
