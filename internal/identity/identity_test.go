package identity

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSaveLoadRoundTrip exercises the new DeviceName field added for the
// tray UI. Older identity.json files without device_name must still load
// (back-compat), and a freshly-saved identity must read back identical.
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Identity{
		DeviceID:                "did_abc123",
		DeviceName:              "alice-laptop",
		NetworkID:               "net_xyz",
		NetworkName:             "alice-net",
		AccountID:               "acct_001",
		AccountEmail:            "alice@example.com",
		OverlayIP:               "100.96.0.10",
		Endpoint:                "udp4:198.51.100.1:41010",
		ControlURL:              "https://control.example.com",
		ControlSigningPublicKey: "AAAA",
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil")
	}
	if *got != *want {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", *got, *want)
	}
}

// TestLoadLegacyWithoutDeviceName verifies that an identity.json written
// before the device_name field existed loads with DeviceName="". The agent
// then falls back to DeviceID for display.
func TestLoadLegacyWithoutDeviceName(t *testing.T) {
	dir := t.TempDir()
	p, err := PathsFor(dir)
	if err != nil {
		t.Fatalf("PathsFor: %v", err)
	}
	legacy := []byte(`{
  "device_id": "did_old",
  "network_id": "net_xyz",
  "network_name": "alice-net",
  "account_id": "acct_001",
  "account_email": "alice@example.com",
  "overlay_ip": "100.96.0.10",
  "endpoint": "udp4:198.51.100.1:41010",
  "control_url": "https://control.example.com",
  "control_signing_public_key": "AAAA"
}`)
	if err := os.WriteFile(p.Identity, legacy, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DeviceID != "did_old" {
		t.Errorf("DeviceID = %q, want did_old", got.DeviceID)
	}
	if got.DeviceName != "" {
		t.Errorf("DeviceName = %q, want empty (legacy file)", got.DeviceName)
	}
}

// TestSaveOmitsEmptyDeviceName ensures the JSON output skips the
// device_name key when empty. Keeps stored identity.json byte-stable for
// the legacy case so a daemon upgrade doesn't rewrite working files.
func TestSaveOmitsEmptyDeviceName(t *testing.T) {
	dir := t.TempDir()
	id := &Identity{
		DeviceID:                "did_x",
		NetworkID:               "n",
		NetworkName:             "n",
		AccountID:               "a",
		AccountEmail:            "a@e.com",
		OverlayIP:               "10.0.0.1",
		Endpoint:                "udp4:1.2.3.4:5",
		ControlURL:              "https://e.com",
		ControlSigningPublicKey: "k",
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if contains(string(body), "device_name") {
		t.Errorf("identity.json contains device_name when empty:\n%s", body)
	}
}

// TestLoadDoesNotTouchFilesystem pins the read-path contract: Load must
// not create or re-permission anything. A non-root `waired status`
// pointed at a root-owned state dir has to surface the real read error
// (or ENOENT), not a chmod EPERM from SecureDir — and a status query on
// a never-enrolled machine must not leave an empty state dir behind.
func TestLoadDoesNotTouchFilesystem(t *testing.T) {
	base := t.TempDir()

	missing := filepath.Join(base, "never-created")
	id, err := Load(missing)
	if err != nil || id != nil {
		t.Fatalf("Load(missing dir) = (%v, %v), want (nil, nil)", id, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Load created the state dir %s", missing)
	}

	dir := filepath.Join(base, "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"),
		[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || got == nil || got.DeviceID != "did_x" {
		t.Fatalf("Load = (%+v, %v), want device did_x", got, err)
	}
	for _, sub := range []string{"secrets", "cache"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !os.IsNotExist(err) {
			t.Errorf("Load created %s/ as a side effect", sub)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("Load changed state dir mode to %v, want 0755", fi.Mode().Perm())
		}
	}
}

// TestPathsForStillSecuresDirs guards the write-path half of the
// PathsUnder/PathsFor split: writers must keep getting the created +
// secured directory tree.
func TestPathsForStillSecuresDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := PathsFor(dir); err != nil {
		t.Fatalf("PathsFor: %v", err)
	}
	for _, sub := range []string{"", "secrets", "cache"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Errorf("PathsFor did not create %q: %v", sub, err)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestTokenMetaReauthRequiredRoundtrip exercises the #115 Phase C
// ReauthRequiredAt field: when set, it round-trips through
// SaveTokenMeta + LoadTokenMeta and NeedsReauth returns true.
func TestTokenMetaReauthRequiredRoundtrip(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	meta := TokenMeta{
		AccessExpiresAt:     time.Now().Add(15 * time.Minute),
		DeviceAuthExpiresAt: time.Now().Add(180 * 24 * time.Hour),
		ReauthRequiredAt:    when,
	}
	if err := SaveTokenMeta(dir, meta); err != nil {
		t.Fatalf("SaveTokenMeta: %v", err)
	}
	got, err := LoadTokenMeta(dir)
	if err != nil {
		t.Fatalf("LoadTokenMeta: %v", err)
	}
	if !got.NeedsReauth() {
		t.Fatalf("loaded meta should report NeedsReauth==true")
	}
	if !got.ReauthRequiredAt.Equal(when) {
		t.Fatalf("ReauthRequiredAt roundtrip mismatch: want %v got %v",
			when, got.ReauthRequiredAt)
	}
}

// TestTokenMetaReauthRequiredOmittedWhenZero covers the on-disk
// representation: the omitzero JSON tag must keep the file clean of
// reauth_required_at when the field hasn't been set, so a fresh enroll
// looks identical byte-for-byte to a pre-#115 state.
func TestTokenMetaReauthRequiredOmittedWhenZero(t *testing.T) {
	dir := t.TempDir()
	meta := TokenMeta{
		AccessExpiresAt:     time.Now().Add(15 * time.Minute),
		DeviceAuthExpiresAt: time.Now().Add(180 * 24 * time.Hour),
	}
	if err := SaveTokenMeta(dir, meta); err != nil {
		t.Fatalf("SaveTokenMeta: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "cache", "token_meta.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if contains(string(body), "reauth_required_at") {
		t.Errorf("token_meta.json contains reauth_required_at when zero:\n%s", body)
	}
	got, _ := LoadTokenMeta(dir)
	if got.NeedsReauth() {
		t.Fatalf("loaded zero-meta should report NeedsReauth==false")
	}
}

// TestTokenMetaReauthRequiredClearedByFreshSave mirrors the enroll
// path semantics: after `waired init` (or its renew variant) writes a
// new TokenMeta, the previous reauth_required flag must be gone.
func TestTokenMetaReauthRequiredClearedByFreshSave(t *testing.T) {
	dir := t.TempDir()
	old := TokenMeta{ReauthRequiredAt: time.Now().UTC()}
	if err := SaveTokenMeta(dir, old); err != nil {
		t.Fatalf("SaveTokenMeta old: %v", err)
	}
	fresh := TokenMeta{
		AccessExpiresAt:     time.Now().Add(15 * time.Minute),
		DeviceAuthExpiresAt: time.Now().Add(180 * 24 * time.Hour),
	}
	if err := SaveTokenMeta(dir, fresh); err != nil {
		t.Fatalf("SaveTokenMeta fresh: %v", err)
	}
	got, _ := LoadTokenMeta(dir)
	if got.NeedsReauth() {
		t.Fatalf("fresh SaveTokenMeta should clear the reauth_required flag, got %v", got.ReauthRequiredAt)
	}
}
