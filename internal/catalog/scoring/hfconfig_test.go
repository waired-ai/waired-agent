package scoring

import (
	"encoding/json"
	"testing"
)

// A vision-language config.json puts the decoder under "text_config" and
// leaves the top level holding only the multimodal wiring. Decoding one
// into the flat ArchConfig read every arch field as zero, and nothing said
// so: FullAttnLayers returned 0 without setting its "inferred" flag,
// ResolvedHeadDim returned 0, KVBytesPerTokenFP16 collapsed to 0 and
// DeriveAttentionArch answered "standard".
//
// That was not hypothetical. Qwen ships qwen3.5, qwen3.6 and qwen3.8 in
// this shape, so `catalog-tool compute --repo Qwen/Qwen3.6-27B` — the
// command the catalog refresh runbook tells an author to derive the
// numbers with — reproduced neither the attention_arch nor the
// kv_bytes_per_token_fp16 the shipped qwen3.6-27b manifest carries. The
// two warnings it printed named a missing head_dim, not a config shape it
// had not been taught to read.
//
// kv_bytes_per_token_fp16 is a routing input (waired-ai/waired#1031:
// hostfit.ServingWindowKVMB and OllamaWindowResidentMB price the serving
// window a node stands behind), so a zero is not a cosmetic gap.
//
// Ratifying source: waired-ai/waired-agent#823.

// qwen38VLMConfig is the shape of Qwen/Qwen3.8-27B's config.json, trimmed
// to the fields ArchConfig reads. layer_types is elided — the interval is
// what a 64-layer/interval-4 model resolves through — but the nesting and
// the empty top level are verbatim.
const qwen38VLMConfig = `{
  "architectures": ["Qwen3_5ForConditionalGeneration"],
  "model_type": "qwen3_5",
  "text_config": {
    "model_type": "qwen3_5_text",
    "num_hidden_layers": 64,
    "hidden_size": 5120,
    "num_attention_heads": 24,
    "num_key_value_heads": 4,
    "head_dim": 256,
    "vocab_size": 248320,
    "max_position_embeddings": 262144,
    "full_attention_interval": 4
  },
  "vision_config": {"depth": 27, "hidden_size": 1152}
}`

func TestArchConfig_ReadsTheDecoderOutOfAVisionLanguageConfig(t *testing.T) {
	var cfg ArchConfig
	if err := json.Unmarshal([]byte(qwen38VLMConfig), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.NumHiddenLayers != 64 {
		t.Errorf("NumHiddenLayers = %d, want 64", cfg.NumHiddenLayers)
	}
	if cfg.NumKeyValueHeads != 4 {
		t.Errorf("NumKeyValueHeads = %d, want 4", cfg.NumKeyValueHeads)
	}
	if cfg.MaxPositionEmbeddings != 262144 {
		t.Errorf("MaxPositionEmbeddings = %d, want 262144", cfg.MaxPositionEmbeddings)
	}

	full, inferred := cfg.FullAttnLayers()
	if full != 16 || inferred {
		t.Errorf("FullAttnLayers() = %d, inferred %v; want 16, false", full, inferred)
	}
	headDim, derived := cfg.ResolvedHeadDim()
	if headDim != 256 || derived {
		t.Errorf("ResolvedHeadDim() = %d, derived %v; want 256, false", headDim, derived)
	}
	if got := KVBytesPerTokenFP16(full, cfg.NumKeyValueHeads, headDim); got != 65536 {
		t.Errorf("KVBytesPerTokenFP16 = %d, want 65536", got)
	}
	if got := cfg.DeriveAttentionArch(); got != archHybridMamba {
		t.Errorf("DeriveAttentionArch() = %q, want %q", got, archHybridMamba)
	}
}

// The text-only shape must keep decoding exactly as before — every config
// the catalog was built from is flat, so a regression here would rewrite
// numbers nobody asked to change.
func TestArchConfig_FlatConfigIsUnchanged(t *testing.T) {
	const flat = `{
	  "num_hidden_layers": 48,
	  "hidden_size": 2048,
	  "num_attention_heads": 16,
	  "num_key_value_heads": 2,
	  "head_dim": 256,
	  "full_attention_interval": 4,
	  "num_experts": 512,
	  "num_experts_per_tok": 10
	}`
	var cfg ArchConfig
	if err := json.Unmarshal([]byte(flat), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	full, _ := cfg.FullAttnLayers()
	headDim, _ := cfg.ResolvedHeadDim()
	if got := KVBytesPerTokenFP16(full, cfg.NumKeyValueHeads, headDim); got != 24576 {
		t.Errorf("KVBytesPerTokenFP16 = %d, want 24576", got)
	}
	if !cfg.IsMoE() {
		t.Error("IsMoE() = false, want true")
	}
}

// A config that declares the decoder at the top level AND carries a
// text_config keeps the top level. Nothing published ships that way today;
// the case exists so the precedence is stated rather than discovered.
func TestArchConfig_TopLevelDecoderWinsOverTextConfig(t *testing.T) {
	const both = `{
	  "num_hidden_layers": 32,
	  "num_key_value_heads": 4,
	  "head_dim": 128,
	  "text_config": {"num_hidden_layers": 64, "num_key_value_heads": 8, "head_dim": 256}
	}`
	var cfg ArchConfig
	if err := json.Unmarshal([]byte(both), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.NumHiddenLayers != 32 || cfg.NumKeyValueHeads != 4 || cfg.HeadDim != 128 {
		t.Errorf("got layers=%d kv=%d head_dim=%d; want the top level (32, 4, 128)",
			cfg.NumHiddenLayers, cfg.NumKeyValueHeads, cfg.HeadDim)
	}
}

// Malformed JSON must still be an error rather than a zero value — the
// custom decoder is the only thing standing between a fetch and the
// numbers a manifest is written from.
func TestArchConfig_MalformedIsAnError(t *testing.T) {
	var cfg ArchConfig
	if err := json.Unmarshal([]byte(`{"num_hidden_layers": "sixty-four"}`), &cfg); err == nil {
		t.Fatal("unmarshal of a mistyped field returned no error")
	}
	if err := json.Unmarshal([]byte(`{"text_config": [1,2,3]}`), &cfg); err == nil {
		t.Fatal("unmarshal of a non-object text_config returned no error")
	}
}
