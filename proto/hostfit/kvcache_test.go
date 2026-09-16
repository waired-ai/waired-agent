package hostfit_test

import (
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestKVCacheChoices pins the ladder the owner decision describes
// ("q4_0 by default; where it cannot be used, the smallest type above
// it", decision 2 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md):
// lowest precision first, narrowed by the build's list and by the GPU on
// vLLM, with the unquantised type always left. A CPU-only host gets the
// ollama ladder a GPU host does
// (docs/decisions/20260916/2250-cpu-kv-cache-defaults-to-q4-0.md).
func TestKVCacheChoices(t *testing.T) {
	gpu := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24467}
	uma := hostfit.Host{RAMTotalGB: 128, UnifiedMemory: true}
	cpu := hostfit.Host{RAMTotalGB: 64}
	ada := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.9"}}
	ampere := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.6"}}
	qwen := []string{catalog.KVCacheQ4_0, catalog.KVCacheQ8_0, catalog.KVCacheF16}
	noQ4 := []string{catalog.KVCacheQ8_0, catalog.KVCacheF16}

	cases := []struct {
		name   string
		engine string
		list   []string
		host   hostfit.Host
		gpus   []signer.HardwareGPUSummary
		want   []string
	}{
		{"ollama, GPU, Qwen list", catalog.RuntimeOllama, qwen, gpu, nil, []string{"q4_0", "q8_0", "f16"}},
		{"ollama, unified memory, Qwen list", catalog.RuntimeOllama, qwen, uma, nil, []string{"q4_0", "q8_0", "f16"}},
		{"ollama, GPU, list without q4_0", catalog.RuntimeOllama, noQ4, gpu, nil, []string{"q8_0", "f16"}},
		{"ollama, GPU, no list is today's two types", catalog.RuntimeOllama, nil, gpu, nil, []string{"q8_0", "f16"}},
		{"ollama, CPU only has the same ladder", catalog.RuntimeOllama, qwen, cpu, nil, []string{"q4_0", "q8_0", "f16"}},
		{"ollama, CPU only, list without q4_0", catalog.RuntimeOllama, noQ4, cpu, nil, []string{"q8_0", "f16"}},
		{"ollama, a list without f16 still offers it", catalog.RuntimeOllama, []string{catalog.KVCacheQ4_0}, gpu, nil, []string{"q4_0", "f16"}},
		{"vLLM, Ada", catalog.RuntimeVLLM, nil, gpu, ada, []string{"fp8", "fp16"}},
		{"vLLM, before Ada has no fp8", catalog.RuntimeVLLM, nil, gpu, ampere, []string{"fp16"}},
		{"vLLM, no GPU detail has no fp8", catalog.RuntimeVLLM, nil, gpu, nil, []string{"fp16"}},
		{"vLLM, list of fp16 only", catalog.RuntimeVLLM, []string{catalog.KVCacheFP16}, gpu, ada, []string{"fp16"}},
		{"unknown engine", "mlx", qwen, gpu, ada, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := catalog.Variant{VariantID: "v", KVCacheTypes: tc.list}
			if got := hostfit.KVCacheChoices(tc.engine, v, tc.host, tc.gpus); !slices.Equal(got, tc.want) {
				t.Errorf("KVCacheChoices = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveKVCacheType pins what a request resolves to: itself when
// allowed, the first allowed rung otherwise, and that same rung for "no
// instruction".
func TestResolveKVCacheType(t *testing.T) {
	gpu := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24467}
	cpu := hostfit.Host{RAMTotalGB: 64}
	qwen := catalog.Variant{VariantID: "q", KVCacheTypes: []string{catalog.KVCacheQ4_0, catalog.KVCacheQ8_0, catalog.KVCacheF16}}
	gptoss := catalog.Variant{VariantID: "g", KVCacheTypes: []string{catalog.KVCacheQ8_0, catalog.KVCacheF16}}
	ada := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.9"}}

	cases := []struct {
		name   string
		engine string
		v      catalog.Variant
		host   hostfit.Host
		gpus   []signer.HardwareGPUSummary
		want   string
		expect string
	}{
		{"no instruction on a GPU host is q4_0", catalog.RuntimeOllama, qwen, gpu, nil, "", "q4_0"},
		{"a build without q4_0 defaults to q8_0", catalog.RuntimeOllama, gptoss, gpu, nil, "", "q8_0"},
		{"asking for q4_0 on that build is q8_0", catalog.RuntimeOllama, gptoss, gpu, nil, "q4_0", "q8_0"},
		{"asking for f16 is honoured", catalog.RuntimeOllama, qwen, gpu, nil, "f16", "f16"},
		{"no instruction on a CPU-only host is q4_0 too", catalog.RuntimeOllama, qwen, cpu, nil, "", "q4_0"},
		{"CPU only honours q8_0", catalog.RuntimeOllama, qwen, cpu, nil, "q8_0", "q8_0"},
		{"a vLLM type asked of ollama is the default", catalog.RuntimeOllama, qwen, gpu, nil, "fp8", "q4_0"},
		{"vLLM on Ada defaults to fp8", catalog.RuntimeVLLM, catalog.Variant{}, gpu, ada, "", "fp8"},
		{"vLLM on Ada honours fp16", catalog.RuntimeVLLM, catalog.Variant{}, gpu, ada, "fp16", "fp16"},
		{"vLLM without Ada refuses fp8", catalog.RuntimeVLLM, catalog.Variant{}, gpu, nil, "fp8", "fp16"},
		{"unknown engine", "mlx", qwen, gpu, ada, "q4_0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.ResolveKVCacheType(tc.engine, tc.v, tc.host, tc.gpus, tc.want); got != tc.expect {
				t.Errorf("ResolveKVCacheType(%q) = %q, want %q", tc.want, got, tc.expect)
			}
		})
	}
}

// TestOllamaDefaultKVCacheType pins the host half of the default. PRODUCT
// CONTRACT: q4_0 on every host, CPU-only included (owner decision
// 2026-09-16, docs/decisions/20260916/2250-cpu-kv-cache-defaults-to-q4-0.md).
func TestOllamaDefaultKVCacheType(t *testing.T) {
	if got := hostfit.OllamaDefaultKVCacheType(hostfit.Host{GPUCount: 1, VRAM0MB: 8000}); got != catalog.KVCacheQ4_0 {
		t.Errorf("GPU host default = %q, want q4_0", got)
	}
	if got := hostfit.OllamaDefaultKVCacheType(hostfit.Host{UnifiedMemory: true, RAMTotalGB: 32}); got != catalog.KVCacheQ4_0 {
		t.Errorf("unified-memory host default = %q, want q4_0", got)
	}
	if got := hostfit.OllamaDefaultKVCacheType(hostfit.Host{RAMTotalGB: 32}); got != catalog.KVCacheQ4_0 {
		t.Errorf("CPU-only host default = %q, want q4_0", got)
	}
}
