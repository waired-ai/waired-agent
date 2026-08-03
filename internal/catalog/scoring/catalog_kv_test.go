package scoring

import (
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
	// L=40, full=10, n_kv=2 → 20480
	"qwen3.5-35b-a3b": {NumHiddenLayers: 40, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 256, NumExpertsPerTok: 8},
	"qwen3.6-35b-a3b": {NumHiddenLayers: 40, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 256, NumExpertsPerTok: 8},
	// L=48, full=12, n_kv=2 → 24576. Header read from the registry blob
	// (ranged GET) rather than a local pull — the artifact is 70 GB.
	"qwen3.5-122b-a10b": {NumHiddenLayers: 48, HiddenSize: 3072, NumAttentionHeads: 32, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 256, NumExpertsPerTok: 8},
	// Already pinned in archCases from the scoring report; the shipped
	// manifest agrees, so it is listed here too rather than left as the
	// one hybrid family with no manifest check.
	"qwen3-coder-next-80b-a3b-instruct": {NumHiddenLayers: 48, HiddenSize: 2048, NumAttentionHeads: 16, NumKeyValueHeads: 2, HeadDim: 256, FullAttentionInterval: 4, NumExperts: 512, NumExpertsPerTok: 10},
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
		want := KVBytesPerTokenFP16(full, cfg.NumKeyValueHeads, headDim)
		if want <= 0 {
			t.Fatalf("%s: the recorded architecture derives no KV footprint", m.ModelID)
		}

		for _, v := range m.Variants {
			// Quantizing the WEIGHTS does not change the KV geometry, so
			// every variant of a model carries the same annotation. A
			// variant that disagrees with its siblings is the same class
			// of error as disagreeing with the architecture.
			if v.KVBytesPerTokenFP16 != want {
				t.Errorf("%s/%s: kv_bytes_per_token_fp16 = %d, want %d "+
					"(2 × %d full-attn layers × %d KV heads × %d head_dim × 2 bytes)",
					m.ModelID, v.VariantID, v.KVBytesPerTokenFP16, want,
					full, cfg.NumKeyValueHeads, headDim)
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
