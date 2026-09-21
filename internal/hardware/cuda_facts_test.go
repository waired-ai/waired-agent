package hardware

import "testing"

// The CUDA reading is attached only where the pairing is certain
// (waired-agent#1482): one NVIDIA entry, one CUDA device.
func TestCUDAFactsFor(t *testing.T) {
	n1x := cudaDevice{integrated: true, totalMB: 46477}
	for _, tc := range []struct {
		name string
		gpus []GPU
		i    int
		devs []cudaDevice
		ok   bool
	}{
		{"one NVIDIA device, one CUDA device", []GPU{{Vendor: "nvidia"}}, 0, []cudaDevice{n1x}, true},
		{"not the NVIDIA entry", []GPU{{Vendor: "nvidia"}, {Vendor: "intel"}}, 1, []cudaDevice{n1x}, false},
		{"an NVIDIA entry beside another vendor's", []GPU{{Vendor: "intel"}, {Vendor: "NVIDIA"}}, 1, []cudaDevice{n1x}, true},
		// CUDA orders devices fastest-first, the profile does not: two of
		// each cannot be paired honestly.
		{"two NVIDIA cards", []GPU{{Vendor: "nvidia"}, {Vendor: "nvidia"}}, 0, []cudaDevice{n1x, n1x}, false},
		{"one card, two CUDA devices", []GPU{{Vendor: "nvidia"}}, 0, []cudaDevice{n1x, n1x}, false},
		{"no CUDA device", []GPU{{Vendor: "nvidia"}}, 0, nil, false},
		{"index out of range", []GPU{{Vendor: "nvidia"}}, 3, []cudaDevice{n1x}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := cudaFactsFor(&Profile{GPUs: tc.gpus}, tc.i, tc.devs)
			if ok != tc.ok {
				t.Errorf("cudaFactsFor ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}

// The N1X measured by unsloth#11208: 54.2 GiB of RAM, 8128 MiB reported by
// nvidia-smi, 46477 MiB allocatable by CUDA. RAM less the OS reserve
// would promise more than the GPU can hold; the CUDA pool caps it.
func TestNVIDIAUnifiedBudgetIsCappedByTheCUDAPool(t *testing.T) {
	n1x := &Profile{
		RAMTotalGB: 54,
		GPUs: []GPU{{
			Vendor: "nvidia", Model: "NVIDIA RTX Spark", VRAMTotalMB: 8128,
			Integrated: true, IntegratedKnown: true, CUDATotalMemMB: 46477,
		}},
	}
	usable, carveOut, ok := unifiedBudgetFor("windows", n1x)
	if !ok || usable != 46477 || carveOut != 0 {
		t.Fatalf("unifiedBudgetFor = (%d, %d, %v), want (46477, 0, true)", usable, carveOut, ok)
	}

	// Without the CUDA reading (Linux, or an old driver) the budget is
	// RAM less the reserve, as #1479 left it.
	n1x.GPUs[0].CUDATotalMemMB = 0
	uncapped, _, _ := unifiedBudgetFor("windows", n1x)
	if uncapped <= 46477 {
		t.Errorf("uncapped budget = %d, want RAM less the reserve, above the CUDA pool", uncapped)
	}
}
