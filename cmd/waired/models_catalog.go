package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/elevation"
)

// catalogDetailResp mirrors management.ModelCatalogResponse (the fields
// the detail view renders). The CLI keeps its own decode struct so it
// stays decoupled from the management package, matching the rest of
// cmd/waired's inline-struct convention.
type catalogDetailResp struct {
	PreferredModelID string `json:"preferred_model_id"`

	// ModelQuestionAnswered reports whether a person at this machine has
	// answered the model question — see the management field of the same
	// name. The install picker keys on this rather than on
	// PreferredModelID, which cannot tell an answer apart from an
	// instruction the setup path applied (waired-agent#627).
	//
	// An older daemon does not send it, so it decodes false: the picker
	// then falls back to the model-history signals it always used.
	ModelQuestionAnswered bool   `json:"model_question_answered"`
	Engine                string `json:"engine"`
	// EngineInstalled is whether Engine is actually on this host, or nil
	// from a daemon predating the field (#852). Engine alone names the
	// engine this computer WOULD use, so on a host with none it still
	// says "ollama" — which is what made every verdict below read as a
	// judgement by an engine that was not there.
	EngineInstalled *bool                 `json:"engine_installed"`
	Host            catalogDetailHost     `json:"host"`
	Families        []catalogDetailFamily `json:"families"`
}

// catalogDetailHost is the host block of the catalog response: the
// figures a surface needs to explain a verdict it did not compute.
//
// Named rather than inline since #625, because the explaining is now
// done here. The agent sends VALUES — what the machine has, and what it
// measured the system to be holding — and this side turns them into the
// sentence, which is the split waired-agent#321 established.
type catalogDetailHost struct {
	RAMTotalGB  int    `json:"ram_total_gb"`
	VRAMTotalMB int    `json:"vram_total_mb"`
	GPUModel    string `json:"gpu_model"`

	// UnifiedMemory says RAMTotalGB and VRAMTotalMB are the same bytes,
	// so a sentence must never add them.
	UnifiedMemory bool `json:"unified_memory"`

	// OSReservedGB is this install's measured OS + resident-application
	// deduction (#568). Absent on a host whose RAM probe failed, which
	// is why 0 means "no figure" rather than "reserves nothing".
	OSReservedGB int `json:"os_reserved_gb"`

	// GPUBudgetMB is the GPU-addressable memory the engine may use, the
	// pool where there is one. 0 is "no figure" — a host with no card,
	// or an agent too old to send it — and both mean this side says
	// nothing about residency rather than claiming a zero budget.
	GPUBudgetMB int `json:"gpu_budget_mb"`
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

	// ModelSize is which class of graphics card runs this model —
	// small / medium / large (#537). A property of the model, so it
	// reads the same on every computer; the FIT and NEEDS columns are
	// the ones about this one.
	ModelSize string `json:"model_size"`

	// Fit is the shared projection (proto/hostfit.Presentation) the
	// agent, the tray and the setup wizard all render from
	// (waired-agent#321). Mirrored rather than imported, like every other
	// decode struct in cmd/waired.
	Fit *catalogDetailFit `json:"fit"`

	// RecommendedPick marks the family this host would choose for itself.
	// Absent on every row only when nothing fits.
	RecommendedPick bool `json:"recommended_pick"`
}

// catalogDetailFit is the subset of hostfit.Presentation this view
// renders.
type catalogDetailFit struct {
	Runnable                 bool   `json:"runnable"`
	Reason                   string `json:"reason"`
	RequiredResidentMB       int    `json:"required_resident_mb"`
	RequiredWindowResidentMB int    `json:"required_window_resident_mb"`
	QualityTier              int    `json:"quality_tier"`
	NotRecommended           bool   `json:"not_recommended"`
	NotRecommendedReason     string `json:"not_recommended_reason"`

	// The engine-version floor's two figures (waired-agent#836), set
	// only alongside reasonEngineTooOld. Absent from an older agent's
	// wire, which is why the warning below still words itself from
	// DeficitLabel when they are empty.
	NeedEngineVersion string `json:"need_engine_version"`
	HaveEngineVersion string `json:"have_engine_version"`
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
// hardware, then each bundled family with what it needs, its fit
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
// agent). engine drives whether the NEEDS fallback column reports RAM
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
	// An engine-less host is a supported state, not a fault — it stays
	// enrolled and its requests go to the other computers (#387, #841) —
	// so the line says that rather than naming an engine that is not
	// here. The verdicts below still render: they are true about what
	// this computer would run once an engine is installed, and saying so
	// is the context that was missing (#852).
	if c.EngineInstalled != nil && !*c.EngineInstalled {
		b.WriteString(" · no AI engine installed\n\n")
		b.WriteString("! No AI engine is installed on this computer, so it cannot run a model itself.\n")
		b.WriteString("  Requests go to your other computers instead.\n")
		fmt.Fprintf(&b, "  %s\n", engineInstallSentence(runtime.GOOS))
		b.WriteString("  The verdicts below are what this computer would run once an engine is installed.\n\n")
	} else {
		engine := c.Engine
		if engine == "" {
			engine = "unknown"
		}
		fmt.Fprintf(&b, " · engine=%s\n\n", engine)
	}

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	// Writes target a strings.Builder-backed tabwriter, so they never
	// error; ignore the returns to satisfy errcheck.
	_, _ = fmt.Fprintln(tw, "  MODEL\tPARAMS\tSIZE\tNEEDS\tFIT")
	for _, f := range c.Families {
		params := "-"
		if f.Recommended != nil {
			params = formatParamCount(f.Recommended.ParamCount, f.Recommended.ActiveParams)
		}
		_, _ = fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\n",
			catalogStateMarker(f), f.ModelID, params,
			catalogSizeColumn(f), catalogNeedsColumn(c.Engine, f), catalogFitColumn(c.Host, f))
	}
	_ = tw.Flush()

	b.WriteString("\nLegend: ● active  → preferred (switching)  ↓ downloaded  ⋯ downloading\n")
	b.WriteString("NEEDS is the memory the model takes to serve a full ~200k-token coding\n" +
		"session: its weights, the engine's overhead, and the context cache.\n")
	b.WriteString("A model is offered whenever this computer has that much memory in total,\n" +
		"counting system RAM and graphics memory together. Waired recommends the\n" +
		"strongest model that can hold a whole coding session here.\n")
	b.WriteString("SIZE is which class of graphics card runs the model at all — small fits an\n" +
		"8 GB card, medium a 32 GB one, large needs more. Unlike NEEDS it says the\n" +
		"same thing on every computer, so it is the one to quote elsewhere.\n")
	b.WriteString("\"context cache in system RAM\" is the part of a full coding session this\n" +
		"computer's graphics card cannot hold. The model still runs; that part is\n" +
		"read from system memory, which is slower than reading it from the card.\n")
	b.WriteString("Why the current pick: `waired infer --explain`.\n")
	b.WriteString("Full hardware-fit reference: https://docs.waired.ai/reference/model-catalog/\n")
	return b.String()
}

// engineInstallSentence offers the install with the command quoted and
// the elevation it needs OUTSIDE the quotes.
//
// It used to interpolate a single string that carried both, so Windows
// rendered "Install one with `waired runtimes install ollama (from an
// elevated prompt)`" — prose inside a quotation that promised a command
// (#852, seen on pc-dell-premium). goos is a parameter so both arms are
// table-testable from any runner.
func engineInstallSentence(goos string) string {
	cmd := elevation.EngineInstallCommandFor(goos)
	if note := elevation.EngineInstallElevationNoteFor(goos); note != "" {
		return fmt.Sprintf("Install one with `%s`, %s.", cmd, note)
	}
	return fmt.Sprintf("Install one with `%s`.", cmd)
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

// catalogSizeColumn is which class of graphics card runs this model —
// small / medium / large, hostfit.ModelSize (#537).
//
// It used to print the raw quality_tier. #518 redefined that number as
// arithmetic over two catalog fields, so a column headed by a quality
// word claimed a measurement behind a composite.
//
// Not the same question as NEEDS beside it. NEEDS is what this model
// costs on THIS computer, counting its RAM and graphics memory together;
// this is a property of the model, the same on every machine, and it is
// the one that survives being read on a machine other than your own.
func catalogSizeColumn(f catalogDetailFamily) string {
	if f.ModelSize == "" {
		return "-"
	}
	return f.ModelSize
}

// catalogNeedsColumn is what the model needs to do coding work here:
// weights, the engine's own overhead, and the KV cache for the whole
// ~200k window (proto/hostfit.Presentation.RequiredWindowResidentMB).
//
// The window figure, not the fit-time one. RequiredResidentMB reserves a
// fixed 16,384 tokens of KV — a floor for "can this run at all" — and
// the two differ by ~2.6 GB on qwen3.5-4b (4,915 MiB vs 7,539 MiB). A
// user reading this column is asking what the model costs them in
// practice, and printing the smaller number is how a host used to be
// shown "needs about 5 GB", pull the model, and then be unable to hold a
// coding session in it (waired-ai/waired#1056 defect 2).
//
// It is memory, not graphics memory: the sum this is compared against is
// RAM plus dedicated VRAM, so labelling it "VRAM" would name the wrong
// pool on every host that spills. RequiredResidentMB stays as the
// fallback for a row the window figure cannot price, and min_ram_gb
// below that.
func catalogNeedsColumn(engine string, f catalogDetailFamily) string {
	if f.Fit != nil && f.Fit.RequiredWindowResidentMB > 0 {
		return fmt.Sprintf("%d GB", (f.Fit.RequiredWindowResidentMB+1023)/1024)
	}
	if f.Fit != nil && f.Fit.RequiredResidentMB > 0 {
		return fmt.Sprintf("%d GB VRAM", (f.Fit.RequiredResidentMB+1023)/1024)
	}
	if f.Recommended == nil {
		return "-"
	}
	return formatRecommendedResource(engine, f.Recommended)
}

// catalogFitColumn says whether the host can serve this model and, when
// it can, whether Waired would CHOOSE it here.
//
// The second half is the gap waired-agent#321 closes on this surface:
// waired-ai/waired#988 shipped the rule that keeps a weights-spilling
// model out of the automatic pick, and this column printed a bare
// "✓ fits" for exactly those models — the rule was real and no surface
// showed it.
// The third half, added by waired-agent#632: a row can be fitting AND
// recommended AND still keep gigabytes of its context cache off the
// card. On the rc8 Windows host the recommended model needed 10719 MB to
// serve the coding window against an 8188 MB budget, and this column
// said "✓ fits · recommended" over a 6.6 GB download that then measured
// 5 tok/s. The shortfall is annotated, never subtracted from the
// verdict: excluding on a PREDICTED rate is what
// docs/decisions/20260804/1937-… decision 4 removed, and it returns
// measured or not at all (waired-agent#466).
func catalogFitColumn(host catalogDetailHost, f catalogDetailFamily) string {
	if !f.Fits {
		if f.Fit != nil && f.Fit.Reason == reasonNoVariantForEngine {
			return "✗ not available on this computer"
		}
		if f.DeficitLabel != "" {
			return "✗ " + f.DeficitLabel
		}
		return "✗"
	}
	out := "✓ fits"
	switch {
	case f.RecommendedPick:
		out = "✓ fits · recommended"
	case f.Fit != nil && f.Fit.NotRecommended:
		out = "✓ fits · not recommended"
		if f.Fit.NotRecommendedReason != "" {
			out = "✓ fits · not recommended (" +
				strings.ReplaceAll(f.Fit.NotRecommendedReason, "_", " ") + ")"
		}
	}
	if mb := contextCacheSpillMB(host, f.Fit); mb > 0 {
		out += " · " + formatSpillGB(mb) + " of context cache in system RAM"
	}
	return out
}

// reasonNoVariantForEngine mirrors hostfit.ReasonNoVariantForEngine.
// cmd/waired decodes the management API with its own structs and string
// literals rather than importing the server-side packages; this keeps
// that convention while naming the value once.
const reasonNoVariantForEngine = "no_variant_for_engine"

// reasonEngineTooOld mirrors hostfit.ReasonEngineTooOld, on the same
// terms as the constant above. Both are pinned against the real ones in
// models_catalog_reason_test.go — a mirrored literal that drifts is a
// gate that silently stops firing, and this one gates a warning whose
// other arm says "memory" (waired-agent#836).
const reasonEngineTooOld = "engine_too_old"

// The capacity refusals, mirrored on the same terms as the two above
// and pinned by the same test.
//
// They are a set rather than a single code because the three name which
// pool fell short: the sum of everything allocatable (the ollama gate
// since waired-ai/waired#1056), RAM as installed, or the graphics card
// alone. What matters to a caller here is only that all three ARE about
// memory — that is the property warnModelWillNotRun keys on, and it is
// why the list has to be written out instead of inferred from "not one
// of the special cases" (waired-agent#862).
const (
	reasonInsufficientMemory = "insufficient_memory"
	reasonInsufficientRAM    = "insufficient_ram"
	reasonInsufficientVRAM   = "insufficient_vram"
)

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
