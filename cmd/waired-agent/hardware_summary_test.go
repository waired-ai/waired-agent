package main

import (
	"reflect"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestHardwareSummaryFor walks the host shapes whose difference is the
// whole point of the host-fit fields. The discrete case doubles as the
// no-drift check: a host with none of the new facts must still produce
// exactly the pre-addition summary.
func TestHardwareSummaryFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		prof hardware.Profile
		want *signer.HardwareSummary
	}{
		{
			name: "discrete nvidia carries the vendor token",
			prof: hardware.Profile{
				RAMTotalGB: 64,
				GPUs: []hardware.GPU{{
					Vendor:        "nvidia",
					Model:         "NVIDIA GeForce RTX 4090",
					VRAMTotalMB:   24564,
					ComputeCap:    "8.9",
					DriverVersion: "535.171.04",
					UUID:          "GPU-12345678",
				}},
			},
			want: &signer.HardwareSummary{
				RAMTotalGB: 64,
				GPUs: []signer.HardwareGPUSummary{{
					Model:       "NVIDIA GeForce RTX 4090",
					VRAMTotalMB: 24564,
					ComputeCap:  "8.9",
					Vendor:      "nvidia",
				}},
			},
		},
		{
			// The case the fields exist for: RAMTotalGB and VRAMTotalMB
			// both overstate the budget, and only UsableVRAMMB is the
			// number a min_vram_mb comparison may use.
			name: "unified memory publishes the usable budget",
			prof: hardware.Profile{
				RAMTotalGB:    64,
				UnifiedMemory: true,
				UsableVRAMMB:  49152,
				GPUs: []hardware.GPU{{
					Vendor:      "apple",
					Model:       "Apple M3 Max",
					VRAMTotalMB: 65536,
				}},
			},
			want: &signer.HardwareSummary{
				RAMTotalGB:    64,
				UnifiedMemory: true,
				UsableVRAMMB:  49152,
				GPUs: []signer.HardwareGPUSummary{{
					Model:       "Apple M3 Max",
					VRAMTotalMB: 65536,
					Vendor:      "apple",
				}},
			},
		},
		{
			name: "cpu-only still reports RAM",
			prof: hardware.Profile{RAMTotalGB: 16},
			want: &signer.HardwareSummary{RAMTotalGB: 16},
		},
		{
			// Nothing worth saying: keep the field off the wire rather
			// than publishing a zero-valued object.
			name: "unprofilable host reports nothing",
			prof: hardware.Profile{},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hardwareSummaryFor(tc.prof)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hardwareSummaryFor()\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestHardwareSummaryFor_MatchesEffectiveVRAM pins the invariant the
// control plane relies on: whatever the agent's own picker would budget
// via EffectiveVRAMMB(), a consumer can recompute from the published
// summary alone.
func TestHardwareSummaryFor_MatchesEffectiveVRAM(t *testing.T) {
	for _, prof := range []hardware.Profile{
		{RAMTotalGB: 64, UnifiedMemory: true, UsableVRAMMB: 49152,
			GPUs: []hardware.GPU{{Vendor: "apple", Model: "Apple M3 Max", VRAMTotalMB: 65536}}},
		{RAMTotalGB: 64,
			GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24564}}},
	} {
		s := hardwareSummaryFor(prof)
		if s == nil {
			t.Fatalf("summary is nil for %+v", prof)
		}
		// The consumer-side rule, stated in the proto doc comment.
		budget := 0
		if s.UnifiedMemory && s.UsableVRAMMB > 0 {
			budget = s.UsableVRAMMB
		} else if len(s.GPUs) > 0 {
			budget = s.GPUs[0].VRAMTotalMB
		}
		if want := prof.EffectiveVRAMMB(); budget != want {
			t.Errorf("recomputed budget = %d, want EffectiveVRAMMB() = %d", budget, want)
		}
	}
}
