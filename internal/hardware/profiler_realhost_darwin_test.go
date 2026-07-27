//go:build darwin

package hardware

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealHostProfileAppleSilicon exercises the actual sysctl-backed
// hardware probe on the developer machine and asserts the fields that
// `waired init` relies on to pick inference defaults. Gated by
// WAIRED_HW_REALHOST=1 so a normal `go test ./...` (which may run on a
// non-Apple-Silicon CI runner) does not fail on the Apple-specific
// assertions below.
//
// To run manually on an Apple Silicon Mac:
//
//	WAIRED_HW_REALHOST=1 go test ./internal/hardware/ -run RealHost -v
func TestRealHostProfileAppleSilicon(t *testing.T) {
	if os.Getenv("WAIRED_HW_REALHOST") == "" {
		t.Skip("set WAIRED_HW_REALHOST=1 to exercise the real hardware probe")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p := NewProfiler("").Profile(ctx)

	// Dump the whole profile so the work record can quote real numbers.
	t.Logf("OS=%s Arch=%s", p.OS, p.Arch)
	t.Logf("CPU.Model=%q CPU.Cores=%d", p.CPU.Model, p.CPU.Cores)
	t.Logf("RAMTotalGB=%d RAMAvailableGB=%d", p.RAMTotalGB, p.RAMAvailableGB)
	t.Logf("UnifiedMemory=%t UsableVRAMMB=%d EffectiveVRAMMB=%d MemoryBandwidthSpecGBs=%v",
		p.UnifiedMemory, p.UsableVRAMMB, p.EffectiveVRAMMB(), p.MemoryBandwidthSpecGBs)
	for i, g := range p.GPUs {
		t.Logf("GPU[%d] vendor=%q model=%q vramMB=%d", i, g.Vendor, g.Model, g.VRAMTotalMB)
	}
	t.Logf("Accelerators=%+v", p.Accelerators)
	t.Logf("Engines=%+v", p.Engines)
	if len(p.Errors) > 0 {
		t.Logf("Errors=%v", p.Errors)
	}

	if p.OS != "darwin" {
		t.Fatalf("OS = %q, want darwin", p.OS)
	}
	if p.CPU.Model == "" {
		t.Errorf("CPU.Model empty; want a sysctl-derived model string")
	}
	if p.CPU.Cores <= 0 {
		t.Errorf("CPU.Cores = %d, want > 0", p.CPU.Cores)
	}
	if p.RAMTotalGB <= 0 {
		t.Errorf("RAMTotalGB = %d, want > 0", p.RAMTotalGB)
	}

	if p.Arch == "arm64" {
		// Apple Silicon: the UMA path must populate a usable VRAM budget
		// that the inference picker (hardwareEnabledDefault: >= 8192 MB)
		// can act on. Without this, init would default inference OFF on
		// a perfectly capable Mac.
		if !p.UnifiedMemory {
			t.Errorf("UnifiedMemory = false on arm64, want true")
		}
		if p.UsableVRAMMB <= 0 {
			t.Errorf("UsableVRAMMB = %d on arm64, want > 0", p.UsableVRAMMB)
		}
		if p.EffectiveVRAMMB() <= 0 {
			t.Errorf("EffectiveVRAMMB = %d, want > 0", p.EffectiveVRAMMB())
		}
		if !p.Accelerators.Metal {
			t.Errorf("Accelerators.Metal = false on arm64, want true")
		}
		// #251: the chip table has to recognise the string THIS macOS
		// actually reports, which is the half no unit test can check —
		// the table is keyed on sysctl output and a unit test supplies
		// its own. An unrecognised part is not a failure of the design
		// (hostfit falls back and stays annotate-only), so this reports
		// rather than fails, and names the exact key to add.
		if p.MemoryBandwidthSpecGBs <= 0 {
			t.Logf("NOTE: %q is not in internal/hardware's chip table, so this host "+
				"publishes no bandwidth and keeps the pre-#251 annotate-only behaviour. "+
				"Add %q with the vendor's published peak to let it decide.",
				p.CPU.Model, normalizeChipName(p.CPU.Model))
		} else if h := p.HostFit(); h.MemoryBandwidthSpecGBs != p.MemoryBandwidthSpecGBs {
			t.Errorf("HostFit() dropped the bandwidth: %v vs %v",
				h.MemoryBandwidthSpecGBs, p.MemoryBandwidthSpecGBs)
		}
	}
}
