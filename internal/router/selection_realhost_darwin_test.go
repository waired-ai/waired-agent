//go:build darwin

package router

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// TestRealHostSelectionAppleSilicon drives the engine + model pickers
// against the ACTUAL hardware probe of the developer's Apple Silicon Mac
// and asserts the routing decision end-to-end: Apple GPU → Ollama/Metal,
// the auto-picked model fits the real UMA budget, and a family far larger
// than that budget is rejected with a GPU-residency deficit label.
//
// This is the #415 "add a gated real-host test if practical" deliverable.
// It needs no enrollment, no WireGuard, and no root: it only exercises the
// pure pickers over the real hardware.Profile, so it complements (rather
// than re-runs) the live Ollama/Metal token-generation proof captured in
// docs/records/20260618/.
//
// Gated by WAIRED_HW_REALHOST=1 (shared with the hardware-probe real-host
// test) so a normal `go test ./...` on a non-Apple CI runner skips it.
// It additionally self-skips if the probe does not report an Apple GPU,
// so it is a no-op on an Intel Mac (which ships no catalog Mac variants).
//
// To run on an Apple Silicon Mac:
//
//	WAIRED_HW_REALHOST=1 go test ./internal/router/ -run RealHostSelection -v
func TestRealHostSelectionAppleSilicon(t *testing.T) {
	if os.Getenv("WAIRED_HW_REALHOST") == "" {
		t.Skip("set WAIRED_HW_REALHOST=1 to exercise selection against the real hardware probe")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hw := hardware.NewProfiler("").Profile(ctx)
	t.Logf("OS=%s Arch=%s RAMTotalGB=%d UnifiedMemory=%v UsableVRAMMB=%d EffectiveVRAMMB=%d",
		hw.OS, hw.Arch, hw.RAMTotalGB, hw.UnifiedMemory, hw.UsableVRAMMB, hw.EffectiveVRAMMB())

	if !hasAppleGPU(hw) {
		t.Skipf("no Apple GPU detected (Arch=%s GPUs=%v); selection assertions are Apple-Silicon-specific", hw.Arch, hw.GPUs)
	}

	// --- Engine pick: Apple GPU must route to Ollama (Metal/MLX engine-side). ---
	ep, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	t.Logf("engine pick: engine=%q source=%q reasons=%v", ep.Engine, ep.Source, ep.Reasons)
	if ep.Engine != catalog.RuntimeOllama {
		t.Errorf("engine = %q, want %q (Apple Silicon is an Ollama/Metal path)", ep.Engine, catalog.RuntimeOllama)
	}
	if ep.Source != EngineSourceAuto {
		t.Errorf("engine source = %q, want %q", ep.Source, EngineSourceAuto)
	}
	if !reasonsMention(ep.Reasons, "Apple GPU detected") {
		t.Errorf("engine reasons do not explain the Apple decision: %v", ep.Reasons)
	}

	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	// EngineVersion = the pinned serving engine we actually ship/proved, so
	// MinEngineVersion-floored variants (e.g. qwen3.6 mtp) are eligible.
	engineVer := runtime.OllamaPinnedVersion

	// --- Model pick: the top auto-selected model must fit the real UMA budget. ---
	pick, err := PickModel(PickInput{
		Catalog:       manifests,
		Hardware:      hw,
		Engine:        catalog.RuntimeOllama,
		EngineVersion: engineVer,
	})
	if err != nil {
		t.Fatalf("PickModel on real Apple Silicon: %v", err)
	}
	t.Logf("auto-pick: model=%s variant=%s weightGB=%.1f minRAMGB=%d quality=%d tag=%q",
		pick.Manifest.ModelID, pick.Variant.VariantID, pick.Variant.EstimatedWeightGB,
		pick.Variant.MinRAMGB, pick.Variant.QualityTier, pick.Variant.Source.Tag)

	if !variantRunsOllama(pick.Variant) {
		t.Errorf("auto-picked variant %s/%s does not declare ollama in runtime_support: %v",
			pick.Manifest.ModelID, pick.Variant.VariantID, pick.Variant.RuntimeSupport)
	}
	// The pick must be resident in the GPU-addressable budget (this is what
	// the picker promises on UMA hosts). Re-check independently here.
	if !ollamaFitsVRAM(pick.Variant, hw) {
		t.Errorf("auto-picked variant %s/%s does not fit EffectiveVRAMMB=%d by ollamaFitsVRAM",
			pick.Manifest.ModelID, pick.Variant.VariantID, hw.EffectiveVRAMMB())
	}

	// Golden assertion for the canonical 16 GB Apple Silicon mini (this
	// machine). On other RAM tiers the pick legitimately differs, so only
	// pin the exact model when the probe reports 16 GB. NOTE: the top pick
	// here is qwen3.5-9b (quality 52), NOT qwen2.5-coder-7b — on a UMA host
	// the picker skips the MinRAMGB gate (qwen3.5-9b declares min_ram_gb=12)
	// and qwen3.5-9b's 6.6 GB weights fit the 12288 MB budget while
	// out-ranking the coder-7b on quality. This corrects the (estimated)
	// expectation in #415's acceptance text. See docs/records/20260619/.
	if hw.RAMTotalGB == 16 {
		const want = "qwen3.5-9b"
		if pick.Manifest.ModelID != want {
			t.Errorf("16 GB Apple Silicon auto-pick = %s, want %s (UMA budget %d MB)",
				pick.Manifest.ModelID, want, hw.EffectiveVRAMMB())
		}
	}

	// --- Deficit labels (the UMA-relevant GPU-residency path) ---
	// A family whose MinRAMGB is satisfied by the host RAM but whose weights
	// exceed the GPU-addressable budget must report the GPU-residency
	// reason. On a 16 GB Mac, qwen2.5-coder-14b (min_ram_gb=16, ~9 GB
	// weights) is exactly this case: the RAM gate passes but ~9 GB + KV +
	// overhead overflows the 12288 MB UMA budget.
	if hw.RAMTotalGB == 16 {
		if m, ok := manifestByPrefix(manifests, "qwen2.5-coder-14b"); ok {
			fit := FamilyBestFit(m, catalog.RuntimeOllama, engineVer, hw)
			t.Logf("family %s: fits=%v deficit=%q", m.ModelID, fit.Fits, fit.DeficitLabel)
			if fit.Fits {
				t.Errorf("family %s unexpectedly fits a 12288 MB UMA budget", m.ModelID)
			}
			if !strings.Contains(fit.DeficitLabel, "GPU-resident") {
				t.Errorf("family %s deficit = %q, want a GPU-residency reason", m.ModelID, fit.DeficitLabel)
			}
		}
	}

	// A family far above the budget must be rejected with the GPU-residency
	// reason. Its MinRAMGB (32) exceeds a 16 GB Mac's RAM, but on a UMA host
	// the fit path ignores MinRAMGB and rejects on GPU residency — so the
	// label must say so too, not "needs 32 GB RAM" (#425 fixed that).
	const bigModel = "qwen3.6-35b-a3b" // ~21 GB q4 weights, min_ram_gb=32
	if m, ok := manifestByPrefix(manifests, bigModel); ok {
		fit := FamilyBestFit(m, catalog.RuntimeOllama, engineVer, hw)
		t.Logf("family %s: fits=%v deficit=%q", m.ModelID, fit.Fits, fit.DeficitLabel)
		if hw.EffectiveVRAMMB() > 0 && hw.EffectiveVRAMMB() < 25000 {
			if fit.Fits {
				t.Errorf("family %s reported as fitting on a %d MB UMA budget; expected rejection",
					m.ModelID, hw.EffectiveVRAMMB())
			}
			if !strings.Contains(fit.DeficitLabel, "GPU-resident") {
				t.Errorf("family %s deficit = %q, want a GPU-residency reason (UMA ignores MinRAMGB, #425)",
					m.ModelID, fit.DeficitLabel)
			}
		}
	} else {
		t.Logf("catalog has no %s family; skipping the over-budget deficit assertion", bigModel)
	}

	// Full ranked candidate list, for the work-record snapshot.
	if ranked, rerr := RankModels(PickInput{
		Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama, EngineVersion: engineVer,
	}); rerr == nil {
		t.Logf("ranked %d fitting candidates on this host:", len(ranked))
		for i, r := range ranked {
			t.Logf("  #%d %s/%s q%d (%.1f GB)", i+1, r.Manifest.ModelID, r.Variant.VariantID,
				r.Variant.QualityTier, r.Variant.EstimatedWeightGB)
		}
	}
}

func hasAppleGPU(hw hardware.Profile) bool {
	for _, g := range hw.GPUs {
		if strings.EqualFold(g.Vendor, "apple") {
			return true
		}
	}
	return false
}

func reasonsMention(reasons []string, substr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func variantRunsOllama(v catalog.Variant) bool {
	for _, rt := range v.RuntimeSupport {
		if rt == catalog.RuntimeOllama {
			return true
		}
	}
	return false
}

// manifestByPrefix lives in uma_tiers_estimated_test.go (untagged) so it is
// available to this darwin-tagged file and to the cross-platform tier tests.
