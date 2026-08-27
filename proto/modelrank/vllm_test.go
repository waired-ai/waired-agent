package modelrank

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestVLLMConstsMatchHostfit pins the two copies of the vLLM sizing
// constants together. They are written out in both packages rather than
// aliased because the proto additive guard compares a const's VALUE AS
// WRITTEN (scripts/ci/protoguard/main.go): `= hostfit.X` reads as a changed
// value even when the number is identical, so an alias would fail the
// guard. This test is what keeps the duplication honest.
func TestVLLMConstsMatchHostfit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		here, over float64
	}{
		{"KVFactorF16", KVFactorF16, hostfit.VLLMKVFactorF16},
		{"KVFactorFP8", KVFactorFP8, hostfit.VLLMKVFactorFP8},
		{"DefaultVLLMGPUMemoryUtilization",
			DefaultVLLMGPUMemoryUtilization, hostfit.DefaultVLLMGPUMemoryUtilization},
	} {
		if tc.here != tc.over {
			t.Errorf("%s: modelrank has %v, hostfit has %v", tc.name, tc.here, tc.over)
		}
	}
}

// TestVLLMWrappersDelegate pins that the published modelrank spellings
// still answer exactly what hostfit does, on inputs that exercise every
// branch the wrappers cover. The bodies moved (waired-agent#1061); the
// names did not, because the additive guard forbids removing them and
// three consumers across two repositories still call them.
func TestVLLMWrappersDelegate(t *testing.T) {
	l4 := signer.HardwareGPUSummary{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}
	ada := signer.HardwareGPUSummary{
		Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034, ComputeCap: "8.9",
	}
	amd := signer.HardwareGPUSummary{Vendor: "amd", Model: "RX 7900", VRAMTotalMB: 24576}
	host := hostfit.Host{GPUCount: 1, VRAM0MB: 23034}

	for name, gpus := range map[string][]signer.HardwareGPUSummary{
		"none":         nil,
		"one L4":       {l4},
		"two L4":       {l4, l4},
		"three L4":     {l4, l4, l4},
		"one Ada L4":   {ada},
		"mixed vendor": {l4, amd},
	} {
		if got, want := VLLMTensorParallelSize(gpus), hostfit.VLLMTensorParallelSize(gpus); got != want {
			t.Errorf("%s: VLLMTensorParallelSize = %d, hostfit = %d", name, got, want)
		}
		if got, want := VLLMUsesFP8KV(gpus), hostfit.VLLMUsesFP8KV(gpus); got != want {
			t.Errorf("%s: VLLMUsesFP8KV = %v, hostfit = %v", name, got, want)
		}
		if got, want := VLLMKVFactor(gpus), hostfit.VLLMKVFactor(gpus); got != want {
			t.Errorf("%s: VLLMKVFactor = %v, hostfit = %v", name, got, want)
		}
		if got, want := VLLMVRAMBudgetMB(host, gpus), hostfit.VLLMVRAMBudgetMB(host, gpus); got != want {
			t.Errorf("%s: VLLMVRAMBudgetMB = %d, hostfit = %d", name, got, want)
		}
		got := VLLMMaxModelLen(14.0, 73728, VLLMTensorParallelSize(gpus),
			DefaultVLLMGPUMemoryUtilization, VLLMKVFactor(gpus), gpus)
		want := hostfit.VLLMMaxModelLen(14.0, 73728, hostfit.VLLMTensorParallelSize(gpus),
			hostfit.DefaultVLLMGPUMemoryUtilization, hostfit.VLLMKVFactor(gpus), gpus)
		if got != want {
			t.Errorf("%s: VLLMMaxModelLen = %d, hostfit = %d", name, got, want)
		}
	}
}
