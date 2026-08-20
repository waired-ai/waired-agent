package runtime

import (
	"testing"
	"time"
)

// TestResolveRequestKeepAlive_ParsesAsADuration asserts the PROPERTY
// that broke rather than a literal: the request field is decoded as a
// duration by the engine, so every value this renders has to survive
// time.ParseDuration. Pinning the string instead is how "-1" was
// asserted as correct while a live engine answered 400 to it
// (waired-agent#927).
func TestResolveRequestKeepAlive_ParsesAsADuration(t *testing.T) {
	for _, idle := range []time.Duration{
		0, -time.Second, -time.Hour, // every spelling of indefinite
		time.Second, 7 * time.Minute, 40 * time.Minute, 8 * time.Hour,
	} {
		got := ResolveRequestKeepAlive(idle)
		d, err := time.ParseDuration(got)
		if err != nil {
			t.Errorf("ResolveRequestKeepAlive(%v) = %q, which the engine cannot parse: %v", idle, got, err)
			continue
		}
		if idle <= 0 && d >= 0 {
			t.Errorf("ResolveRequestKeepAlive(%v) = %q (%v); indefinite has to be negative", idle, got, d)
		}
		if idle > 0 && d != idle {
			t.Errorf("ResolveRequestKeepAlive(%v) = %q (%v), want the same duration", idle, got, d)
		}
	}
}

// TestResolveKeepAlive_EnvKeepsItsOwnSpelling: the environment variable
// is a separate grammar and must not be dragged along by the request
// fix. It accepts the unitless form, and changing it would alter how
// every engine spawn is configured for no reason.
func TestResolveKeepAlive_EnvKeepsItsOwnSpelling(t *testing.T) {
	if got := ResolveKeepAlive(0); got != KeepAliveIndefinite {
		t.Errorf("ResolveKeepAlive(0) = %q, want %q", got, KeepAliveIndefinite)
	}
	if got := ResolveKeepAlive(30 * time.Minute); got != "30m0s" {
		t.Errorf("ResolveKeepAlive(30m) = %q, want 30m0s", got)
	}
}

// TestKeepAliveMethodIsRequestShaped: OllamaAdapter.KeepAlive feeds the
// warm path's per-request field, so it must use the request grammar. It
// did not, which meant every warm-up under the default setting was
// answered with a 400 — the mechanism that keeps the model resident
// could not load it while the setting said to keep it resident.
func TestKeepAliveMethodIsRequestShaped(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{Host: "127.0.0.1", Port: 1, KeepAlive: 0})
	got := a.KeepAlive()
	if _, err := time.ParseDuration(got); err != nil {
		t.Fatalf("KeepAlive() = %q, which the engine cannot parse: %v", got, err)
	}
}
