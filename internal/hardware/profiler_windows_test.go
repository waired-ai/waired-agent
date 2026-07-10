//go:build windows

package hardware

import (
	"context"
	"testing"
)

// TestDefaultUMA_Windows table-tests the Strix Halo UMA detector on
// Windows. The function is pure given a Profile so we can build the
// fixture profiles directly without touching the registry; the OS-
// specific bits (registry read of HardwareInformation.qwMemorySize) are
// already absorbed into Profile.GPUs by the time defaultUMA runs in
// the real call path.
func TestDefaultUMA_Windows(t *testing.T) {
	const (
		strixHaloCPU  = "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"
		phoenixCPU    = "AMD Ryzen 9 7940HS w/ Radeon 780M Graphics"
		intelCPU      = "13th Gen Intel(R) Core(TM) i7-13700K"
		amdGPUModel   = "AMD Radeon(TM) 8060S Graphics"
		nvidiaGPU     = "NVIDIA GeForce RTX 4090"
		strixHaloCap  = 96 * 1024
		ramTotalGB128 = 128
		ramTotalGB32  = 32
	)
	cases := []struct {
		name              string
		profile           Profile
		wantUnifiedMemory bool
		wantUsableVRAMMB  int
	}{
		{
			name: "strix halo + AMD GPU with VRAM from registry",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: ramTotalGB128,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 64 * 1024}},
			},
			wantUnifiedMemory: true,
			// carve-out reading present → min(64 GB, cap 96 GB) = 64 GB.
			// The 75%-of-RAM heuristic is NOT consulted when the registry
			// value is available.
			wantUsableVRAMMB: 64 * 1024,
		},
		{
			// Regression for the BIOS carve-out machine (Ryzen AI Max+ 395
			// with 96 GB fixed to the iGPU). The OS sees only the ~31 GB
			// leftover as system RAM, so the old min(96, 75%×31≈23, 96)
			// wrongly clamped to ~23 GB and hid the pool. The carve-out
			// reading (qwMemorySize = 96 GB) must win.
			name: "strix halo carve-out: registry 96 GB + leftover RAM 31 GB → 96 GB",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: 31,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 96 * 1024}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  strixHaloCap,
		},
		{
			name: "strix halo + AMD GPU with no VRAM falls back to 75% heuristic",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: ramTotalGB32,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 0}},
			},
			wantUnifiedMemory: true,
			// min(heuristic = 32 * 0.75 * 1024 = 24576, cap 98304) = 24576
			wantUsableVRAMMB: 24 * 1024,
		},
		{
			name: "strix halo + no AMD GPU (registry walk failed) still heuristic-only",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: ramTotalGB128,
				GPUs:       nil,
			},
			wantUnifiedMemory: true,
			// min(heuristic = 128 * 0.75 * 1024 = 98304, cap 98304) = 98304
			wantUsableVRAMMB: strixHaloCap,
		},
		{
			name: "registry over-reports above 96 GB cap → capped at 96 GB",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: 256,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 200 * 1024}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  strixHaloCap,
		},
		{
			name: "phoenix APU is not strix halo → no-op",
			profile: Profile{
				CPU:        CPUInfo{Model: phoenixCPU},
				RAMTotalGB: 64,
				GPUs:       []GPU{{Vendor: "amd", Model: "AMD Radeon 780M Graphics", VRAMTotalMB: 8 * 1024}},
			},
			wantUnifiedMemory: false,
			wantUsableVRAMMB:  0,
		},
		{
			name: "Intel CPU → no-op",
			profile: Profile{
				CPU:        CPUInfo{Model: intelCPU},
				RAMTotalGB: 32,
				GPUs:       []GPU{{Vendor: "nvidia", Model: nvidiaGPU, VRAMTotalMB: 24 * 1024}},
			},
			wantUnifiedMemory: false,
			wantUsableVRAMMB:  0,
		},
		{
			name: "empty CPU model → no-op",
			profile: Profile{
				CPU:        CPUInfo{Model: ""},
				RAMTotalGB: 128,
			},
			wantUnifiedMemory: false,
			wantUsableVRAMMB:  0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.profile
			defaultUMA(context.Background(), &p)
			if p.UnifiedMemory != c.wantUnifiedMemory {
				t.Errorf("UnifiedMemory = %v, want %v", p.UnifiedMemory, c.wantUnifiedMemory)
			}
			if p.UsableVRAMMB != c.wantUsableVRAMMB {
				t.Errorf("UsableVRAMMB = %d, want %d", p.UsableVRAMMB, c.wantUsableVRAMMB)
			}
		})
	}
}
