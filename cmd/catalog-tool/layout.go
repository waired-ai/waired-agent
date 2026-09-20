package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/gguf"
	"github.com/waired-ai/waired-agent/internal/catalog/ollamaregistry"
)

// layoutResult is what `catalog-tool layout` reports for one ollama tag:
// the facts from the tag's manifest and its GGUF header that the VRAM
// sizing reads (waired-agent#1337). Byte figures are exact sums over the
// tensor table, so each is re-derivable by anyone who reads the header.
type layoutResult struct {
	Ref            string `json:"ref"`
	ModelID        string `json:"model_id,omitempty"`
	VariantID      string `json:"variant_id,omitempty"`
	Architecture   string `json:"architecture"`
	ModelBytes     int64  `json:"model_bytes"`
	ProjectorBytes int64  `json:"projector_bytes,omitempty"`
	TensorBytes    uint64 `json:"tensor_bytes"`

	BlockCount          int `json:"block_count"`
	NextNLayers         int `json:"nextn_predict_layers,omitempty"`
	FullAttentionLayers int `json:"full_attention_layers,omitempty"`
	DraftNumPredict     int `json:"draft_num_predict,omitempty"`

	TokenEmbdBytes         uint64 `json:"token_embd_bytes"`
	TokenEmbdType          string `json:"token_embd_type"`
	PerLayerTokenEmbdBytes uint64 `json:"per_layer_token_embd_bytes,omitempty"`
	OutputBytes            uint64 `json:"output_bytes,omitempty"`
	TiedOutput             bool   `json:"tied_output,omitempty"`
	RepeatingBytes         uint64 `json:"repeating_bytes"`
	ExpertBytes            uint64 `json:"expert_bytes,omitempty"`
	NextNBytes             uint64 `json:"nextn_bytes,omitempty"`
	InlineProjectorBytes   uint64 `json:"inline_projector_bytes,omitempty"`

	ArchScalars map[string]any `json:"arch_scalars,omitempty"`
	Error       string         `json:"error,omitempty"`

	// Manifest is what the variant's manifest carries, derived from the
	// facts above: the layout the VRAM sizing reads and the host-resident
	// weight (catalog.Variant).
	Manifest manifestLayout `json:"manifest"`
}

// manifestLayout is the two catalog.Variant fields `layout` derives.
type manifestLayout struct {
	HostResidentWeightGB float64            `json:"host_resident_weight_gb"`
	GGUF                 catalog.GGUFLayout `json:"gguf"`
}

func init() {
	subcommands["layout"] = subcommand{run: runLayout, summary: "read an ollama tag's GGUF header: the per-term facts the VRAM sizing needs"}
}

var blkIndexRe = regexp.MustCompile(`^blk\.(\d+)\.`)

func runLayout(args []string) error {
	fs := flag.NewFlagSet("layout", flag.ContinueOnError)
	tag := fs.String("tag", "", "ollama reference (e.g. qwen3.8:27b-mtp-q4_K_M or hf.co/org/repo:quant)")
	bundled := fs.Bool("bundled", false, "report every ollama variant of the bundled catalog")
	verbose := fs.Bool("arch-scalars", false, "include every <architecture>.* scalar in the report")
	check := fs.Bool("check", false, "with --bundled: fail when a manifest's gguf / host_resident_weight_gb differ from what the registry header derives")
	if err := fs.Parse(args); err != nil {
		return err
	}
	type target struct {
		ref, model, variant string
		shipped             catalog.Variant
	}
	var targets []target
	switch {
	case *tag != "":
		targets = append(targets, target{ref: *tag})
	case *bundled:
		ms, err := catalog.BundledManifests()
		if err != nil {
			return err
		}
		for _, m := range ms {
			for _, v := range m.Variants {
				if v.Source.Type != "ollama" || v.Source.Tag == "" {
					continue
				}
				targets = append(targets, target{ref: v.Source.Tag, model: m.ModelID, variant: v.VariantID, shipped: v})
			}
		}
	default:
		return fmt.Errorf("layout: --tag or --bundled is required")
	}
	if *check && *tag != "" {
		return fmt.Errorf("layout: --check compares the bundled catalog; use it with --bundled")
	}
	c := &ollamaregistry.Client{}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	var drift []string
	for _, t := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		res, err := readLayout(ctx, c, t.ref, *verbose)
		cancel()
		res.ModelID, res.VariantID = t.model, t.variant
		if err != nil {
			res.Ref = t.ref
			res.Error = err.Error()
		}
		if *check {
			if msg := layoutDrift(t.shipped, res); msg != "" {
				drift = append(drift, fmt.Sprintf("%s/%s: %s", t.model, t.variant, msg))
			}
			continue
		}
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
	if len(drift) > 0 {
		for _, d := range drift {
			fmt.Fprintln(os.Stderr, d)
		}
		return fmt.Errorf("layout: %d variant(s) differ from their registry header", len(drift))
	}
	return nil
}

// layoutDrift says how a shipped variant's layout fields differ from what
// its tag derives, or "" when they agree. A variant that carries no layout
// is not drift: the sizing falls back for it, and deriving one is a catalog
// change of its own (qwen3.8-flash-next waits on waired-agent#1305 /
// #1349, whose size correction has to land with it).
func layoutDrift(shipped catalog.Variant, derived layoutResult) string {
	if shipped.GGUF == nil {
		return ""
	}
	if derived.Error != "" {
		return "registry read failed: " + derived.Error
	}
	// The product writes mtp_draft_tokens onto a tag that publishes no
	// draft (waired-ai/waired#1433). Two changes upstream make that write
	// wrong, and each is named rather than left to the field dump below.
	if d := shipped.MTPDraftTokens; d > 0 {
		if g := derived.Manifest.GGUF; g.DraftMaxTokens > 0 {
			return fmt.Sprintf("mtp_draft_tokens = %d is written onto the tag, but the tag now sets draft_num_predict %d itself: "+
				"remove mtp_draft_tokens and take gguf.draft_max_tokens", d, g.DraftMaxTokens)
		} else if g.NextNLayers == 0 {
			return fmt.Sprintf("mtp_draft_tokens = %d, but the tag's GGUF no longer has nextn layers to draft with", d)
		}
	}
	if *shipped.GGUF != derived.Manifest.GGUF {
		return fmt.Sprintf("gguf = %+v, header derives %+v", *shipped.GGUF, derived.Manifest.GGUF)
	}
	if shipped.HostResidentWeightGB != derived.Manifest.HostResidentWeightGB {
		return fmt.Sprintf("host_resident_weight_gb = %v, header derives %v", shipped.HostResidentWeightGB, derived.Manifest.HostResidentWeightGB)
	}
	return ""
}

func readLayout(ctx context.Context, c *ollamaregistry.Client, ref string, verbose bool) (layoutResult, error) {
	out := layoutResult{Ref: ref}
	layers, err := c.Layers(ctx, ref)
	if err != nil {
		return out, err
	}
	out.ModelBytes, out.ProjectorBytes = layers.ModelBytes, layers.ProjectorBytes
	if n, ok := layers.ParamInt("draft_num_predict"); ok {
		out.DraftNumPredict = n
	}
	body, err := c.OpenBlob(ctx, ref, layers.ModelDigest)
	if err != nil {
		return out, err
	}
	h, err := gguf.Read(body)
	body.Close()
	if err != nil {
		return out, err
	}
	return layoutFromHeader(out, h, verbose)
}

// layoutFromHeader derives the report from a decoded header. Split out so
// the arithmetic is testable against a synthetic header.
func layoutFromHeader(out layoutResult, h gguf.Header, verbose bool) (layoutResult, error) {
	out.Architecture = h.Architecture()
	if n, ok := h.ArchUint("block_count"); ok {
		out.BlockCount = int(n)
	}
	if n, ok := h.ArchUint("nextn_predict_layers"); ok {
		out.NextNLayers = int(n)
	}
	if arr, ok := h.Arrays[out.Architecture+".attention.head_count_kv"]; ok {
		for _, v := range arr {
			if v > 0 {
				out.FullAttentionLayers++
			}
		}
	}
	nextnFrom := out.BlockCount - out.NextNLayers
	hasOutput := false
	for _, t := range h.Tensors {
		b, err := t.Bytes()
		if err != nil {
			return out, err
		}
		out.TensorBytes += b
		switch {
		case t.Name == "token_embd.weight":
			out.TokenEmbdBytes += b
			out.TokenEmbdType = gguf.TypeName(t.Type)
		case strings.HasPrefix(t.Name, "per_layer_token_embd"):
			out.PerLayerTokenEmbdBytes += b
		case strings.HasPrefix(t.Name, "output.") && !strings.HasPrefix(t.Name, "output_norm"):
			out.OutputBytes += b
			hasOutput = true
		case strings.HasPrefix(t.Name, "mtp."):
			// ollama's library builds of qwen3.5 carry their next-token head
			// under its own prefix rather than as trailing blocks; llama.cpp
			// loads it only to draft with.
			out.NextNBytes += b
		case strings.HasPrefix(t.Name, "v.") || strings.HasPrefix(t.Name, "mm.") || strings.HasPrefix(t.Name, "a."):
			// The prefixes ollama's mmprojMemoryRequirement sums for an
			// inline projector (llm/llama_server.go, v0.33.3).
			out.InlineProjectorBytes += b
		}
		if m := blkIndexRe.FindStringSubmatch(t.Name); m != nil {
			idx, _ := strconv.Atoi(m[1])
			if out.NextNLayers > 0 && idx >= nextnFrom {
				out.NextNBytes += b
				continue
			}
			out.RepeatingBytes += b
			if strings.Contains(t.Name, "_exps.") {
				out.ExpertBytes += b
			}
		}
	}
	out.TiedOutput = !hasOutput
	out.Manifest = manifestFromLayout(out, h)
	if verbose {
		out.ArchScalars = map[string]any{}
		keys := make([]string, 0)
		for k := range h.Scalars {
			if strings.HasPrefix(k, out.Architecture+".") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.ArchScalars[k] = h.Scalars[k]
		}
		for k, v := range h.Arrays {
			if strings.HasPrefix(k, out.Architecture+".") {
				out.ArchScalars[k] = v
			}
		}
	}
	return out, nil
}

// swaDefaultPeriod is the window/full period of a sliding-window
// architecture whose GGUF names no sliding_window_pattern, as llama.cpp
// decides it. laguna repeats full, SWA, SWA, SWA starting with a full
// block (src/models/laguna.cpp @b10760, the default of swa_period); its
// GGUFs carry only attention.sliding_window, and reading them as
// alternating counted 24 full blocks where the engine allocates 12. An
// architecture not listed alternates, which is gpt-oss.
var swaDefaultPeriod = map[string]int{"laguna": 4}

// swaFullLayers counts the full-attention blocks of a sliding-window
// model. A per-layer sliding_window_pattern array marks every windowed
// block non-zero (step35 writes one), a scalar one is the period with a
// full block first, and without either the architecture's default
// period applies.
func swaFullLayers(h gguf.Header, blocks int) int {
	arch := h.Architecture()
	if arr, ok := h.Arrays[arch+".attention.sliding_window_pattern"]; ok && len(arr) > 0 {
		n := 0
		for i, v := range arr {
			if i >= blocks {
				break
			}
			if v == 0 {
				n++
			}
		}
		return n
	}
	period := 2
	if p, ok := h.ArchUint("attention.sliding_window_pattern"); ok && p > 0 {
		period = int(p)
	} else if p, ok := swaDefaultPeriod[arch]; ok {
		period = p
	}
	return (blocks + period - 1) / period
}

// manifestFromLayout derives the catalog.Variant fields from the header.
//
// Full-attention blocks: a per-layer head_count_kv array names them (a
// zero is a linear block); a scalar with full_attention_interval spaces
// them evenly; a sliding_window model counts them the way llama.cpp does
// (swaFullLayers); anything else is all full attention.
//
// Recurrent state (qwen35 / qwen35moe / qwen4exp gated DeltaNet), per
// sequence, f32: R = n_linear·(conv_kernel−1)·(2·group_count·state_size +
// inner_size)·4 and S = n_linear·time_step_rank·state_size·
// (inner_size/time_step_rank)·4 — equal to llama_memory_recurrent's
// R (f32) 5.62 MiB and S (f32) 144.00 MiB for a dense 27B.
func manifestFromLayout(l layoutResult, h gguf.Header) manifestLayout {
	blocks := l.BlockCount - l.NextNLayers
	full := l.FullAttentionLayers
	if full == 0 && blocks > 0 {
		switch interval, ok := h.ArchUint("full_attention_interval"); {
		case ok && interval > 0:
			full = blocks / int(interval)
		default:
			if _, swa := h.ArchUint("attention.sliding_window"); swa {
				full = swaFullLayers(h, blocks)
			} else {
				full = blocks
			}
		}
	}
	var rs int64
	if kernel, ok := h.ArchUint("ssm.conv_kernel"); ok && full < blocks {
		nk, _ := h.ArchUint("ssm.group_count")
		dk, _ := h.ArchUint("ssm.state_size")
		inner, _ := h.ArchUint("ssm.inner_size")
		nv, _ := h.ArchUint("ssm.time_step_rank")
		if nv > 0 {
			linear := uint64(blocks - full)
			r := linear * (kernel - 1) * (2*nk*dk + inner) * 4
			st := linear * nv * dk * (inner / nv) * 4
			rs = int64(r + st)
		}
	}
	draft := 0
	if l.NextNLayers > 0 {
		draft = l.DraftNumPredict
	}
	host := l.TokenEmbdBytes + l.PerLayerTokenEmbdBytes
	// With no output tensor llama.cpp builds the output layer from a second
	// copy of token_embd, and that copy goes to the device while the input
	// copy stays in system RAM: the 4B's 2,513.56 MiB CUDA0 buffer is its
	// 2,016.2 MiB of blocks plus the 497.3 MiB embedding.
	var tied int64
	if l.TiedOutput {
		tied = int64(l.TokenEmbdBytes)
	}
	return manifestLayout{
		HostResidentWeightGB: math.Round(float64(host)/1e9*1000) / 1000,
		GGUF: catalog.GGUFLayout{
			BlockCount:           l.BlockCount,
			FullAttentionLayers:  full,
			RecurrentStateBytes:  rs,
			DraftMaxTokens:       draft,
			TensorBytes:          int64(l.TensorBytes),
			ProjectorBytes:       l.ProjectorBytes,
			NextNLayers:          l.NextNLayers,
			NextNBytes:           int64(l.NextNBytes),
			InlineProjectorBytes: int64(l.InlineProjectorBytes),
			TiedOutputBytes:      tied,
			RepeatingBytes:       int64(l.RepeatingBytes),
			ExpertBytes:          int64(l.ExpertBytes),
		},
	}
}
