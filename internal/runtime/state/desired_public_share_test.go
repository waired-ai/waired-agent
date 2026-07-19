package state

import (
	"os"
	"testing"
)

// Missing file = "operator never enabled it" — callers treat the empty
// state as OFF (public serving is strictly opt-in).
func TestReadDesiredPublicShareMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredPublicShare(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPublicShare: %v", err)
	}
	if got != "" {
		t.Fatalf("missing file: got %q, want empty", got)
	}
}

func TestReadDesiredPublicShareRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []PublicShareState{PublicShareOn, PublicShareOff} {
		if err := WriteDesiredPublicShare(dir, s); err != nil {
			t.Fatalf("WriteDesiredPublicShare(%q): %v", s, err)
		}
		got, err := ReadDesiredPublicShare(dir)
		if err != nil {
			t.Fatalf("ReadDesiredPublicShare after %q: %v", s, err)
		}
		if got != s {
			t.Fatalf("round trip: got %q, want %q", got, s)
		}
	}
}

func TestWriteDesiredPublicShareRejectsInvalid(t *testing.T) {
	if err := WriteDesiredPublicShare(t.TempDir(), "sorta-public"); err == nil {
		t.Fatal("WriteDesiredPublicShare(invalid) = nil, want error")
	}
}

func TestReadDesiredPublicShareRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/runtime", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredPublicSharePath(dir), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredPublicShare(dir); err == nil {
		t.Fatal("ReadDesiredPublicShare(garbage) = nil error, want error")
	}
}
