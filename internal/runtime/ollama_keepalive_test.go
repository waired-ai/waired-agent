package runtime

import (
	"strings"
	"testing"
	"time"
)

// TestResolveKeepAlive pins the mapping from the operator's idle timeout
// onto the value the engine expects.
//
// Product contract for the zero case: owner ruling on waired-agent#861,
// recorded in docs/decisions/20260820/0130-model-residency-is-a-setting.md.
// The rest is a record of Ollama's documented input handling — a negative
// duration means "never unload on idle", and a duration string is parsed
// as written.
func TestResolveKeepAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero holds indefinitely", 0, "-1"},
		{"negative holds indefinitely", -5 * time.Minute, "-1"},
		{"finite is rendered as a duration", 10 * time.Minute, "10m0s"},
		{"sub-minute survives", 30 * time.Second, "30s"},
		{"hours survive", 8 * time.Hour, "8h0m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveKeepAlive(tc.in); got != tc.want {
				t.Errorf("ResolveKeepAlive(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestOllamaAdapterKeepAlive checks the per-request value an adapter
// reports matches what its spawn would export. The two must agree: an
// adopted engine's environment belongs to a previous run, so the warm
// path sends the value explicitly (waired-agent#320), and a disagreement
// there would silently undo the operator's setting minutes later.
func TestOllamaAdapterKeepAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  time.Duration
		want string
	}{
		// The request grammar, not the environment variable's: this method
		// feeds the per-request field. Pinning "-1" here is what certified
		// the value a live engine answers with 400 (waired-agent#927).
		{"default holds", 0, requestKeepAliveIndefinite},
		{"finite passes through", 45 * time.Minute, "45m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewOllamaAdapter(OllamaConfig{KeepAlive: tc.cfg})
			if got := a.KeepAlive(); got != tc.want {
				t.Errorf("KeepAlive() = %q, want %q", got, tc.want)
			}
			if got := a.KeepAliveDuration(); got != tc.cfg {
				t.Errorf("KeepAliveDuration() = %v, want %v", got, tc.cfg)
			}
		})
	}
}

// TestOllamaAdapterResidency covers the three states the status surfaces
// must tell apart: never observed, observed-and-empty, and resident.
// Merging the first two is the defect waired-agent#879 records.
func TestOllamaAdapterResidency(t *testing.T) {
	a := &OllamaAdapter{}
	if got := a.Residency(); got.Observed || got.Resident() {
		t.Fatalf("zero value should be unobserved, got %+v", got)
	}

	a.SetResidency(ModelResidency{Observed: true})
	got := a.Residency()
	if !got.Observed {
		t.Errorf("Observed = false after an empty observation")
	}
	if got.Resident() {
		t.Errorf("Resident() = true with no model")
	}

	until := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	a.SetResidency(ModelResidency{Observed: true, Model: "m:q4", Until: until})
	got = a.Residency()
	if !got.Resident() || got.Model != "m:q4" || !got.Until.Equal(until) {
		t.Errorf("resident observation not round-tripped: %+v", got)
	}
}

// TestOllamaAdapterSetKeepAlive covers the live path the residency
// setting needs: OLLAMA_KEEP_ALIVE is read only when the engine spawns
// (processEnv), so applying a change by restarting would unload the
// model the operator is configuring the residency of. SetKeepAlive
// moves the value the next spawn exports AND the per-request value the
// caller re-stamps the resident copy with (#861).
func TestOllamaAdapterSetKeepAlive(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{KeepAlive: 10 * time.Minute})
	if got := a.KeepAlive(); got != "10m0s" {
		t.Fatalf("seeded KeepAlive() = %q, want 10m0s", got)
	}

	a.SetKeepAlive(0)
	// KeepAlive() feeds the per-request field, which the engine decodes
	// as a duration — NOT the environment variable's grammar. Asserting
	// KeepAliveIndefinite here is what let "-1" ship as the request
	// value while a live engine answered 400 to it (waired-agent#927).
	// The env spelling is asserted separately, below.
	if got := a.KeepAlive(); got != ResolveRequestKeepAlive(0) {
		t.Errorf("after SetKeepAlive(0), KeepAlive() = %q, want the request spelling %q",
			got, ResolveRequestKeepAlive(0))
	}
	if _, err := time.ParseDuration(a.KeepAlive()); err != nil {
		t.Errorf("KeepAlive() = %q, which the engine cannot parse: %v", a.KeepAlive(), err)
	}
	if got := a.KeepAliveDuration(); got != 0 {
		t.Errorf("after SetKeepAlive(0), KeepAliveDuration() = %v, want 0", got)
	}
	if got := keepAliveFromEnv(t, a.processEnv()); got != KeepAliveIndefinite {
		t.Errorf("spawn env OLLAMA_KEEP_ALIVE = %q, want %q", got, KeepAliveIndefinite)
	}

	a.SetKeepAlive(45 * time.Minute)
	if got := keepAliveFromEnv(t, a.processEnv()); got != "45m0s" {
		t.Errorf("spawn env OLLAMA_KEEP_ALIVE = %q, want 45m0s", got)
	}
}

func keepAliveFromEnv(t *testing.T, env []string) string {
	t.Helper()
	got := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "OLLAMA_KEEP_ALIVE=") {
			got = strings.TrimPrefix(kv, "OLLAMA_KEEP_ALIVE=")
		}
	}
	if got == "" {
		t.Fatalf("no OLLAMA_KEEP_ALIVE in spawn env")
	}
	return got
}
