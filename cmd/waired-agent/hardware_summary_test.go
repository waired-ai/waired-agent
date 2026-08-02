package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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

// TestHardwareSummaryFn_ResamplesAfterTheTTL is the second half of #387
// (product contract): the summary used to be sampled once at boot and
// captured as a pointer, so a host that gained a GPU or a driver kept
// reporting the old answer until the daemon restarted.
//
// The getter now re-derives from a live Profiler, whose own TTL paces the
// re-detection — no extra ticker, and a detection cost of one probe per
// hardwareResampleInterval rather than one per probe tick. The seam is
// the Profiler's clock, so this runs in microseconds instead of waiting
// out a real interval.
func TestHardwareSummaryFn_ResamplesAfterTheTTL(t *testing.T) {
	now := time.Now()
	gpus := []hardware.GPU{}
	p := hardware.NewProfiler("",
		hardware.WithTTL(hardwareResampleInterval),
		hardware.WithNow(func() time.Time { return now }),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return gpus, hardware.Accelerators{}, nil
		}),
		hardware.WithRAM(func(context.Context) (int, int, error) { return 64, 32, nil }),
		hardware.WithStorage(func(context.Context, string) (int64, error) { return 1 << 40, nil }),
		hardware.WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		hardware.WithUMA(func(context.Context, *hardware.Profile) {}),
	)
	get := hardwareSummaryFn(context.Background(), p)
	if get == nil {
		t.Fatal("hardwareSummaryFn returned nil for a real profiler")
	}

	if got := get(); got == nil || len(got.GPUs) != 0 {
		t.Fatalf("first sample = %+v, want a GPU-less host", got)
	}

	// The driver lands. Inside the TTL the answer must not change — that
	// is the cache doing its job, and it is what keeps the per-tick call
	// cheap.
	gpus = []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564}}
	now = now.Add(hardwareResampleInterval - time.Second)
	if got := get(); got == nil || len(got.GPUs) != 0 {
		t.Errorf("re-detected inside the TTL: %+v", got)
	}

	// Past it, the host republishes what it now is — without a restart.
	now = now.Add(2 * time.Second)
	got := get()
	if got == nil || len(got.GPUs) != 1 {
		t.Fatalf("after the TTL = %+v, want the new GPU", got)
	}
	if got.GPUs[0].Vendor != "nvidia" || got.GPUs[0].VRAMTotalMB != 24564 {
		t.Errorf("resampled GPU = %+v, want the RTX 4090", got.GPUs[0])
	}
}

// TestHardwareSummaryFn_NilProfiler: --disable-inference builds no
// profiler, and the probe loop reads "no getter" as "this host has
// nothing to say" (product contract — it is what keeps a disabled host
// completely silent).
func TestHardwareSummaryFn_NilProfiler(t *testing.T) {
	if got := hardwareSummaryFn(context.Background(), nil); got != nil {
		t.Errorf("hardwareSummaryFn(nil) = non-nil, want nil")
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

// notPublishedByAgent lists summary fields the agent does not set. It is
// empty, and an addition needs a stated reason: every field on these two
// structs exists because a consumer asked for a fact only the agent can
// observe, so "nobody fills it in" is the defect this file exists to
// prevent, not a design choice.
//
// It briefly held HardwareSummary.MemoryBandwidthSpecGBs while the proto
// contract was on main and its producer was not — the gap a required
// proto-only PR opens. This PR lands internal/hardware's chip table, so
// the entry is deleted rather than edited, which is the whole point of
// recording it as a debt.
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
		RAMTotalGB:             64,
		UnifiedMemory:          true,
		UsableVRAMMB:           49152,
		MemoryBandwidthSpecGBs: 400, // Apple M3 Max, matching the GPU below
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
		{
			// The waired#942 host: plenty of system RAM, a card that
			// cannot hold what that RAM figure suggests. The control
			// plane compared RAM alone and offered this machine a 62 GB
			// model as its DEFAULT.
			name: "big ram, small gpu",
			prof: hardware.Profile{
				RAMTotalGB: 128,
				GPUs: []hardware.GPU{{
					Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090",
					VRAMTotalMB: 24564, ComputeCap: "8.9",
				}},
			},
			want: `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
				`"compute_cap":"8.9","vendor":"nvidia"}],"ram_total_gb":128}`,
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

// TestHardwareSummaryFor_SurvivesTheRoundTrip is the producer↔consumer
// contract for host fit (waired-ai/waired#946 L3).
//
// Two parties judge the same machine: this agent, from its own
// hardware.Profile, and the control plane, from the summary this
// function publishes. Since proto/hostfit they run the SAME rules — but
// only if the publish → decode round trip preserves every fact those
// rules read. Drop a field from hardwareSummaryFor and the agent keeps
// deciding correctly while the control plane silently decides something
// else, which is exactly how #180 stayed invisible for weeks.
//
// So: marshal what the agent broadcasts, decode it the way the control
// plane does, and require the resulting Host to equal the one the agent
// judges itself with.
func TestHardwareSummaryFor_SurvivesTheRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		prof hardware.Profile
	}{
		{
			"discrete nvidia",
			hardware.Profile{
				RAMTotalGB: 64,
				GPUs: []hardware.GPU{{
					Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090",
					VRAMTotalMB: 24564, ComputeCap: "8.9",
					DriverVersion: "535.171.04", UUID: "GPU-12345678",
				}},
			},
		},
		{
			"big ram, small gpu",
			hardware.Profile{
				RAMTotalGB: 128,
				GPUs: []hardware.GPU{{
					Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564,
				}},
			},
		},
		{
			"unified memory",
			hardware.Profile{
				RAMTotalGB: 16, UnifiedMemory: true, UsableVRAMMB: 12288,
				GPUs: []hardware.GPU{{Vendor: "apple", Model: "Apple M3", VRAMTotalMB: 16384}},
			},
		},
		{"cpu only", hardware.Profile{RAMTotalGB: 32}},
		{
			// Multi-GPU. Every device has always ridden the wire, and
			// since #264 both adapters read all of them: the ollama pool
			// is computed from the published list on each side rather
			// than published as a number, so this case is what pins the
			// two computations together. It went in asserting the
			// opposite ("ollama never aggregates, so the shared Host
			// carries the first device") — that claim was wrong about
			// the engine, which pools a whole ml.ByLibrary group when a
			// model will not fit one card.
			//
			// (vLLM's tensor-parallel aggregate is still deliberately
			// not part of Host; see router.hostFits.)
			"two identical cards",
			hardware.Profile{
				RAMTotalGB: 256,
				GPUs: []hardware.GPU{
					{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034},
					{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(hardwareSummaryFor(tc.prof))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded signer.HardwareSummary
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			want := tc.prof.HostFit()
			got := hostfit.FromHardwareSummary(&decoded)
			if got != want {
				t.Errorf("the control plane would judge a different machine than this agent does:\n"+
					" published %s\n as host   %+v\n agent has %+v", data, got, want)
			}
			if got.EffectiveVRAMMB() != tc.prof.EffectiveVRAMMB() {
				t.Errorf("effective VRAM after the round trip = %d, agent sees %d",
					got.EffectiveVRAMMB(), tc.prof.EffectiveVRAMMB())
			}
			if got.OllamaVRAMBudgetMB() != tc.prof.OllamaVRAMBudgetMB() {
				t.Errorf("ollama VRAM budget after the round trip = %d, agent sees %d",
					got.OllamaVRAMBudgetMB(), tc.prof.OllamaVRAMBudgetMB())
			}
		})
	}
}

// TestHardwareSummaryFor_RoundTripCarriesThePool is the anti-vacuity
// guard for the multi-GPU case above.
//
// That case asserts the two sides AGREE, and two sides that both
// computed "no pool" agree perfectly. If the fixture's cards ever stop
// pooling — a vendor rename, a VRAM figure dropped from the summary,
// GPUs[1:] stopped crossing the wire — the equality assertion would
// stay green while the fact it exists to protect had gone. So assert
// separately that the pool is engaged at all, and that it is the figure
// the ollama path actually spends.
func TestHardwareSummaryFor_RoundTripCarriesThePool(t *testing.T) {
	prof := hardware.Profile{
		RAMTotalGB: 256,
		GPUs: []hardware.GPU{
			{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034},
			{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034},
		},
	}
	data, err := json.Marshal(hardwareSummaryFor(prof))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded signer.HardwareSummary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	host := hostfit.FromHardwareSummary(&decoded)

	// 23034 + 23034 - 1024 for the second card's device context.
	const wantPool = 45044
	if host.VRAMPoolMB != wantPool {
		t.Errorf("the control plane pools %d MB from %s, want %d — "+
			"two cards are being judged as one",
			host.VRAMPoolMB, data, wantPool)
	}
	if host.OllamaVRAMBudgetMB() <= host.EffectiveVRAMMB() {
		t.Errorf("ollama budget %d does not exceed the single-device figure %d, "+
			"so this fixture no longer demonstrates the multi-GPU case",
			host.OllamaVRAMBudgetMB(), host.EffectiveVRAMMB())
	}
}

// TestHardwareSummaryFor_MemoryBandwidthWireForm pins the #251 field end
// to end: a recognised part puts the number on the wire, and an
// unrecognised one puts NOTHING there.
//
// The second half is the compatibility contract. The field is populated
// from a chip table, so the hosts that leave it unset are not just old
// agents — they are every part nobody has added yet, indefinitely. A
// summary that started emitting "memory_bandwidth_spec_gbs":0 for those
// would churn the signed NetworkMap for every peer on the mesh.
func TestHardwareSummaryFor_MemoryBandwidthWireForm(t *testing.T) {
	mac := func(bandwidthGBs float64) hardware.Profile {
		return hardware.Profile{
			RAMTotalGB:             24,
			UnifiedMemory:          true,
			UsableVRAMMB:           18432,
			MemoryBandwidthSpecGBs: bandwidthGBs,
			GPUs: []hardware.GPU{{
				Vendor: "apple", Model: "Apple M4", VRAMTotalMB: 24576,
			}},
		}
	}
	for _, tc := range []struct {
		name string
		prof hardware.Profile
		want string
	}{
		{
			"a recognised part publishes its peak",
			mac(120),
			`{"gpus":[{"model":"Apple M4","vram_total_mb":24576,"vendor":"apple"}],` +
				`"ram_total_gb":24,"unified_memory":true,"usable_vram_mb":18432,` +
				`"memory_bandwidth_spec_gbs":120}`,
		},
		{
			"an unrecognised part publishes nothing",
			mac(0),
			`{"gpus":[{"model":"Apple M4","vram_total_mb":24576,"vendor":"apple"}],` +
				`"ram_total_gb":24,"unified_memory":true,"usable_vram_mb":18432}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := hardwareSummaryFor(tc.prof)
			if summary == nil {
				t.Fatal("hardwareSummaryFor() = nil")
			}
			got, err := json.Marshal(summary)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("wire form drifted:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}
