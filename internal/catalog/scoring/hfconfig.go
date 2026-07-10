package scoring

import "slices"

// Attention-arch tags. These intentionally mirror the literal values of the
// catalog.Attention* constants (manifest.go) so a DeriveAttentionArch result
// can be written straight into a manifest's attention_arch field. They are
// re-declared here (rather than imported) to keep this pure-formula file free
// of any catalog dependency.
const (
	archStandard      = "standard"
	archGQA           = "gqa"
	archHybridMamba   = "hybrid_mamba"
	archSlidingWindow = "sliding_window"
)

// ArchConfig is the subset of a HuggingFace config.json needed to compute a
// variant's footprint fields. It carries the UNION of the fields different
// model families use to express the same concept (e.g. num_experts vs
// num_local_experts, full_attention_interval vs layer_types); the helpers
// below branch on whichever are populated.
type ArchConfig struct {
	NumHiddenLayers       int `json:"num_hidden_layers"`
	HiddenSize            int `json:"hidden_size"`
	NumAttentionHeads     int `json:"num_attention_heads"`
	NumKeyValueHeads      int `json:"num_key_value_heads"`
	HeadDim               int `json:"head_dim"`
	VocabSize             int `json:"vocab_size"`
	MaxPositionEmbeddings int `json:"max_position_embeddings"`

	// MoE: Qwen uses num_experts; gpt-oss / Mixtral use num_local_experts.
	NumExperts       int `json:"num_experts"`
	NumLocalExperts  int `json:"num_local_experts"`
	NumExpertsPerTok int `json:"num_experts_per_tok"`

	// Hybrid attention (Qwen3-Next / Qwen3.6): every Nth layer is a full
	// attention layer, the rest are linear/Mamba layers with constant state.
	FullAttentionInterval int `json:"full_attention_interval"`

	// Sliding-window attention (gpt-oss): layer_types alternates
	// "full_attention" / "sliding_attention"; sliding_window is the window.
	LayerTypes    []string `json:"layer_types"`
	SlidingWindow int      `json:"sliding_window"`
}

// ResolvedHeadDim returns the per-head dimension. Modern configs declare
// head_dim explicitly (it is frequently NOT hidden_size/num_attention_heads —
// e.g. Qwen3-Coder-30B has hidden_size 2048, 32 heads, but head_dim 128).
// The bool reports whether the value had to be derived (caller should warn).
func (c ArchConfig) ResolvedHeadDim() (int, bool) {
	if c.HeadDim > 0 {
		return c.HeadDim, false
	}
	if c.NumAttentionHeads > 0 && c.HiddenSize > 0 {
		return c.HiddenSize / c.NumAttentionHeads, true
	}
	return 0, true
}

// FullAttnLayers returns the number of full-attention layers — the only layers
// whose KV cache grows with context. For hybrid models (Mamba/linear or
// sliding-window) this is fewer than NumHiddenLayers. The bool reports whether
// the value was inferred from a heuristic (caller should warn).
func (c ArchConfig) FullAttnLayers() (int, bool) {
	// gpt-oss style: explicit per-layer type list.
	if len(c.LayerTypes) > 0 {
		cnt := 0
		for _, t := range c.LayerTypes {
			if t == "full_attention" {
				cnt++
			}
		}
		if cnt > 0 {
			return cnt, false
		}
	}
	// Qwen3-Next / Qwen3.6 style: every Nth layer is full attention.
	if c.FullAttentionInterval > 1 && c.NumHiddenLayers > 0 {
		return c.NumHiddenLayers / c.FullAttentionInterval, false
	}
	// Default: every layer is a full-attention layer.
	return c.NumHiddenLayers, false
}

// IsMoE reports whether the config describes a mixture-of-experts model.
func (c ArchConfig) IsMoE() bool {
	return c.NumExperts > 0 || c.NumLocalExperts > 0
}

// IsSlidingWindow reports whether the config uses alternating sliding-window
// attention (gpt-oss family).
func (c ArchConfig) IsSlidingWindow() bool {
	if c.SlidingWindow > 0 {
		return true
	}
	return slices.Contains(c.LayerTypes, "sliding_attention")
}

// DeriveAttentionArch maps the config onto the catalog attention_arch enum.
// Empty input fields collapse to "standard". The result is a best-effort
// classification; compute() surfaces it as a value to review, not a fact.
func (c ArchConfig) DeriveAttentionArch() string {
	switch {
	case c.IsSlidingWindow():
		return archSlidingWindow
	case c.FullAttentionInterval > 1:
		// Hybrid full + linear/Mamba layers (Qwen3-Next, Qwen3.6).
		return archHybridMamba
	case c.NumKeyValueHeads > 0 && c.NumAttentionHeads > 0 && c.NumKeyValueHeads < c.NumAttentionHeads:
		return archGQA
	default:
		return archStandard
	}
}
