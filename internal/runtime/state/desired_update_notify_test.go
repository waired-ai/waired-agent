package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A missing desired-update-notify file means the operator has never
// touched the toggle, so update prompts default ON (#294).
func TestReadDesiredUpdateNotifyMissingDefaultsOn(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredUpdateNotify(dir)
	if err != nil {
		t.Fatalf("ReadDesiredUpdateNotify: %v", err)
	}
	if got != UpdateNotifyOn {
		t.Errorf("missing file should default to %q, got %q", UpdateNotifyOn, got)
	}
	if !got.Enabled() {
		t.Errorf("default should be enabled")
	}
}

func TestReadDesiredUpdateNotifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredUpdateNotify(dir, UpdateNotifyOff); err != nil {
		t.Fatalf("WriteDesiredUpdateNotify off: %v", err)
	}
	got, err := ReadDesiredUpdateNotify(dir)
	if err != nil {
		t.Fatalf("ReadDesiredUpdateNotify: %v", err)
	}
	if got != UpdateNotifyOff || got.Enabled() {
		t.Errorf("got %q (enabled=%v), want off/disabled", got, got.Enabled())
	}

	if err := WriteDesiredUpdateNotify(dir, UpdateNotifyOn); err != nil {
		t.Fatalf("WriteDesiredUpdateNotify on: %v", err)
	}
	got, err = ReadDesiredUpdateNotify(dir)
	if err != nil {
		t.Fatalf("ReadDesiredUpdateNotify: %v", err)
	}
	if got != UpdateNotifyOn || !got.Enabled() {
		t.Errorf("got %q (enabled=%v), want on/enabled", got, got.Enabled())
	}
}

// An empty file is indistinguishable from "never touched" and must
// default ON, not error.
func TestReadDesiredUpdateNotifyEmptyDefaultsOn(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredUpdateNotifyPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredUpdateNotifyPath(dir), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredUpdateNotify(dir)
	if err != nil {
		t.Fatalf("ReadDesiredUpdateNotify: %v", err)
	}
	if got != UpdateNotifyOn {
		t.Errorf("empty file should default to %q, got %q", UpdateNotifyOn, got)
	}
}

func TestReadDesiredUpdateNotifyUnknownErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredUpdateNotifyPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredUpdateNotifyPath(dir), []byte("enabled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredUpdateNotify(dir); err == nil {
		t.Error("expected error for unknown update-notify value, got nil")
	}
}

func TestWriteDesiredUpdateNotifyRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredUpdateNotify(dir, UpdateNotifyState("enabled")); err == nil {
		t.Error("expected error for invalid update-notify state, got nil")
	}
	if _, err := os.Stat(DesiredUpdateNotifyPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should not be created for invalid value, stat err=%v", err)
	}
}
