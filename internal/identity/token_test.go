package identity

import (
	"os"
	"testing"
)

func TestAccessToken_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	if got, err := LoadAccessToken(dir); err != nil || got != "" {
		t.Fatalf("missing token: got %q err=%v, want \"\" nil", got, err)
	}
	if err := SaveAccessToken(dir, "tok-abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadAccessToken(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "tok-abc" {
		t.Fatalf("got %q, want tok-abc", got)
	}
}

// The file is the only copy. Deleting it must read back as "no token"
// — the ("" , nil) the callers branch on to start a fresh sign-in —
// rather than resurrecting the old one from somewhere else. On darwin
// the token used to be mirrored into the System keychain and read from
// there first, which is what made a logged-out host still hold a
// credential (#261).
func TestAccessToken_FileLossReadsAsAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := SaveAccessToken(dir, "tok-abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	p, err := PathsFor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.AccessToken); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAccessToken(dir)
	if err != nil {
		t.Fatalf("load after file loss: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q after the file was deleted, want \"\"", got)
	}
}

func TestRefreshToken_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	if got, err := LoadRefreshToken(dir); err != nil || got != "" {
		t.Fatalf("missing token: got %q err=%v, want \"\" nil", got, err)
	}
	if err := SaveRefreshToken(dir, "refresh-xyz"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadRefreshToken(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "refresh-xyz" {
		t.Fatalf("got %q, want refresh-xyz", got)
	}
}
