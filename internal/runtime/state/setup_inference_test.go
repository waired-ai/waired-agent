package state

import "testing"

func TestSetupInferenceRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file: the ordinary state of every device no wizard has
	// answered for — zero record, no error.
	rec, err := ReadSetupInference(dir)
	if err != nil || rec.Value != "" {
		t.Fatalf("missing file: got %+v err=%v, want zero and nil", rec, err)
	}

	want := SetupInference{Value: "off", AppliedAt: "2026-08-09T08:00:00Z"}
	if err := WriteSetupInference(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec, err = ReadSetupInference(dir)
	if err != nil || rec != want {
		t.Fatalf("read back %+v err=%v, want %+v", rec, err, want)
	}

	// A later answer replaces the record wholesale.
	if err := WriteSetupInference(dir, SetupInference{Value: "on"}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	rec, _ = ReadSetupInference(dir)
	if rec.Value != "on" {
		t.Fatalf("rewrite read back %+v, want on", rec)
	}
}

// "Never acted" is the absence of the file — an empty value on disk
// would read back as a record of nothing, so the write refuses it.
func TestSetupInference_EmptyValueIsRejected(t *testing.T) {
	if err := WriteSetupInference(t.TempDir(), SetupInference{}); err == nil {
		t.Fatal("an empty value must be rejected, not written")
	}
}
