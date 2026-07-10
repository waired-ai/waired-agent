package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what it
// wrote. Output is small (a single JSON blob), so the pipe buffer suffices.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// qwen3Coder30bConfig is the published architecture of Qwen3-Coder-30B-A3B
// (scoring report §3.1), used as a compute fixture.
const qwen3Coder30bConfig = `{
	"num_hidden_layers": 48,
	"hidden_size": 2048,
	"num_attention_heads": 32,
	"num_key_value_heads": 4,
	"head_dim": 128,
	"num_experts": 128,
	"num_experts_per_tok": 8,
	"max_position_embeddings": 262144
}`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestComputeSubcommand(t *testing.T) {
	cfg := writeTemp(t, "config.json", qwen3Coder30bConfig)
	out, err := captureStdout(t, func() error {
		return runCompute([]string{
			"--config", cfg,
			"--quant", "Q4_K_M",
			"--context", "32768",
			"--total-params", "30530000000",
			"--active-params", "3300000000",
		})
	})
	if err != nil {
		t.Fatalf("runCompute: %v", err)
	}
	var res computeResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse compute output: %v\n%s", err, out)
	}
	if res.KVBytesPerTokenFP16 != 98304 {
		t.Errorf("kv_bytes_per_token_fp16 = %d, want 98304", res.KVBytesPerTokenFP16)
	}
	if res.AttentionArch != "gqa" {
		t.Errorf("attention_arch = %q, want gqa", res.AttentionArch)
	}
	if res.QuantizationTier != 4 {
		t.Errorf("quantization_tier = %d, want 4", res.QuantizationTier)
	}
	if res.DecodeFLOPsPerTok != 6_600_000_000 {
		t.Errorf("decode_flops_per_tok = %d, want 6.6e9", res.DecodeFLOPsPerTok)
	}
	// Bare weight ≈ 18.4 GB (matches the committed manifest).
	if res.EstimatedWeightGB < 17.5 || res.EstimatedWeightGB > 19.3 {
		t.Errorf("estimated_weight_gb = %.1f, want ~18.4", res.EstimatedWeightGB)
	}
	if len(res.VRAMByContext) != len(standardContexts) {
		t.Errorf("vram curve has %d points, want %d", len(res.VRAMByContext), len(standardContexts))
	}
}

func TestComputeSubcommand_MXFP4Warns(t *testing.T) {
	cfg := writeTemp(t, "config.json", qwen3Coder30bConfig)
	out, err := captureStdout(t, func() error {
		return runCompute([]string{
			"--config", cfg, "--quant", "MXFP4",
			"--total-params", "20910000000", "--active-params", "3610000000",
			"--mxfp4-native",
		})
	})
	if err != nil {
		t.Fatalf("runCompute: %v", err)
	}
	var res computeResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse: %v", err)
	}
	joined := strings.ToLower(strings.Join(res.Warnings, " "))
	if !strings.Contains(joined, "lower bound") {
		t.Errorf("expected a lower-bound warning for MXFP4, got %v", res.Warnings)
	}
}

func TestComputeSubcommand_Errors(t *testing.T) {
	cfg := writeTemp(t, "config.json", qwen3Coder30bConfig)
	cases := [][]string{
		{"--config", cfg, "--total-params", "1"},                                       // missing --quant
		{"--config", cfg, "--quant", "Q4_K_M"},                                         // missing --total-params
		{"--quant", "Q4_K_M", "--total-params", "1"},                                   // neither config nor repo
		{"--config", cfg, "--repo", "x/y", "--quant", "Q4_K_M", "--total-params", "1"}, // both
		{"--config", cfg, "--quant", "NONSENSE", "--total-params", "1"},                // bad quant
	}
	for i, args := range cases {
		if _, err := captureStdout(t, func() error { return runCompute(args) }); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestValidateSubcommand_All(t *testing.T) {
	if _, err := captureStdout(t, func() error { return runValidate([]string{"--all"}) }); err != nil {
		t.Fatalf("validate --all on bundled catalog: %v", err)
	}
}

func TestValidateSubcommand_File(t *testing.T) {
	// A valid, new manifest whose tier (99) is not used by the bundled set.
	good := catalog.Manifest{
		ModelID: "test-new-model", ContextLength: 32768,
		Runtime: catalog.RuntimePolicy{Preferred: "ollama"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag",
			RuntimeSupport: []string{"ollama"}, QualityTier: 99,
			ParamCount: 7_000_000_000, QuantizationTier: 4,
			Source: catalog.VariantSource{Type: "ollama", Tag: "x:y"},
		}},
	}
	data, _ := json.MarshalIndent(good, "", "  ")
	path := writeTemp(t, "good.json", string(data))
	if _, err := captureStdout(t, func() error { return runValidate([]string{"--file", path}) }); err != nil {
		t.Errorf("validate valid file: %v", err)
	}

	// A manifest reusing a tier the bundled catalog already owns must be
	// rejected by the against-bundled uniqueness check.
	bundled, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("bundled: %v", err)
	}
	existingTier := bundled[0].Variants[0].QualityTier
	clash := good
	clash.ModelID = "clashing-model"
	clash.Variants = []catalog.Variant{clash.Variants[0]}
	clash.Variants[0].QualityTier = existingTier
	cdata, _ := json.MarshalIndent(clash, "", "  ")
	cpath := writeTemp(t, "clash.json", string(cdata))
	if _, err := captureStdout(t, func() error { return runValidate([]string{"--file", cpath}) }); err == nil {
		t.Errorf("expected tier-collision error for tier %d", existingTier)
	}
}
