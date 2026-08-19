package hardware

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfile_FromStubs(t *testing.T) {
	now := time.Date(2026, 5, 2, 17, 30, 0, 0, time.UTC)
	p := NewProfiler("/tmp/cache",
		WithNow(func() time.Time { return now }),
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo {
			return CPUInfo{Model: "Test CPU", Cores: 8}
		}),
		WithRAM(func(context.Context) (int, int, error) { return 64, 48, nil }),
		WithStorage(func(_ context.Context, path string) (int64, error) {
			if path != "/tmp/cache" {
				t.Errorf("storage path = %q, want /tmp/cache", path)
			}
			return 500_000_000_000, nil
		}),
		WithEngineVersion(func(_ context.Context, binary string) (bool, string) {
			if binary == "ollama" {
				return true, "0.22.1"
			}
			return false, ""
		}),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{
				Vendor:        "nvidia",
				Model:         "NVIDIA RTX PRO 4000 Blackwell",
				VRAMTotalMB:   24467,
				DriverVersion: "595.58.03",
				ComputeCap:    "12.0",
				UUID:          "GPU-test",
			}}, Accelerators{CUDA: true}, nil
		}),
	)

	prof := p.Profile(context.Background())

	if prof.OS != "linux" || prof.Arch != "x86_64" {
		t.Errorf("OS/Arch = %q/%q, want linux/x86_64", prof.OS, prof.Arch)
	}
	if prof.CPU.Cores != 8 || prof.CPU.Model != "Test CPU" {
		t.Errorf("CPU = %+v", prof.CPU)
	}
	if prof.RAMTotalGB != 64 || prof.RAMAvailableGB != 48 {
		t.Errorf("RAM = total %d / avail %d", prof.RAMTotalGB, prof.RAMAvailableGB)
	}
	if prof.Storage.CachePath != "/tmp/cache" || prof.Storage.CacheFreeBytes != 500_000_000_000 {
		t.Errorf("Storage = %+v", prof.Storage)
	}
	if !prof.Engines.Ollama.Installed || prof.Engines.Ollama.Version != "0.22.1" {
		t.Errorf("Engines.Ollama = %+v", prof.Engines.Ollama)
	}
	if prof.Engines.VLLM.Installed {
		t.Errorf("vLLM should not be installed in this stub setup")
	}
	if !prof.CollectedAt.Equal(now) {
		t.Errorf("CollectedAt = %v, want %v", prof.CollectedAt, now)
	}
	// Step 2: GPU detection is wired through WithGPU.
	if len(prof.GPUs) != 1 {
		t.Fatalf("GPUs length = %d, want 1", len(prof.GPUs))
	}
	g := prof.GPUs[0]
	if g.Vendor != "nvidia" || g.Model != "NVIDIA RTX PRO 4000 Blackwell" {
		t.Errorf("GPU vendor/model = %q/%q", g.Vendor, g.Model)
	}
	if g.VRAMTotalMB != 24467 {
		t.Errorf("VRAMTotalMB = %d, want 24467", g.VRAMTotalMB)
	}
	if g.DriverVersion != "595.58.03" || g.ComputeCap != "12.0" || g.UUID != "GPU-test" {
		t.Errorf("GPU metadata = %+v", g)
	}
	if !prof.Accelerators.CUDA {
		t.Errorf("Accelerators.CUDA should be true when an NVIDIA GPU is present")
	}
	if prof.Accelerators.ROCm || prof.Accelerators.Metal {
		t.Errorf("Nvidia-only stub: ROCm/Metal should remain false, got %+v", prof.Accelerators)
	}
}

// TestProfile_AMDFromStub mirrors TestProfile_FromStubs for an AMD
// detector result: a Radeon card surfaces as vendor "amd" with
// Accelerators.ROCm set; CUDA stays false because no Nvidia detector
// fired.
func TestProfile_AMDFromStub(t *testing.T) {
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "windows", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 32, 24, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 100_000_000_000, nil }),
		WithEngineVersion(func(_ context.Context, b string) (bool, string) {
			if b == "ollama" {
				return true, "0.22.1"
			}
			return false, ""
		}),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{
				Vendor:        "amd",
				Model:         "Radeon RX 7900 XTX",
				VRAMTotalMB:   24560,
				DriverVersion: "6.7.0",
				UUID:          "card0",
			}}, Accelerators{ROCm: true}, nil
		}),
	)
	prof := p.Profile(context.Background())
	if len(prof.GPUs) != 1 || prof.GPUs[0].Vendor != "amd" {
		t.Fatalf("GPUs = %+v, want one amd entry", prof.GPUs)
	}
	if !prof.Accelerators.ROCm {
		t.Errorf("Accelerators.ROCm should be true for AMD-only host, got %+v", prof.Accelerators)
	}
	if prof.Accelerators.CUDA {
		t.Errorf("Accelerators.CUDA should remain false for AMD-only host, got %+v", prof.Accelerators)
	}
}

func TestProfile_GPUEmpty(t *testing.T) {
	// nvidia-smi unavailable: empty GPUs slice, Accelerators all false,
	// no error in Profile.Errors (treated as "no GPU on this host").
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Cores: 4} }),
		WithRAM(func(context.Context) (int, int, error) { return 16, 12, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{}, Accelerators{}, nil
		}),
	)
	prof := p.Profile(context.Background())
	if len(prof.GPUs) != 0 {
		t.Errorf("GPUs should be empty when detector returns nothing, got %+v", prof.GPUs)
	}
	if prof.Accelerators.CUDA {
		t.Errorf("CUDA should be false when no NVIDIA GPU detected")
	}
	for _, e := range prof.Errors {
		if strings.Contains(e, "gpu") {
			t.Errorf("missing GPU should not surface as a Profile.Errors entry, got %q", e)
		}
	}
}

func TestProfile_GPUDetectionError(t *testing.T) {
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Cores: 4} }),
		WithRAM(func(context.Context) (int, int, error) { return 16, 12, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return nil, Accelerators{}, errors.New("nvidia-smi: parse failure")
		}),
	)
	prof := p.Profile(context.Background())
	if len(prof.GPUs) != 0 {
		t.Errorf("GPUs should be empty on detector error, got %+v", prof.GPUs)
	}
	found := false
	for _, e := range prof.Errors {
		if strings.Contains(e, "gpu") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected gpu error in Profile.Errors, got %+v", prof.Errors)
	}
}

// TestProfile_GPUPartialSuccess covers the AMD-on-Windows registry
// fallback semantics: the detector returns valid GPUs + Accelerators
// AND a non-nil error (the "VRAM unknown" soft warning). Profile
// must propagate the GPU data and flip the accelerator flag while
// also surfacing the warning via Profile.Errors.
func TestProfile_GPUPartialSuccess(t *testing.T) {
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "windows", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 32, 24, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "amd", Model: "Radeon RX 7900 XTX"}},
				Accelerators{ROCm: true},
				errors.New("gpu(amd): VRAM unknown without rocm-smi")
		}),
	)
	prof := p.Profile(context.Background())
	if len(prof.GPUs) != 1 || prof.GPUs[0].Vendor != "amd" {
		t.Errorf("partial success should preserve GPU data, got %+v", prof.GPUs)
	}
	if !prof.Accelerators.ROCm {
		t.Errorf("partial success should preserve Accelerators, got %+v", prof.Accelerators)
	}
	found := false
	for _, e := range prof.Errors {
		if strings.Contains(e, "VRAM unknown") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("partial-success warning should appear in Profile.Errors, got %+v", prof.Errors)
	}
}

func TestProfile_RAMError(t *testing.T) {
	// A RAM detection error must not panic; instead the Profile fields
	// fall back to zero and the error is surfaced via Profile.Errors.
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithRAM(func(context.Context) (int, int, error) { return 0, 0, errors.New("boom") }),
		WithStorage(func(context.Context, string) (int64, error) { return 0, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
	)
	prof := p.Profile(context.Background())
	if prof.RAMTotalGB != 0 {
		t.Errorf("RAMTotalGB = %d, want 0 on detection error", prof.RAMTotalGB)
	}
	found := false
	for _, e := range prof.Errors {
		if strings.Contains(e, "ram") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ram error in Profile.Errors, got %+v", prof.Errors)
	}
}

func TestProfile_TTLCacheReturnsSameValue(t *testing.T) {
	calls := 0
	p := NewProfiler("/tmp/cache",
		WithTTL(30*time.Second),
		WithNow(func() time.Time { return time.Unix(int64(1000+calls), 0) }),
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo {
			calls++
			return CPUInfo{Cores: calls}
		}),
		WithRAM(func(context.Context) (int, int, error) { return 1, 1, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
	)
	a := p.Profile(context.Background())
	b := p.Profile(context.Background())
	if a.CPU.Cores != b.CPU.Cores {
		t.Errorf("cached profile changed: a=%d b=%d (cpu fn called %d times)",
			a.CPU.Cores, b.CPU.Cores, calls)
	}
	if calls > 1 {
		t.Errorf("CPU detector called %d times, expected 1 within TTL", calls)
	}
}

func TestProfile_TTLExpiryReDetects(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	p := NewProfiler("/tmp/cache",
		WithTTL(1*time.Second),
		WithNow(func() time.Time { return now }),
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo {
			calls++
			return CPUInfo{Cores: calls}
		}),
		WithRAM(func(context.Context) (int, int, error) { return 1, 1, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
	)
	a := p.Profile(context.Background())
	now = now.Add(2 * time.Second) // exceed TTL
	b := p.Profile(context.Background())
	if a.CPU.Cores == b.CPU.Cores {
		t.Errorf("expected re-detection after TTL expiry, both calls returned cores=%d", a.CPU.Cores)
	}
	if calls != 2 {
		t.Errorf("CPU detector called %d times, expected 2 across TTL boundary", calls)
	}
}

func TestParseProcMeminfo(t *testing.T) {
	sample := "MemTotal:       65856900 kB\nMemFree:        12345678 kB\nMemAvailable:   49876543 kB\nBuffers:         123 kB\n"
	total, avail, err := parseProcMeminfo(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseProcMeminfo: %v", err)
	}
	if total != 63 { // 65856900 KiB ≈ 62.8 GiB → round to 63 (#61)
		t.Errorf("total GB = %d, want 63", total)
	}
	if avail != 48 { // 49876543 KiB ≈ 47.6 GiB → round to 48 (#61)
		t.Errorf("avail GB = %d, want 48", avail)
	}
}

func TestBytesToGBRounded(t *testing.T) {
	const gib = uint64(1) << 30
	cases := []struct {
		name  string
		bytes uint64
		want  int
	}{
		// The #61 incident: a 32 GB box exposes ~31.9 GiB of usable RAM
		// after hardware reserve; flooring gave 31 and failed a 32 GB
		// threshold. Rounding must recover 32.
		{"32GB box reports 31.9 GiB", 34_270_000_000, 32},
		{"exact 32 GiB", 32 * gib, 32},
		{"exact 64 GiB", 64 * gib, 64},
		{"62.8 GiB rounds up to 63", 67_437_465_600, 63},
		{"16.4 GiB rounds down to 16", 16*gib + gib/3, 16},
		{"15.6 GiB rounds up to 16", 16*gib - gib/3, 16},
		{"just over half rounds up", 8*gib + gib/2 + 1, 9},
		{"zero", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bytesToGBRounded(c.bytes); got != c.want {
				t.Errorf("bytesToGBRounded(%d) = %d, want %d", c.bytes, got, c.want)
			}
		})
	}
}

// A /proc/meminfo with no MemAvailable line is "not measured", and the
// one thing it must not become is the number 0: measureAndPersistHostMemory
// floors a probe's answer at 1 so a truthfully exhausted host cannot read
// as unmeasured, and a 0 arriving from a missing line would be persisted
// as "1 GB available" — hostfit would then charge the OS RAMTotalGB−1 and
// this host would fit nothing. Available == total is how the darwin probe
// already says "no reading", and it lands on the constant deduction.
func TestParseProcMeminfo_MissingMemAvailableIsNotZero(t *testing.T) {
	total, avail, err := parseProcMeminfo(strings.NewReader("MemTotal: 65856900 kB\n"))
	if err != nil {
		t.Fatalf("parseProcMeminfo: %v", err)
	}
	if total != 63 {
		t.Errorf("total GB = %d, want 63", total)
	}
	if avail != total {
		t.Errorf("avail GB = %d, want %d (== total, the 'no reading' signal)", avail, total)
	}
}

func TestParseProcMeminfo_MissingFields(t *testing.T) {
	if _, _, err := parseProcMeminfo(strings.NewReader("Buffers: 123 kB\n")); err == nil {
		t.Errorf("expected error when MemTotal is missing")
	}
}

// `ollama --version` prints one of two shapes, and which one depends on
// whether a server is answering — not on the engine's version. Measured
// on 2026-08-16 against 0.31.1 and 0.32.13, both waired-managed builds on
// the same host (#826):
//
//	server up:   "ollama version is 0.32.13"
//	server down: "Warning: could not connect to a running Ollama instance"
//	             "Warning: client version is 0.32.13"
//
// The two never appear together. This test used to assert a third shape —
// the "could not connect" warning followed by "ollama version is" — that
// no build produces; it was written by hand rather than captured, so the
// server-down case had never actually been parsed. That is why the
// version of a STOPPED engine read as "" everywhere.
func TestParseOllamaVersion(t *testing.T) {
	cases := map[string]string{
		"ollama version is 0.22.1\n":                   "0.22.1",
		"ollama version is 0.1.0\nWarning: foo bar\n":  "0.1.0",
		"Warning: ...\nollama version is 0.99.9-rc1\n": "0.99.9-rc1",
		// Server up (0.32.13, captured).
		"ollama version is 0.32.13\n": "0.32.13",
		// Server down (0.31.1 and 0.32.13, captured). The old parser
		// returned "" here, so a stopped engine had no version at all.
		"Warning: could not connect to a running Ollama instance\nWarning: client version is 0.31.1\n":  "0.31.1",
		"Warning: could not connect to a running Ollama instance\nWarning: client version is 0.32.13\n": "0.32.13",
		// Nothing recognisable stays "".
		"Warning: could not connect to a running Ollama instance\n": "",
	}
	for in, want := range cases {
		got := ParseEngineVersion("ollama", in)
		if got != want {
			t.Errorf("ParseEngineVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The server's version wins over the client's when both are somehow
// present. They are the same binary for a waired-managed install, so the
// case is theoretical — but stating the precedence keeps the client line
// strictly additive: it can only fill in an answer that used to be empty,
// never change one that was already right.
func TestParseOllamaVersion_ServerLineWins(t *testing.T) {
	const both = "Warning: client version is 0.31.1\nollama version is 0.32.13\n"
	if got := ParseEngineVersion("ollama", both); got != "0.32.13" {
		t.Errorf("ParseEngineVersion(both lines) = %q, want the server's 0.32.13", got)
	}
}

func TestParseVLLMVersion(t *testing.T) {
	if got := ParseEngineVersion("vllm", "0.6.3.post1\n"); got != "0.6.3.post1" {
		t.Errorf("vllm parse = %q, want 0.6.3.post1", got)
	}
}

func TestDefaultProfilerSmokeTest(t *testing.T) {
	// The default profiler must be safe to construct and call without
	// panicking even if all detection paths fail (e.g. on a CI runner
	// without ollama).
	p := NewProfiler("/tmp")
	prof := p.Profile(context.Background())
	if prof.OS == "" || prof.Arch == "" {
		t.Errorf("default profiler should always return OS/Arch, got %q/%q", prof.OS, prof.Arch)
	}
	if prof.CollectedAt.IsZero() {
		t.Errorf("CollectedAt should be set by default profiler")
	}
}

// TestProfile_GPUSummary_NilForCPUOnly checks that a CPU-only host
// returns nil rather than an empty slice. This is the documented
// signal the agent uses to skip the Hardware field on its
// InferenceState push entirely (keeping NetworkMap broadcasts compact
// for the long tail of CPU-only laptops).
func TestProfile_GPUSummary_NilForCPUOnly(t *testing.T) {
	p := Profile{GPUs: nil}
	if got := p.GPUSummary(); got != nil {
		t.Errorf("GPUSummary() on CPU-only host = %+v, want nil", got)
	}
	p2 := Profile{GPUs: []GPU{}}
	if got := p2.GPUSummary(); got != nil {
		t.Errorf("GPUSummary() with zero-length GPUs slice = %+v, want nil", got)
	}
}

// TestProfile_GPUSummary_StripsOperatorMetadata verifies the helper
// drops DriverVersion / UUID — these are operator-side concerns, and
// broadcasting them would leak machine-fingerprintable detail with no
// routing utility.
//
// Vendor is deliberately NOT stripped (waired-agent#142): the control
// plane's onboarding host-fit needs it to decide which serving engines
// a device may be offered, and it adds no fingerprinting surface — it
// is a three-value token that the Model string already spells out
// ("NVIDIA GeForce RTX 4090"). Publishing it is what lets consumers
// honour Model's "do not parse for routing decisions" rule.
func TestProfile_GPUSummary_StripsOperatorMetadata(t *testing.T) {
	p := Profile{
		GPUs: []GPU{
			{
				Vendor:        "nvidia",
				Model:         "NVIDIA GeForce RTX 4090",
				VRAMTotalMB:   24564,
				DriverVersion: "535.171.04",
				ComputeCap:    "8.9",
				UUID:          "GPU-12345678",
			},
			{
				Vendor:      "nvidia",
				Model:       "NVIDIA GeForce RTX 3060",
				VRAMTotalMB: 12000,
				ComputeCap:  "8.6",
			},
		},
	}
	got := p.GPUSummary()
	want := []GPUSummary{
		{Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564, ComputeCap: "8.9", Vendor: "nvidia"},
		{Model: "NVIDIA GeForce RTX 3060", VRAMTotalMB: 12000, ComputeCap: "8.6", Vendor: "nvidia"},
	}
	if len(got) != len(want) {
		t.Fatalf("GPUSummary() len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("GPUSummary()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestProfile_UMAInjected verifies that the umaFn hook runs after
// GPU / RAM detection and that the new fields (UnifiedMemory,
// UsableVRAMMB) propagate into the Profile snapshot. Mirrors the
// pattern used by every existing With* injection test in this file.
func TestProfile_UMAInjected(t *testing.T) {
	var calls int
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: "AMD Ryzen AI Max 395", Cores: 16} }),
		WithRAM(func(context.Context) (int, int, error) { return 128, 96, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "amd", Model: "Radeon 8060S", VRAMTotalMB: 32768}}, Accelerators{ROCm: true}, nil
		}),
		WithUMA(func(_ context.Context, prof *Profile) {
			calls++
			// Assert ordering: the UMA hook sees the already-populated
			// GPU and RAM fields, so it can branch on them.
			if len(prof.GPUs) != 1 || prof.RAMTotalGB == 0 {
				t.Errorf("UMA hook called before GPU/RAM populated: gpus=%d ram=%d", len(prof.GPUs), prof.RAMTotalGB)
			}
			prof.UnifiedMemory = true
			prof.UsableVRAMMB = 96 * 1024
		}),
	)
	prof := p.Profile(context.Background())
	if calls != 1 {
		t.Errorf("umaFn calls = %d, want 1", calls)
	}
	if !prof.UnifiedMemory {
		t.Errorf("Profile.UnifiedMemory should be true after UMA hook")
	}
	if prof.UsableVRAMMB != 96*1024 {
		t.Errorf("Profile.UsableVRAMMB = %d, want %d", prof.UsableVRAMMB, 96*1024)
	}
	if prof.EffectiveVRAMMB() != 96*1024 {
		t.Errorf("EffectiveVRAMMB() = %d, want %d (UMA cap, not raw VRAMTotalMB)", prof.EffectiveVRAMMB(), 96*1024)
	}
}

// TestProfile_NoUMAByDefault confirms that hosts without UMA hooks
// (the existing NVIDIA path, today's CI) keep UnifiedMemory=false and
// EffectiveVRAMMB() falls back to GPUs[0].VRAMTotalMB.
func TestProfile_NoUMAByDefault(t *testing.T) {
	p := NewProfiler("/tmp/cache",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 64, 48, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24000}}, Accelerators{CUDA: true}, nil
		}),
		WithUMA(func(context.Context, *Profile) { /* no-op (e.g. Windows) */ }),
	)
	prof := p.Profile(context.Background())
	if prof.UnifiedMemory {
		t.Errorf("Profile.UnifiedMemory should remain false for discrete GPU")
	}
	if prof.UsableVRAMMB != 0 {
		t.Errorf("Profile.UsableVRAMMB = %d, want 0 for discrete GPU", prof.UsableVRAMMB)
	}
	if prof.EffectiveVRAMMB() != 24000 {
		t.Errorf("EffectiveVRAMMB() = %d, want 24000 (discrete-GPU fallback)", prof.EffectiveVRAMMB())
	}
}

// TestProfile_GPUSummary_FreshAllocation guards against the helper
// aliasing Profile.GPUs — callers must be free to mutate the result
// without spooky-action on the cached Profile.
func TestProfile_GPUSummary_FreshAllocation(t *testing.T) {
	p := Profile{
		GPUs: []GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24564}},
	}
	out := p.GPUSummary()
	out[0].Model = "TAMPERED"
	if p.GPUs[0].Model != "RTX 4090" {
		t.Errorf("GPUSummary() result aliases Profile.GPUs; Profile.GPUs[0].Model = %q after caller mutation", p.GPUs[0].Model)
	}
}

// TestFreeDiskBytes_ExistingAndMissing checks the install-time disk
// probe: it returns a positive figure for an existing dir and, crucially,
// for a not-yet-created nested target (the bundled models dir during
// init) by walking up to the nearest existing ancestor.
func TestFreeDiskBytes_ExistingAndMissing(t *testing.T) {
	dir := t.TempDir()

	got, err := FreeDiskBytes(dir)
	if err != nil {
		t.Fatalf("FreeDiskBytes(existing): %v", err)
	}
	if got <= 0 {
		t.Fatalf("FreeDiskBytes(existing) = %d, want > 0", got)
	}

	// A path several levels below an existing dir that does not exist yet
	// must still resolve via the ancestor walk to the same filesystem.
	missing := filepath.Join(dir, "runtimes", "ollama", "models")
	gotMissing, err := FreeDiskBytes(missing)
	if err != nil {
		t.Fatalf("FreeDiskBytes(missing nested): %v", err)
	}
	if gotMissing <= 0 {
		t.Fatalf("FreeDiskBytes(missing nested) = %d, want > 0", gotMissing)
	}
}

func TestNearestExistingDir(t *testing.T) {
	dir := t.TempDir()
	if got := nearestExistingDir(filepath.Join(dir, "a", "b", "c")); got != dir {
		t.Errorf("nearestExistingDir = %q, want %q", got, dir)
	}
	if got := nearestExistingDir(dir); got != dir {
		t.Errorf("nearestExistingDir(existing) = %q, want %q", got, dir)
	}
}

// TestProfile_MemoryBandwidthFromChipName pins the wiring #251 adds: the
// profiler fills MemoryBandwidthSpecGBs from CPU.Model AFTER the UMA hook
// has run, and carries it through HostFit() into the shared fit rules.
//
// It runs on every OS because the population step is untagged — the
// lookup is identical on all three, and keeping it out of the per-OS
// hooks is what stops them drifting (CLAUDE.md §Cross-OS parity).
func TestProfile_MemoryBandwidthFromChipName(t *testing.T) {
	newProfiler := func(cpuModel string, uma func(context.Context, *Profile)) *Profiler {
		return NewProfiler("/tmp/cache",
			WithOSArch(func() (string, string) { return "darwin", "arm64" }),
			WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: cpuModel, Cores: 10} }),
			WithRAM(func(context.Context) (int, int, error) { return 24, 18, nil }),
			WithStorage(func(context.Context, string) (int64, error) { return 1, nil }),
			WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
			WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
				return []GPU{{Vendor: "apple", Model: "Apple GPU"}}, Accelerators{Metal: true}, nil
			}),
			WithUMA(uma),
		)
	}
	markUnified := func(_ context.Context, prof *Profile) {
		prof.UnifiedMemory = true
		prof.UsableVRAMMB = 18432
	}

	t.Run("known part", func(t *testing.T) {
		prof := newProfiler("Apple M4", markUnified).Profile(context.Background())
		if prof.MemoryBandwidthSpecGBs != 120 {
			t.Errorf("MemoryBandwidthSpecGBs = %v, want 120", prof.MemoryBandwidthSpecGBs)
		}
		if got := prof.HostFit().MemoryBandwidthSpecGBs; got != 120 {
			t.Errorf("HostFit() dropped the bandwidth: %v", got)
		}
	})

	t.Run("unknown part stays at zero", func(t *testing.T) {
		prof := newProfiler("Apple M9 Ultra", markUnified).Profile(context.Background())
		if prof.MemoryBandwidthSpecGBs != 0 {
			t.Errorf("MemoryBandwidthSpecGBs = %v, want 0 for an unrecognised part",
				prof.MemoryBandwidthSpecGBs)
		}
	})

	t.Run("ordering: the lookup runs after the UMA hook", func(t *testing.T) {
		// The hook is what decides UnifiedMemory, and the lookup is gated
		// on it. Running the lookup first would silently yield 0 on every
		// unified host — a regression that changes no test that does not
		// assert this ordering.
		prof := newProfiler("Apple M4 Max", func(_ context.Context, p *Profile) {
			if p.MemoryBandwidthSpecGBs != 0 {
				t.Errorf("bandwidth was populated BEFORE the UMA hook (%v); the hook "+
					"sets UnifiedMemory, which the lookup is gated on",
					p.MemoryBandwidthSpecGBs)
			}
			markUnified(context.Background(), p)
		}).Profile(context.Background())
		if prof.MemoryBandwidthSpecGBs != 546 {
			t.Errorf("MemoryBandwidthSpecGBs = %v, want 546", prof.MemoryBandwidthSpecGBs)
		}
	})

	t.Run("no UMA hook means no bandwidth", func(t *testing.T) {
		prof := newProfiler("Apple M4", func(context.Context, *Profile) {}).Profile(context.Background())
		if prof.MemoryBandwidthSpecGBs != 0 {
			t.Errorf("MemoryBandwidthSpecGBs = %v, want 0 when nothing marked the host unified",
				prof.MemoryBandwidthSpecGBs)
		}
	})
}

// TestProfile_InstallTimeMemoryFigure pins the #568 split (2026-08-08
// owner rulings on waired-agent#568): the install-time figure
// (WithRAMAvailableAtInstall) is what HostFit projects — never the
// live RAMAvailableGB, which stays on the profile for diagnostics. A
// live figure in the fit adapter would count a resident model against
// the very host serving it.
func TestProfile_InstallTimeMemoryFigure(t *testing.T) {
	p := NewProfiler("",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{Model: "Test CPU", Cores: 8} }),
		WithRAM(func(context.Context) (int, int, error) { return 64, 48, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 0, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) { return nil, Accelerators{}, nil }),
		WithUMA(func(context.Context, *Profile) {}),
		WithRAMAvailableAtInstall(41, "2026-08-09T16:47:06.123456789Z"),
	)
	prof := p.Profile(context.Background())
	if prof.RAMAvailableGB != 48 {
		t.Errorf("live RAMAvailableGB = %d, want 48", prof.RAMAvailableGB)
	}
	if prof.RAMAvailableAtInstallGB != 41 {
		t.Errorf("RAMAvailableAtInstallGB = %d, want 41", prof.RAMAvailableAtInstallGB)
	}
	// The date rides with the figure and is carried verbatim
	// (waired-agent#699) — it is a fact about the install, so
	// re-detection never changes it any more than the value.
	if got := prof.RAMAvailableAtInstallMeasuredAt; got != "2026-08-09T16:47:06.123456789Z" {
		t.Errorf("RAMAvailableAtInstallMeasuredAt = %q, want the injected timestamp", got)
	}
	h := prof.HostFit()
	if h.RAMAvailableGB != 41 {
		t.Errorf("HostFit().RAMAvailableGB = %d, want the install-time 41, never the live 48", h.RAMAvailableGB)
	}
}

// TestProfile_NoInstallFigure pins the fallback: with no injected
// figure the adapter hands hostfit 0, which OSMemoryDeductionGB reads
// as "measurement unavailable" and answers with the constant.
func TestProfile_NoInstallFigure(t *testing.T) {
	p := NewProfiler("",
		WithOSArch(func() (string, string) { return "linux", "x86_64" }),
		WithCPU(func(context.Context) CPUInfo { return CPUInfo{} }),
		WithRAM(func(context.Context) (int, int, error) { return 64, 48, nil }),
		WithStorage(func(context.Context, string) (int64, error) { return 0, nil }),
		WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) { return nil, Accelerators{}, nil }),
		WithUMA(func(context.Context, *Profile) {}),
	)
	if h := p.Profile(context.Background()).HostFit(); h.RAMAvailableGB != 0 {
		t.Errorf("HostFit().RAMAvailableGB = %d, want 0 (live figure must not leak)", h.RAMAvailableGB)
	}
}
