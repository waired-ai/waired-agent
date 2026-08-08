package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostMemoryRecord_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := HostMemoryRecord{AvailableGB: 11, MeasuredAt: "2026-08-09T00:00:00Z", AgentVersion: "1.2.3"}
	if err := WriteHostMemory(dir, rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadHostMemory(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != rec {
		t.Errorf("round trip: got %+v, want %+v", got, rec)
	}
}

// Missing and corrupt files are both the zero record and no error: the
// record is advisory (it re-measures), and refusing to boot over it
// would be the worse failure — the ReadHostSpeed contract.
func TestHostMemoryRecord_MissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadHostMemory(dir)
	if err != nil || got != (HostMemoryRecord{}) {
		t.Fatalf("missing file: got %+v err %v, want zero record and nil", got, err)
	}
	path := HostMemoryPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ReadHostMemory(dir)
	if err != nil || got != (HostMemoryRecord{}) {
		t.Fatalf("corrupt file: got %+v err %v, want zero record and nil", got, err)
	}
}
