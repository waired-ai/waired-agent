package hardware

import (
	"context"
	"testing"
)

// The merge rule is asymmetric on purpose, and the asymmetry is the
// thing worth pinning: an unknown source may promote but never demote,
// and never erases a known answer. Every row names the hazard it stops.
func TestIntegrationMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b integration
		want integration
		why  string
	}{
		{
			name: "no source spoke",
			a:    integrationUnknown(), b: integrationUnknown(),
			want: integrationUnknown(),
			why:  "silence is not an answer either way",
		},
		{
			name: "a knowing source answers yes",
			a:    integrationUnknown(), b: integratedKnown(true),
			want: integratedKnown(true),
		},
		{
			name: "a knowing source answers no",
			a:    integrationUnknown(), b: integratedKnown(false),
			want: integratedKnown(false),
		},
		{
			name: "an unknown source cannot erase a known yes",
			a:    integratedKnown(true), b: integrationUnknown(),
			want: integratedKnown(true),
			why:  "a probe that is merely absent must not undo one that ran",
		},
		{
			name: "an unknown source cannot erase a known no",
			a:    integratedKnown(false), b: integrationUnknown(),
			want: integratedKnown(false),
		},
		{
			name: "an unknown source may promote to yes",
			a:    integrationUnknown(), b: integration{integrated: true},
			want: integration{integrated: true},
			why: "seeing evidence of one pool is a positive observation " +
				"even from a source that cannot rule the opposite out",
		},
		{
			name: "an unknown source may not demote a known yes",
			a:    integratedKnown(true), b: integration{integrated: false},
			want: integratedKnown(true),
			why: "crediting a single-pool host with two pools is the defect " +
				"waired-agent#459 describes",
		},
		{
			name: "a knowing source overrides a known answer both ways",
			a:    integratedKnown(true), b: integratedKnown(false),
			want: integratedKnown(false),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.merge(tc.b); got != tc.want {
				t.Errorf("%+v.merge(%+v) = %+v, want %+v%s",
					tc.a, tc.b, got, tc.want, whySuffix(tc.why))
			}
		})
	}
}

func whySuffix(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}

// The profiler must record the reading on every device and must not let
// one device's answer reach another's.
func TestProfile_RecordsIntegratedPerDevice(t *testing.T) {
	prof := NewProfiler(t.TempDir(),
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: "test", Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 64, 32, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1 << 40, nil }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{
				{Vendor: "nvidia", Model: "discrete card", VRAMTotalMB: 24576},
				{Vendor: "amd", Model: "integrated part", VRAMTotalMB: 2048},
				{Vendor: "intel", Model: "unanswered", VRAMTotalMB: 128},
			}, Accelerators{}, nil
		}),
		WithUMA(func(context.Context, *Profile) {}),
		WithIntegratedDetector(func(_ *Profile, i int) integration {
			switch i {
			case 0:
				return integratedKnown(false)
			case 1:
				return integratedKnown(true)
			default:
				return integrationUnknown()
			}
		}),
	).Profile(context.Background())

	if len(prof.GPUs) != 3 {
		t.Fatalf("GPUs = %d, want 3", len(prof.GPUs))
	}
	for i, want := range []integration{
		integratedKnown(false), integratedKnown(true), integrationUnknown(),
	} {
		got := integration{
			integrated: prof.GPUs[i].Integrated,
			known:      prof.GPUs[i].IntegratedKnown,
		}
		if got != want {
			t.Errorf("GPUs[%d] (%s) = %+v, want %+v",
				i, prof.GPUs[i].Model, got, want)
		}
	}
}

// A profiler with no detector at all leaves every device unanswered
// rather than answering "discrete", which is the one direction that
// would be a regression rather than the status quo.
func TestProfile_NoDetectorLeavesUnknown(t *testing.T) {
	prof := NewProfiler(t.TempDir(),
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: "test", Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 64, 32, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1 << 40, nil }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "amd", Model: "some part", VRAMTotalMB: 2048}}, Accelerators{}, nil
		}),
		WithUMA(func(context.Context, *Profile) {}),
		WithIntegratedDetector(nil),
	).Profile(context.Background())

	if prof.GPUs[0].IntegratedKnown {
		t.Errorf("IntegratedKnown = true with no detector; silence must not read as an answer")
	}
	if prof.GPUs[0].Integrated {
		t.Errorf("Integrated = true with no detector")
	}
}

// The reading must not move UnifiedMemory. The two are deliberately
// separate switches (see the field doc on GPU.Integrated): a budget
// rule travels with the class, and knowing a part is integrated does not
// supply one. If a later change wires the report into the policy, this
// test is where it announces itself.
func TestIntegratedReportDoesNotSetUnifiedMemory(t *testing.T) {
	prof := NewProfiler(t.TempDir(),
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: "not a strix halo", Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 64, 32, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1 << 40, nil }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "amd", Model: "an integrated part", VRAMTotalMB: 2048}}, Accelerators{}, nil
		}),
		WithIntegratedDetector(func(*Profile, int) integration { return integratedKnown(true) }),
	).Profile(context.Background())

	if !prof.GPUs[0].IntegratedKnown || !prof.GPUs[0].Integrated {
		t.Fatalf("precondition: the device should be reported integrated, got %+v", prof.GPUs[0])
	}
	if prof.UnifiedMemory {
		t.Errorf("UnifiedMemory = true from the report alone; the budget rule " +
			"(UsableVRAMMB) has not been settled for this part, and a class " +
			"without a budget reports EffectiveVRAMMB 0, i.e. \"CPU only\"")
	}
	if prof.UsableVRAMMB != 0 {
		t.Errorf("UsableVRAMMB = %d, want 0", prof.UsableVRAMMB)
	}
}

// The Windows rule, with the numbers it was derived from.
//
// The reference-host rows are measurements, not constructions: the
// 0.50 GiB configuration was read off the machine on 2026-09-20, and the
// 96 GiB one is the configuration waired-agent#863 loaded a 76.3 GB
// model in. Together they are why the rule is an inequality — an
// equality test passes the second and fails the first.
func TestCarvedFromSystemRAM(t *testing.T) {
	const (
		gib             = uint64(1) << 30
		mib             = uint64(1) << 20
		installed128GiB = 128 * gib
	)
	for _, tc := range []struct {
		name                        string
		adapter, installed, visible uint64
		want                        integration
		why                         string
	}{
		{
			name:      "reference host as measured 2026-09-20: 0.5 GiB carve-out",
			adapter:   512 * mib,
			installed: installed128GiB,
			visible:   136523354112, // 127.15 GiB, GlobalMemoryStatusEx.TotalPhys
			want:      integratedKnown(true),
			why: "the deduction is 0.85 GiB where the carve-out is 0.50 — " +
				"an equality test fails exactly here, on the configuration AMD recommends",
		},
		{
			name:      "reference host in the 96 GiB carve-out configuration (#863)",
			adapter:   96 * gib,
			installed: installed128GiB,
			visible:   33982614733, // 31.65 GiB, the figure #863 recorded
			want:      integratedKnown(true),
		},
		{
			name:      "discrete 24 GiB card on the same class of machine",
			adapter:   24 * gib,
			installed: installed128GiB,
			visible:   136523354112,
			want:      integrationUnknown(),
			why:       "unknown, not \"discrete\": this arithmetic can only say yes",
		},
		{
			name:      "an Intel iGPU's 128 MiB carve-out",
			adapter:   128 * mib,
			installed: 32 * gib,
			visible:   32*gib - 900*mib,
			want:      integratedKnown(true),
		},
		{
			name:      "adapter reports no memory at all",
			adapter:   0,
			installed: installed128GiB,
			visible:   136523354112,
			want:      integrationUnknown(),
			why:       "a driver that will not say is not a driver saying no",
		},
		{
			name:      "malformed SMBIOS: installed below visible",
			adapter:   512 * mib,
			installed: 64 * gib,
			visible:   128 * gib,
			want:      integrationUnknown(),
		},
		{
			name:      "no installed reading",
			adapter:   512 * mib,
			installed: 0,
			visible:   136523354112,
			want:      integrationUnknown(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := carvedFromSystemRAM(tc.adapter, tc.installed, tc.visible)
			if got != tc.want {
				t.Errorf("carvedFromSystemRAM(%d, %d, %d) = %+v, want %+v%s",
					tc.adapter, tc.installed, tc.visible, got, tc.want, whySuffix(tc.why))
			}
		})
	}
}

// The rule must never answer "discrete". Stated as its own assertion
// because it is the property that makes the rule safe on a single-pool
// NVIDIA part, where the figure handed in is the whole pool rather than
// a carve-out.
func TestCarvedFromSystemRAM_NeverAnswersDiscrete(t *testing.T) {
	const gib = uint64(1) << 30
	for adapter := uint64(0); adapter <= 256*gib; adapter += 7 * gib {
		for _, gap := range []uint64{0, gib / 2, gib, 96 * gib} {
			got := carvedFromSystemRAM(adapter, 128*gib, 128*gib-gap)
			if got.known && !got.integrated {
				t.Fatalf("adapter=%d gap=%d answered a confident \"discrete\"", adapter, gap)
			}
		}
	}
}
