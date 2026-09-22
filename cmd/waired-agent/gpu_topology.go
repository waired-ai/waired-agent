package main

import (
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// The GPU topology reading an elevated setup took, read back for a daemon
// that could not take it itself (waired-agent#459).
//
// The daemon opens /dev/dri/renderD* itself where its service user is
// in the `render` group, which the installer arranges (#1535). Where it
// is not, its own reading of "is this accelerator's memory the system's
// memory?" is unknown on Linux. `sudo waired init` can open the node,
// takes the reading once and writes gpu-topology.json; this is the read
// side, and a live reading still wins over it.
//
// Read per profiler construction rather than cached in a package
// variable, for the reason hostMemoryMeasurement is: the same process
// builds several profilers and a re-run of setup can replace the file
// underneath a long-lived daemon. A file read per construction is
// cheaper than a stale answer.

// persistedGPUIntegration returns the lookup hardware.WithPersistedIntegration
// takes: given an accelerator's PCI vendor:device pair, what an earlier
// elevated run found.
//
// A missing or unreadable file yields a lookup that answers "no reading"
// for everything, which is what the host already reports without it.
func persistedGPUIntegration(stateDir string) func(pciID string) (bool, bool) {
	return func(pciID string) (bool, bool) {
		rec, err := state.ReadGPUTopology(stateDir)
		if err != nil {
			return false, false
		}
		return rec.IntegratedFor(pciID)
	}
}

// persistedGPUVRAM is the same record's memory reading, for the parts
// whose size the daemon may not be able to read either
// (waired-agent#1483).
func persistedGPUVRAM(stateDir string) func(pciID string) (int, bool) {
	return func(pciID string) (int, bool) {
		rec, err := state.ReadGPUTopology(stateDir)
		if err != nil {
			return 0, false
		}
		return rec.VRAMFor(pciID)
	}
}
