package scoring

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/proto/gguf"
)

// KVEstimate is the KV-cache cost of one token of context for a model a
// person imports (waired-ai/waired#1476), with the answer "unknown" kept
// distinct from 0. hostfit reads a 0 kv_bytes_per_token_fp16 as "fits", so
// a model whose cache cannot be derived must say so rather than report 0.
type KVEstimate struct {
	// BytesPerTokenFP16 is the per-token KV footprint at FP16, the value a
	// manifest's kv_bytes_per_token_fp16 carries. 0 when !Known.
	BytesPerTokenFP16 int
	// FullAttnLayers is how many layers hold a cache that grows with the
	// context. 0 when !Known.
	FullAttnLayers int
	// AttentionArch is the catalog attention_arch tag for the model.
	AttentionArch string
	// ContextLength is the model's own window, from max_position_embeddings
	// or <arch>.context_length. 0 when the source does not say.
	ContextLength int
	// Known is true only when the attention shape is one this package can
	// price: every layer full attention (standard or GQA), or a hybrid
	// whose full-attention layers are listed.
	Known bool
	// Reason says why the estimate is unknown, for the person reading the
	// preview. Empty when Known.
	Reason string
}

// The largest shape the estimate prices. The numbers come from the file a
// person names, so they are input: a header or config.json that states
// 2^32 layers is not a model, and pricing it would allocate one entry per
// stated layer (review of waired-ai/waired#1473, 2026-09-22) or overflow
// the products below. Published models stay far inside both — a few
// hundred layers, at most a few hundred heads of at most a few thousand
// dimensions.
const (
	maxLayers  = 4096
	maxHeads   = 1 << 16
	maxHeadDim = 1 << 16
)

// implausibleShape is the reason a stated shape is out of range, or "".
func implausibleShape(layers, heads, kvHeads, keyLen, valLen uint64) string {
	switch {
	case layers > maxLayers:
		return fmt.Sprintf("the file states %d layers, more than any model has", layers)
	case heads > maxHeads || kvHeads > maxHeads:
		return "the file states more attention heads than any model has"
	case keyLen > maxHeadDim || valLen > maxHeadDim:
		return "the file states a head dimension larger than any model has"
	}
	return ""
}

// nonNeg is n as a uint64, with a negative config.json value read as 0.
func nonNeg(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// configProbe reads the config.json keys that mark an attention shape the
// ArchConfig formula does not price. Each is a real family: DeepSeek's
// latent attention, Nemotron-H's hybrid pattern, Jamba / Falcon-H's
// per-layer block type, MiniMax's attention-type list.
type configProbe struct {
	KVLoraRank            int             `json:"kv_lora_rank"`
	HybridOverridePattern string          `json:"hybrid_override_pattern"`
	LayersBlockType       json.RawMessage `json:"layers_block_type"`
	AttnTypeList          json.RawMessage `json:"attn_type_list"`
	TextConfig            json.RawMessage `json:"text_config"`
}

// EstimateKVFromConfig prices a Hugging Face config.json, the source a vLLM
// import has. It uses the same derivation the catalog's own manifests are
// annotated with (FullAttnLayers, ResolvedHeadDim,
// KVBytesPerTokenFP16ForConfig), and answers unknown for any shape that
// derivation does not model.
func EstimateKVFromConfig(data []byte) (KVEstimate, error) {
	var c ArchConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return KVEstimate{}, fmt.Errorf("config.json: %w", err)
	}
	var p configProbe
	if err := json.Unmarshal(data, &p); err != nil {
		return KVEstimate{}, fmt.Errorf("config.json: %w", err)
	}
	if len(p.TextConfig) > 0 {
		var inner configProbe
		if err := json.Unmarshal(p.TextConfig, &inner); err == nil {
			if p.KVLoraRank == 0 {
				p.KVLoraRank = inner.KVLoraRank
			}
			if p.HybridOverridePattern == "" {
				p.HybridOverridePattern = inner.HybridOverridePattern
			}
			if len(p.LayersBlockType) == 0 {
				p.LayersBlockType = inner.LayersBlockType
			}
			if len(p.AttnTypeList) == 0 {
				p.AttnTypeList = inner.AttnTypeList
			}
		}
	}
	est := KVEstimate{ContextLength: c.MaxPositionEmbeddings, AttentionArch: c.DeriveAttentionArch()}
	switch {
	case p.KVLoraRank > 0:
		return unknownKV(est, "multi-head latent attention (kv_lora_rank)", "mla"), nil
	case p.HybridOverridePattern != "", len(p.LayersBlockType) > 0, len(p.AttnTypeList) > 0:
		return unknownKV(est, "a hybrid layer pattern this estimate does not read", ""), nil
	case c.NumHiddenLayers <= 0:
		return unknownKV(est, "config.json declares no layers", ""), nil
	case c.SlidingWindow > 0 && len(c.LayerTypes) == 0:
		return unknownKV(est, "sliding-window attention without a per-layer type list", ""), nil
	}
	headDim, _ := c.ResolvedHeadDim()
	if headDim <= 0 {
		return unknownKV(est, "config.json gives no head dimension", ""), nil
	}
	if r := implausibleShape(nonNeg(c.NumHiddenLayers),
		max(nonNeg(c.NumAttentionHeads), nonNeg(c.IndexerKVHeads)), nonNeg(c.NumKeyValueHeads),
		nonNeg(headDim), nonNeg(c.IndexerHeadDim)); r != "" {
		return unknownKV(est, r, ""), nil
	}
	if c.NumKeyValueHeads <= 0 {
		// Hugging Face's own default: no num_key_value_heads means one KV
		// head per attention head.
		c.NumKeyValueHeads = c.NumAttentionHeads
	}
	full, _ := c.FullAttnLayers()
	kv := KVBytesPerTokenFP16ForConfig(c, full, headDim)
	if kv <= 0 {
		return unknownKV(est, "config.json gives no key/value heads", ""), nil
	}
	est.BytesPerTokenFP16, est.FullAttnLayers, est.Known = kv, full, true
	return est, nil
}

// EstimateKVFromGGUF prices a GGUF from its header — as little as the first
// few kilobytes (gguf.ReadPrefix), the source an ollama import has.
//
//	kv_bytes_per_token = Σ_layers head_count_kv(l) × (key_length + value_length) × 2
//
// head_count_kv is a scalar on a standard or GQA model and a per-layer
// array on a hybrid, where the linear or recurrent layers carry 0. A hybrid
// that keeps the scalar says which layers are full attention with
// full_attention_interval instead: layer i is one when i+1 is a multiple of
// it, as llama.cpp reads it for the qwen35 family. Where
// key_length / value_length are absent, the head dimension is
// embedding_length / head_count, which is llama.cpp's own default. The last
// nextn_predict_layers blocks are multi-token prediction layers; their
// cache is priced apart in the catalog (Variant.MTPKVBytesPerTokenFP16) and
// is left out here the same way.
//
// Unknown: latent attention (attention.kv_lora_rank), a sliding window, a
// block-sparse indexer, and a recurrent (ssm.*) hybrid that gives its KV
// heads as one scalar and no full_attention_interval, which cannot say
// which layers hold a cache.
func EstimateKVFromGGUF(h gguf.Header) KVEstimate {
	arch := h.Architecture()
	est := KVEstimate{}
	if arch == "" {
		return unknownKV(est, "the GGUF names no architecture", "")
	}
	if n, ok := h.ArchUint("context_length"); ok {
		est.ContextLength = int(n)
	}
	blocks, ok := h.ArchUint("block_count")
	if !ok || blocks == 0 {
		return unknownKV(est, "the GGUF header gives no block_count", "")
	}
	if r := implausibleShape(blocks, 0, 0, 0, 0); r != "" {
		return unknownKV(est, r, "")
	}
	prefix := arch + "."
	var recurrent, indexer, sliding bool
	for k := range h.Scalars {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		switch {
		case rest == "attention.kv_lora_rank":
			return unknownKV(est, "multi-head latent attention (kv_lora_rank)", "mla")
		case strings.HasPrefix(rest, "ssm."):
			recurrent = true
		case strings.Contains(rest, "indexer"):
			indexer = true
		case strings.HasPrefix(rest, "attention.sliding_window"):
			sliding = true
		}
	}
	for k := range h.Arrays {
		if strings.HasPrefix(k, prefix+"attention.sliding_window") {
			sliding = true
		}
	}
	switch {
	case indexer:
		return unknownKV(est, "a block-sparse attention indexer", "")
	case sliding:
		return unknownKV(est, "sliding-window attention", "")
	}
	nextn, _ := h.ArchUint("nextn_predict_layers")
	if nextn >= blocks {
		return unknownKV(est, "the GGUF header's MTP layer count covers every block", "")
	}
	decoder := int(blocks - nextn)

	heads, _ := h.ArchUint("attention.head_count")
	keyLen, kOK := h.ArchUint("attention.key_length")
	valLen, vOK := h.ArchUint("attention.value_length")
	if !kOK || !vOK {
		embd, eOK := h.ArchUint("embedding_length")
		if !eOK || heads == 0 {
			return unknownKV(est, "the GGUF header gives no head dimension", "")
		}
		dim := embd / heads
		if !kOK {
			keyLen = dim
		}
		if !vOK {
			valLen = dim
		}
	}
	if keyLen == 0 || valLen == 0 {
		return unknownKV(est, "the GGUF header gives a zero head dimension", "")
	}
	if r := implausibleShape(0, heads, 0, keyLen, valLen); r != "" {
		return unknownKV(est, r, "")
	}

	var kvHeads []int64
	if arr, ok := h.Arrays[prefix+"attention.head_count_kv"]; ok {
		if len(arr) < decoder {
			return unknownKV(est, "the per-layer KV head list is shorter than the model", "")
		}
		kvHeads = arr[:decoder]
		for _, n := range kvHeads {
			if n > maxHeads {
				return unknownKV(est, implausibleShape(0, 0, uint64(n), 0, 0), "")
			}
		}
	} else {
		n, ok := h.ArchUint("attention.head_count_kv")
		if !ok {
			n = heads // no head_count_kv: one KV head per attention head
		}
		if n == 0 {
			return unknownKV(est, "the GGUF header gives no KV heads", "")
		}
		if r := implausibleShape(0, 0, n, 0, 0); r != "" {
			return unknownKV(est, r, "")
		}
		interval, _ := h.ArchUint("full_attention_interval")
		if recurrent && interval <= 1 {
			return unknownKV(est, "a recurrent hybrid that does not list which layers hold a KV cache", "")
		}
		kvHeads = make([]int64, decoder)
		for i := range kvHeads {
			if interval <= 1 || uint64(i+1)%interval == 0 {
				kvHeads[i] = int64(n)
			}
		}
	}
	perHead := int(keyLen+valLen) * bytesKVFP16
	full, maxKV, uniform := 0, int64(0), true
	for _, n := range kvHeads {
		if n <= 0 {
			uniform = false
			continue
		}
		full++
		est.BytesPerTokenFP16 += int(n) * perHead
		if maxKV != 0 && n != maxKV {
			uniform = false
		}
		maxKV = max(maxKV, n)
	}
	if full == 0 {
		return unknownKV(est, "no layer holds a KV cache", "")
	}
	est.FullAttnLayers, est.Known = full, true
	switch {
	case full < decoder:
		est.AttentionArch = archHybridMamba
	case heads > 0 && uniform && uint64(maxKV) < heads:
		est.AttentionArch = archGQA
	default:
		est.AttentionArch = archStandard
	}
	return est
}

func unknownKV(est KVEstimate, reason, arch string) KVEstimate {
	est.BytesPerTokenFP16, est.FullAttnLayers, est.Known, est.Reason = 0, 0, false, reason
	if arch != "" {
		est.AttentionArch = arch
	}
	return est
}
