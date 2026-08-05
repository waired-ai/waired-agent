package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReadDesiredInferenceStateMissingIsUnset inverts what this file
// used to pin ("missing defaults to enabled"). That default made this
// reader the only one of the four desired-* readers that could not say
// "the user has never touched this toggle", so the boot path had no way
// to fall back to the agentconfig default the way desired-share does —
// and agentconfig.Inference.Enabled ended up gating the subsystem
// instead (waired-ai/waired#1056, #465). It is a record of today's
// behaviour, matching ReadDesiredShareMesh / ReadDesiredPublicShare.
func TestReadDesiredInferenceStateMissingIsUnset(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredInferenceState(dir)
	if err != nil {
		t.Fatalf("ReadDesiredInferenceState: %v", err)
	}
	if got != "" {
		t.Errorf("missing file should read as unset, got %q", got)
	}
}

func TestReadDesiredInferenceStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredInferenceState(dir, InferenceDisabled); err != nil {
		t.Fatalf("WriteDesiredInferenceState disabled: %v", err)
	}
	got, err := ReadDesiredInferenceState(dir)
	if err != nil {
		t.Fatalf("ReadDesiredInferenceState: %v", err)
	}
	if got != InferenceDisabled {
		t.Errorf("got %q, want %q", got, InferenceDisabled)
	}

	if err := WriteDesiredInferenceState(dir, InferenceEnabled); err != nil {
		t.Fatalf("WriteDesiredInferenceState enabled: %v", err)
	}
	got, err = ReadDesiredInferenceState(dir)
	if err != nil {
		t.Fatalf("ReadDesiredInferenceState: %v", err)
	}
	if got != InferenceEnabled {
		t.Errorf("got %q, want %q", got, InferenceEnabled)
	}
}

func TestReadDesiredInferenceStateTolerantOfWhitespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredInferencePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredInferencePath(dir), []byte("  disabled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredInferenceState(dir)
	if err != nil {
		t.Fatalf("ReadDesiredInferenceState: %v", err)
	}
	if got != InferenceDisabled {
		t.Errorf("got %q, want %q", got, InferenceDisabled)
	}
}

func TestReadDesiredInferenceStateEmptyIsUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredInferencePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredInferencePath(dir), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredInferenceState(dir)
	if err != nil {
		t.Fatalf("ReadDesiredInferenceState: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want unset (a truncated file is not a choice)", got)
	}
}

func TestReadDesiredInferenceStateUnknownErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredInferencePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredInferencePath(dir), []byte("paused\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredInferenceState(dir); err == nil {
		t.Error("expected error for unknown inference state value, got nil")
	}
}

func TestWriteDesiredInferenceStateRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredInferenceState(dir, InferenceState("paused")); err == nil {
		t.Error("expected error for invalid inference state, got nil")
	}
	if _, err := os.Stat(DesiredInferencePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should not be created for invalid value, stat err=%v", err)
	}
}
