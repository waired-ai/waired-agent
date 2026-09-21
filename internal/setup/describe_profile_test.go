package setup

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// The selection note's one-line hardware summary. The rows added for
// waired-agent#1484 name a GPU the engine does not use by default, so a
// "CPU host" line on a machine with a visible iGPU says why.
func TestDescribeProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		hw   hardware.Profile
		want string
	}{
		{
			name: "no GPU at all",
			hw:   hardware.Profile{RAMTotalGB: 16},
			want: "CPU host (16 GB RAM)",
		},
		{
			name: "only an iGPU the engine leaves off",
			hw: hardware.Profile{RAMTotalGB: 32, UnusedGPUs: []hardware.UnusedGPU{
				{GPU: hardware.GPU{Vendor: "amd", Model: "AMD Radeon 780M Graphics"}},
			}},
			want: "CPU host (32 GB RAM; the engine does not use AMD Radeon 780M Graphics by default)",
		},
		{
			name: "an unnamed one falls back to the vendor",
			hw: hardware.Profile{RAMTotalGB: 32, UnusedGPUs: []hardware.UnusedGPU{
				{GPU: hardware.GPU{Vendor: "intel"}},
			}},
			want: "CPU host (32 GB RAM; the engine does not use intel by default)",
		},
		{
			// waired-agent#1483: an Intel card whose memory was not read
			// is named differently — the engine may use it, so the note
			// must not say it does not.
			name: "a card whose memory was not read",
			hw: hardware.Profile{RAMTotalGB: 64, UnusedGPUs: []hardware.UnusedGPU{
				{GPU: hardware.GPU{Vendor: "intel", Model: "Intel GPU 8086:e20b"}, MemoryUnread: true},
			}},
			want: "CPU host (64 GB RAM; the memory size of Intel GPU 8086:e20b is not known)",
		},
		{
			name: "a card in use is described as before, the unused iGPU unmentioned",
			hw: hardware.Profile{
				RAMTotalGB: 128,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}},
				UnusedGPUs: []hardware.UnusedGPU{{GPU: hardware.GPU{Vendor: "amd", Model: "AMD Radeon Graphics"}}},
			},
			want: "NVIDIA RTX PRO 4000 Blackwell (24467 MB VRAM)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeProfile(tc.hw); got != tc.want {
				t.Errorf("describeProfile = %q, want %q", got, tc.want)
			}
		})
	}
}
