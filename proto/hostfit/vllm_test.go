package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// L4-shaped devices, the same fixtures internal/router's sizing tests use
// (internal/router/coding_floor_test.go). This is the first test in this
// package to build a per-device GPU list: everything before it went
// through hostFromWire, which projects the device list away.
var (
	l4    = signer.HardwareGPUSummary{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}
	adaL4 = signer.HardwareGPUSummary{
		Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034, ComputeCap: "8.9",
	}
)

func windowManifest(ctx int) catalog.Manifest {
	return catalog.Manifest{ModelID: "m", ContextLength: ctx}
}

// TestVLLMRecommendModelOnHost pins the clause waired-agent#1061 added.
// PRODUCT CONTRACT (waired-agent#1061, waired-agent#1029): a vLLM row is
// demoted when this host would have to clamp the window below the coding
// target, exactly as the ollama tab is demoted by OllamaDeclaresWindow.
func TestVLLMRecommendModelOnHost(t *testing.T) {
	// 14 GB weights ×1.15 + 73728 B/tok × 200704 ≈ 30.9 GB: over one L4's
	// ~20.5 GB utilization budget, within 2×L4's ~41 GB.
	wide := catalog.Variant{EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728}
	m := windowManifest(262144)

	t.Run("one L4 cannot hold the coding window", func(t *testing.T) {
		got := hostfit.VLLMRecommendModelOnHost(m, wide, hostfit.Host{}, []signer.HardwareGPUSummary{l4})
		if got.Fits || got.Reason != hostfit.ReasonWindowExceedsMemory {
			t.Fatalf("got %+v, want a window_exceeds_memory demotion", got)
		}
		if got.NeedMB != 0 || got.HaveMB != 0 {
			t.Errorf("NeedMB/HaveMB = %d/%d, want both unset: what does not fit is a "+
				"token count, and the console copy for this reason takes no sizes",
				got.NeedMB, got.HaveMB)
		}
	})

	t.Run("two L4s can, so the same model is recommended", func(t *testing.T) {
		got := hostfit.VLLMRecommendModelOnHost(m, wide, hostfit.Host{}, []signer.HardwareGPUSummary{l4, l4})
		if !got.Fits {
			t.Fatalf("got %+v, want Fits at TP=2", got)
		}
	})

	t.Run("the manifest clause still wins, and names itself", func(t *testing.T) {
		// Below the declarable window: no hardware moves this, so the host
		// clause must not get a chance to relabel it.
		got := hostfit.VLLMRecommendModelOnHost(
			windowManifest(131072), wide, hostfit.Host{}, []signer.HardwareGPUSummary{l4, l4})
		if got.Fits || got.Reason != hostfit.ReasonWindowTooSmall {
			t.Fatalf("got %+v, want window_too_small", got)
		}
	})

	t.Run("fp8 KV on Ada widens one card past the floor", func(t *testing.T) {
		v8 := catalog.Variant{EstimatedWeightGB: 8.0, KVBytesPerTokenFP16: 73728}
		if got := hostfit.VLLMRecommendModelOnHost(
			m, v8, hostfit.Host{}, []signer.HardwareGPUSummary{l4}); got.Fits {
			t.Errorf("f16 KV (no compute_cap) on one L4: got %+v, want demoted", got)
		}
		if got := hostfit.VLLMRecommendModelOnHost(
			m, v8, hostfit.Host{}, []signer.HardwareGPUSummary{adaL4}); !got.Fits {
			t.Errorf("fp8 KV (Ada compute_cap 8.9) on one L4: got %+v, want Fits", got)
		}
	})

	t.Run("permissive on inputs it does not have", func(t *testing.T) {
		for name, gpus := range map[string][]signer.HardwareGPUSummary{
			"no device detail": nil,
			"no NVIDIA device": {{Vendor: "amd", Model: "RX 7900", VRAMTotalMB: 24576}},
		} {
			if got := hostfit.VLLMRecommendModelOnHost(m, wide, hostfit.Host{}, gpus); !got.Fits {
				t.Errorf("%s: got %+v, want Fits — capacity owns the rejection", name, got)
			}
		}
		unannotated := catalog.Variant{}
		if got := hostfit.VLLMRecommendModelOnHost(
			m, unannotated, hostfit.Host{}, []signer.HardwareGPUSummary{l4}); !got.Fits {
			t.Errorf("unannotated variant: got %+v, want Fits", got)
		}
	})
}

// TestVLLMRecommendModelKeepsItsManifestOnlyAnswer pins that the published
// entry point did NOT change when the host clause arrived: it is what a
// caller holding no device detail gets, and three call sites across two
// repositories reach the recommendation through it.
func TestVLLMRecommendModelKeepsItsManifestOnlyAnswer(t *testing.T) {
	// Wide enough that the host clause WOULD demote it on one L4.
	wide := catalog.Variant{EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728}
	m := windowManifest(262144)
	if got := hostfit.VLLMRecommendModel(m, wide, hostfit.Host{VRAM0MB: 23034}); !got.Fits {
		t.Fatalf("got %+v, want Fits: this entry point cannot see the device list", got)
	}
}

// TestProjectModelIsProjectModelFromWithoutDevices pins the delegation, so
// every existing caller of ProjectModel keeps its exact answer.
func TestProjectModelIsProjectModelFromWithoutDevices(t *testing.T) {
	m := windowManifest(262144)
	v := catalog.Variant{
		VariantID: "awq", RuntimeSupport: []string{catalog.RuntimeVLLM},
		QualityTier: 80, MinVRAMMB: 20480,
		EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728,
	}
	h := hostfit.Host{GPUCount: 1, VRAM0MB: 23034, RAMTotalGB: 64}
	for _, engine := range []string{catalog.RuntimeVLLM, catalog.RuntimeOllama, "nonesuch"} {
		want := hostfit.ProjectModelFrom(hostfit.ModelProjection{
			Manifest: m, Variant: v, Engine: engine, Host: h, BudgetMB: 23034,
		})
		got := hostfit.ProjectModel(m, v, engine, h, 23034)
		if got != want {
			t.Errorf("engine=%s: ProjectModel = %+v, ProjectModelFrom(nil GPUs) = %+v",
				engine, got, want)
		}
	}
}

// TestProjectModelFromDemotesOnTheHostClause is the surface half of
// waired-agent#1061: the wizard row, not just the verdict function.
func TestProjectModelFromDemotesOnTheHostClause(t *testing.T) {
	m := windowManifest(262144)
	v := catalog.Variant{
		VariantID: "awq", RuntimeSupport: []string{catalog.RuntimeVLLM},
		QualityTier: 80, MinVRAMMB: 20480,
		EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728,
	}
	h := hostfit.Host{GPUCount: 1, VRAM0MB: 23034, RAMTotalGB: 64}
	in := hostfit.ModelProjection{
		Manifest: m, Variant: v, Engine: catalog.RuntimeVLLM, Host: h,
		// A budget the weights clear, so capacity is not what this measures.
		BudgetMB: 23034,
		GPUs:     []signer.HardwareGPUSummary{l4},
	}
	got := hostfit.ProjectModelFrom(in)
	if !got.Runnable {
		t.Fatalf("got %+v, want Runnable: capacity must not be what demotes this row", got)
	}
	if !got.NotRecommended || got.NotRecommendedReason != hostfit.ReasonWindowExceedsMemory {
		t.Fatalf("got %+v, want a window_exceeds_memory annotation", got)
	}
	in.GPUs = []signer.HardwareGPUSummary{l4, l4}
	if got := hostfit.ProjectModelFrom(in); got.NotRecommended {
		t.Fatalf("TP=2 got %+v, want no annotation", got)
	}
}
