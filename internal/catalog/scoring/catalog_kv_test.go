package scoring

import (
	"fmt"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
)

// This file closes the loop that waired-agent#448 slipped through.
//
// archCases (scoring_test.go) pins the FORMULA against the scoring
// report, and Manifest.Validate pins the field's RANGE (≥ 0). Nothing
// checked that a shipped manifest actually carries the number its own
// architecture produces, so `qwen3.5-4b` sat at 12288 — the value of its
// 2b sibling — while its architecture derives 32768.
//
// That mattered because waired-ai/waired#1031 turned
// kv_bytes_per_token_fp16 into a routing input: hostfit.ServingWindowKVMB
// and OllamaWindowResidentMB price the serving window a node proposes to
// stand behind, so an under-stated KV lets a host declare a window it
// cannot hold — the exact failure the window contract exists to remove.
//
// Scope: HYBRID-ATTENTION models. Their KV is not a function of parameter
// count — only the full-attention layers hold a cache that grows with the
// sequence, and the linear/Mamba layers carry a constant state — which is
// precisely the assumption that produces a transcription like #448's. The
// dense and sliding-window families are deliberately NOT covered here;
// several of them disagree with this package's own formula and settling
// them needs a per-family convention decision, tracked separately.

// hybridArchConfigs maps a bundled model_id to its published attention
// architecture.
//
// PROVENANCE, and it differs from archCases: these values are read from
// the GGUF metadata of the artifact we actually ship, not from the
// scoring report. For a hybrid model llama.cpp writes
// `<arch>.attention.head_count_kv` as a PER-LAYER array whose zero
// entries are the linear layers, so the full-attention count is the
// number of non-zero entries and needs no interval heuristic:
//
//	qwen35.block_count            = 32
//	qwen35.attention.head_count_kv = [0,0,0,4, 0,0,0,4, ...]  → 8 non-zero
//	qwen35.attention.key_length    = 256
//
// Recorded as FullAttentionInterval (every measured family is a clean
// block_count/full_attn = 4) so the derivation runs through the same
// ArchConfig.FullAttnLayers path the catalog-tool generator uses.
//
// See docs/knowledges/20260803/1327-hybrid-attention-kv-from-gguf.md for
// how to re-measure, including the trap that sinks a naive reading: the
// `-mtp-` builds emit head_count_kv as a SCALAR, which means the array is
// absent, NOT that every layer is full attention.
var hybridArchConfigs = map[string]ArchConfig{
	// L=24, full=6, n_kv=2, head_dim=256 → 2×6×2×256×2 = 12288
	"qwen3.5-0.8b": {NumHiddenLayers: 24, HiddenSize: 1024, NumAttentionHeads: 8, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4},
	"qwen3.5-2b":   {NumHiddenLayers: 24, HiddenSize: 2048, NumAttentionHeads: 8, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4},
	// L=32, full=8, n_kv=4, head_dim=256 → 2×8×4×256×2 = 32768.
	// The 4b and the 9b genuinely share a KV footprint: same depth, same
	// head count, different hidden size. #448 annotated the 4b with the
	// 2b's number instead.
	"qwen3.5-4b": {NumHiddenLayers: 32, HiddenSize: 2560, NumAttentionHeads: 16, NumKeyValueHeads: 4, HeadDim: 256, FullAttentionInterval: 4},
	"qwen3.5-9b": {NumHiddenLayers: 32, HiddenSize: 4096, NumAttentionHeads: 16, NumKeyValueHeads: 4, HeadDim: 256, FullAttentionInterval: 4},
	// L=64, full=16, n_kv=4 → 65536
	"qwen3.5-27b": {NumHiddenLayers: 64, HiddenSize: 5120, NumAttentionHeads: 24, NumKeyValueHeads: 4, HeadDim: 256, FullAttentionInterval: 4},
	"qwen3.6-27b": {NumHiddenLayers: 64, HiddenSize: 5120, NumAttentionHeads: 24, NumKeyValueHeads: 4, HeadDim: 256, FullAttentionInterval: 4},
	// Same geometry, and read differently. Every ollama build of qwen3.8
	// carries the MTP head — `qwen3.8:27b-q4_K_M` and
	// `qwen3.8:27b-mtp-q4_K_M` are one blob whose tags differ only in a
	// params layer — so head_count_kv is a SCALAR on both and the trap in
	// docs/knowledges/20260803/1327-hybrid-attention-kv-from-gguf.md
	// applies with no non-mtp sibling left to fall back to. Two other
	// readings give the layer count instead: `qwen35.full_attention_interval
	// = 4` over `block_count = 65` (64 decoder layers plus the one
	// `nextn_predict_layers` head), and Qwen/Qwen3.8-27B's config.json,
	// whose text_config.layer_types holds exactly 16 "full_attention"
	// entries out of 64. attention.key_length is 256, as across the family.
	"qwen3.8-27b": {NumHiddenLayers: 64, HiddenSize: 5120, NumAttentionHeads: 24, NumKeyValueHeads: 4, HeadDim: 256, FullAttentionInterval: 4},
	// L=40, full=10, n_kv=2 → 20480
	"qwen3.5-35b-a3b": {NumHiddenLayers: 40, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 256, NumExpertsPerTok: 8},
	"qwen3.6-35b-a3b": {NumHiddenLayers: 40, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 256, NumExpertsPerTok: 8},
	// L=48, full=12, n_kv=2 → 24576. Header read from the registry blob
	// (ranged GET) rather than a local pull — the artifact is 70 GB.
	"qwen3.5-122b-a10b": {NumHiddenLayers: 48, HiddenSize: 3072, NumAttentionHeads: 32, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 256, NumExpertsPerTok: 8},
	// L=48, full=12, n_kv=2 → 24576 for attention, plus 3072 for the QSA
	// indexer's key cache = 27648. Same attention geometry as the 122b
	// above, a different model behind it: 512 experts, 10 active, and a
	// block-sparse indexer the 122b does not have.
	//
	// This is the only entry with a THIRD cache. Serving it at 262,144 on
	// a 128 GB UMA host the engine allocates:
	//
	//	llama_kv_cache:      6144.00 MiB (262144 cells, 12 layers)  K 3072.00  V 3072.00
	//	llama_kv_cache:      2304.00 MiB (262144 cells, 12 layers)  K  768.00  V 1536.00
	//	llama_memory_recurrent: 112.57 MiB (1 cells, 48 layers)
	//
	// The second cache is the indexer, and both halves are exactly
	// derivable — the asymmetry between them is the whole point:
	//
	//	K  12 × 1 × indexer_head_dim 128 × 2 B = 3072 B/tok   ( 768 MiB)
	//	V  12 × 1 × head_dim         256 × 2 B = 6144 B/tok   (1536 MiB)
	//
	// The K half is the model's, and this row derives it. The V half is
	// llama.cpp's: the model has no index_v_proj and the graph issues only
	// cpy_k/get_k on that cache, but llama_memory_hybrid_idx reuses the
	// general llama_kv_cache, whose has_v = !is_mla is true here, and its
	// constructor overrides only the key width — so a V half is allocated
	// at the MODEL's head_dim and then never written or read. That is
	// ggml-org/llama.cpp#28330, open at b10760, the llama.cpp that ollama
	// 0.33.3 vendors.
	//
	// So the engine really does hold 33792 B/tok today while this row says
	// 27648. That is deliberate: 6144 of the difference is an upstream
	// over-allocation rather than a property of the model, and annotating
	// around it would bake the bug in and be wrong again when #28330
	// lands.
	// docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md
	// carries the measurement and what to re-read at the next pin bump.
	"qwen3.8-flash-next": {NumHiddenLayers: 48, HiddenSize: 2560, NumAttentionHeads: 24, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 512, NumExpertsPerTok: 10, IndexerKVHeads: 1, IndexerHeadDim: 128},
	// qwen3-coder-next-80b-a3b-instruct sat here until #522 retired the
	// 2025 generation. Its row is gone because this map is checked against
	// the SHIPPED manifests and a name the catalog no longer carries fails
	// below — deliberately, so an annotation cannot outlive the model it
	// annotates. The architecture itself is not lost: archCases still pins
	// it from the scoring report, where it is evidence about a derivation
	// rather than a claim about a shipped file.
}

// TestBundledHybridManifestsMatchTheDerivation is the check that was
// missing. It is deliberately NOT a golden table of expected numbers: it
// re-derives from the architecture, so a reviewer sees WHY a value is
// what it is, and a future annotation is written by stating layer counts
// rather than by copying a sibling.
func TestBundledHybridManifestsMatchTheDerivation(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range manifests {
		cfg, ok := hybridArchConfigs[m.ModelID]
		if !ok {
			continue
		}
		seen[m.ModelID] = true

		full, inferred := cfg.FullAttnLayers()
		if inferred {
			t.Errorf("%s: full-attention layer count had to be guessed; record the architecture instead", m.ModelID)
		}
		headDim, derived := cfg.ResolvedHeadDim()
		if derived {
			t.Errorf("%s: head_dim had to be derived from hidden_size/num_heads; read attention.key_length instead", m.ModelID)
		}
		want := KVBytesPerTokenFP16ForConfig(cfg, full, headDim)
		if want <= 0 {
			t.Fatalf("%s: the recorded architecture derives no KV footprint", m.ModelID)
		}
		// Spell the arithmetic back at whoever the assertion fails for, so
		// the number can be checked against the model's config.json without
		// reading this file's helpers.
		derivation := fmt.Sprintf("2 × %d full-attn layers × %d KV heads × %d head_dim × 2 bytes",
			full, cfg.NumKeyValueHeads, headDim)
		if cfg.IndexerKVHeads > 0 && cfg.IndexerHeadDim > 0 {
			derivation += fmt.Sprintf(", plus the indexer key cache %d × %d × %d × 2 bytes",
				full, cfg.IndexerKVHeads, cfg.IndexerHeadDim)
		}

		for _, v := range m.Variants {
			// Quantizing the WEIGHTS does not change the KV geometry, so
			// every variant of a model carries the same annotation. A
			// variant that disagrees with its siblings is the same class
			// of error as disagreeing with the architecture.
			if v.KVBytesPerTokenFP16 != want {
				t.Errorf("%s/%s: kv_bytes_per_token_fp16 = %d, want %d (%s)",
					m.ModelID, v.VariantID, v.KVBytesPerTokenFP16, want, derivation)
			}
			if v.AttentionArch != catalog.AttentionHybridMamba {
				t.Errorf("%s/%s: attention_arch = %q, want %q — this table asserts a hybrid geometry",
					m.ModelID, v.VariantID, v.AttentionArch, catalog.AttentionHybridMamba)
			}
		}
	}
	for id := range hybridArchConfigs {
		if !seen[id] {
			t.Errorf("hybridArchConfigs names %q, which the bundled catalog no longer ships — drop the row", id)
		}
	}
}

// TestEveryAnnotatedHybridManifestIsDerived is the half that makes the
// table load-bearing rather than decorative: a new hybrid model added
// with a hand-written KV number and no architecture row fails here, which
// is the only mechanism that would have caught #448.
//
// Variants carrying NO annotation are skipped — 0 means "unknown /
// unmeasured" per Variant.KVBytesPerTokenFP16's doc, and demanding a
// derivation for a model nobody has sized yet would be a different rule.
func TestEveryAnnotatedHybridManifestIsDerived(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, m := range manifests {
		if _, ok := hybridArchConfigs[m.ModelID]; ok {
			continue
		}
		for _, v := range m.Variants {
			if v.AttentionArch != catalog.AttentionHybridMamba || v.KVBytesPerTokenFP16 <= 0 {
				continue
			}
			t.Errorf("%s/%s annotates kv_bytes_per_token_fp16 = %d but has no row in "+
				"hybridArchConfigs: a hybrid model's KV is not a function of parameter "+
				"count, so the number cannot be reviewed without its layer counts. Read "+
				"<arch>.attention.head_count_kv from the shipped GGUF and add a row.",
				m.ModelID, v.VariantID, v.KVBytesPerTokenFP16)
		}
	}
}
