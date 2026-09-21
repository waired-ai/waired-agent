package main

import (
	"context"
	"time"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Taking the GPU topology reading during `sudo waired init`, because the
// daemon cannot take it (waired-agent#459).
//
// On Linux the fact — is this accelerator's memory the system's memory?
// — is behind /dev/dri/renderD*, which is mode 0660 root:render while
// the unit runs as User=waired with no supplementary groups. An elevated
// setup can open it; the service never can. So the reading is taken here
// and persisted, the way host-memory.json persists a measurement the
// daemon can only take under conditions it has to arrange.
//
// `init` rather than the installer, because a re-setup is the supported
// way to re-take it — which is also what makes a swapped GPU stop being
// described by a stale entry.
//
// BEST EFFORT, ALWAYS. Nothing about enrolling a device depends on this,
// and an unelevated `waired init` is a perfectly ordinary thing to run.
// A failure here must not fail init, and an unelevated run must not
// wipe a reading an elevated one already took.

// gpuTopologyFrom builds the record for a profile, or ok=false when the
// profile says nothing worth writing.
//
// A device is recorded only when BOTH its reading and its PCI pair are
// in hand: without the pair there is no key to match it by later, and
// without the reading there is nothing to say. Recording neither is the
// honest outcome on a host where the node would not open — and is why
// an unelevated run writes nothing rather than writing an empty record
// over a good one.
//
// Every DETECTED device, including the ones the engine will not use: the
// reading is a fact about the hardware, and it is exactly the reading
// that decides a device is unused (waired-agent#1484) — a daemon that
// could not read it again would put the device back in use.
func gpuTopologyFrom(prof hardware.Profile, now func() time.Time) (state.GPUTopologyRecord, bool) {
	var devices []state.GPUTopologyDevice
	for _, g := range prof.DetectedGPUs() {
		if !g.IntegratedKnown || g.PCIID == "" {
			continue
		}
		devices = append(devices, state.GPUTopologyDevice{
			PCIID:      g.PCIID,
			Integrated: g.Integrated,
			// Carried for the parts whose size is also behind the render
			// node: an Intel discrete card on Linux (waired-agent#1483).
			VRAMTotalMB: g.VRAMTotalMB,
		})
	}
	if len(devices) == 0 {
		return state.GPUTopologyRecord{}, false
	}
	return state.GPUTopologyRecord{
		Devices:      devices,
		MeasuredAt:   now().UTC().Format(time.RFC3339),
		AgentVersion: buildinfo.Version,
	}, true
}

// persistGPUTopology takes the reading and writes it, returning any
// write error for the caller to log. A profile that yields no record is
// not an error: it is the ordinary result of an unelevated run.
func persistGPUTopology(ctx context.Context, stateDir string, now func() time.Time) error {
	prof := hardware.NewProfiler("").Profile(ctx)
	rec, ok := gpuTopologyFrom(prof, now)
	if !ok {
		return nil
	}
	return state.WriteGPUTopology(stateDir, rec)
}

// persistGPUTopologyFn is the seam init calls, so a test can drive the
// "it failed" branch without a host that fails.
//
// A `var fn = realFn` seam needs a table test on realFn or the real one
// is never called by any test (CLAUDE.md §Test discipline); that is
// TestGPUTopologyFrom below, which exercises everything persistGPUTopology
// does except the profile read and the file write.
var persistGPUTopologyFn = persistGPUTopology
