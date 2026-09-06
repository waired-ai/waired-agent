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
		_ = json.NewEncoder(w).Encode(management.ResidencyResponse{
			IdleTimeout: got.IdleTimeout, Effect: management.ResidencyEffectLive,
		})
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

// TestInferenceResidency_EffectIsReportedNotAssumed: "(applied live)" is
// a claim about the engine, and it is only true when a model was
// resident to re-stamp. Each other effect has to reach the operator as
// itself, and an effect this build does not know must produce no claim
// at all rather than the old blanket one (waired-agent#908).
func TestInferenceResidency_EffectIsReportedNotAssumed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		effect  management.ResidencyEffect
		want    string
		notWant string
	}{
		{"live", management.ResidencyEffectLive, "applied live", "restarted"},
		{"respawned", management.ResidencyEffectEngineRestarted, "engine restarted", "applied live"},
		{"parked", management.ResidencyEffectOnEngineStart, "when the engine starts", "applied live"},
		{"adopted", management.ResidencyEffectNeedsEngineRestart, "started outside waired", "applied live"},
		{"unknown to this build", management.ResidencyEffect("something-new"), "Keep-alive", "applied live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&management.ResidencyRequest{})
				_ = json.NewEncoder(w).Encode(management.ResidencyResponse{
					IdleTimeout: "30m0s", Effect: tc.effect,
				})
			}))
			defer srv.Close()
			out := captureStdout(t, func() {
				if err := runInference([]string{"residency", "30m", "--mgmt", srv.URL}); err != nil {
					t.Fatalf("residency set: %v", err)
				}
			})
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("output = %q, must not contain %q", out, tc.notWant)
			}
		})
	}
}

// TestInferenceResidency_OldAgentGetsNoClaim: an agent that predates the
// effect field reports nothing about how the value landed, and the CLI
// must not fill that silence with the old always-live assertion.
func TestInferenceResidency_OldAgentGetsNoClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"idle_timeout":"30m0s","holds_indefinitely":false}`))
	}))
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runInference([]string{"residency", "30m", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("residency set: %v", err)
		}
	})
	if strings.Contains(out, "applied live") {
		t.Errorf("output = %q, must not claim the change is live without the agent saying so", out)
	}
	if !strings.Contains(out, "30m0s") {
		t.Errorf("output = %q, want the value it set", out)
	}
}

// TestInferenceResidency_AlwaysIsAccepted: "always" is the word the CLI
// prints, the tray and the console label the button with, and the ja
// mirror pins — and it was the one word the parser rejected
// (waired-agent#909). Typing back what the product just said has to
// work.
func TestInferenceResidency_AlwaysIsAccepted(t *testing.T) {
	var got management.ResidencyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(management.ResidencyResponse{
			IdleTimeout: "0s", HoldsIndefinitely: true, Effect: management.ResidencyEffectLive,
		})
	}))
	defer srv.Close()

	captureStdout(t, func() {
		if err := runInference([]string{"residency", "always", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("residency always: %v", err)
		}
	})
	if got.IdleTimeout != "0s" {
		t.Errorf("daemon received %q, want 0s", got.IdleTimeout)
	}
}

// TestInferenceResidency_RejectsWordsThatMeanTheOpposite: "never" and
// "off" used to be accepted for "hold it indefinitely", which is the
// reverse of how either reads. Rejecting them is the point, and the
// error has to steer to the word that works (waired-agent#909).
func TestInferenceResidency_RejectsWordsThatMeanTheOpposite(t *testing.T) {
	for _, arg := range []string{"never", "off"} {
		t.Run(arg, func(t *testing.T) {
			err := runInference([]string{"residency", arg, "--mgmt", "http://127.0.0.1:0"})
			if err == nil {
				t.Fatalf("residency %s: want an error", arg)
			}
			if !strings.Contains(err.Error(), "always") {
				t.Errorf("error = %v, want it to point at \"always\"", err)
			}
		})
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
	if !strings.Contains(out, "applies on the next start") {
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
	if !strings.Contains(out, "isn't running") {
		t.Errorf("output = %q, want it to note the daemon is not running", out)
	}
}
