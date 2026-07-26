package main

import (
	"encoding/json"
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

// notPublishedByAgent lists summary fields the agent deliberately leaves
// unset. It is empty, and an addition needs a stated reason: every field
// on these two structs exists because a consumer asked for a fact only
// the agent can observe, so "nobody fills it in" is the defect this file
// exists to prevent, not a design choice.
var notPublishedByAgent = map[string]bool{}

// TestHardwareSummaryFor_PublishesEveryWireField guards the bug class
// rather than the three fields: a field added to the broadcast summary
// and read by the control plane, but never wired into the producer,
// fails here instead of shipping green (#180 was the third instance).
// It is a completeness check, so it deliberately asserts nothing about
// the values — the tables above own those.
func TestHardwareSummaryFor_PublishesEveryWireField(t *testing.T) {
	// No real host looks like this — an Apple GPU does not report a CUDA
	// compute capability. That is the point: every fact the profile can
	// carry is present, so any zero in the output is the producer
	// dropping it rather than the host not having it.
	prof := hardware.Profile{
		RAMTotalGB:    64,
		UnifiedMemory: true,
		UsableVRAMMB:  49152,
		GPUs: []hardware.GPU{{
			Vendor:        "apple",
			Model:         "Apple M3 Max",
			VRAMTotalMB:   65536,
			ComputeCap:    "8.9",
			DriverVersion: "535.171.04",
			UUID:          "GPU-12345678",
		}},
	}
	summary := hardwareSummaryFor(prof)
	if summary == nil {
		t.Fatal("hardwareSummaryFor() = nil for a host with every fact populated")
	}
	assertEveryFieldPublished(t, "HardwareSummary", reflect.ValueOf(*summary))
	if len(summary.GPUs) == 0 {
		t.Fatal("HardwareSummary.GPUs is empty for a host with a GPU")
	}
	assertEveryFieldPublished(t, "HardwareGPUSummary", reflect.ValueOf(summary.GPUs[0]))
}

func assertEveryFieldPublished(t *testing.T, typeName string, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		name := typeName + "." + f.Name
		if notPublishedByAgent[name] {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("%s is zero for a fully-populated profile: hardwareSummaryFor does not "+
				"publish it. Wire it up in hardware_summary.go, or record it in "+
				"notPublishedByAgent with the reason the agent does not send it.", name)
		}
	}
}

// TestHardwareSummaryFor_WireForm pins the exact bytes the agent puts on
// the wire for the host shapes the control plane's onboarding fixtures
// stand in for (waired-ai/waired,
// internal/controlplane/api/management_device_model_catalog_unit_test.go).
// Those fixtures are captures of these strings: hand-written ones are
// what let #180 ship — they carried vendor / unified_memory /
// usable_vram_mb for weeks while no agent sent any of them, so the
// consumer's tests stayed green against a producer that did not exist.
// Changing a string here means the CP captures must be refreshed.
func TestHardwareSummaryFor_WireForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		prof hardware.Profile
		want string
	}{
		{
			name: "discrete nvidia",
			prof: hardware.Profile{
				RAMTotalGB: 64,
				GPUs: []hardware.GPU{{
					Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090",
					VRAMTotalMB: 24564, ComputeCap: "8.9",
					DriverVersion: "535.171.04", UUID: "GPU-12345678",
				}},
			},
			want: `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
				`"compute_cap":"8.9","vendor":"nvidia"}],"ram_total_gb":64}`,
		},
		{
			// Under the CP's vLLM VRAM floor: the consumer needs a real
			// vendor token here, or the shortfall reads as "no GPU".
			name: "nvidia below the vllm vram floor",
			prof: hardware.Profile{
				RAMTotalGB: 16,
				GPUs: []hardware.GPU{{
					Vendor: "nvidia", Model: "NVIDIA T600",
					VRAMTotalMB: 4096, ComputeCap: "7.5",
				}},
			},
			want: `{"gpus":[{"model":"NVIDIA T600","vram_total_mb":4096,` +
				`"compute_cap":"7.5","vendor":"nvidia"}],"ram_total_gb":16}`,
		},
		{
			// Both ram_total_gb and the GPU's vram_total_mb overstate the
			// budget; usable_vram_mb is the only figure a fit may use.
			name: "unified memory",
			prof: hardware.Profile{
				RAMTotalGB: 16, UnifiedMemory: true, UsableVRAMMB: 12288,
				GPUs: []hardware.GPU{{
					Vendor: "apple", Model: "Apple M3", VRAMTotalMB: 16384,
				}},
			},
			want: `{"gpus":[{"model":"Apple M3","vram_total_mb":16384,"vendor":"apple"}],` +
				`"ram_total_gb":16,"unified_memory":true,"usable_vram_mb":12288}`,
		},
		{
			name: "cpu only",
			prof: hardware.Profile{RAMTotalGB: 32},
			want: `{"ram_total_gb":32}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(hardwareSummaryFor(tc.prof))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := string(data); got != tc.want {
				t.Errorf("published wire form drifted:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}
