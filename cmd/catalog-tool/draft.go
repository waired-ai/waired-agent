package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
)

func init() {
	subcommands["draft"] = subcommand{run: runDraft, summary: "assemble a manifest JSON from inputs + computed fields"}
}

// draftSpec is the compact input an author (or the catalog-radar LLM step)
// writes: the prose/identity fields plus, per variant, the published facts
// (params, quant, source, config). `draft` computes every numeric footprint
// field from these so they are never hand-typed. quality_tier is intentionally
// NOT part of the spec — it is assigned catalog-wide by `tier` (#133).
type draftSpec struct {
	ModelID       string                `json:"model_id"`
	DisplayName   string                `json:"display_name,omitempty"`
	ModelAliases  []string              `json:"model_aliases,omitempty"`
	License       string                `json:"license,omitempty"`
	ContextLength int                   `json:"context_length"`
	Capabilities  []string              `json:"capabilities,omitempty"`
	Runtime       catalog.RuntimePolicy `json:"runtime"`
	Security      catalog.Security      `json:"security"`
	Variants      []draftVariant        `json:"variants"`
}

type draftVariant struct {
	VariantID      string                       `json:"variant_id"`
	Format         string                       `json:"format"`
	Quantization   string                       `json:"quantization"`
	RuntimeSupport []string                     `json:"runtime_support"`
	Source         catalog.VariantSource        `json:"source"`
	VendorSupport  *catalog.VendorSupportMatrix `json:"vendor_support,omitempty"`

	TotalParams  int64 `json:"total_params"`
	ActiveParams int64 `json:"active_params,omitempty"`

	// One of ConfigPath / ConfigRepo supplies the architecture for KV/arch.
	ConfigPath     string `json:"config_path,omitempty"`
	ConfigRepo     string `json:"config_repo,omitempty"`
	ConfigRevision string `json:"config_revision,omitempty"`

	MXFP4Native      bool    `json:"mxfp4_native,omitempty"`
	MeasuredWeightGB float64 `json:"measured_weight_gb,omitempty"`
	MinEngineVersion string  `json:"min_engine_version,omitempty"`

	// ContextForSizing sizes the suggested min_vram_mb/min_ram_gb. Defaults to
	// the manifest ContextLength when 0.
	ContextForSizing int `json:"context_for_sizing,omitempty"`

	// QualityTier may be left 0 here; `tier` assigns it. If set, it is kept
	// (lets an author pin a value the formula then validates for uniqueness).
	QualityTier int `json:"quality_tier,omitempty"`
}

func runDraft(args []string) error {
	fs := flag.NewFlagSet("draft", flag.ContinueOnError)
	specPath := fs.String("spec", "", "path to a draft-spec JSON (default: read stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var raw []byte
	var err error
	if *specPath == "" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*specPath)
	}
	if err != nil {
		return fmt.Errorf("draft: read spec: %w", err)
	}
	var spec draftSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("draft: parse spec: %w", err)
	}
	if spec.ModelID == "" {
		return fmt.Errorf("draft: spec.model_id required")
	}
	if len(spec.Variants) == 0 {
		return fmt.Errorf("draft: spec.variants must list at least one variant")
	}

	m := catalog.Manifest{
		ModelID:       spec.ModelID,
		DisplayName:   spec.DisplayName,
		ModelAliases:  spec.ModelAliases,
		License:       spec.License,
		ContextLength: spec.ContextLength,
		Capabilities:  spec.Capabilities,
		Runtime:       spec.Runtime,
		Security:      spec.Security,
	}

	var warnings []string
	for i, dv := range spec.Variants {
		v, w, err := expandVariant(spec, dv)
		if err != nil {
			return fmt.Errorf("draft: variant %d (%s): %w", i, dv.VariantID, err)
		}
		warnings = append(warnings, w...)
		m.Variants = append(m.Variants, v)
	}

	if err := printJSON(m); err != nil {
		return err
	}
	allTiersSet := true
	for _, v := range m.Variants {
		if v.QualityTier == 0 {
			allTiersSet = false
		}
	}
	if allTiersSet {
		if err := m.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "draft: WARNING manifest fails Validate(): %v\n", err)
		}
	} else {
		warnings = append(warnings, "quality_tier left 0 on one or more variants — run `catalog-tool tier` to assign before validating")
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "draft: warning:", w)
	}
	return nil
}

func expandVariant(spec draftSpec, dv draftVariant) (catalog.Variant, []string, error) {
	if dv.TotalParams <= 0 {
		return catalog.Variant{}, nil, fmt.Errorf("total_params required")
	}
	q, ok := scoring.QuantByName(dv.Quantization)
	if !ok {
		return catalog.Variant{}, nil, fmt.Errorf("unknown quantization %q", dv.Quantization)
	}
	if (dv.ConfigPath == "") == (dv.ConfigRepo == "") {
		return catalog.Variant{}, nil, fmt.Errorf("exactly one of config_path / config_repo required")
	}
	cfg, err := loadArchConfig(dv.ConfigPath, dv.ConfigRepo, dv.ConfigRevision)
	if err != nil {
		return catalog.Variant{}, nil, err
	}

	var warnings []string
	full, fullDerived := cfg.FullAttnLayers()
	headDim, headDerived := cfg.ResolvedHeadDim()
	if fullDerived {
		warnings = append(warnings, dv.VariantID+": full-attention layer count assumed = all layers")
	}
	if headDerived {
		warnings = append(warnings, fmt.Sprintf("%s: head_dim derived = %d", dv.VariantID, headDim))
	}
	kv := scoring.KVBytesPerTokenFP16(full, cfg.NumKeyValueHeads, headDim)

	weightGB := scoring.WeightGB(dv.TotalParams, q)
	if dv.MeasuredWeightGB > 0 {
		weightGB = dv.MeasuredWeightGB
	} else if isPartialQuant(q.Name) || dv.MXFP4Native {
		warnings = append(warnings, fmt.Sprintf("%s: %s weight %.1f GB is a lower bound; set measured_weight_gb from the real artifact", dv.VariantID, q.Name, round1(weightGB)))
	}

	sizingCtx := dv.ContextForSizing
	if sizingCtx == 0 {
		sizingCtx = spec.ContextLength
	}
	vramAtCtx := scoring.VRAMGB(weightGB, kv, sizingCtx)

	v := catalog.Variant{
		VariantID:           dv.VariantID,
		Format:              dv.Format,
		Quantization:        dv.Quantization,
		RuntimeSupport:      dv.RuntimeSupport,
		EstimatedWeightGB:   round1(weightGB),
		QualityTier:         dv.QualityTier,
		ParamCount:          dv.TotalParams,
		QuantizationTier:    q.Tier,
		Source:              dv.Source,
		ActiveParams:        moeActive(dv.ActiveParams),
		KVBytesPerTokenFP16: kv,
		AttentionArch:       cfg.DeriveAttentionArch(),
		VendorSupport:       dv.VendorSupport,
		MXFP4Native:         dv.MXFP4Native,
		MinEngineVersion:    dv.MinEngineVersion,
	}
	// min_ram_gb applies to ollama runtimes, min_vram_mb to vllm.
	if slices.Contains(dv.RuntimeSupport, catalog.RuntimeVLLM) {
		v.MinVRAMMB = scoring.SuggestMinVRAMMB(vramAtCtx)
	}
	if slices.Contains(dv.RuntimeSupport, catalog.RuntimeOllama) {
		v.MinRAMGB = scoring.SuggestMinRAMGB(vramAtCtx)
	}
	return v, warnings, nil
}

// moeActive keeps an explicit MoE active count but leaves dense (unset) at 0,
// matching the manifest convention (0 == "= ParamCount").
func moeActive(active int64) int64 { return active }
