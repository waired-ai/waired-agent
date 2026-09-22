package scoring

import (
	"encoding/json"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/gguf"
)

// The estimators price models a person imports (waired-ai/waired#1476). A
// 0 would read as "fits" to hostfit, so every shape they cannot price must
// come back !Known with a reason. These tests are product contracts for
// that rule, and records of today's derivation for the numbers.

func TestEstimateKVFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   int
		known  bool
		arch   string
	}{
		{"GQA, head_dim derived", `{"num_hidden_layers":32,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":8,"max_position_embeddings":131072}`,
			2 * 32 * 8 * 128 * 2, true, "gqa"},
		{"no num_key_value_heads is multi-head", `{"num_hidden_layers":24,"hidden_size":2048,"num_attention_heads":16}`,
			2 * 24 * 16 * 128 * 2, true, "standard"},
		{"hybrid by interval", `{"num_hidden_layers":32,"hidden_size":2560,"num_attention_heads":16,"num_key_value_heads":4,"head_dim":256,"full_attention_interval":4}`,
			2 * 8 * 4 * 256 * 2, true, "hybrid_mamba"},
		{"sliding window with a type list", `{"num_hidden_layers":4,"hidden_size":2880,"num_attention_heads":64,"num_key_value_heads":8,"head_dim":64,"sliding_window":128,"layer_types":["sliding_attention","full_attention","sliding_attention","full_attention"]}`,
			2 * 2 * 8 * 64 * 2, true, "sliding_window"},
		{"nested text_config", `{"architectures":["X"],"text_config":{"num_hidden_layers":32,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":8}}`,
			2 * 32 * 8 * 128 * 2, true, "gqa"},
		{"latent attention", `{"num_hidden_layers":61,"hidden_size":7168,"num_attention_heads":128,"kv_lora_rank":512}`, 0, false, "mla"},
		{"nemotron-h pattern", `{"num_hidden_layers":52,"hidden_size":4480,"num_attention_heads":40,"num_key_value_heads":8,"hybrid_override_pattern":"M-M-M*-"}`, 0, false, ""},
		{"jamba block types", `{"num_hidden_layers":8,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":8,"layers_block_type":["mamba","attention"]}`, 0, false, ""},
		{"sliding window without types", `{"num_hidden_layers":26,"hidden_size":2304,"num_attention_heads":8,"num_key_value_heads":4,"head_dim":256,"sliding_window":4096}`, 0, false, ""},
		{"no layers", `{"hidden_size":4096}`, 0, false, ""},
		// Out-of-range shapes from a crafted config.json are unknown, not an
		// overflowed product (review of waired-ai/waired#1473, 2026-09-22).
		{"2^40 layers", `{"num_hidden_layers":1099511627776,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":8}`, 0, false, ""},
		{"2^40 KV heads", `{"num_hidden_layers":32,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":1099511627776}`, 0, false, ""},
		{"2^40 head dimension", `{"num_hidden_layers":32,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":1099511627776}`, 0, false, ""},
	} {
		got, err := EstimateKVFromConfig([]byte(tc.config))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.Known != tc.known || got.BytesPerTokenFP16 != tc.want {
			t.Errorf("%s: known=%v kv=%d, want known=%v kv=%d (reason %q)", tc.name, got.Known, got.BytesPerTokenFP16, tc.known, tc.want, got.Reason)
		}
		if !got.Known && got.Reason == "" {
			t.Errorf("%s: unknown with no reason", tc.name)
		}
		if tc.arch != "" && got.AttentionArch != tc.arch {
			t.Errorf("%s: attention_arch %q, want %q", tc.name, got.AttentionArch, tc.arch)
		}
	}
	if _, err := EstimateKVFromConfig([]byte(`{`)); err == nil {
		t.Error("malformed JSON accepted")
	}
}

// TestEstimateKVFromConfigMatchesTheCatalog feeds the architectures the
// catalog's hybrid annotations were derived from back through the importer's
// path: a custom import of the same config must price it as the bundled
// manifest does.
func TestEstimateKVFromConfigMatchesTheCatalog(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, m := range manifests {
		cfg, ok := hybridArchConfigs[m.ModelID]
		if !ok {
			continue
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		est, err := EstimateKVFromConfig(data)
		if err != nil {
			t.Fatalf("%s: %v", m.ModelID, err)
		}
		for _, v := range m.Variants {
			if v.KVBytesPerTokenFP16 == 0 {
				continue
			}
			checked++
			if !est.Known || est.BytesPerTokenFP16 != v.KVBytesPerTokenFP16 {
				t.Errorf("%s/%s: estimate known=%v %d, manifest %d (%s)", m.ModelID, v.VariantID, est.Known, est.BytesPerTokenFP16, v.KVBytesPerTokenFP16, est.Reason)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no bundled variant was checked")
	}
}

func header(arch string, scalars map[string]any, arrays map[string][]int64) gguf.Header {
	h := gguf.Header{Scalars: map[string]any{"general.architecture": arch}, Arrays: map[string][]int64{}}
	for k, v := range scalars {
		h.Scalars[arch+"."+k] = v
	}
	for k, v := range arrays {
		h.Arrays[arch+"."+k] = v
	}
	return h
}

func TestEstimateKVFromGGUF(t *testing.T) {
	u := func(n uint64) any { return n }
	for _, tc := range []struct {
		name  string
		h     gguf.Header
		want  int
		known bool
		arch  string
	}{
		{"llama GQA, head dim from embedding", header("llama", map[string]any{
			"block_count": u(32), "context_length": u(131072), "embedding_length": u(4096),
			"attention.head_count": u(32), "attention.head_count_kv": u(8),
		}, nil), 32 * 8 * (128 + 128) * 2, true, "gqa"},
		{"qwen3 with key/value lengths", header("qwen3", map[string]any{
			"block_count": u(28), "embedding_length": u(1024), "attention.head_count": u(16),
			"attention.head_count_kv": u(8), "attention.key_length": u(128), "attention.value_length": u(128),
		}, nil), 28 * 8 * 256 * 2, true, "gqa"},
		{"hybrid per-layer array, nextn left out", header("qwen35", map[string]any{
			"block_count": u(9), "nextn_predict_layers": u(1), "attention.head_count": u(16),
			"attention.key_length": u(256), "attention.value_length": u(256), "ssm.state_size": u(128),
		}, map[string][]int64{"attention.head_count_kv": {0, 0, 0, 4, 0, 0, 0, 4, 4}}), 2 * 4 * 512 * 2, true, "hybrid_mamba"},
		{"hybrid by interval, scalar heads, nextn left out", header("qwen35", map[string]any{
			"block_count": u(65), "nextn_predict_layers": u(1), "attention.head_count": u(24), "attention.head_count_kv": u(4),
			"attention.key_length": u(256), "attention.value_length": u(256), "full_attention_interval": u(4), "ssm.state_size": u(128),
		}, nil), 16 * 4 * 512 * 2, true, "hybrid_mamba"},
		{"latent attention", header("deepseek2", map[string]any{
			"block_count": u(61), "attention.head_count": u(128), "attention.kv_lora_rank": u(512),
		}, nil), 0, false, "mla"},
		{"sliding window", header("gemma3", map[string]any{
			"block_count": u(26), "attention.head_count": u(8), "attention.head_count_kv": u(4),
			"attention.key_length": u(256), "attention.value_length": u(256), "attention.sliding_window": u(512),
		}, nil), 0, false, ""},
		{"recurrent hybrid with a scalar head count", header("nemotron_h", map[string]any{
			"block_count": u(52), "attention.head_count": u(40), "attention.head_count_kv": u(8),
			"attention.key_length": u(128), "attention.value_length": u(128), "ssm.state_size": u(128),
		}, nil), 0, false, ""},
		{"indexer", header("qwen4exp", map[string]any{
			"block_count": u(48), "attention.head_count": u(24), "attention.key_length": u(256),
			"attention.value_length": u(256), "attention.indexer.head_count": u(1),
		}, map[string][]int64{"attention.head_count_kv": {2, 2}}), 0, false, ""},
		{"no architecture", gguf.Header{Scalars: map[string]any{}}, 0, false, ""},
		{"no block count", header("llama", map[string]any{"attention.head_count": u(32)}, nil), 0, false, ""},
		{"no head dimension", header("llama", map[string]any{"block_count": u(4), "attention.head_count_kv": u(4)}, nil), 0, false, ""},
		{"short per-layer list", header("qwen35", map[string]any{
			"block_count": u(8), "attention.head_count": u(16), "attention.key_length": u(256), "attention.value_length": u(256),
		}, map[string][]int64{"attention.head_count_kv": {0, 4}}), 0, false, ""},

		// A crafted header (review of waired-ai/waired#1473, 2026-09-22):
		// block_count used to size an allocation, so 2^32 layers took the
		// control plane's memory during a preview. Each out-of-range number
		// is unknown with a reason, before any arithmetic on it.
		{"2^32 layers", header("llama", map[string]any{
			"block_count": u(0xFFFFFFFF), "embedding_length": u(4096),
			"attention.head_count": u(32), "attention.head_count_kv": u(8),
		}, nil), 0, false, ""},
		{"2^63 layers", header("llama", map[string]any{
			"block_count": u(1 << 63), "embedding_length": u(4096),
			"attention.head_count": u(32), "attention.head_count_kv": u(8),
		}, nil), 0, false, ""},
		{"2^40 KV heads", header("llama", map[string]any{
			"block_count": u(32), "embedding_length": u(4096),
			"attention.head_count": u(32), "attention.head_count_kv": u(1 << 40),
		}, nil), 0, false, ""},
		{"2^40 KV heads in the per-layer list", header("qwen35", map[string]any{
			"block_count": u(2), "attention.head_count": u(16), "attention.key_length": u(256), "attention.value_length": u(256),
		}, map[string][]int64{"attention.head_count_kv": {0, 1 << 40}}), 0, false, ""},
		{"2^40 head dimension", header("qwen3", map[string]any{
			"block_count": u(28), "attention.head_count": u(16), "attention.head_count_kv": u(8),
			"attention.key_length": u(1 << 40), "attention.value_length": u(128),
		}, nil), 0, false, ""},
	} {
		got := EstimateKVFromGGUF(tc.h)
		if got.Known != tc.known || got.BytesPerTokenFP16 != tc.want {
			t.Errorf("%s: known=%v kv=%d, want known=%v kv=%d (reason %q)", tc.name, got.Known, got.BytesPerTokenFP16, tc.known, tc.want, got.Reason)
		}
		if !got.Known && got.Reason == "" {
			t.Errorf("%s: unknown with no reason", tc.name)
		}
		if tc.arch != "" && got.AttentionArch != tc.arch {
			t.Errorf("%s: attention_arch %q, want %q", tc.name, got.AttentionArch, tc.arch)
		}
	}
	if got := EstimateKVFromGGUF(header("llama", map[string]any{"block_count": u(4), "context_length": u(8192), "attention.head_count": u(4), "embedding_length": u(512)}, nil)); got.ContextLength != 8192 {
		t.Errorf("context_length = %d, want 8192", got.ContextLength)
	}
}
