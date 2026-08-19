package runtime

import (
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
		{"default holds", 0, "-1"},
		{"finite passes through", 45 * time.Minute, "45m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &OllamaAdapter{cfg: OllamaConfig{KeepAlive: tc.cfg}}
			if got := a.KeepAlive(); got != tc.want {
				t.Errorf("KeepAlive() = %q, want %q", got, tc.want)
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
