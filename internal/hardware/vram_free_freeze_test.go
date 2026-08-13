package hardware

import (
	"context"
	"path/filepath"
	"testing"
)

// TestProfile_VRAMFreeIsFrozenAfterTheFirstReading is the guard on the
// spiral waired-agent#69's free-VRAM reading would otherwise cause.
//
// PRODUCT CONTRACT — docs/decisions/20260813/1120 §Decision 5, on the
// grounds signer.HardwareSummary.RAMAvailableGB already states for its
// own quantity (#568): a live figure "would count a resident model
// against the very host that serves it".
//
// The profile is re-sampled on a TTL, including long after this agent's
// engine has loaded weights. If the free reading moved with it, the
// budget would shrink, the next re-tune would size against the smaller
// budget, and the reading after that would be smaller again.
func TestProfile_VRAMFreeIsFrozenAfterTheFirstReading(t *testing.T) {
	free := 20000
	p := NewProfiler(filepath.Join(t.TempDir(), "cache.json"),
		WithTTL(0), // every call re-detects, which is the hostile case
		// Seal the per-OS UMA hook. On a darwin runner it would set
		// UnifiedMemory and UsableVRAMMB from the real machine, and a
		// unified-memory host ignores the free reading BY DESIGN — so
		// this test would assert the discrete rule against a host the
		// rule does not apply to, and fail for the right reason on the
		// wrong grounds (CLAUDE.md §Test discipline: a clean CI runner
		// hides every dependency on the developer's machine).
		WithUMA(func(context.Context, *Profile) {}),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{
				Vendor: "nvidia", Model: "NVIDIA L4", UUID: "GPU-aaa",
				VRAMTotalMB: 23034, VRAMFreeMB: free,
			}}, Accelerators{CUDA: true}, nil
		}))

	first := p.Profile(context.Background())
	if got := first.GPUs[0].VRAMFreeMB; got != 20000 {
		t.Fatalf("first reading = %d, want 20000", got)
	}

	// The engine loads 12 GB of weights. A live reading would now report
	// what is left AFTER our own model.
	free = 8000

	second := p.Profile(context.Background())
	if got := second.GPUs[0].VRAMFreeMB; got != 20000 {
		t.Errorf("second reading = %d, want the frozen 20000: a re-sample took our own "+
			"resident weights off the host's budget, which shrinks it again on every "+
			"re-tune", got)
	}
	if got := second.HostFit().OllamaVRAMBudgetMB(); got != 20000 {
		t.Errorf("ollama budget = %d, want 20000 — the budget followed the moving reading", got)
	}
}

// TestProfile_VRAMFreeFreezeIsPerDevice pins that the frozen value is
// keyed to the card it was read from. Enumeration order is not
// guaranteed stable, and replaying one card's free figure onto another
// would be worse than having none.
func TestProfile_VRAMFreeFreezeIsPerDevice(t *testing.T) {
	order := 0
	p := NewProfiler(filepath.Join(t.TempDir(), "cache.json"),
		WithTTL(0),
		WithUMA(func(context.Context, *Profile) {}),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			big := GPU{Vendor: "nvidia", Model: "NVIDIA L40S", UUID: "GPU-big", VRAMTotalMB: 49152, VRAMFreeMB: 40000}
			small := GPU{Vendor: "nvidia", Model: "NVIDIA L4", UUID: "GPU-small", VRAMTotalMB: 23034, VRAMFreeMB: 10000}
			order++
			if order == 1 {
				return []GPU{big, small}, Accelerators{CUDA: true}, nil
			}
			// Same host, opposite enumeration order, and both cards now
			// reporting nonsense a frozen value must override.
			big.VRAMFreeMB, small.VRAMFreeMB = 1, 1
			return []GPU{small, big}, Accelerators{CUDA: true}, nil
		}))

	_ = p.Profile(context.Background())
	second := p.Profile(context.Background())

	byUUID := map[string]int{}
	for _, g := range second.GPUs {
		byUUID[g.UUID] = g.VRAMFreeMB
	}
	if byUUID["GPU-big"] != 40000 {
		t.Errorf("GPU-big free = %d, want 40000", byUUID["GPU-big"])
	}
	if byUUID["GPU-small"] != 10000 {
		t.Errorf("GPU-small free = %d, want 10000", byUUID["GPU-small"])
	}
}

// TestProfile_AnUnreadableFreeFigureIsNotFrozenAsZero pins that "the
// driver would not say" is not remembered as an answer. A detector that
// could not read free memory once must not pin 0 for the process: the
// next profile may come from a source that can, and 0 means "unknown"
// everywhere downstream — freezing it would make the host permanently
// unmeasured rather than temporarily.
func TestProfile_AnUnreadableFreeFigureIsNotFrozenAsZero(t *testing.T) {
	free := 0
	p := NewProfiler(filepath.Join(t.TempDir(), "cache.json"),
		WithTTL(0),
		WithUMA(func(context.Context, *Profile) {}),
		WithGPU(func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{
				Vendor: "nvidia", Model: "NVIDIA L4", UUID: "GPU-aaa",
				VRAMTotalMB: 23034, VRAMFreeMB: free,
			}}, Accelerators{CUDA: true}, nil
		}))

	if got := p.Profile(context.Background()).GPUs[0].VRAMFreeMB; got != 0 {
		t.Fatalf("first reading = %d, want 0", got)
	}
	free = 21000
	if got := p.Profile(context.Background()).GPUs[0].VRAMFreeMB; got != 21000 {
		t.Errorf("second reading = %d, want 21000: an unknown was frozen as if it were a "+
			"measurement", got)
	}
	// And once a real reading lands, it is the one that sticks.
	free = 3000
	if got := p.Profile(context.Background()).GPUs[0].VRAMFreeMB; got != 21000 {
		t.Errorf("third reading = %d, want the frozen 21000", got)
	}
}
