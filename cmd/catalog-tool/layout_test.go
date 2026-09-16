package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/gguf"
)

// qwen35Header is the header shape of a dense 27B qwen3.8 build: 64
// decoder blocks (16 full attention, every fourth) plus one next-token
// block, gated DeltaNet state on the other 48. Tensor sizes are shrunk to
// keep the fixture small; only the keys are real.
func qwen35Header() gguf.Header {
	return gguf.Header{
		Scalars: map[string]any{
			"general.architecture":           "qwen35",
			"qwen35.block_count":             uint64(65),
			"qwen35.nextn_predict_layers":    uint64(1),
			"qwen35.full_attention_interval": uint64(4),
			"qwen35.attention.head_count_kv": uint64(4),
			"qwen35.ssm.conv_kernel":         uint64(4),
			"qwen35.ssm.group_count":         uint64(16),
			"qwen35.ssm.state_size":          uint64(128),
			"qwen35.ssm.inner_size":          uint64(6144),
			"qwen35.ssm.time_step_rank":      uint64(48),
		},
		Arrays: map[string][]int64{},
		Tensors: []gguf.Tensor{
			{Name: "token_embd.weight", Shape: []uint64{5120, 248320}, Type: 12},
			{Name: "output.weight", Shape: []uint64{5120, 1024}, Type: 14},
			{Name: "blk.0.ffn_up.weight", Shape: []uint64{256, 256}, Type: 8},
			{Name: "blk.63.attn_q.weight", Shape: []uint64{256, 256}, Type: 8},
			{Name: "blk.64.nextn.eh_proj.weight", Shape: []uint64{256, 256}, Type: 1},
		},
	}
}

// TestManifestFromLayout pins the derivations against the engine's own
// allocation on a 24 GB card (llama_memory_recurrent 149.62 MiB per
// sequence, 748.12 MiB with four drafted tokens; CPU_Mapped 682.03 MiB).
func TestManifestFromLayout(t *testing.T) {
	t.Run("dense hybrid with an MTP draft", func(t *testing.T) {
		out, err := layoutFromHeader(layoutResult{DraftNumPredict: 4}, qwen35Header(), false)
		if err != nil {
			t.Fatal(err)
		}
		g := out.Manifest.GGUF
		if g.BlockCount != 65 || g.NextNLayers != 1 || g.FullAttentionLayers != 16 {
			t.Errorf("blocks %d nextn %d full %d, want 65 / 1 / 16", g.BlockCount, g.NextNLayers, g.FullAttentionLayers)
		}
		if g.RecurrentStateBytes != 156893184 { // 149.625 MiB
			t.Errorf("recurrent state = %d bytes, want 156893184 (R 5.625 + S 144 MiB)", g.RecurrentStateBytes)
		}
		if g.DraftMaxTokens != 4 {
			t.Errorf("draft = %d, want the tag's 4", g.DraftMaxTokens)
		}
		if g.NextNBytes != 256*256*2 || g.RepeatingBytes != 2*(256*256/32*34) {
			t.Errorf("nextn %d repeating %d: the next-token block must not count as a repeating block", g.NextNBytes, g.RepeatingBytes)
		}
		if out.Manifest.HostResidentWeightGB != 0.715 { // 715,161,600 bytes of token_embd
			t.Errorf("host resident = %v GB, want 0.715", out.Manifest.HostResidentWeightGB)
		}
	})

	t.Run("the same blocks with no draft in the tag", func(t *testing.T) {
		out, _ := layoutFromHeader(layoutResult{}, qwen35Header(), false)
		if g := out.Manifest.GGUF; g.DraftMaxTokens != 0 || g.NextNLayers != 1 {
			t.Errorf("draft %d nextn %d: a tag without draft_num_predict drafts nothing, whatever the GGUF carries", g.DraftMaxTokens, g.NextNLayers)
		}
	})

	t.Run("a per-layer head_count_kv array names the full layers", func(t *testing.T) {
		h := qwen35Header()
		delete(h.Scalars, "qwen35.nextn_predict_layers")
		h.Scalars["qwen35.block_count"] = uint64(32)
		h.Scalars["qwen35.ssm.inner_size"] = uint64(4096)
		h.Scalars["qwen35.ssm.time_step_rank"] = uint64(32)
		h.Arrays["qwen35.attention.head_count_kv"] = []int64{0, 0, 0, 4, 0, 0, 0, 4, 0, 0, 0, 4, 0, 0, 0, 4, 0, 0, 0, 4, 0, 0, 0, 4, 0, 0, 0, 4, 0, 0, 0, 4}
		out, _ := layoutFromHeader(layoutResult{}, h, false)
		// qwen3.5-4b: llama_memory_recurrent 50.25 MiB
		// (docs/knowledges/20260803/1327-hybrid-attention-kv-from-gguf.md).
		if g := out.Manifest.GGUF; g.FullAttentionLayers != 8 || g.RecurrentStateBytes != 52690944 {
			t.Errorf("full %d rs %d, want 8 and 52690944 (50.25 MiB)", g.FullAttentionLayers, g.RecurrentStateBytes)
		}
	})

	t.Run("sliding window alternates and holds no recurrent state", func(t *testing.T) {
		h := gguf.Header{
			Scalars: map[string]any{
				"general.architecture":            "gptoss",
				"gptoss.block_count":              uint64(24),
				"gptoss.attention.sliding_window": uint64(128),
				"gptoss.attention.head_count_kv":  uint64(8),
			},
			Arrays: map[string][]int64{},
			Tensors: []gguf.Tensor{
				{Name: "token_embd.weight", Shape: []uint64{2880, 201088}, Type: 30},
				{Name: "blk.0.ffn_up_exps.weight", Shape: []uint64{32, 32}, Type: 39},
				{Name: "blk.0.attn_q.weight", Shape: []uint64{32, 32}, Type: 30},
			},
		}
		out, _ := layoutFromHeader(layoutResult{}, h, false)
		g := out.Manifest.GGUF
		if g.FullAttentionLayers != 12 || g.RecurrentStateBytes != 0 {
			t.Errorf("full %d rs %d, want 12 and 0", g.FullAttentionLayers, g.RecurrentStateBytes)
		}
		if g.ExpertBytes != 32*32/32*17 || g.ExpertBytes >= g.RepeatingBytes {
			t.Errorf("expert %d repeating %d: expert tensors are part of their block", g.ExpertBytes, g.RepeatingBytes)
		}
	})

	t.Run("a per-layer embedding table is host-resident", func(t *testing.T) {
		h := qwen35Header()
		h.Scalars["general.architecture"] = "qwen4exp"
		h.Tensors = append(h.Tensors, gguf.Tensor{Name: "per_layer_token_embd.weight", Shape: []uint64{1000, 1000}, Type: 0})
		for k, v := range h.Scalars {
			if len(k) > 7 && k[:7] == "qwen35." {
				h.Scalars["qwen4exp."+k[7:]] = v
			}
		}
		out, _ := layoutFromHeader(layoutResult{}, h, false)
		if out.PerLayerTokenEmbdBytes != 4_000_000 {
			t.Errorf("per-layer table = %d bytes, want 4000000", out.PerLayerTokenEmbdBytes)
		}
		if out.Manifest.HostResidentWeightGB != 0.719 { // 715,161,600 + 4,000,000
			t.Errorf("host resident = %v GB, want token_embd + per_layer_token_embd = 0.719", out.Manifest.HostResidentWeightGB)
		}
	})
}

// TestLayoutDrift is the --check rule: shipped layout fields must equal
// the header's derivation, a variant without a layout is left alone, and
// a registry that could not be read is reported rather than passed.
func TestLayoutDrift(t *testing.T) {
	derived, _ := layoutFromHeader(layoutResult{DraftNumPredict: 4}, qwen35Header(), false)
	g := derived.Manifest.GGUF
	same := catalog.Variant{GGUF: &g, HostResidentWeightGB: derived.Manifest.HostResidentWeightGB}
	if msg := layoutDrift(same, derived); msg != "" {
		t.Errorf("identical layout reported drift: %s", msg)
	}
	if msg := layoutDrift(catalog.Variant{}, derived); msg != "" {
		t.Errorf("a variant with no layout is not drift: %s", msg)
	}
	stale := g
	stale.DraftMaxTokens = 0
	if msg := layoutDrift(catalog.Variant{GGUF: &stale, HostResidentWeightGB: same.HostResidentWeightGB}, derived); msg == "" {
		t.Error("a changed draft count must be drift")
	}
	if msg := layoutDrift(catalog.Variant{GGUF: &g, HostResidentWeightGB: 0.5}, derived); msg == "" {
		t.Error("a changed host-resident weight must be drift")
	}
	if msg := layoutDrift(same, layoutResult{Error: "status 502"}); msg == "" {
		t.Error("an unreadable registry must be reported, not passed")
	}

	// A tag the product writes a draft onto (waired-ai/waired#1433).
	noDraft, _ := layoutFromHeader(layoutResult{}, qwen35Header(), false)
	ng := noDraft.Manifest.GGUF
	stamped := catalog.Variant{GGUF: &ng, HostResidentWeightGB: noDraft.Manifest.HostResidentWeightGB, MTPDraftTokens: 2}
	if msg := layoutDrift(stamped, noDraft); msg != "" {
		t.Errorf("a draft the product writes onto a tag without one is not drift: %s", msg)
	}
	if msg := layoutDrift(stamped, derived); !strings.Contains(msg, "now sets draft_num_predict 4") {
		t.Errorf("a tag that started publishing its own draft: %q", msg)
	}
	gone := noDraft
	gone.Manifest.GGUF.NextNLayers = 0
	if msg := layoutDrift(stamped, gone); !strings.Contains(msg, "no longer has nextn layers") {
		t.Errorf("a tag that lost its nextn layers: %q", msg)
	}
}
