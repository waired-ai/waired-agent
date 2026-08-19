package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// `waired inference residency` (waired-agent#861). The dual-path shape
// is `waired config log-level`'s: apply through the daemon when it is
// up, write agent.json when it is not.

func TestInferenceResidency_ShowFromDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/waired/v1/inference/residency" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(management.ResidencyResponse{IdleTimeout: "30m0s"})
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runInference([]string{"residency", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("residency show: %v", err)
		}
	})
	if !strings.Contains(out, "30m0s") {
		t.Errorf("output = %q, want the current setting", out)
	}
}

// TestInferenceResidency_IndefiniteIsSpelledOut: the default is a zero
// (owner ruling on waired-agent#861, recorded in
// docs/decisions/20260820/0130-model-residency-is-a-setting.md). Printed
// as a duration it would read "0s" — i.e. "unloads immediately", the
// opposite of what it means.
func TestInferenceResidency_IndefiniteIsSpelledOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.ResidencyResponse{
			IdleTimeout: "0s", HoldsIndefinitely: true,
		})
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runInference([]string{"residency", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("residency show: %v", err)
		}
	})
	if strings.Contains(out, "0s") {
		t.Errorf("output = %q, must not render the indefinite setting as a duration", out)
	}
	if !strings.Contains(out, "always") {
		t.Errorf("output = %q, want it to say the model always stays", out)
	}
}

func TestInferenceResidency_SetPostsToDaemon(t *testing.T) {
	var got management.ResidencyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(management.ResidencyResponse{IdleTimeout: got.IdleTimeout})
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runInference([]string{"residency", "45m", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("residency set: %v", err)
		}
	})
	if got.IdleTimeout != "45m0s" {
		t.Errorf("daemon received %q, want 45m0s", got.IdleTimeout)
	}
	if !strings.Contains(out, "applied live") {
		t.Errorf("output = %q, want it to say the change is live", out)
	}
}

// TestInferenceResidency_NeverIsAccepted: the surfaces accept the word
// so an operator does not have to know the product spells "never" as a
// zero.
func TestInferenceResidency_NeverIsAccepted(t *testing.T) {
	var got management.ResidencyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(management.ResidencyResponse{
			IdleTimeout: "0s", HoldsIndefinitely: true,
		})
	}))
	defer srv.Close()

	captureStdout(t, func() {
		if err := runInference([]string{"residency", "never", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("residency never: %v", err)
		}
	})
	if got.IdleTimeout != "0s" {
		t.Errorf("daemon received %q, want 0s", got.IdleTimeout)
	}
}

func TestInferenceResidency_RejectsGarbageLocally(t *testing.T) {
	// No daemon: the parse fails before any network call.
	err := runInference([]string{"residency", "soon", "--mgmt", "http://127.0.0.1:0"})
	if err == nil {
		t.Fatal("expected an error for an unparseable duration")
	}
	if !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

func TestInferenceResidency_SetFallsBackToFileWhenDaemonDown(t *testing.T) {
	dir := t.TempDir()
	seed := agentconfig.Defaults()
	seed.Inference.MaxCacheGB = 7
	if err := seed.Save(filepath.Join(dir, "agent.json")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close()

	out := captureStdout(t, func() {
		if err := runInference([]string{"residency", "2h", "--mgmt", downURL, "--state-dir", dir}); err != nil {
			t.Fatalf("residency set fallback: %v", err)
		}
	})
	if !strings.Contains(out, "applies on next start") {
		t.Errorf("output = %q, want the 'applies on next start' note", out)
	}

	reloaded := agentconfig.Defaults()
	if err := reloaded.MergeJSON(filepath.Join(dir, "agent.json")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.Inference.IdleTimeout.Duration().String(); got != "2h0m0s" {
		t.Errorf("persisted idle timeout = %q, want 2h0m0s", got)
	}
	if reloaded.Inference.MaxCacheGB != 7 {
		t.Errorf("fallback clobbered an unrelated field: MaxCacheGB=%d, want 7", reloaded.Inference.MaxCacheGB)
	}
}

func TestInferenceResidency_ShowFallsBackToFileWhenDaemonDown(t *testing.T) {
	dir := t.TempDir()
	seed := agentconfig.Defaults()
	seed.Inference.IdleTimeout = agentconfig.NewDuration(90 * 60 * 1e9) // 90m
	if err := seed.Save(filepath.Join(dir, "agent.json")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close()

	out := captureStdout(t, func() {
		if err := runInference([]string{"residency", "--mgmt", downURL, "--state-dir", dir}); err != nil {
			t.Fatalf("residency show fallback: %v", err)
		}
	})
	if !strings.Contains(out, "1h30m0s") {
		t.Errorf("output = %q, want the persisted setting", out)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("output = %q, want it to note the daemon is not running", out)
	}
}
