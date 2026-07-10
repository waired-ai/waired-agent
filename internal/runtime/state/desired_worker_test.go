package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDesiredWorkerMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredWorker(dir)
	if err != nil {
		t.Fatalf("ReadDesiredWorker: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("missing file should return zero RoutingPreference, got %#v", got)
	}
}

func TestReadDesiredWorkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cases := []RoutingPreference{
		{Mode: RoutingModeAuto},
		{Mode: RoutingModeLocalOnly},
		{Mode: RoutingModePeerPreferred},
		{Mode: RoutingModePinned, PinnedPeerDeviceID: "dev_abc123"},
	}
	for _, want := range cases {
		t.Run(string(want.Mode), func(t *testing.T) {
			if err := WriteDesiredWorker(dir, want); err != nil {
				t.Fatalf("WriteDesiredWorker(%v): %v", want, err)
			}
			got, err := ReadDesiredWorker(dir)
			if err != nil {
				t.Fatalf("ReadDesiredWorker: %v", err)
			}
			if got != want {
				t.Errorf("round-trip mismatch: got %#v want %#v", got, want)
			}
		})
	}
}

func TestWriteDesiredWorkerRejectsPinnedWithoutPeer(t *testing.T) {
	dir := t.TempDir()
	err := WriteDesiredWorker(dir, RoutingPreference{Mode: RoutingModePinned})
	if err == nil {
		t.Fatal("expected error for pinned mode without peer, got nil")
	}
	if _, statErr := os.Stat(DesiredWorkerPath(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("file should not be created on validation failure, stat err=%v", statErr)
	}
}

func TestWriteDesiredWorkerRejectsNonPinnedWithPeer(t *testing.T) {
	dir := t.TempDir()
	err := WriteDesiredWorker(dir, RoutingPreference{
		Mode:               RoutingModeAuto,
		PinnedPeerDeviceID: "dev_abc",
	})
	if err == nil {
		t.Fatal("expected error for auto mode with stray peer, got nil")
	}
}

func TestWriteDesiredWorkerRejectsUnknownMode(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredWorker(dir, RoutingPreference{Mode: RoutingMode("bogus")}); err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

func TestReadDesiredWorkerMalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredWorkerPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredWorkerPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredWorker(dir); err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}

func TestReadDesiredWorkerRejectsInconsistentOnDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredWorkerPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	// Hand-craft a malformed pair (pinned mode + no peer) to verify
	// Read validates as well as Write — protects against a third party
	// touching the file directly.
	if err := os.WriteFile(DesiredWorkerPath(dir), []byte(`{"mode":"pinned"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredWorker(dir); err == nil {
		t.Error("expected error for pinned mode without peer on read, got nil")
	}
}

func TestRoutingPreferenceIsZero(t *testing.T) {
	if !(RoutingPreference{}).IsZero() {
		t.Error("empty RoutingPreference should be IsZero")
	}
	if (RoutingPreference{Mode: RoutingModeAuto}).IsZero() {
		t.Error("auto mode should not be IsZero")
	}
	if (RoutingPreference{Mode: RoutingModePinned, PinnedPeerDeviceID: "x"}).IsZero() {
		t.Error("pinned mode should not be IsZero")
	}
}
