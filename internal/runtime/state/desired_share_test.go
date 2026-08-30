package state

import (
	"os"
	"path/filepath"
	"testing"
)

// waired#1297. The machine keeps one sharing answer — whether it lends
// itself out at all — and the control plane keeps the rest. These pin
// the owner ruling recorded on that issue, not today's behaviour.

func TestDesiredSharingRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Absent is not off. A computer nobody has touched shares, so the
	// reader has to be able to say "no answer here" rather than
	// answering for the operator.
	got, err := ReadDesiredSharing(dir)
	if err != nil {
		t.Fatalf("read on an empty state dir: %v", err)
	}
	if got != "" {
		t.Fatalf("ReadDesiredSharing on an empty dir = %q, want empty", got)
	}

	for _, want := range []SharingState{SharingOn, SharingOff} {
		if err := WriteDesiredSharing(dir, want); err != nil {
			t.Fatalf("write %q: %v", want, err)
		}
		got, err := ReadDesiredSharing(dir)
		if err != nil {
			t.Fatalf("read back %q: %v", want, err)
		}
		if got != want {
			t.Errorf("round trip = %q, want %q", got, want)
		}
	}

	if err := WriteDesiredSharing(dir, SharingState("maybe")); err == nil {
		t.Error("writing a value outside the two-word set was accepted")
	}

	// A value this build does not know is an error rather than a silent
	// default: guessing would let a newer daemon's vocabulary read as
	// "share", which is the direction that cannot be taken back.
	if err := os.WriteFile(DesiredSharingPath(dir), []byte("sometimes\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ReadDesiredSharing(dir); err == nil {
		t.Error("an unknown persisted value was read without an error")
	}
}

func TestAppliedMeshShareRoundTrip(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadAppliedMeshShare(dir)
	if err != nil {
		t.Fatalf("read on an empty state dir: %v", err)
	}
	if got != "" {
		t.Fatalf("ReadAppliedMeshShare on an empty dir = %q, want empty", got)
	}
	for _, want := range []MeshShareState{MeshShareOn, MeshShareOff} {
		if err := WriteAppliedMeshShare(dir, want); err != nil {
			t.Fatalf("write %q: %v", want, err)
		}
		got, err := ReadAppliedMeshShare(dir)
		if err != nil {
			t.Fatalf("read back %q: %v", want, err)
		}
		if got != want {
			t.Errorf("round trip = %q, want %q", got, want)
		}
	}
	if err := WriteAppliedMeshShare(dir, MeshShareState("on-ish")); err == nil {
		t.Error("writing a value outside the two-word set was accepted")
	}
}

// The two files that used to hold sharing intent are deleted rather than
// read. They answered different questions — one was "not to my own
// mesh", the other "not to strangers" — and the ruling is that every
// computer starts sharing again. A file left behind is one a later
// reader can resurrect with the wrong meaning.
func TestRemoveRetiredSharingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "runtime"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := []string{
		filepath.Join(dir, "runtime", "desired-share"),
		filepath.Join(dir, "runtime", "desired-public-share"),
	}
	for _, p := range old {
		if err := os.WriteFile(p, []byte("not_shared\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	if err := RemoveRetiredSharingFiles(dir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, p := range old {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", filepath.Base(p), err)
		}
	}

	// Idempotent: the common case is a state dir that never had them,
	// and a daemon start must not fail on that.
	if err := RemoveRetiredSharingFiles(dir); err != nil {
		t.Fatalf("second remove: %v", err)
	}

	// It leaves the new files alone — they live in the same directory,
	// and a sweep that took them would turn every restart into a reset.
	if err := WriteDesiredSharing(dir, SharingOff); err != nil {
		t.Fatalf("write desired-sharing: %v", err)
	}
	if err := RemoveRetiredSharingFiles(dir); err != nil {
		t.Fatalf("third remove: %v", err)
	}
	if got, _ := ReadDesiredSharing(dir); got != SharingOff {
		t.Errorf("the sweep took the current file: got %q", got)
	}
}
