package main

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// What an elevated setup writes, and — more importantly — what it does
// NOT write. An unelevated `waired init` is an ordinary thing to run,
// and it must not replace a good reading with an empty one
// (waired-agent#459).
func TestGPUTopologyFrom(t *testing.T) {
	at := func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name    string
		gpus    []hardware.GPU
		wantOK  bool
		wantIDs []string
	}{
		{
			name: "an elevated run on a host with one integrated part",
			gpus: []hardware.GPU{
				{Vendor: "amd", PCIID: "1002:1586", Integrated: true, IntegratedKnown: true},
			},
			wantOK:  true,
			wantIDs: []string{"1002:1586"},
		},
		{
			name: "a discrete answer is worth writing too",
			gpus: []hardware.GPU{
				{Vendor: "amd", PCIID: "1002:744c", Integrated: false, IntegratedKnown: true},
			},
			wantOK:  true,
			wantIDs: []string{"1002:744c"},
		},
		{
			name: "two devices, only one of which could be read",
			gpus: []hardware.GPU{
				{Vendor: "nvidia", PCIID: "10de:2c34"},
				{Vendor: "amd", PCIID: "1002:13c0", Integrated: true, IntegratedKnown: true},
			},
			wantOK:  true,
			wantIDs: []string{"1002:13c0"},
		},
		{
			name: "an unelevated run: the node would not open, so nothing was read",
			gpus: []hardware.GPU{
				{Vendor: "amd", PCIID: "1002:1586"},
			},
		},
		{
			name: "a reading with no PCI pair to key it by",
			gpus: []hardware.GPU{
				{Vendor: "apple", Integrated: true, IntegratedKnown: true},
			},
		},
		{
			name: "no accelerator at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, ok := gpuTopologyFrom(hardware.Profile{GPUs: tc.gpus}, at)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (record %+v)", ok, tc.wantOK, rec)
			}
			if !ok {
				if len(rec.Devices) != 0 {
					t.Errorf("a record was built anyway: %+v", rec.Devices)
				}
				return
			}
			var got []string
			for _, d := range rec.Devices {
				got = append(got, d.PCIID)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("devices = %v, want %v", got, tc.wantIDs)
			}
			for i := range got {
				if got[i] != tc.wantIDs[i] {
					t.Errorf("devices = %v, want %v", got, tc.wantIDs)
					break
				}
			}
			if rec.MeasuredAt != "2026-09-20T12:00:00Z" {
				t.Errorf("MeasuredAt = %q, want the injected clock's time", rec.MeasuredAt)
			}
		})
	}
}

// A device the engine does not use by default is still a device whose
// reading is worth keeping — the reading is what put it in UnusedGPUs,
// and a daemon that could not read it back would put the device back in
// use (waired-agent#1484). The sv-mag shape: a discrete NVIDIA card in
// use, a 2-CU AMD iGPU set aside.
func TestGPUTopologyFrom_RecordsUnusedGPUs(t *testing.T) {
	at := func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) }
	prof := hardware.Profile{
		GPUs: []hardware.GPU{{Vendor: "nvidia", PCIID: "10de:2c34"}},
		UnusedGPUs: []hardware.UnusedGPU{{
			GPU:    hardware.GPU{Vendor: "amd", PCIID: "1002:13c0", Integrated: true, IntegratedKnown: true},
			Reason: "not used by default",
		}},
	}
	rec, ok := gpuTopologyFrom(prof, at)
	if !ok || len(rec.Devices) != 1 || rec.Devices[0].PCIID != "1002:13c0" || !rec.Devices[0].Integrated {
		t.Fatalf("record = %+v ok=%v, want the unused iGPU's reading", rec, ok)
	}
}

// The Apple Silicon row above is not an oversight: the reading is
// certain there (the architecture says so) and there is no PCI bus to
// key it by. Nothing is lost — that reading needs no privilege, so the
// daemon takes it for itself every time.
func TestGPUTopologyFrom_AppleSiliconNeedsNoRecord(t *testing.T) {
	_, ok := gpuTopologyFrom(hardware.Profile{GPUs: []hardware.GPU{
		{Vendor: "apple", Integrated: true, IntegratedKnown: true},
	}}, time.Now)
	if ok {
		t.Error("wrote a record for a part with no PCI pair; nothing could match it later")
	}
}
