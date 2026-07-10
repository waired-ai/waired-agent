package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDesiredPhaseMissingDefaultsToActive(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredPhase(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPhase: %v", err)
	}
	if got != PhaseActive {
		t.Errorf("missing file should default to %q, got %q", PhaseActive, got)
	}
}

func TestReadDesiredPhaseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredPhase(dir, PhasePaused); err != nil {
		t.Fatalf("WriteDesiredPhase paused: %v", err)
	}
	got, err := ReadDesiredPhase(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPhase: %v", err)
	}
	if got != PhasePaused {
		t.Errorf("got %q, want %q", got, PhasePaused)
	}

	if err := WriteDesiredPhase(dir, PhaseActive); err != nil {
		t.Fatalf("WriteDesiredPhase active: %v", err)
	}
	got, err = ReadDesiredPhase(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPhase: %v", err)
	}
	if got != PhaseActive {
		t.Errorf("got %q, want %q", got, PhaseActive)
	}
}

func TestReadDesiredPhaseTolerantOfWhitespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredPhasePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredPhasePath(dir), []byte("  paused\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredPhase(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPhase: %v", err)
	}
	if got != PhasePaused {
		t.Errorf("got %q, want %q", got, PhasePaused)
	}
}

func TestReadDesiredPhaseEmptyMeansActive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredPhasePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredPhasePath(dir), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredPhase(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPhase: %v", err)
	}
	if got != PhaseActive {
		t.Errorf("got %q, want %q (empty file should mean active)", got, PhaseActive)
	}
}

func TestReadDesiredPhaseUnknownErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredPhasePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredPhasePath(dir), []byte("standby\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredPhase(dir); err == nil {
		t.Error("expected error for unknown phase value, got nil")
	}
}

func TestWriteDesiredPhaseRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredPhase(dir, Phase("standby")); err == nil {
		t.Error("expected error for invalid phase, got nil")
	}
	if _, err := os.Stat(DesiredPhasePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should not be created for invalid phase, stat err=%v", err)
	}
}
