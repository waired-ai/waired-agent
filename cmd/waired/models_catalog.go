package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
)

// catalogDetailResp mirrors management.ModelCatalogResponse (the fields
// the detail view renders). The CLI keeps its own decode struct so it
// stays decoupled from the management package, matching the rest of
// cmd/waired's inline-struct convention.
type catalogDetailResp struct {
	PreferredModelID string `json:"preferred_model_id"`
	Engine           string `json:"engine"`
	Host             struct {
		RAMTotalGB  int    `json:"ram_total_gb"`
		VRAMTotalMB int    `json:"vram_total_mb"`
		GPUModel    string `json:"gpu_model"`
	} `json:"host"`
	Families []catalogDetailFamily `json:"families"`
}

type catalogDetailFamily struct {
	ModelID      string             `json:"model_id"`
	DisplayName  string             `json:"display_name"`
	Fits         bool               `json:"fits"`
	Active       bool               `json:"active"`
	Preferred    bool               `json:"preferred"`
	Downloaded   bool               `json:"downloaded"`
	Downloading  bool               `json:"downloading"`
	DeficitLabel string             `json:"deficit_label"`
	Recommended  *catalogDetailSpec `json:"recommended"`
}

type catalogDetailSpec struct {
	VariantID    string `json:"variant_id"`
	Quantization string `json:"quantization"`
	MinRAMGB     int    `json:"min_ram_gb"`
	MinVRAMMB    int    `json:"min_vram_mb"`
	QualityTier  int    `json:"quality_tier"`
	ParamCount   int64  `json:"param_count"`
	ActiveParams int64  `json:"active_params"`
}

// runModelsCatalog renders `waired models ls --detail`: the host's
// hardware, then each bundled family with its recommended specs, fit
// verdict, and download/selection state. Reads /inference/catalog so it
// shares the agent's fit logic and recommended-spec source of truth with
// the tray and docs page.
func runModelsCatalog(mgmt string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(mgmt + "/waired/v1/inference/catalog")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// The endpoint is unmounted on builds without a preference store
		// (older agents / minimal configs). Degrade to a clear message
		// instead of an opaque "status 404" error.
		fmt.Println("Catalog view unavailable: this agent does not expose the model catalog endpoint.")
		fmt.Println("Use `waired models ls` for the download inventory.")
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var cat catalogDetailResp
	if err := json.Unmarshal(body, &cat); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Print(formatCatalogDetail(cat))
	return nil
}

// formatCatalogDetail is the pure renderer (unit-tested without a live
// agent). engine drives whether the RECOMMENDED column reports RAM
// (ollama) or VRAM (vllm) — the host serves one engine at a time.
func formatCatalogDetail(c catalogDetailResp) string {
	var b strings.Builder

	b.WriteString("Host: ")
	if c.Host.GPUModel != "" {
		b.WriteString(c.Host.GPUModel)
		if c.Host.VRAMTotalMB > 0 {
			fmt.Fprintf(&b, " %d GB VRAM", (c.Host.VRAMTotalMB+512)/1024)
		}
		fmt.Fprintf(&b, " / %d GB RAM", c.Host.RAMTotalGB)
	} else {
		fmt.Fprintf(&b, "%d GB RAM (no GPU)", c.Host.RAMTotalGB)
	}
	engine := c.Engine
	if engine == "" {
		engine = "unknown"
	}
	fmt.Fprintf(&b, " · engine=%s\n\n", engine)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	// Writes target a strings.Builder-backed tabwriter, so they never
	// error; ignore the returns to satisfy errcheck.
	_, _ = fmt.Fprintln(tw, "  MODEL\tPARAMS\tTIER\tRECOMMENDED\tFIT")
	for _, f := range c.Families {
		params, tier, rec := "-", "-", "-"
		if f.Recommended != nil {
			params = formatParamCount(f.Recommended.ParamCount, f.Recommended.ActiveParams)
			if f.Recommended.QualityTier > 0 {
				tier = fmt.Sprintf("%d", f.Recommended.QualityTier)
			}
			rec = formatRecommendedResource(c.Engine, f.Recommended)
		}
		fit := "✓ fits"
		if !f.Fits {
			fit = "✗"
			if f.DeficitLabel != "" {
				fit = "✗ " + f.DeficitLabel
			}
		}
		_, _ = fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\n",
			catalogStateMarker(f), f.ModelID, params, tier, rec, fit)
	}
	_ = tw.Flush()

	b.WriteString("\nLegend: ● active  → preferred (switching)  ↓ downloaded  ⋯ downloading\n")
	b.WriteString("The Auto-Selector serves the highest quality-tier model that fits this host.\n")
	b.WriteString("Why the current pick: `waired infer --explain`.\n")
	b.WriteString("Full hardware-fit reference: https://docs.waired.ai/reference/model-catalog/\n")
	return b.String()
}

// catalogStateMarker returns a one-rune status glyph for a family row.
func catalogStateMarker(f catalogDetailFamily) string {
	switch {
	case f.Active:
		return "●"
	case f.Preferred:
		return "→"
	case f.Downloading:
		return "⋯"
	case f.Downloaded:
		return "↓"
	default:
		return " "
	}
}

// formatRecommendedResource picks the engine-appropriate recommended
// memory figure: min VRAM for vllm, min RAM for ollama (the default).
func formatRecommendedResource(engine string, s *catalogDetailSpec) string {
	if engine == "vllm" {
		if s.MinVRAMMB > 0 {
			return fmt.Sprintf("%d GB VRAM", (s.MinVRAMMB+1023)/1024)
		}
		return "-"
	}
	if s.MinRAMGB > 0 {
		return fmt.Sprintf("%d GB RAM", s.MinRAMGB)
	}
	return "-"
}

// formatParamCount humanizes the total parameter count and appends the
// MoE active count when it differs (e.g. "30B (3.3B act)").
func formatParamCount(total, active int64) string {
	if total <= 0 {
		return "-"
	}
	s := humanizeParams(total)
	if active > 0 && active != total {
		s += fmt.Sprintf(" (%s act)", humanizeParams(active))
	}
	return s
}

func humanizeParams(n int64) string {
	const billion = 1_000_000_000
	const million = 1_000_000
	switch {
	case n >= billion:
		v := float64(n) / billion
		if v >= 100 || v == float64(int64(v)) {
			return fmt.Sprintf("%.0fB", v)
		}
		return fmt.Sprintf("%.1fB", v)
	case n >= million:
		return fmt.Sprintf("%.0fM", float64(n)/million)
	default:
		return fmt.Sprintf("%d", n)
	}
}
