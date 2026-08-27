package hardware

import (
	"context"
	"path/filepath"
	"testing"
)

func fixedDetector(gpus ...GPU) VendorDetector {
	return func(context.Context) ([]GPU, Accelerators, error) {
		return gpus, Accelerators{}, nil
	}
}

func tightestOf(t *testing.T, gpus ...GPU) (int, bool) {
	t.Helper()
	got, _, _ := composeDetectors(context.Background(), []VendorDetector{fixedDetector(gpus...)})
	mb, ok := 0, false
	for _, g := range got {
		if g.VRAMFreeMB <= 0 {
			continue
		}
		if !ok || g.VRAMFreeMB < mb {
			mb, ok = g.VRAMFreeMB, true
		}
	}
	return mb, ok
}

func TestTightestGPUFreeMB(t *testing.T) {
	cases := []struct {
		name    string
		gpus    []GPU
		wantMB  int
		wantOK  bool
		comment string
	}{
		{
			name:   "single card",
			gpus:   []GPU{{Vendor: "nvidia", VRAMTotalMB: 24467, VRAMFreeMB: 491}},
			wantMB: 491, wantOK: true,
		},
		{
			name: "smallest across devices",
			gpus: []GPU{
				{Vendor: "nvidia", VRAMTotalMB: 24467, VRAMFreeMB: 3141},
				{Vendor: "nvidia", VRAMTotalMB: 24467, VRAMFreeMB: 945},
			},
			wantMB: 945, wantOK: true,
		},
		{
			name: "a device with a total but no free figure is not a zero",
			gpus: []GPU{
				{Vendor: "amd", VRAMTotalMB: 16384},
				{Vendor: "nvidia", VRAMTotalMB: 24467, VRAMFreeMB: 945},
			},
			wantMB: 945, wantOK: true,
		},
		{
			name: "nothing reported a free figure",
			// Every AMD host today: rocm-smi's CSV has no used/free column.
			gpus:   []GPU{{Vendor: "amd", VRAMTotalMB: 16384}},
			wantMB: 0, wantOK: false,
		},
		{
			name:   "no GPU at all",
			gpus:   nil,
			wantMB: 0, wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mb, ok := tightestOf(t, tc.gpus...)
			if mb != tc.wantMB || ok != tc.wantOK {
				t.Errorf("= (%d, %v), want (%d, %v)", mb, ok, tc.wantMB, tc.wantOK)
			}
		})
	}
}

// TestLiveGPUFree_IsNotTheFrozenProfileReading pins the reason this
// function exists: Profiler.Profile replays the FIRST free reading
// forever (freezeVRAMFree, see vram_free_freeze_test.go), so a post-load
// check that read it would read a boot-time number and never fire.
func TestLiveGPUFree_IsNotTheFrozenProfileReading(t *testing.T) {
	reading := 3969
	gpuFn := func(context.Context) ([]GPU, Accelerators, error) {
		return []GPU{{Vendor: "nvidia", UUID: "GPU-1", VRAMTotalMB: 24467, VRAMFreeMB: reading}}, Accelerators{}, nil
	}
	p := NewProfiler(filepath.Join(t.TempDir(), "cache.json"),
		WithTTL(0),
		// Seal the per-OS UMA hook: on a darwin runner it would overwrite
		// the fixture with the real machine's unified memory.
		WithUMA(func(context.Context, *Profile) {}),
		WithGPU(gpuFn))

	first := p.Profile(context.Background())
	if got := first.GPUs[0].VRAMFreeMB; got != 3969 {
		t.Fatalf("first profile free = %d, want 3969", got)
	}

	// The model loads and takes the card down to 491 MiB free.
	reading = 491

	if got := p.Profile(context.Background()).GPUs[0].VRAMFreeMB; got != 3969 {
		t.Fatalf("profile free = %d, want the frozen 3969 (the freeze is a product contract)", got)
	}

	live, _, _ := gpuFn(context.Background())
	if live[0].VRAMFreeMB != 491 {
		t.Fatalf("live free = %d, want 491 — the live read must see the load", live[0].VRAMFreeMB)
	}
}
