package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGPUTopologyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := GPUTopologyRecord{
		Devices: []GPUTopologyDevice{
			{PCIID: "1002:1586", Integrated: true},
			{PCIID: "10de:2c34", Integrated: false},
		},
		MeasuredAt:   "2026-09-20T12:00:00Z",
		AgentVersion: "0.0.3-rc6",
	}
	if err := WriteGPUTopology(dir, want); err != nil {
		t.Fatalf("WriteGPUTopology: %v", err)
	}
	got, err := ReadGPUTopology(dir)
	if err != nil {
		t.Fatalf("ReadGPUTopology: %v", err)
	}
	if len(got.Devices) != 2 || got.Devices[0] != want.Devices[0] || got.Devices[1] != want.Devices[1] {
		t.Errorf("Devices = %+v, want %+v", got.Devices, want.Devices)
	}
	if got.MeasuredAt != want.MeasuredAt || got.AgentVersion != want.AgentVersion {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A missing or corrupt file is a zero record and no error. The record is
// advisory — the host falls back to reading nothing, which is what it
// does without the file at all — and refusing to boot over it would be
// the worse failure. Same rule as ReadHostMemory.
func TestReadGPUTopologyTolerates(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		got, err := ReadGPUTopology(t.TempDir())
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(got.Devices) != 0 {
			t.Errorf("Devices = %+v, want none", got.Devices)
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(GPUTopologyPath(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(GPUTopologyPath(dir), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ReadGPUTopology(dir)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(got.Devices) != 0 {
			t.Errorf("Devices = %+v, want none", got.Devices)
		}
	})
}

// The PCI pair is the key, and that is what makes a swapped card stop
// being described by the old reading (waired-agent#459).
func TestGPUTopologyIntegratedFor(t *testing.T) {
	rec := GPUTopologyRecord{Devices: []GPUTopologyDevice{
		{PCIID: "1002:1586", Integrated: true},
		{PCIID: "10de:2c34", Integrated: false},
	}}
	for _, tc := range []struct {
		name           string
		pciID          string
		wantIntegrated bool
		wantOK         bool
	}{
		{name: "the recorded integrated part", pciID: "1002:1586", wantIntegrated: true, wantOK: true},
		{name: "the recorded discrete part", pciID: "10de:2c34", wantIntegrated: false, wantOK: true},
		{name: "a card that was not there when the reading was taken", pciID: "10de:2b85"},
		{name: "no pair at all (Apple Silicon)", pciID: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			integrated, ok := rec.IntegratedFor(tc.pciID)
			if ok != tc.wantOK || integrated != tc.wantIntegrated {
				t.Errorf("IntegratedFor(%q) = (%v, %v), want (%v, %v)",
					tc.pciID, integrated, ok, tc.wantIntegrated, tc.wantOK)
			}
		})
	}
}

// An empty record answers "no reading" rather than "discrete" — the
// distinction the three-state merge in internal/hardware rests on.
func TestGPUTopologyZeroRecordSaysNothing(t *testing.T) {
	var rec GPUTopologyRecord
	if _, ok := rec.IntegratedFor("1002:1586"); ok {
		t.Error("a record that was never written answered; it must say nothing")
	}
}
