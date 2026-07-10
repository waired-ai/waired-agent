package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/hfclient"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
)

// standardContexts are the context lengths the VRAM curve is reported at, so a
// reviewer can see how footprint scales before committing to a min_vram_mb.
var standardContexts = []int{4096, 32768, 131072, 262144}

type vramPoint struct {
	Context int     `json:"context"`
	VRAMGB  float64 `json:"vram_gb"`
}

// computeResult is the JSON the `compute` subcommand prints. The numeric
// manifest fields (kv_bytes_per_token_fp16, attention_arch, estimated_weight_gb,
// decode_flops_per_tok) are deterministic physics; suggested_* thresholds carry
// headroom and are explicitly a starting point the author reviews.
type computeResult struct {
	Quantization        string      `json:"quantization"`
	QuantizationTier    int         `json:"quantization_tier"`
	AttentionArch       string      `json:"attention_arch"`
	KVBytesPerTokenFP16 int         `json:"kv_bytes_per_token_fp16"`
	DecodeFLOPsPerTok   int64       `json:"decode_flops_per_tok"`
	EstimatedWeightGB   float64     `json:"estimated_weight_gb"`
	VRAMByContext       []vramPoint `json:"vram_gb_by_context"`
	SuggestedMinVRAMMB  int         `json:"suggested_min_vram_mb"`
	SuggestedMinRAMGB   int         `json:"suggested_min_ram_gb"`
	Warnings            []string    `json:"warnings,omitempty"`
}

func init() {
	subcommands["compute"] = subcommand{run: runCompute, summary: "compute footprint fields from a config.json + quant"}
}

func runCompute(args []string) error {
	fs := flag.NewFlagSet("compute", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to a local HuggingFace config.json (alternative to --repo)")
	repo := fs.String("repo", "", "HuggingFace repo id to fetch config.json from (e.g. Qwen/Qwen3-Coder-30B-A3B-Instruct)")
	revision := fs.String("revision", "main", "HuggingFace revision (commit/branch) when using --repo")
	quantName := fs.String("quant", "", "quantization name (Q4_K_M, AWQ-int4, MXFP4, FP8, BF16, ...) — required")
	contextLen := fs.Int("context", 32768, "context length used to size the suggested thresholds")
	totalParams := fs.Int64("total-params", 0, "total parameter count (required; config.json does not carry it)")
	activeParams := fs.Int64("active-params", 0, "MoE active parameter count (default: = total for dense models)")
	mxfp4Native := fs.Bool("mxfp4-native", false, "model is distributed natively in MXFP4 (gpt-oss family)")
	measuredWeightGB := fs.Float64("measured-weight-gb", 0, "measured on-disk weight in GB; overrides the formula (recommended for AWQ/GPTQ/MXFP4)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *quantName == "" {
		return fmt.Errorf("compute: --quant is required")
	}
	if *totalParams <= 0 {
		return fmt.Errorf("compute: --total-params is required and must be > 0")
	}
	if (*configPath == "") == (*repo == "") {
		return fmt.Errorf("compute: exactly one of --config or --repo is required")
	}

	cfg, err := loadArchConfig(*configPath, *repo, *revision)
	if err != nil {
		return err
	}

	q, ok := scoring.QuantByName(*quantName)
	if !ok {
		return fmt.Errorf("compute: unknown quantization %q (see scoring report §2.3 for the supported set)", *quantName)
	}

	var warnings []string

	full, fullDerived := cfg.FullAttnLayers()
	headDim, headDerived := cfg.ResolvedHeadDim()
	if fullDerived {
		warnings = append(warnings, "full-attention layer count could not be determined from config; assumed every layer is full attention")
	}
	if headDerived {
		warnings = append(warnings, fmt.Sprintf("head_dim absent from config; derived hidden_size/num_attention_heads = %d", headDim))
	}
	kv := scoring.KVBytesPerTokenFP16(full, cfg.NumKeyValueHeads, headDim)
	if kv == 0 {
		warnings = append(warnings, "kv_bytes_per_token_fp16 computed as 0 — config is missing layers / kv-heads / head_dim")
	}

	active := *activeParams
	if active == 0 {
		active = *totalParams // dense default
		if cfg.IsMoE() {
			warnings = append(warnings, "config looks like MoE but --active-params not given; decode FLOPs assume active == total (overestimate)")
		}
	}

	weightGB := scoring.WeightGB(*totalParams, q)
	if *measuredWeightGB > 0 {
		weightGB = *measuredWeightGB
	} else if isPartialQuant(q.Name) || *mxfp4Native {
		warnings = append(warnings, fmt.Sprintf(
			"%s is partially quantized (embeddings/attention stay higher precision); formula weight %.1f GB is a LOWER BOUND — pass --measured-weight-gb from the real artifact when known",
			q.Name, round1(weightGB)))
	}

	curve := make([]vramPoint, 0, len(standardContexts))
	for _, l := range standardContexts {
		curve = append(curve, vramPoint{Context: l, VRAMGB: round1(scoring.VRAMGB(weightGB, kv, l))})
	}
	vramAtCtx := scoring.VRAMGB(weightGB, kv, *contextLen)

	res := computeResult{
		Quantization:        q.Name,
		QuantizationTier:    q.Tier,
		AttentionArch:       cfg.DeriveAttentionArch(),
		KVBytesPerTokenFP16: kv,
		DecodeFLOPsPerTok:   scoring.DecodeFLOPsPerTok(active),
		EstimatedWeightGB:   round1(weightGB),
		VRAMByContext:       curve,
		SuggestedMinVRAMMB:  scoring.SuggestMinVRAMMB(vramAtCtx),
		SuggestedMinRAMGB:   scoring.SuggestMinRAMGB(vramAtCtx),
		Warnings:            warnings,
	}
	return printJSON(res)
}

// loadArchConfig reads config.json from a local path or fetches it from the Hub.
func loadArchConfig(path, repo, revision string) (scoring.ArchConfig, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return scoring.ArchConfig{}, fmt.Errorf("compute: read --config: %w", err)
		}
		var cfg scoring.ArchConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return scoring.ArchConfig{}, fmt.Errorf("compute: parse --config: %w", err)
		}
		return cfg, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, _, err := hfclient.New().FetchConfig(ctx, repo, revision)
	if err != nil {
		return scoring.ArchConfig{}, fmt.Errorf("compute: fetch config for %s: %w", repo, err)
	}
	return cfg, nil
}

// isPartialQuant reports whether a quant only compresses the expert/FFN weights
// (so the weight formula under-estimates real on-disk size).
func isPartialQuant(name string) bool {
	switch name {
	case "MXFP4", "AWQ-int4", "GPTQ-int4":
		return true
	}
	return false
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// printJSON writes v as indented JSON to stdout with a trailing newline.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
