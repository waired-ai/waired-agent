package agentconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicUse_LoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference", PublicUseFileName)

	// Missing file → not found, no error.
	if _, ok, err := LoadPublicUse(path); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}

	want := PublicUse{
		Mode:           PublicUseModeAuto,
		MinQualityTier: 3,
		Main:           true,
		Sub:            false,
		Consent:        &PublicConsent{AcceptedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC), WarningVersion: 1},
	}
	if err := SavePublicUse(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := LoadPublicUse(path)
	if !ok || err != nil {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.Mode != want.Mode || got.MinQualityTier != want.MinQualityTier ||
		got.Main != want.Main || got.Sub != want.Sub {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Consent == nil || !got.Consent.AcceptedAt.Equal(want.Consent.AcceptedAt) ||
		got.Consent.WarningVersion != 1 {
		t.Fatalf("consent round-trip mismatch: %+v", got.Consent)
	}
}

func TestPublicUse_InvalidModeRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), PublicUseFileName)
	if err := SavePublicUse(path, PublicUse{Mode: "always"}); err == nil {
		t.Fatalf("save with invalid mode must error")
	}
	if err := os.WriteFile(path, []byte(`{"mode":"bogus"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadPublicUse(path); err == nil || !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("load with invalid stored mode must error, got %v", err)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadPublicUse(path); err == nil {
		t.Fatalf("malformed file must error")
	}
}

func TestPublicUse_EffectiveModeRequiresConsent(t *testing.T) {
	// No consent → off, regardless of stored mode.
	p := PublicUse{Mode: PublicUseModeExplicit}
	if got := p.EffectiveMode(1); got != PublicUseModeOff {
		t.Fatalf("unconsented effective mode = %q, want off", got)
	}
	// Consent for the current version → stored mode.
	p.Consent = &PublicConsent{WarningVersion: 1}
	if got := p.EffectiveMode(1); got != PublicUseModeExplicit {
		t.Fatalf("consented effective mode = %q, want explicit", got)
	}
	// Warning text bumped → stale consent, back to off until re-consent.
	if got := p.EffectiveMode(2); got != PublicUseModeOff {
		t.Fatalf("stale-consent effective mode = %q, want off", got)
	}
	// Consented but mode empty → off.
	p.Mode = ""
	if got := p.EffectiveMode(1); got != PublicUseModeOff {
		t.Fatalf("empty-mode effective mode = %q, want off", got)
	}
}
