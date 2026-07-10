package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

func nvidiaGPU(model string, vramMB int) hardware.GPU {
	return hardware.GPU{Vendor: "nvidia", Model: model, VRAMTotalMB: vramMB}
}

// nvidiaGPUCap builds an NVIDIA GPU with a compute-capability string, as
// nvidia-smi reports it (e.g. "8.9" for Ada L4).
func nvidiaGPUCap(model string, vramMB int, computeCap string) hardware.GPU {
	return hardware.GPU{Vendor: "nvidia", Model: model, VRAMTotalMB: vramMB, ComputeCap: computeCap}
}

func TestVLLMTensorParallelSize(t *testing.T) {
	l4 := nvidiaGPU("NVIDIA L4", 23034)
	cases := []struct {
		name string
		gpus []hardware.GPU
		want int
	}{
		{"no GPUs", nil, 1},
		{"single GPU", []hardware.GPU{l4}, 1},
		{"two identical", []hardware.GPU{l4, l4}, 2},
		{"three identical rounds down to 2", []hardware.GPU{l4, l4, l4}, 2},
		{"four identical", []hardware.GPU{l4, l4, l4, l4}, 4},
		{"five identical rounds down to 4", []hardware.GPU{l4, l4, l4, l4, l4}, 4},
		{"eight identical", []hardware.GPU{l4, l4, l4, l4, l4, l4, l4, l4}, 8},
		{
			"heterogeneous models never co-shard",
			[]hardware.GPU{l4, nvidiaGPU("NVIDIA GeForce RTX 4090", 24564)},
			1,
		},
		{
			// Same marketing name, different VRAM (e.g. RTX 3080 10G vs
			// 12G) must not co-shard either.
			"same model different VRAM never co-shards",
			[]hardware.GPU{nvidiaGPU("NVIDIA GeForce RTX 3080", 10240), nvidiaGPU("NVIDIA GeForce RTX 3080", 12288)},
			1,
		},
		{
			"non-NVIDIA GPUs are ignored",
			[]hardware.GPU{l4, l4, {Vendor: "amd", Model: "Radeon RX 7900 XTX", VRAMTotalMB: 24576}},
			2,
		},
		{
			"only non-NVIDIA GPUs",
			[]hardware.GPU{{Vendor: "amd", Model: "Radeon RX 7900 XTX", VRAMTotalMB: 24576}},
			1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hw := hardware.Profile{GPUs: tc.gpus}
			if got := VLLMTensorParallelSize(hw); got != tc.want {
				t.Errorf("VLLMTensorParallelSize(%d GPUs) = %d, want %d", len(tc.gpus), got, tc.want)
			}
		})
	}
}

func TestVLLMVRAMBudgetMB(t *testing.T) {
	l4 := nvidiaGPU("NVIDIA L4", 23034)
	cases := []struct {
		name string
		hw   hardware.Profile
		want int
	}{
		{"no GPUs", hardware.Profile{}, 0},
		{"single GPU keeps EffectiveVRAMMB exactly", hardware.Profile{GPUs: []hardware.GPU{l4}}, 23034},
		{"two identical aggregate minus per-GPU overhead", hardware.Profile{GPUs: []hardware.GPU{l4, l4}}, 2 * (23034 - 1024)},
		{"three identical budget at TP=2", hardware.Profile{GPUs: []hardware.GPU{l4, l4, l4}}, 2 * (23034 - 1024)},
		{"four identical", hardware.Profile{GPUs: []hardware.GPU{l4, l4, l4, l4}}, 4 * (23034 - 1024)},
		{
			"heterogeneous mix falls back to GPUs[0]",
			hardware.Profile{GPUs: []hardware.GPU{l4, nvidiaGPU("NVIDIA GeForce RTX 4090", 24564)}},
			23034,
		},
		{
			"UMA host keeps UsableVRAMMB",
			hardware.Profile{
				GPUs:          []hardware.GPU{{Vendor: "apple", Model: "Apple M4 Max", VRAMTotalMB: 98304}},
				UnifiedMemory: true,
				UsableVRAMMB:  98304,
			},
			98304,
		},
		{
			"non-NVIDIA only falls back to GPUs[0]",
			hardware.Profile{GPUs: []hardware.GPU{{Vendor: "amd", Model: "Radeon RX 7900 XTX", VRAMTotalMB: 24576}}},
			24576,
		},
		{
			// Degenerate: per-GPU overhead swallows tiny devices; the
			// budget never drops below the single-GPU (TP=1) figure.
			"aggregate never below single-GPU budget",
			hardware.Profile{GPUs: []hardware.GPU{nvidiaGPU("tiny", 1024), nvidiaGPU("tiny", 1024)}},
			1024,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VLLMVRAMBudgetMB(tc.hw); got != tc.want {
				t.Errorf("VLLMVRAMBudgetMB() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestVLLMMaxModelLen(t *testing.T) {
	l4 := nvidiaGPU("NVIDIA L4", 23034)
	oneL4 := hardware.Profile{GPUs: []hardware.GPU{l4}}
	twoL4 := hardware.Profile{GPUs: []hardware.GPU{l4, l4}}
	// gpt-oss-20b-class inputs (internal/catalog/bundled/gpt-oss-20b.json):
	// estimated_weight_gb 14.0, kv_bytes_per_token_fp16 73728, native 131072.
	const (
		weightGB = 14.0
		kvBytes  = 73728
	)

	t.Run("single L4 clamps a 131072-native window", func(t *testing.T) {
		got := VLLMMaxModelLen(weightGB, kvBytes, 1, 0.85, scoring.KVFactorF16, oneL4)
		// budget = (0.85 × 23034 − 1024) MiB ≈ 19.46 GB; weights ×1.15
		// = 16.1 GB; leftover ≈ 3.36 GB / 73728 B/tok → 1024-aligned.
		if got != 45056 {
			t.Errorf("VLLMMaxModelLen(1×L4) = %d, want 45056", got)
		}
		if got >= 131072 {
			t.Errorf("expected a clamp below the 131072 native window, got %d", got)
		}
	})

	t.Run("fp8 KV factor roughly doubles the f16 window", func(t *testing.T) {
		// Same budget, KV bytes halved (kvFactor 0.5) → the leftover holds
		// twice the tokens: 3.356 GB / 36864 B/tok → 90112 (1024-aligned).
		got := VLLMMaxModelLen(weightGB, kvBytes, 1, 0.85, scoring.KVFactorFP8, oneL4)
		if got != 90112 {
			t.Errorf("VLLMMaxModelLen(1×L4, fp8) = %d, want 90112", got)
		}
		f16 := VLLMMaxModelLen(weightGB, kvBytes, 1, 0.85, scoring.KVFactorF16, oneL4)
		if got <= f16 {
			t.Errorf("fp8 window %d should exceed the f16 window %d", got, f16)
		}
	})

	t.Run("TP=2 doubles the budget past the native window", func(t *testing.T) {
		got := VLLMMaxModelLen(weightGB, kvBytes, 2, 0.85, scoring.KVFactorF16, twoL4)
		if got < 131072 {
			t.Errorf("VLLMMaxModelLen(2×L4, tp=2) = %d, want ≥ 131072 (no clamp)", got)
		}
	})

	t.Run("tp argument is honoured, not re-derived", func(t *testing.T) {
		// Two GPUs present but tp=1 (operator escape hatch): budget stays
		// single-GPU.
		got := VLLMMaxModelLen(weightGB, kvBytes, 1, 0.85, scoring.KVFactorF16, twoL4)
		if got != 45056 {
			t.Errorf("VLLMMaxModelLen(2×L4, tp=1) = %d, want 45056", got)
		}
	})

	t.Run("unknown inputs return 0", func(t *testing.T) {
		if got := VLLMMaxModelLen(0, kvBytes, 1, 0.85, scoring.KVFactorF16, oneL4); got != 0 {
			t.Errorf("weightGB=0: got %d, want 0", got)
		}
		if got := VLLMMaxModelLen(weightGB, 0, 1, 0.85, scoring.KVFactorF16, oneL4); got != 0 {
			t.Errorf("kvBytes=0: got %d, want 0", got)
		}
		if got := VLLMMaxModelLen(weightGB, kvBytes, 1, 0, scoring.KVFactorF16, oneL4); got != 0 {
			t.Errorf("util=0: got %d, want 0", got)
		}
		if got := VLLMMaxModelLen(weightGB, kvBytes, 1, 0.85, 0, oneL4); got != 0 {
			t.Errorf("kvFactor=0: got %d, want 0", got)
		}
		if got := VLLMMaxModelLen(weightGB, kvBytes, 1, 0.85, scoring.KVFactorF16, hardware.Profile{}); got != 0 {
			t.Errorf("no GPU: got %d, want 0", got)
		}
	})

	t.Run("weights alone exceeding the budget return 0", func(t *testing.T) {
		if got := VLLMMaxModelLen(40.0, kvBytes, 1, 0.85, scoring.KVFactorF16, oneL4); got != 0 {
			t.Errorf("40 GB weights on 1×L4: got %d, want 0", got)
		}
	})
}

func TestVLLMUsesFP8KV(t *testing.T) {
	cases := []struct {
		name string
		gpus []hardware.GPU
		want bool
	}{
		{"ada L4 8.9", []hardware.GPU{nvidiaGPUCap("NVIDIA L4", 23034, "8.9")}, true},
		{"two ada L4", []hardware.GPU{nvidiaGPUCap("NVIDIA L4", 23034, "8.9"), nvidiaGPUCap("NVIDIA L4", 23034, "8.9")}, true},
		{"hopper H100 9.0", []hardware.GPU{nvidiaGPUCap("NVIDIA H100", 81559, "9.0")}, true},
		{"blackwell 12.0", []hardware.GPU{nvidiaGPUCap("NVIDIA RTX 5090", 32768, "12.0")}, true},
		{"ampere A100 8.0", []hardware.GPU{nvidiaGPUCap("NVIDIA A100", 81920, "8.0")}, false},
		{"ampere 3090 8.6", []hardware.GPU{nvidiaGPUCap("NVIDIA GeForce RTX 3090", 24576, "8.6")}, false},
		{"turing T4 7.5", []hardware.GPU{nvidiaGPUCap("NVIDIA T4", 15360, "7.5")}, false},
		{"mixed ada+ampere fails closed", []hardware.GPU{nvidiaGPUCap("NVIDIA L4", 23034, "8.9"), nvidiaGPUCap("NVIDIA A100", 81920, "8.0")}, false},
		{"empty compute cap fails closed", []hardware.GPU{nvidiaGPU("NVIDIA L4", 23034)}, false},
		{"malformed compute cap fails closed", []hardware.GPU{nvidiaGPUCap("NVIDIA L4", 23034, "sm_89")}, false},
		{"no NVIDIA GPU", []hardware.GPU{{Vendor: "amd", Model: "Radeon RX 7900 XTX", VRAMTotalMB: 24576}}, false},
		{"no GPU at all", nil, false},
		{
			"non-NVIDIA GPUs ignored; NVIDIA Ada engages",
			[]hardware.GPU{nvidiaGPUCap("NVIDIA L4", 23034, "8.9"), {Vendor: "amd", Model: "Radeon", VRAMTotalMB: 24576}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hw := hardware.Profile{GPUs: tc.gpus}
			if got := VLLMUsesFP8KV(hw); got != tc.want {
				t.Errorf("VLLMUsesFP8KV(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestVLLMKVFactor(t *testing.T) {
	ada := hardware.Profile{GPUs: []hardware.GPU{nvidiaGPUCap("NVIDIA L4", 23034, "8.9")}}
	if got := VLLMKVFactor(ada); got != scoring.KVFactorFP8 {
		t.Errorf("VLLMKVFactor(Ada) = %v, want %v (fp8)", got, scoring.KVFactorFP8)
	}
	ampere := hardware.Profile{GPUs: []hardware.GPU{nvidiaGPUCap("NVIDIA A100", 81920, "8.0")}}
	if got := VLLMKVFactor(ampere); got != scoring.KVFactorF16 {
		t.Errorf("VLLMKVFactor(Ampere) = %v, want %v (f16)", got, scoring.KVFactorF16)
	}
	if got := VLLMKVFactor(hardware.Profile{}); got != scoring.KVFactorF16 {
		t.Errorf("VLLMKVFactor(no GPU) = %v, want %v (f16)", got, scoring.KVFactorF16)
	}
}
