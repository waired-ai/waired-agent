package scoring

import (
	"math"
	"testing"
)

// archCase bundles a model's published architecture (scoring report §3) with
// the KV/token and attention-arch the formulas must reproduce.
type archCase struct {
	name         string
	cfg          ArchConfig
	wantFullAttn int
	wantHeadDim  int
	wantKV       int    // kv_bytes_per_token_fp16
	wantArch     string // DeriveAttentionArch
}

// archCases use the exact published architecture params from the scoring
// report §3 so the formulas are pinned to a citable source.
var archCases = []archCase{
	{
		name:         "qwen3-coder-30b-a3b (dense full-attn GQA)",
		cfg:          ArchConfig{NumHiddenLayers: 48, HiddenSize: 2048, NumAttentionHeads: 32, NumKeyValueHeads: 4, HeadDim: 128, NumExperts: 128, NumExpertsPerTok: 8},
		wantFullAttn: 48, wantHeadDim: 128, wantKV: 98304, wantArch: archGQA,
	},
	{
		name:         "qwen3-coder-next-80b (hybrid mamba, interval 4)",
		cfg:          ArchConfig{NumHiddenLayers: 48, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 512, NumExpertsPerTok: 10},
		wantFullAttn: 12, wantHeadDim: 256, wantKV: 24576, wantArch: archHybridMamba,
	},
	{
		name: "qwen3.6-27b (hybrid linear, interval 4)",
		cfg:  ArchConfig{NumHiddenLayers: 64, HiddenSize: 5120, NumAttentionHeads: 24, NumKeyValueHeads: 4, HeadDim: 256, FullAttentionInterval: 4},
		// 2×16×4×256×2 = 65536. The committed qwen3.6-27b manifest agrees;
		// scoring report §3.7/§4.1 prose ("32 KB") is an arithmetic typo.
		wantFullAttn: 16, wantHeadDim: 256, wantKV: 65536, wantArch: archHybridMamba,
	},
	{
		name:         "gpt-oss-20b (sliding window)",
		cfg:          ArchConfig{NumHiddenLayers: 24, HiddenSize: 2880, NumAttentionHeads: 64, NumKeyValueHeads: 8, HeadDim: 64, SlidingWindow: 128, NumLocalExperts: 32, NumExpertsPerTok: 4, LayerTypes: alternating(24)},
		wantFullAttn: 12, wantHeadDim: 64, wantKV: 24576, wantArch: archSlidingWindow,
	},
	{
		name:         "gpt-oss-120b (sliding window)",
		cfg:          ArchConfig{NumHiddenLayers: 36, HiddenSize: 2880, NumAttentionHeads: 64, NumKeyValueHeads: 8, HeadDim: 64, SlidingWindow: 128, NumLocalExperts: 128, NumExpertsPerTok: 4, LayerTypes: alternating(36)},
		wantFullAttn: 18, wantHeadDim: 64, wantKV: 36864, wantArch: archSlidingWindow,
	},
	{
		name:         "qwen2.5-coder-7b (dense GQA)",
		cfg:          ArchConfig{NumHiddenLayers: 28, HiddenSize: 3584, NumAttentionHeads: 28, NumKeyValueHeads: 4, HeadDim: 128},
		wantFullAttn: 28, wantHeadDim: 128, wantKV: 57344, wantArch: archGQA,
	},
	{
		name:         "qwen2.5-coder-14b (dense GQA)",
		cfg:          ArchConfig{NumHiddenLayers: 48, HiddenSize: 5120, NumAttentionHeads: 40, NumKeyValueHeads: 8, HeadDim: 128},
		wantFullAttn: 48, wantHeadDim: 128, wantKV: 196608, wantArch: archGQA,
	},
	{
		name:         "qwen2.5-coder-3b (dense GQA)",
		cfg:          ArchConfig{NumHiddenLayers: 36, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 128},
		wantFullAttn: 36, wantHeadDim: 128, wantKV: 36864, wantArch: archGQA,
	},
	{
		name:         "glm-4.5-air (dense full-attn GQA)",
		cfg:          ArchConfig{NumHiddenLayers: 46, HiddenSize: 4096, NumAttentionHeads: 96, NumKeyValueHeads: 8, HeadDim: 128, NumExperts: 128, NumExpertsPerTok: 8},
		wantFullAttn: 46, wantHeadDim: 128, wantKV: 188416, wantArch: archGQA,
	},
}

// alternating builds a gpt-oss-style layer_types list: even index sliding,
// odd index full → exactly n/2 full-attention layers.
func alternating(n int) []string {
	out := make([]string, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = "sliding_attention"
		} else {
			out[i] = "full_attention"
		}
	}
	return out
}

func TestArchConfig_KVAndArch(t *testing.T) {
	for _, c := range archCases {
		t.Run(c.name, func(t *testing.T) {
			full, _ := c.cfg.FullAttnLayers()
			if full != c.wantFullAttn {
				t.Errorf("FullAttnLayers = %d, want %d", full, c.wantFullAttn)
			}
			hd, _ := c.cfg.ResolvedHeadDim()
			if hd != c.wantHeadDim {
				t.Errorf("ResolvedHeadDim = %d, want %d", hd, c.wantHeadDim)
			}
			kv := KVBytesPerTokenFP16(full, c.cfg.NumKeyValueHeads, hd)
			if kv != c.wantKV {
				t.Errorf("KVBytesPerTokenFP16 = %d, want %d", kv, c.wantKV)
			}
			if got := c.cfg.DeriveAttentionArch(); got != c.wantArch {
				t.Errorf("DeriveAttentionArch = %q, want %q", got, c.wantArch)
			}
		})
	}
}

// TestWeightGB_GoldenTable pins the bare weight formula against scoring report
// §2.5 (Q-quant rows match exactly; MXFP4 rows match the pure-expert weight
// the report documents in §3.4/§3.5).
func TestWeightGB_GoldenTable(t *testing.T) {
	cases := []struct {
		name     string
		params   int64
		quant    string
		wantGB   float64
		tolerPct float64
	}{
		{"qwen3-coder-30b-a3b Q4_K_M", 30_530_000_000, "Q4_K_M", 18.4, 5},
		{"qwen3-coder-480b Q4_K_M", 480_000_000_000, "Q4_K_M", 290, 5},
		{"qwen3.6-27b Q4_K_M", 27_000_000_000, "Q4_K_M", 16.3, 5},
		{"qwen2.5-coder-3b Q4_K_M", 3_090_000_000, "Q4_K_M", 1.9, 5},
		{"qwen2.5-coder-7b Q4_K_M", 7_610_000_000, "Q4_K_M", 4.6, 5},
		{"qwen2.5-coder-14b Q4_K_M", 14_700_000_000, "Q4_K_M", 8.9, 5},
		{"gpt-oss-120b MXFP4 (pure)", 116_830_000_000, "MXFP4", 62.0, 5},
		{"gpt-oss-20b MXFP4 (pure)", 20_910_000_000, "MXFP4", 11.1, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, ok := QuantByName(c.quant)
			if !ok {
				t.Fatalf("QuantByName(%q) not found", c.quant)
			}
			got := WeightGB(c.params, q)
			if pctDiff(got, c.wantGB) > c.tolerPct {
				t.Errorf("WeightGB = %.2f GB, want %.2f GB (±%.0f%%)", got, c.wantGB, c.tolerPct)
			}
		})
	}
}

func TestQuantByName(t *testing.T) {
	cases := []struct {
		in       string
		wantBPW  float64
		wantTier int
		wantOK   bool
	}{
		{"Q4_K_M", 4.83, 4, true},
		{"q4_k_m", 4.83, 4, true},
		{"AWQ-int4", 4.5, 4, true},
		{"awq", 4.5, 4, true},
		{"MXFP4", 4.25, 4, true},
		{"Q5_K_M", 5.69, 5, true},
		{"Q6_K", 6.57, 6, true},
		{"Q8_0", 8.5, 8, true},
		{"FP8", 8.0, 8, true},
		{"bf16", 16.0, 8, true},
		{"banana", 0, 0, false},
	}
	for _, c := range cases {
		q, ok := QuantByName(c.in)
		if ok != c.wantOK {
			t.Errorf("QuantByName(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (q.BPW != c.wantBPW || q.Tier != c.wantTier) {
			t.Errorf("QuantByName(%q) = {bpw %.2f tier %d}, want {bpw %.2f tier %d}", c.in, q.BPW, q.Tier, c.wantBPW, c.wantTier)
		}
	}
}

func TestResolvedHeadDim_DerivedWarns(t *testing.T) {
	// head_dim absent → derive hidden/heads and flag it.
	c := ArchConfig{HiddenSize: 4096, NumAttentionHeads: 32}
	hd, derived := c.ResolvedHeadDim()
	if hd != 128 || !derived {
		t.Errorf("ResolvedHeadDim() = (%d, %v), want (128, true)", hd, derived)
	}
	// head_dim present → no derivation.
	c2 := ArchConfig{HiddenSize: 2048, NumAttentionHeads: 32, HeadDim: 128}
	if hd, derived := c2.ResolvedHeadDim(); hd != 128 || derived {
		t.Errorf("ResolvedHeadDim() = (%d, %v), want (128, false)", hd, derived)
	}
}

func TestDecodeFLOPsPerTok(t *testing.T) {
	if got := DecodeFLOPsPerTok(3_300_000_000); got != 6_600_000_000 {
		t.Errorf("DecodeFLOPsPerTok = %d, want 6.6e9", got)
	}
	if got := DecodeFLOPsPerTok(0); got != 0 {
		t.Errorf("DecodeFLOPsPerTok(0) = %d, want 0", got)
	}
}

func TestVRAMAndSuggestions(t *testing.T) {
	q, _ := QuantByName("Q4_K_M")
	// qwen3-coder-30b-a3b Q4 weight ≈ 18.4 GB; KV/tok 98304.
	w := WeightGB(30_530_000_000, q)
	vram := VRAMGB(w, 98304, 32768) // ~18.4*1.15 + 3.2 ≈ 24.4 GB
	if vram < 23 || vram > 26 {
		t.Errorf("VRAMGB@32K = %.1f GB, want ~24 GB", vram)
	}
	if got := SuggestMinVRAMMB(vram); got != int(math.Ceil(vram))*1024 {
		t.Errorf("SuggestMinVRAMMB inconsistent: %d", got)
	}
	if got := SuggestMinRAMGB(vram); got != int(math.Ceil(vram+2)) {
		t.Errorf("SuggestMinRAMGB inconsistent: %d", got)
	}
}

func pctDiff(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return 100
	}
	return math.Abs(a-b) / math.Abs(b) * 100
}

func TestMaxContextTokens(t *testing.T) {
	cases := []struct {
		name     string
		weightGB float64
		kvBpt    int
		factor   float64
		budgetGB float64
		want     int
	}{
		// weight 21 GB over a 20 GB budget — weights alone don't fit.
		{"weights-dont-fit", 21.0, 20480, KVFactorF16, 20.0, 0},
		{"zero-kv", 21.0, 0, KVFactorF16, 40.0, 0},
		{"zero-weight", 0, 20480, KVFactorF16, 40.0, 0},
		{"zero-budget", 21.0, 20480, KVFactorF16, 0, 0},
		// leftover = 10 - 1 = 9 GB; f16 @ 20480 B/tok → 439453.1 tok
		// → 439296 after 1024-rounding.
		{"round-to-1024", 1.0, 20480, KVFactorF16, 10.0, 439296},
		// leftover = 90 - 22 = 68 GB; q8_0 @ 10240 B/tok → 6640625 tok
		// → 6639616 after 1024-rounding. Caller caps at manifest (262144).
		{"large-budget", 22.0, 20480, KVFactorQ8_0, 90.0, 6639616},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MaxContextTokens(c.weightGB, c.kvBpt, c.factor, c.budgetGB); got != c.want {
				t.Errorf("MaxContextTokens(%v,%d,%v,%v) = %d, want %d",
					c.weightGB, c.kvBpt, c.factor, c.budgetGB, got, c.want)
			}
		})
	}

	// q8_0 halves the per-token KV cost, so the token budget doubles
	// (up to 1024-rounding).
	f16 := MaxContextTokens(10.0, 20480, KVFactorF16, 20.0)
	q8 := MaxContextTokens(10.0, 20480, KVFactorQ8_0, 20.0)
	if q8 < 2*f16-1024 || q8 > 2*f16+1024 {
		t.Errorf("q8_0 budget %d not ~2x f16 budget %d", q8, f16)
	}
}
