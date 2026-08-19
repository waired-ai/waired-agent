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
//
// A record of today's behaviour, sourced from the measurement in
// waired-ai/waired-agent#863: the carve-out reading is ignored here in
// both positions, so every row's budget is the OS-visible RAM minus the
// OS deduction and every row's carve-out is 0. CarveOutVRAMMB is
// asserted because it is the figure hostfit.TotalMemoryMB adds to RAM —
// a non-zero one would restore the capacity overstatement #863 is about.
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
		name               string
		profile            Profile
		wantUnifiedMemory  bool
		wantUsableVRAMMB   int
		wantCarveOutVRAMMB int
	}{
		{
			name: "strix halo + AMD GPU with VRAM from registry",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: ramTotalGB128,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 64 * 1024}},
			},
			wantUnifiedMemory: true,
			// The 64 GB registry reading is not consulted: (128-2) GiB
			// clamped to the 96 GiB ceiling.
			wantUsableVRAMMB: strixHaloCap,
		},
		{
			// The measured failing host (Ryzen AI Max+ 395, 96 GB fixed to
			// the iGPU, ~31 GB left to the OS). The carve-out reading used
			// to win here and made a 76.3 GB model look like it fit; what
			// the load path actually had was the 29 GiB below.
			name: "strix halo carve-out: registry 96 GB + leftover RAM 31 GB -> 29 GiB",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: 31,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 96 * 1024}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  29 * 1024,
		},
		{
			// The measured working host: the same machine with the
			// carve-out shrunk to 512 MB, so the OS sees all of it.
			name: "strix halo with a 512 MB carve-out: the whole machine is the budget",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: 127,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 512}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  strixHaloCap,
		},
		{
			name: "strix halo + AMD GPU with no VRAM reading",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: ramTotalGB32,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 0}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  30 * 1024,
		},
		{
			name: "strix halo + no AMD GPU (registry walk failed)",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: ramTotalGB128,
				GPUs:       nil,
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  strixHaloCap,
		},
		{
			name: "registry over-reports above the 96 GB ceiling",
			profile: Profile{
				CPU:        CPUInfo{Model: strixHaloCPU},
				RAMTotalGB: 256,
				GPUs:       []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 200 * 1024}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  strixHaloCap,
		},
		{
			// The install-time available-memory measurement (#568) raises
			// the OS deduction above its 2 GB floor, and the budget must
			// follow it — hostfit's capacity gate uses the same figure.
			name: "a measured OS deduction lowers the budget",
			profile: Profile{
				CPU:                     CPUInfo{Model: strixHaloCPU},
				RAMTotalGB:              31,
				RAMAvailableAtInstallGB: 24,
				GPUs:                    []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 96 * 1024}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  24 * 1024,
		},
		{
			// Inverted vs. the pre-#863 table, which returned the ceiling:
			// a failed RAM probe used to publish the largest budget this
			// code can express.
			name: "strix halo with no RAM reading -> unknown, not the ceiling",
			profile: Profile{
				CPU:  CPUInfo{Model: strixHaloCPU},
				GPUs: []GPU{{Vendor: "amd", Model: amdGPUModel, VRAMTotalMB: 96 * 1024}},
			},
			wantUnifiedMemory: true,
			wantUsableVRAMMB:  0,
		},
		{
			name: "phoenix APU is not strix halo -> no-op",
			profile: Profile{
				CPU:        CPUInfo{Model: phoenixCPU},
				RAMTotalGB: 64,
				GPUs:       []GPU{{Vendor: "amd", Model: "AMD Radeon 780M Graphics", VRAMTotalMB: 8 * 1024}},
			},
			wantUnifiedMemory: false,
			wantUsableVRAMMB:  0,
		},
		{
			name: "Intel CPU -> no-op",
			profile: Profile{
				CPU:        CPUInfo{Model: intelCPU},
				RAMTotalGB: 32,
				GPUs:       []GPU{{Vendor: "nvidia", Model: nvidiaGPU, VRAMTotalMB: 24 * 1024}},
			},
			wantUnifiedMemory: false,
			wantUsableVRAMMB:  0,
		},
		{
			name: "empty CPU model -> no-op",
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
			if p.CarveOutVRAMMB != c.wantCarveOutVRAMMB {
				t.Errorf("CarveOutVRAMMB = %d, want %d", p.CarveOutVRAMMB, c.wantCarveOutVRAMMB)
			}
			// The registry reading is still published for diagnostics: it
			// is the fact that explains the budget above to an operator.
			if len(c.profile.GPUs) > 0 && p.GPUs[0].VRAMTotalMB != c.profile.GPUs[0].VRAMTotalMB {
				t.Errorf("GPUs[0].VRAMTotalMB = %d, want the untouched reading %d",
					p.GPUs[0].VRAMTotalMB, c.profile.GPUs[0].VRAMTotalMB)
			}
		})
	}
}

// TestDefaultRAM_Windows exercises the real GlobalMemoryStatusEx
// reader on the machine the test runs on — the parity gap called out
// on waired-agent#568 (linux and darwin had real-probe coverage,
// windows had none). Bounds only: the figures are the host's own.
func TestDefaultRAM_Windows(t *testing.T) {
	totalGB, availGB, err := defaultRAM(context.Background())
	if err != nil {
		t.Fatalf("defaultRAM: %v", err)
	}
	if totalGB <= 0 {
		t.Fatalf("totalGB = %d, want > 0", totalGB)
	}
	if availGB <= 0 || availGB > totalGB {
		t.Fatalf("availGB = %d, want in (0, %d]", availGB, totalGB)
	}
}
