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
	"slices"
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

	// InternalOnly, when non-empty, keeps this model out of everything a
	// person sees or is given: auto-selection, the install picker
	// (including its below-the-floor fallback for under-spec hosts), the
	// tray catalog, the control plane's device catalog, `models ls
	// --detail`, and the generated docs tables. The value is the REASON
	// it is withheld.
	//
	// It does NOT make the model unresolvable. An internal entry still
	// resolves by model id or alias, still pulls, and still serves —
	// that is the whole point. The routing sentinel needs a real
	// catalog model cheap enough to pull on every PR across three
	// operating systems, and the daemon resolves the pinned
	// `--inference-bundled-model-id` against this catalog, so a test
	// fixture cannot simply live outside it.
	//
	// A reason string rather than a bool, for the same reason the
	// agent-grade store's "unmeasurable" map carries reasons: an
	// exemption nobody has to justify is an exemption nobody revisits.
	//
	// Withholding is orthogonal to quality. quality_tier and the install
	// quality floor answer "is this good enough to recommend"; this
	// answers "is this ours to offer at all".
	InternalOnly string `json:"internal_only,omitempty"`

	// ManualOnly, when non-empty, keeps this model out of every
	// AUTOMATIC choice — the install pick, the auto-picker's ranking,
	// the recommended-family badge, the upgrade step — while leaving it
	// in the catalog a person browses and can select for themselves.
	// The value is the REASON, same as InternalOnly. Empty means the
	// model participates normally.
	//
	// InternalOnly answers "is this ours to offer at all"; this answers
	// "would we ever choose it for someone". A model can be popular and
	// worth carrying without being what we put in front of somebody who
	// has not asked for it, and before this field there was no way to
	// say so: withholding removed the entry from the catalog entirely,
	// so a person could not pick what they could no longer see.
	//
	// The two compose and InternalOnly wins — a model we do not offer at
	// all is not one we could have chosen. BundledManifests therefore
	// still filters on InternalOnly alone; a manual-only entry stays in
	// its result, and the pickers are what skip it.
	//
	// Like InternalOnly this does NOT affect resolution: a manual-only
	// entry resolves by model id and by alias, pulls, and serves. That
	// is what makes "the person picked it themselves" work, and it is
	// what keeps an explicit pin somebody already wrote — in agent.json,
	// in preferred-model.json, or as a control-plane desired model —
	// working after the model stops being recommended.
	//
	// Ratifying source: docs/decisions/20260805/1427-quality-tier-is-a-
	// curated-ladder.md and issue #520.
	ManualOnly string `json:"manual_only,omitempty"`

	// DefaultVariant names, per engine (RuntimeOllama / RuntimeVLLM), the
	// variant a device serves when the user has not chosen one. The owner
	// hand-picks it per model (decision 6 of
	// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md);
	// the values arrive with waired-ai/waired-agent#1349. Empty = no
	// default recorded, and callers keep choosing as they do today.
	DefaultVariant map[string]string `json:"default_variant,omitempty"`
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

	// Renderer and Parser name the engine-side prompt renderer and
	// response parser to stamp onto a pulled ollama tag whose publisher
	// left them unset. Empty (the usual case) means "use the tag as
	// published"; the values are ollama's own registry names, not an
	// enum this project owns.
	//
	// They exist because the two paths that produce an ollama tag do not
	// agree. Converting safetensors stamps a renderer automatically, so
	// every MLX tag for a family carries one; packaging a GGUF does not,
	// and no community publisher types it by hand. A tag with neither a
	// renderer nor a template layer falls through to the GGUF's embedded
	// Jinja (ollama server/prompt.go), and Qwen's Jinja raises
	// "System message must be at the beginning." on three of the six
	// shapes a coding agent sends — measured on
	// frob/qwen3.8-flash-next, where stamping renderer "qwen3.8" turned
	// all three from 500 into 200 with no other change
	// (waired-agent#1192).
	//
	// NOT part of VariantSHA, though they plainly influence what the
	// runtime serves: that payload is frozen, and widening it would make
	// every persisted measurement on every host stop matching. The guard
	// against a silently un-stamped variant is therefore to record the
	// renderer in the measurement and compare it on check, not to hash
	// it here.
	Renderer string `json:"renderer,omitempty"`
	Parser   string `json:"parser,omitempty"`

	// KVCacheTypes lists the KV-cache types this variant may be served
	// with (KVCache* constants). Empty means what the engines serve today:
	// f16 and q8_0 on ollama, fp16 and fp8 on vLLM. The owner decision
	// that lets a user choose the type, and the values per model, are
	// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md
	// and waired-ai/waired-agent#1349.
	KVCacheTypes []string `json:"kv_cache_types,omitempty"`

	// HostResidentWeightGB is the part of EstimatedWeightGB that
	// llama.cpp keeps in system RAM whatever the accelerator budget: the
	// input-layer tensors (token_embd, and per_layer_token_embd on
	// architectures that have one). llama.cpp places the input layer on
	// the CPU unconditionally because a table lookup gains nothing from
	// offload, and ollama's /api/ps does not count it
	// (docs/knowledges/20260914/0120-input-layer-tensors-stay-on-the-cpu.md).
	// Decimal GB, derived from the GGUF tensor table by
	// `catalog-tool layout`. 0 = unknown, priced as all-device.
	HostResidentWeightGB float64 `json:"host_resident_weight_gb,omitempty"`

	// GGUF is the layout of an ollama build that the VRAM sizing reads
	// beyond the weight total (waired-ai/waired-agent#1337). nil = not
	// derived; the sizing then falls back to its older overhead term.
	GGUF *GGUFLayout `json:"gguf,omitempty"`

	// MTPLayers is how many multi-token prediction (MTP) layers a vLLM
	// build's checkpoint carries: the Hugging Face config's
	// mtp_num_hidden_layers (under text_config on Qwen's
	// vision-language configs), derived by `catalog-tool compute`. vLLM
	// can run them as the draft of speculative decoding (method "mtp").
	// 0 = none, or not derived. An ollama build records the same fact as
	// gguf.nextn_layers. waired-ai/waired#1432.
	MTPLayers int `json:"mtp_layers,omitempty"`

	// MTPKVBytesPerTokenFP16 is the fp16 KV cache one token costs in those
	// MTP layers' own attention (layers × 2 × KV heads × head dim × 2
	// bytes). When the draft runs, vLLM allocates it from the same pool as
	// the main model's KV, so the max-model-len sizing adds it then.
	MTPKVBytesPerTokenFP16 int `json:"mtp_kv_bytes_per_token_fp16,omitempty"`

	// MTPDraftTokens is how many tokens the product has the MTP draft
	// propose per step on this build, set per build from a measurement.
	// On vLLM it is num_speculative_tokens. On ollama it is the
	// draft_num_predict the product stamps onto a tag whose publisher left
	// it unset; a tag that sets its own keeps it (gguf.draft_max_tokens).
	// 0 = the product runs no draft it chose. Read the draft that actually
	// runs through MTPDraftTokens(v). waired-ai/waired#1432,
	// waired-ai/waired#1433.
	MTPDraftTokens int `json:"mtp_draft_tokens,omitempty"`

	// MaxParallel is the most requests the engine serves at once for this
	// build, whatever the tuning or an admin override asks. 0 means the
	// catalog sets no limit. Read it for a served build through
	// ServedMaxParallel.
	//
	// It exists for ollama's scheduler, which starts some model families
	// with one slot whatever OLLAMA_NUM_PARALLEL says (ollama v0.34.0
	// server/sched.go Scheduler.load: "model architecture does not
	// currently support parallel requests"). The family it compares is
	// the tag's config-blob model_family. The owner decided on 2026-09-17
	// to follow that limit, and to hold capacity to it from the catalog
	// rather than request a second slot the engine turns down
	// (waired-ai/waired-agent#1423). Only a build ollama serves carries
	// one: vLLM batches the same models.
	MaxParallel int `json:"max_parallel,omitempty"`
}

// MTPDraftTokens is the number of tokens the MTP draft proposes per step
// when v is served: on an ollama build the tag's own draft_num_predict if
// it declares one, else the value the product stamps (Variant.MTPDraftTokens);
// on a vLLM build the product's choice when the checkpoint carries MTP
// layers. 0 means no draft runs. The VRAM sizing reads this rather than
// either field, so selection and serving price the same draft.
func MTPDraftTokens(v Variant) int {
	if v.GGUF != nil {
		if v.GGUF.DraftMaxTokens > 0 {
			return v.GGUF.DraftMaxTokens
		}
		if v.GGUF.NextNLayers > 0 && v.MTPDraftTokens > 0 {
			return v.MTPDraftTokens
		}
		return 0
	}
	if v.MTPLayers > 0 && v.MTPDraftTokens > 0 {
		return v.MTPDraftTokens
	}
	return 0
}

// GGUFLayout is what a GGUF header and its ollama tag say about how a
// build occupies memory once llama.cpp loads it. Every figure is an exact
// read or sum over the header (`catalog-tool layout --tag`), so a reviewer
// can re-derive it without the weights.
type GGUFLayout struct {
	// BlockCount is <arch>.block_count, including any next-token (MTP)
	// prediction blocks. llama.cpp's "offloaded N/M layers" counts
	// BlockCount + 1: the output layer is the extra one.
	BlockCount int `json:"block_count"`

	// FullAttentionLayers is how many of the repeating blocks keep a KV
	// cache that grows with the context window, spaced evenly with the
	// last block of each group being the full-attention one (qwen3.5's
	// full_attention_interval, gpt-oss's alternating layer_types). On a
	// hybrid model the other blocks hold a fixed recurrent state instead.
	FullAttentionLayers int `json:"full_attention_layers,omitempty"`

	// RecurrentStateBytes is the recurrent state one sequence holds
	// (llama_memory_recurrent, f32 R + S), independent of the window.
	RecurrentStateBytes int64 `json:"recurrent_state_bytes,omitempty"`

	// DraftMaxTokens is the tag's draft_num_predict: how many tokens the
	// next-token prediction head drafts per step. ollama enables the MTP
	// draft only when the tag (or the request) sets it, even for a GGUF
	// that carries nextn blocks (server/routes.go modelOptions, ollama
	// v0.33.3). 0 = no draft context. When set, llama.cpp keeps
	// 1 + DraftMaxTokens copies of the recurrent state and a second,
	// f16 context for the draft head.
	DraftMaxTokens int `json:"draft_max_tokens,omitempty"`

	// TensorBytes is the whole tensor table of the GGUF weights file.
	TensorBytes int64 `json:"tensor_bytes"`

	// ProjectorBytes is the multimodal projector blob ollama loads beside
	// the model and offloads with it. 0 when the tag carries none (a build
	// with its vision tensors inline reports them in InlineProjectorBytes).
	ProjectorBytes int64 `json:"projector_bytes,omitempty"`

	// InlineProjectorBytes is the part of TensorBytes in vision / audio
	// tensors (v.*, mm.*, a.*) that ollama loads as the projector when the
	// build carries them inside the weights file.
	InlineProjectorBytes int64 `json:"inline_projector_bytes,omitempty"`

	// NextNLayers is how many of the BlockCount blocks are next-token
	// prediction blocks (<arch>.nextn_predict_layers). They sit after the
	// repeating blocks.
	NextNLayers int `json:"nextn_layers,omitempty"`

	// NextNBytes is the part of TensorBytes in next-token prediction
	// blocks. llama.cpp loads them only when the MTP draft runs, so a tag
	// with DraftMaxTokens 0 does not occupy them.
	NextNBytes int64 `json:"nextn_bytes,omitempty"`

	// RepeatingBytes is the sum of the blk.* tensors outside the nextn
	// blocks: what moves to system RAM, a block at a time, when a dense
	// model does not fit.
	RepeatingBytes int64 `json:"repeating_bytes,omitempty"`

	// TiedOutputBytes is token_embd's size on a model with no output tensor:
	// llama.cpp builds the output layer from a second copy of the embedding,
	// which goes to the device while the input copy stays in system RAM. 0
	// when the model has its own output tensor.
	TiedOutputBytes int64 `json:"tied_output_bytes,omitempty"`

	// ExpertBytes is the part of RepeatingBytes in *_exps tensors. llama.cpp's
	// fit moves these to system RAM before any whole block. 0 on a dense model.
	ExpertBytes int64 `json:"expert_bytes,omitempty"`
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

	// Digest pins an ollama Tag to the registry manifest it had when the
	// catalog entry was measured ("sha256:<64 hex>"). A community tag can
	// be re-pushed under the same name: frob/qwen3.8-flash-next's grew
	// from 55 GB to 79 GB with no change to the tag
	// (waired-ai/waired-agent#1305), and every size, fit and ETA figure the
	// catalog carried went stale with it. A puller compares it against the
	// registry before pulling and refuses a different manifest.
	//
	// Deliberately outside VariantSHA: that payload is frozen, and folding
	// the pin into it would invalidate every recorded measurement keyed by
	// the variant (request shapes, boot-bench caches) without the weights
	// having moved. Empty = not pinned.
	Digest string `json:"digest,omitempty"`
}

// validDigest reports whether d is a registry manifest digest in the
// only form the pin accepts: "sha256:" and 64 lowercase hex digits.
func validDigest(d string) bool {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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

	// KV-cache types, as the engines spell them: ollama's
	// OLLAMA_KV_CACHE_TYPE values and vLLM's --kv-cache-dtype values.
	KVCacheF16  = "f16"
	KVCacheQ8_0 = "q8_0"
	KVCacheQ4_0 = "q4_0"
	KVCacheFP16 = "fp16"
	KVCacheFP8  = "fp8"

	// VendorSupport status enum used in VendorRuntimeSupport cells.
	VendorSupportStable       = "stable"       // production-ready
	VendorSupportExperimental = "experimental" // runs, edge cases / perf caveats
	VendorSupportCommunity    = "community"    // unofficial build only
	VendorSupportUnsupported  = "unsupported"  // does not work; picker must exclude
)

// BundledManifests decodes the models this build OFFERS: every JSON
// file under proto/catalog/bundled except those marked InternalOnly.
// They are sorted alphabetically by file name so the order is
// deterministic across builds.
//
// "Offered" is the default on purpose. Every surface that shows a model
// to a person or picks one on their behalf — auto-selection, the
// install picker and its under-spec fallback, the tray catalog, the
// control plane's device catalog, `models ls --detail`, the generated
// docs table — reaches the catalog through this one function. Filtering
// HERE means a surface that forgets the distinction shows too little
// rather than too much, and too little is recoverable.
//
// The inverse default was considered and rejected: it would put the
// obligation on every present and future caller, and one miss is the
// defect class this exists to prevent — a model nobody should be
// offered turning up as somebody's recommendation.
//
// Callers that must see EVERY entry — model-name resolution, tier
// lookup for usage ingest, the catalog tooling that validates the whole
// set — call BundledManifestsIncludingInternal and say why.
func BundledManifests() ([]Manifest, error) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		return nil, err
	}
	out := make([]Manifest, 0, len(all))
	for _, m := range all {
		if m.InternalOnly != "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// BundledManifestsIncludingInternal decodes every JSON file under
// proto/catalog/bundled, including entries marked InternalOnly.
//
// Use it only where the question is "does this name resolve" rather
// than "what may we offer". An internal model has to stay resolvable:
// the routing sentinel pins one as the daemon's bundled model, and a
// device already serving one still has to resolve to a quality tier
// when its usage is ingested.
func BundledManifestsIncludingInternal() ([]Manifest, error) {
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
//   - AWQ-quantized variants are Hugging Face repositories (any org, waired-ai/waired#1427)
//   - max_parallel ≥ 0, and nonzero only on a build ollama alone serves (waired-ai/waired-agent#1423)
//   - context_length > 0
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
		// An AWQ build is a Hugging Face repository. Which org publishes it
		// is not checked here: the owner allowed quantizations published
		// outside the model's own org (2026-09-16, waired-ai/waired#1427),
		// which retired the rule that AWQ comes from Qwen/ only. The agent's
		// bundled-catalog tests hold such a variant to a pinned revision.
		if isAWQ(v.Quantization) && v.Source.Type != SourceHuggingFace {
			return fmt.Errorf("manifest %s variant %s: AWQ quantization requires source.type=huggingface", m.ModelID, v.VariantID)
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
		if err := validateSizingLayout(m.ModelID, v); err != nil {
			return err
		}
		if err := validateMTP(m.ModelID, v); err != nil {
			return err
		}
		if v.MaxParallel < 0 {
			return fmt.Errorf("manifest %s variant %s: max_parallel must be ≥ 0, got %d", m.ModelID, v.VariantID, v.MaxParallel)
		}
		if v.MaxParallel > 0 && !runtimesEqual(v.RuntimeSupport, []string{RuntimeOllama}) {
			return fmt.Errorf("manifest %s variant %s: max_parallel is read only for a build ollama serves; runtime_support is %v", m.ModelID, v.VariantID, v.RuntimeSupport)
		}
		if d := v.Source.Digest; d != "" {
			if v.Source.Type != SourceOllama {
				return fmt.Errorf("manifest %s variant %s: source.digest pins an ollama tag; source.type is %q", m.ModelID, v.VariantID, v.Source.Type)
			}
			if !validDigest(d) {
				return fmt.Errorf("manifest %s variant %s: source.digest %q is not sha256:<64 lowercase hex>", m.ModelID, v.VariantID, d)
			}
		}
	}
	for engine, id := range m.DefaultVariant {
		if engine != RuntimeOllama && engine != RuntimeVLLM {
			return fmt.Errorf("manifest %s: default_variant names unknown engine %q", m.ModelID, engine)
		}
		found := false
		for _, v := range m.Variants {
			if v.VariantID == id && slices.Contains(v.RuntimeSupport, engine) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("manifest %s: default_variant[%s] = %q names no variant that %s serves", m.ModelID, engine, id, engine)
		}
	}
	// Last, matching the bullet order above, so a manifest with a
	// structural fault still reports that fault rather than this one.
	//
	// A missing context_length is a silent opt-out rather than an obvious
	// hole: hostfit.OllamaServedWindows is empty for it, so a manifest
	// that lost its window has no rung ladder and the tuner exports no
	// window at all (the engine keeps its 32k default). Nothing reaches that today — every bundled entry
	// has one, and MeetsNativeContextFloor needs >= 200000, so a
	// zero-window model is unselectable anyway — but a PIN skips both,
	// and a pin is the one input that arrives from outside.
	//
	// Found by a concurrent session while reviewing #552's capacity
	// pricing; closed here because #522 is the change that touches these
	// manifests.
	if m.ContextLength <= 0 {
		return fmt.Errorf("manifest %s: context_length must be > 0, got %d", m.ModelID, m.ContextLength)
	}
	return nil
}

// validateSizingLayout checks the fields the VRAM sizing reads beyond the
// weight total: KV-cache types the variant's engines understand, a
// host-resident part no larger than the whole, and a non-negative layout.
func validateSizingLayout(modelID string, v Variant) error {
	for _, t := range v.KVCacheTypes {
		ok := false
		for _, rt := range v.RuntimeSupport {
			switch rt {
			case RuntimeOllama:
				ok = ok || t == KVCacheF16 || t == KVCacheQ8_0 || t == KVCacheQ4_0
			case RuntimeVLLM:
				ok = ok || t == KVCacheFP16 || t == KVCacheFP8
			}
		}
		if !ok {
			return fmt.Errorf("manifest %s variant %s: kv_cache_types value %q is not a KV-cache type of %v", modelID, v.VariantID, t, v.RuntimeSupport)
		}
	}
	if v.HostResidentWeightGB < 0 || (v.EstimatedWeightGB > 0 && v.HostResidentWeightGB > v.EstimatedWeightGB) {
		return fmt.Errorf("manifest %s variant %s: host_resident_weight_gb %.2f must be in [0, estimated_weight_gb %.2f]", modelID, v.VariantID, v.HostResidentWeightGB, v.EstimatedWeightGB)
	}
	if g := v.GGUF; g != nil {
		if g.BlockCount <= 0 || g.FullAttentionLayers < 0 || g.FullAttentionLayers > g.BlockCount ||
			g.RecurrentStateBytes < 0 || g.DraftMaxTokens < 0 || g.RepeatingBytes < 0 ||
			g.ExpertBytes < 0 || g.ExpertBytes > g.RepeatingBytes ||
			g.TensorBytes <= 0 || g.ProjectorBytes < 0 || g.NextNBytes < 0 ||
			g.InlineProjectorBytes < 0 || g.InlineProjectorBytes > g.TensorBytes ||
			g.TiedOutputBytes < 0 || g.TiedOutputBytes > g.TensorBytes ||
			g.NextNLayers < 0 || g.NextNLayers >= g.BlockCount ||
			g.RepeatingBytes+g.NextNBytes > g.TensorBytes {
			return fmt.Errorf("manifest %s variant %s: gguf layout %+v is inconsistent", modelID, v.VariantID, *g)
		}
	}
	return nil
}

// validateMTP checks the multi-token prediction fields (waired-ai/waired#1432,
// #1433): the checkpoint facts belong to vLLM builds and come as a pair, and
// a draft the product chooses has layers to run on — MTP layers on a vLLM
// build, nextn blocks on an ollama tag that does not already set its own
// draft_num_predict.
func validateMTP(modelID string, v Variant) error {
	if v.MTPLayers < 0 || v.MTPKVBytesPerTokenFP16 < 0 || v.MTPDraftTokens < 0 {
		return fmt.Errorf("manifest %s variant %s: mtp fields must be ≥ 0", modelID, v.VariantID)
	}
	if (v.MTPLayers > 0) != (v.MTPKVBytesPerTokenFP16 > 0) {
		return fmt.Errorf("manifest %s variant %s: mtp_layers and mtp_kv_bytes_per_token_fp16 are set together", modelID, v.VariantID)
	}
	if v.MTPLayers > 0 && v.Format != FormatSafetensors {
		return fmt.Errorf("manifest %s variant %s: mtp_layers describes a safetensors checkpoint; an ollama build records gguf.nextn_layers", modelID, v.VariantID)
	}
	if v.MTPDraftTokens == 0 {
		return nil
	}
	if v.Format == FormatSafetensors {
		if v.MTPLayers == 0 {
			return fmt.Errorf("manifest %s variant %s: mtp_draft_tokens needs mtp_layers", modelID, v.VariantID)
		}
		return nil
	}
	if v.GGUF == nil || v.GGUF.NextNLayers == 0 {
		return fmt.Errorf("manifest %s variant %s: mtp_draft_tokens needs gguf.nextn_layers", modelID, v.VariantID)
	}
	if v.GGUF.DraftMaxTokens > 0 {
		return fmt.Errorf("manifest %s variant %s: the tag already sets draft_num_predict=%d; mtp_draft_tokens only stamps a tag that does not", modelID, v.VariantID, v.GGUF.DraftMaxTokens)
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
