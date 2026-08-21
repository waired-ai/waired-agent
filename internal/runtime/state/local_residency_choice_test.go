package state

import (
	"os"
	"path/filepath"
	"testing"
)

// PRODUCT CONTRACT (waired#1232): a missing file is "no ordering
// available", which the control plane must answer by leaving its own
// instruction alone. It is NOT "nobody has ever chosen" and it is not an
// error — an agent that has simply never had residency set locally is the
// ordinary case, and reading the absence as a claim would let the control
// plane realign onto a value nobody picked.
func TestLocalResidencyChoiceRoundTrip(t *testing.T) {
	dir := t.TempDir()

	got, err := ReadLocalResidencyChoice(dir)
	if err != nil {
		t.Fatalf("a missing record must not be an error: %v", err)
	}
	if got.ChosenAt != "" {
		t.Errorf("ChosenAt = %q, want empty on a fresh host", got.ChosenAt)
	}

	want := LocalResidencyChoice{ChosenAt: "2026-08-21T09:15:04.5Z", Value: "45m0s"}
	if err := WriteLocalResidencyChoice(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err = ReadLocalResidencyChoice(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Errorf("round trip: got %+v want %+v", got, want)
	}

	// A later choice replaces the earlier one — the record answers "last",
	// not "first", because the ordering is against the CURRENT instruction.
	later := LocalResidencyChoice{ChosenAt: "2026-08-21T11:00:00Z", Value: "0s"}
	if err := WriteLocalResidencyChoice(dir, later); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got, _ = ReadLocalResidencyChoice(dir); got != later {
		t.Errorf("after a second choice: got %+v want %+v", got, later)
	}
}

// An empty instant is rejected rather than written: "nobody has chosen
// here" is the ABSENCE of the file, so a record with no instant in it
// would read back as one that answers nothing while looking like one that
// answers something.
func TestWriteLocalResidencyChoiceRejectsAnEmptyInstant(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLocalResidencyChoice(dir, LocalResidencyChoice{Value: "45m0s"}); err == nil {
		t.Fatal("an empty instant was accepted")
	}
	if _, err := os.Stat(LocalResidencyChoicePath(dir)); !os.IsNotExist(err) {
		t.Errorf("the rejected record still reached disk: %v", err)
	}
}

// A record this build cannot parse is an error, not a silent zero: the
// caller decides what to do with it, and the caller's answer ("no claim")
// must be its own choice rather than one this layer made for it.
func TestReadLocalResidencyChoiceReportsAnUnparseableRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(LocalResidencyChoicePath(dir)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(LocalResidencyChoicePath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadLocalResidencyChoice(dir); err == nil {
		t.Error("an unparseable record read back as a clean zero")
	}
}

// The two records are siblings and must not collide: one answers what the
// CONTROL PLANE asked and the other what a PERSON HERE chose, and a
// shared path would make each overwrite the other's answer.
func TestLocalResidencyChoiceIsASeparateFileFromAppliedResidency(t *testing.T) {
	dir := t.TempDir()
	if LocalResidencyChoicePath(dir) == AppliedResidencyPath(dir) {
		t.Fatal("the local-choice record and the applied-instruction record share a path")
	}
	if err := WriteAppliedResidency(dir, AppliedResidency{Value: "15m0s", AppliedAt: "2026-08-21T02:00:00Z"}); err != nil {
		t.Fatalf("write applied: %v", err)
	}
	if err := WriteLocalResidencyChoice(dir, LocalResidencyChoice{ChosenAt: "2026-08-21T09:15:04.5Z", Value: "45m0s"}); err != nil {
		t.Fatalf("write choice: %v", err)
	}
	applied, err := ReadAppliedResidency(dir)
	if err != nil || applied.Value != "15m0s" {
		t.Errorf("the local choice clobbered the applied instruction: %+v (%v)", applied, err)
	}
	choice, err := ReadLocalResidencyChoice(dir)
	if err != nil || choice.Value != "45m0s" {
		t.Errorf("the applied instruction clobbered the local choice: %+v (%v)", choice, err)
	}
}
