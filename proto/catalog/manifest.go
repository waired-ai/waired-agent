// Package catalog is the shared model-catalog data layer: the manifest
// schema, the bundled catalog embedded into every binary, and the
// quality-tier resolver over advertised model names.
//
// It lives in the proto module (not the agent's internal/catalog) so the
// private control plane can consume the SAME bundled data the agent
// ships — the single source of truth for model→quality_tier resolution
// (Public Share matchmaking §6.1-6, usage ingest). The agent's
// internal/catalog re-exports these types and keeps everything
// runtime-behavioural (local install state, discovery, tier re-ranking
// tooling) to itself.
//
// Like the rest of the proto module this package is stdlib-only
// (dependency allowlist, CI-enforced) and additive-only across
// published proto tags.
package catalog

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

//go:embed bundled/*.json
var bundledFS embed.FS

// Manifest mirrors the JSON-on-disk schema for one model. Keep
// json tags in sync with both the embedded bundled/*.json files and
// the future CP /model-manifests endpoint payload.
type Manifest struct {
	ModelID       string        `json:"model_id"`
	DisplayName   string        `json:"display_name,omitempty"`
	ModelAliases  []string      `json:"model_aliases,omitempty"`
	License       string        `json:"license,omitempty"`
	ContextLength int           `json:"context_length"`
	Capabilities  []string      `json:"capabilities,omitempty"`
	Runtime       RuntimePolicy `json:"runtime"`
	Variants      []Variant     `json:"variants"`
	Security      Security      `json:"security"`
}

// RuntimePolicy expresses the manifest author's runtime preference.
// Phase A only honours `Preferred`; `Fallback` is reserved for later.
type RuntimePolicy struct {
	Preferred string   `json:"preferred"`
	Fallback  []string `json:"fallback,omitempty"`
}

// Variant is one (format × runtime × hardware footprint) combination
// of the model. A manifest must have at least one variant.
//
// QualityTier is the maintainer-assigned ranking the auto-picker uses
// to break ties: higher tier wins when multiple variants fit the host.
// Range [1, 100]; values must be unique across the bundled catalog.
//
// MinVRAMMB / MinRAMMB are the host-fit thresholds the auto-picker
// compares against hardware.Profile.GPUs[0].VRAMTotalMB / RAMTotalGB
// (RAM stays in GB because /proc/meminfo precision is plenty there).
// MinVRAMMB only applies to GPU runtimes (vllm); MinRAMGB only applies
// to CPU runtimes (ollama).
//
// ParamCount is the total parameter count (e.g. 8e9 for Qwen3-8B). For
// MoE models this is the TOTAL parameter count, not the active count,
// because "model quality / capability" — which the Phase 7 router
// scoring uses — scales with the full parameter pool, not just the
// active subset that fits in VRAM.
//
// QuantizationTier is the weight-precision ladder used in the Phase 7
// router score (`score = ParamCount × QuantizationTier`). Higher = more
// precision retained. Range [1, 8]:
//
//   - 4: AWQ-int4 / Q4_K_M / Q4_0
//   - 5: Q5_K_M
//   - 6: Q6_K
//   - 8: Q8_0 / FP16 / BF16 (treated as saturation; coding-agent
//     quality differences below ~8 bit start to matter much less than
//     param count, so 8 is the cap).
type Variant struct {
	VariantID         string        `json:"variant_id"`
	Format            string        `json:"format"` // safetensors | gguf | ollama-tag
	Quantization      string        `json:"quantization,omitempty"`
	DType             string        `json:"dtype,omitempty"`
	RuntimeSupport    []string      `json:"runtime_support"` // subset of {ollama, vllm}
	EstimatedWeightGB float64       `json:"estimated_weight_gb,omitempty"`
	MinRAMGB          int           `json:"min_ram_gb,omitempty"`
	MinVRAMMB         int           `json:"min_vram_mb,omitempty"`
	QualityTier       int           `json:"quality_tier"`
	ParamCount        int64         `json:"param_count"`
	QuantizationTier  int           `json:"quantization_tier"`
	Source            VariantSource `json:"source"`

	// ActiveParams is the MoE active parameter count (= decode FLOPs/tok
	// / 2). For dense models leave 0 — callers treat 0 as "= ParamCount".
	// Validate() enforces 0 ≤ ActiveParams ≤ ParamCount.
	ActiveParams int64 `json:"active_params,omitempty"`

	// KVBytesPerTokenFP16 is the per-token KV-cache footprint in bytes
	// assuming FP16 KV, after hybrid-mamba / sliding-window correction
	// (i.e. the value the Auto Selector should use directly when
	// budgeting context length). 0 means "unknown / unmeasured".
	KVBytesPerTokenFP16 int `json:"kv_bytes_per_token_fp16,omitempty"`

	// AttentionArch tags the attention topology so the Auto Selector can
	// reason about KV-cache scaling vs context length. Empty == unknown
	// (treated as standard for budgeting).
	AttentionArch string `json:"attention_arch,omitempty"`

	// VendorSupport is the GPU-vendor × runtime compatibility matrix.
	// nil == permissive (every supported runtime / vendor combination is
	// assumed "stable"). Empty per-cell strings have the same meaning.
	VendorSupport *VendorSupportMatrix `json:"vendor_support,omitempty"`

	// MXFP4Native is set for models distributed natively in MXFP4 (e.g.
	// openai/gpt-oss-*). When true the on-disk size matches MXFP4 even
	// without an extra quantization step, and the runtime must support
	// MXFP4 ingest (vLLM ≥ 0.6 stable, Ollama 0.4+ via llama.cpp).
	MXFP4Native bool `json:"mxfp4_native,omitempty"`

	// MinEngineVersion is the minimum SERVING-engine version (dotted,
	// e.g. "0.30.0") required to load this variant — e.g. qwen3.6 mtp
	// tags need Ollama >= 0.30 or the registry refuses the pull
	// server-side with no useful indication why. Compared against the
	// live engine version (HTTP /api/version; binary --version
	// fallback). Empty = no floor. An UNKNOWN live version excludes
	// the variant: a silent server-side failure is exactly the
	// incident this field prevents, so the gate fails closed.
	MinEngineVersion string `json:"min_engine_version,omitempty"`
}

// VendorSupportMatrix records, for one variant, which GPU vendor / runtime
// combinations the manifest author considers production-ready. Missing
// cells (zero value VendorRuntimeSupport / empty status strings) default
// to "stable" so manifests can be terse for the common case.
type VendorSupportMatrix struct {
	Nvidia VendorRuntimeSupport `json:"nvidia"`
	AMD    VendorRuntimeSupport `json:"amd"`
	Mac    VendorRuntimeSupport `json:"mac"`
}

// VendorRuntimeSupport carries one status string per runtime adapter.
// Values must be one of the VendorSupport* constants below; an empty
// string is treated as VendorSupportStable.
type VendorRuntimeSupport struct {
	VLLM     string `json:"vllm,omitempty"`
	Ollama   string `json:"ollama,omitempty"`
	LlamaCPP string `json:"llama_cpp,omitempty"`
	MLX      string `json:"mlx,omitempty"`
}

// VariantSource is the location from which the binary weights are
// fetched. Type=="ollama" uses Tag; Type=="huggingface" uses RepoID
// (and optionally Revision = commit SHA for reproducible pulls).
type VariantSource struct {
	Type     string `json:"type"`
	Tag      string `json:"tag,omitempty"`
	RepoID   string `json:"repo_id,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// SourceHF is the Type value for Hugging Face Hub repositories.
const SourceHuggingFace = "huggingface"

// SourceOllama is the Type value for Ollama tag-named blobs.
const SourceOllama = "ollama"

// Security captures the manifest-author-declared safety posture.
type Security struct {
	TrustRemoteCodeRequired bool `json:"trust_remote_code_required"`
	AllowPersistentKVCache  bool `json:"allow_persistent_kv_cache"`
}

// Known runtime / format identifiers. Kept as constants so spec
// changes propagate via compile errors instead of silent typos.
const (
	RuntimeOllama = "ollama"
	RuntimeVLLM   = "vllm"

	FormatSafetensors = "safetensors"
	FormatGGUF        = "gguf"
	FormatOllamaTag   = "ollama-tag"

	// Attention topology tags used by Variant.AttentionArch. The
	// Auto Selector uses these to pick a KV-cache scaling formula when
	// the manifest does not record KVBytesPerTokenFP16 directly.
	AttentionStandard      = "standard"       // every layer = full attention, no GQA
	AttentionGQA           = "gqa"            // grouped-query attention (Llama 2+/Qwen3)
	AttentionMLA           = "mla"            // multi-head latent attention (DeepSeek-V2/V3)
	AttentionHybridMamba   = "hybrid_mamba"   // mixed full-attention + Mamba/linear layers
	AttentionSlidingWindow = "sliding_window" // alternating full + window-capped layers

	// VendorSupport status enum used in VendorRuntimeSupport cells.
	VendorSupportStable       = "stable"       // production-ready
	VendorSupportExperimental = "experimental" // runs, edge cases / perf caveats
	VendorSupportCommunity    = "community"    // unofficial build only
	VendorSupportUnsupported  = "unsupported"  // does not work; picker must exclude
)

// BundledManifests decodes every JSON file under proto/catalog/bundled
// into a Manifest. They are sorted alphabetically by file name so the
// order is deterministic across builds.
func BundledManifests() ([]Manifest, error) {
	entries, err := bundledFS.ReadDir("bundled")
	if err != nil {
		return nil, fmt.Errorf("catalog: read bundled dir: %w", err)
	}
	out := make([]Manifest, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := bundledFS.ReadFile(path.Join("bundled", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("catalog: read %s: %w", e.Name(), err)
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("catalog: parse %s: %w", e.Name(), err)
		}
		out = append(out, m)
	}
	return out, nil
}

// LookupByAlias finds the first manifest whose ModelID equals name or
// whose ModelAliases contains name. Empty `name` always misses.
func LookupByAlias(name string, manifests []Manifest) (Manifest, bool) {
	if name == "" {
		return Manifest{}, false
	}
	for _, m := range manifests {
		if m.ModelID == name {
			return m, true
		}
		for _, a := range m.ModelAliases {
			if a == name {
				return m, true
			}
		}
	}
	return Manifest{}, false
}

// Validate enforces spec waired_inference_spec.md §5.2 plus Step 2
// invariants:
//   - at least one variant
//   - each variant lists at least one runtime in runtime_support
//   - format=safetensors → runtime_support must be {vllm}, source.type=huggingface, repo_id required
//   - format=gguf       → runtime_support must be {ollama}
//   - format=ollama-tag → runtime_support must be {ollama}, source.type=ollama, tag required
//   - quality_tier ∈ [1, 100]
//   - param_count > 0 (Phase 7 router score input)
//   - quantization_tier ∈ [1, 8] (Phase 7 router score input)
//   - AWQ-quantized variants must source from the official Qwen/* org
func (m *Manifest) Validate() error {
	if m.ModelID == "" {
		return errors.New("manifest: model_id required")
	}
	if len(m.Variants) == 0 {
		return errors.New("manifest: at least one variant required")
	}
	for i, v := range m.Variants {
		if len(v.RuntimeSupport) == 0 {
			return fmt.Errorf("manifest %s variant %d: runtime_support must list at least one engine", m.ModelID, i)
		}
		if v.QualityTier < 1 || v.QualityTier > 100 {
			return fmt.Errorf("manifest %s variant %s: quality_tier must be in [1, 100], got %d", m.ModelID, v.VariantID, v.QualityTier)
		}
		if v.ParamCount <= 0 {
			return fmt.Errorf("manifest %s variant %s: param_count must be > 0, got %d", m.ModelID, v.VariantID, v.ParamCount)
		}
		if v.QuantizationTier < 1 || v.QuantizationTier > 8 {
			return fmt.Errorf("manifest %s variant %s: quantization_tier must be in [1, 8], got %d", m.ModelID, v.VariantID, v.QuantizationTier)
		}
		switch v.Format {
		case FormatSafetensors:
			if !runtimesEqual(v.RuntimeSupport, []string{RuntimeVLLM}) {
				return fmt.Errorf("manifest %s variant %s: format=safetensors requires runtime_support=[vllm]", m.ModelID, v.VariantID)
			}
			if v.Source.Type != SourceHuggingFace {
				return fmt.Errorf("manifest %s variant %s: format=safetensors requires source.type=huggingface, got %q", m.ModelID, v.VariantID, v.Source.Type)
			}
			if v.Source.RepoID == "" {
				return fmt.Errorf("manifest %s variant %s: format=safetensors requires source.repo_id", m.ModelID, v.VariantID)
			}
		case FormatGGUF, FormatOllamaTag:
			if !runtimesEqual(v.RuntimeSupport, []string{RuntimeOllama}) {
				return fmt.Errorf("manifest %s variant %s: format=%s requires runtime_support=[ollama]", m.ModelID, v.VariantID, v.Format)
			}
			if v.Format == FormatOllamaTag {
				if v.Source.Type != SourceOllama {
					return fmt.Errorf("manifest %s variant %s: format=ollama-tag requires source.type=ollama, got %q", m.ModelID, v.VariantID, v.Source.Type)
				}
				if v.Source.Tag == "" {
					return fmt.Errorf("manifest %s variant %s: format=ollama-tag requires source.tag", m.ModelID, v.VariantID)
				}
			}
		case "":
			return fmt.Errorf("manifest %s variant %s: format required", m.ModelID, v.VariantID)
		default:
			return fmt.Errorf("manifest %s variant %s: unknown format %q", m.ModelID, v.VariantID, v.Format)
		}
		if isAWQ(v.Quantization) {
			if v.Source.Type != SourceHuggingFace {
				return fmt.Errorf("manifest %s variant %s: AWQ quantization requires source.type=huggingface", m.ModelID, v.VariantID)
			}
			if !strings.HasPrefix(v.Source.RepoID, "Qwen/") {
				return fmt.Errorf("manifest %s variant %s: AWQ source.repo_id %q must come from the official Qwen/ org", m.ModelID, v.VariantID, v.Source.RepoID)
			}
		}
		if v.ActiveParams < 0 {
			return fmt.Errorf("manifest %s variant %s: active_params must be ≥ 0, got %d", m.ModelID, v.VariantID, v.ActiveParams)
		}
		if v.ActiveParams > v.ParamCount {
			return fmt.Errorf("manifest %s variant %s: active_params %d must not exceed param_count %d", m.ModelID, v.VariantID, v.ActiveParams, v.ParamCount)
		}
		if v.KVBytesPerTokenFP16 < 0 {
			return fmt.Errorf("manifest %s variant %s: kv_bytes_per_token_fp16 must be ≥ 0, got %d", m.ModelID, v.VariantID, v.KVBytesPerTokenFP16)
		}
		if !isAttentionArchValid(v.AttentionArch) {
			return fmt.Errorf("manifest %s variant %s: unknown attention_arch %q", m.ModelID, v.VariantID, v.AttentionArch)
		}
		if v.MinEngineVersion != "" && !validDottedVersion(v.MinEngineVersion) {
			return fmt.Errorf("manifest %s variant %s: min_engine_version %q is not a dotted version", m.ModelID, v.VariantID, v.MinEngineVersion)
		}
		if err := validateVendorSupport(m.ModelID, v); err != nil {
			return err
		}
	}
	return nil
}

func isAttentionArchValid(a string) bool {
	switch a {
	case "",
		AttentionStandard,
		AttentionGQA,
		AttentionMLA,
		AttentionHybridMamba,
		AttentionSlidingWindow:
		return true
	}
	return false
}

func isVendorSupportStatusValid(s string) bool {
	switch s {
	case "",
		VendorSupportStable,
		VendorSupportExperimental,
		VendorSupportCommunity,
		VendorSupportUnsupported:
		return true
	}
	return false
}

func validateVendorSupport(modelID string, v Variant) error {
	if v.VendorSupport == nil {
		return nil
	}
	cells := []struct {
		vendor string
		cell   VendorRuntimeSupport
	}{
		{"nvidia", v.VendorSupport.Nvidia},
		{"amd", v.VendorSupport.AMD},
		{"mac", v.VendorSupport.Mac},
	}
	for _, c := range cells {
		for _, status := range []struct {
			runtime, value string
		}{
			{"vllm", c.cell.VLLM},
			{"ollama", c.cell.Ollama},
			{"llama_cpp", c.cell.LlamaCPP},
			{"mlx", c.cell.MLX},
		} {
			if !isVendorSupportStatusValid(status.value) {
				return fmt.Errorf("manifest %s variant %s: vendor_support.%s.%s = %q is not a valid status",
					modelID, v.VariantID, c.vendor, status.runtime, status.value)
			}
		}
	}
	return nil
}

// isAWQ reports whether quantization names an AWQ variant. Allows
// "AWQ" / "AWQ-int4" / "awq-int4" / etc.
func isAWQ(q string) bool {
	return strings.Contains(strings.ToUpper(q), "AWQ")
}

func runtimesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	// Order-independent equality check on small fixed sets.
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// validDottedVersion reports whether s parses as a dotted-numeric
// version, with the same prefix/suffix tolerance as the agent's
// internal/version package (leading "v", trailing "-rc1"/".post1"
// style suffixes). Mirrored here because the proto module cannot
// import agent internals and must stay dependency-free.
func validDottedVersion(s string) bool {
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, " "); idx >= 0 {
		s = s[idx+1:] // drop "ollama version is " style prefix
	}
	s = strings.TrimPrefix(s, "v")
	// Cut at the first non [0-9.] char so "-rc1"/".post1" don't break us.
	for i, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			s = s[:i]
			break
		}
	}
	n := 0
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			continue
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
		n++
	}
	return n > 0
}
