package identity

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// useMemKeychain swaps in an in-memory Keychain so these tests never exec
// /usr/bin/security (and never trigger an auth prompt) on darwin.
func useMemKeychain(t *testing.T) {
	t.Helper()
	t.Cleanup(securestore.SwapStoreForTest(securestore.NewMemStore()))
}

func TestAccessToken_RoundTrip(t *testing.T) {
	useMemKeychain(t)
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

func TestAccessToken_KeychainSurvivesFileLoss(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	if err := SaveAccessToken(dir, "tok-keychain"); err != nil {
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
	if got != "tok-keychain" {
		t.Fatalf("got %q, want tok-keychain (from keychain)", got)
	}
}

func TestRefreshToken_RoundTrip(t *testing.T) {
	useMemKeychain(t)
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
