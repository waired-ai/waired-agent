package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// TestVerifyOllamaTuning_InlineProjectorIsNotASpill holds the verify pass to
// the model buffers llama.cpp really reports for the qwen3.5 GGUF builds,
// whose vision tensors sit inside the weights file. ollama loads those
// through mtmd, outside load_tensors, so the device buffer never holds them;
// a plan that kept them read every full offload as a spill of the
// projector's size (waired-agent#1506: 193 MiB on the 0.8b, "0 of 26 layers
// … planned 0 MiB"). Record of today's behaviour, from the issue's
// measurements.
//
// The device and host figures are engine output, not arithmetic:
//   - 4b: CUDA0 2,513.56 MiB at full offload
//     (docs/knowledges/20260914/1230-llamacpp-fit-memory-terms.md); the
//     host side is the input copy of the tied embedding, 497.3 MiB, as that
//     note decomposes it.
//   - 9b: Vulkan0 4,717.38 MiB and Vulkan_Host 545.63 MiB at 34/34
//     (docs/knowledges/20260920/1400-engine-pins-0342-and-uv-01217.md). The
//     weights on the device do not depend on the backend.
//   - 0.8b: DERIVED, not captured at full offload: the captured 14/26 load
//     (same 1400 note) puts 510.72 + 510.72 MiB on the two sides, which is
//     the catalog's blocks + both embedding copies with no projector, and
//     that sum split at full offload is 763.78 device / 257.66 host.
func TestVerifyOllamaTuning_InlineProjectorIsNotASpill(t *testing.T) {
	// The 9b was captured on a 128 GiB unified-memory host, where the
	// verify pass has no allocation probe to fall back on, so it runs on
	// that host shape as well as on the 24 GB card.
	strixHalo := hardware.Profile{OS: "windows", Arch: "x86_64", RAMTotalGB: 128,
		UnifiedMemory: true, UsableVRAMMB: 98304,
		GPUs: []hardware.GPU{{Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics", Integrated: true, IntegratedKnown: true}}}
	cases := []struct {
		name, model, variant string
		hw                   hardware.Profile
		layers               int
		deviceMiB, hostMiB   float64
	}{
		{"0.8b-24gb", "qwen3.5-0.8b", "q8-gguf", discrete24GB(), 26, 763.78, 257.66},
		{"4b-24gb", "qwen3.5-4b", "q4-gguf", discrete24GB(), 34, 2513.56, 497.30},
		{"9b-24gb", "qwen3.5-9b", "q4-gguf", discrete24GB(), 34, 4717.38, 545.63},
		{"9b-unified", "qwen3.5-9b", "q4-gguf", strixHalo, 34, 4717.38, 545.63},
	}
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	find := func(model, variant string) (catalog.Manifest, catalog.Variant) {
		for _, m := range manifests {
			if m.ModelID != model {
				continue
			}
			for _, v := range m.Variants {
				if v.VariantID == variant {
					return m, v
				}
			}
		}
		t.Fatalf("%s/%s is not in the bundled catalog", model, variant)
		return catalog.Manifest{}, catalog.Variant{}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, v := find(c.model, c.variant)
			if v.GGUF == nil || v.GGUF.InlineProjectorBytes <= 0 {
				t.Fatalf("%s/%s no longer carries an inline projector; this case tests nothing", c.model, c.variant)
			}
			hw := c.hw
			tn := computeOllamaTuning(m, v, hw, "q8_0", ollamaObservedServe{})
			if tn.PlannedDeviceWeightMB <= 0 || tn.ExpectedSpillFraction != 0 {
				t.Fatalf("fixture drifted: planned device %d MiB, expected spill %.2f; the case needs a full offload with a device plan",
					tn.PlannedDeviceWeightMB, tn.ExpectedSpillFraction)
			}
			cells := tn.ContextLength * max(tn.NumParallel, 1)
			weight := int64(c.deviceMiB * (1 << 20))
			f := &fakeOllamaAPI{psName: "qwen3.5:test", psSize: weight, psVRAM: weight, psCtx: cells, tagSize: weight}
			srv := f.server(t)
			defer srv.Close()
			el := &fakeEngineLog{text: llamaLoadLog(cells, c.layers, c.layers, c.deviceMiB, c.hostMiB, tn.KVCacheType)}
			verdict, detail := verifyOllamaTuning(context.Background(), srv.Client(), srv.URL, tn, "qwen3.5:test", hw,
				ollamaVerifyDeps{EngineLog: el.tail})
			if verdict != tuningOK {
				t.Errorf("= (%v, %q), want tuningOK: every layer is on the GPU and the device buffer is the plan less the projector",
					verdict, detail)
			}
		})
	}
}
