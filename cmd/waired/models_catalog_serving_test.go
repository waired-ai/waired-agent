package main

import (
	"strings"
	"testing"
)

// The sentence the engine recorded on the reproduction host
// (waired-agent#1038).
const svMagServingWarning = "qwen3.8:27b-mtp-q4_K_M-wb2048 loaded with only 491 MB of GPU " +
	"memory left free (a prompt needs at least 768 MB here); real requests will fail with " +
	"an out-of-memory error"

func degradedActiveRow() catalogDetailFamily {
	return catalogDetailFamily{
		ModelID:         "qwen3.8-27b",
		Fits:            true,
		Active:          true,
		Downloaded:      true,
		MeasuredTokps:   21,
		ServingWarning:  svMagServingWarning,
		ServingDegraded: true,
	}
}

func TestCatalogFitColumn_DegradedRowDoesNotSayFits(t *testing.T) {
	got := catalogFitColumn(catalogDetailHost{}, degradedActiveRow())
	if strings.Contains(got, "fits") {
		t.Errorf("FIT = %q — the engine already recorded that it could not hold this "+
			"configuration here", got)
	}
	if !strings.Contains(got, "warning") {
		t.Errorf("FIT = %q, want it to say the model is running with a warning", got)
	}
}

func TestCatalogFitColumn_PlannedSpillStillSaysFits(t *testing.T) {
	// The trap this gate exists to avoid: a warning is set on every host
	// serving the planned #624 spill, and those hosts work.
	f := degradedActiveRow()
	f.ServingDegraded = false
	f.ServingWarning = "context window set to 200704 tokens for coding-agent workloads"
	if got := catalogFitColumn(catalogDetailHost{}, f); !strings.Contains(got, "✓ fits") {
		t.Errorf("FIT = %q, want a working host to keep its verdict", got)
	}
}

func TestFormatCatalogDetail_FooterQuotesTheEngineSentence(t *testing.T) {
	out := formatCatalogDetail(catalogDetailResp{
		Engine:   "ollama",
		Families: []catalogDetailFamily{degradedActiveRow()},
	})
	if !strings.Contains(out, svMagServingWarning) {
		t.Errorf("the engine's own sentence is not printed:\n%s", out)
	}
	if !strings.Contains(out, "waired doctor") {
		t.Errorf("the footer should point at the command that says what to do:\n%s", out)
	}
}

func TestFormatCatalogDetail_NoFooterWhenNothingIsDegraded(t *testing.T) {
	f := degradedActiveRow()
	f.ServingDegraded = false
	out := formatCatalogDetail(catalogDetailResp{
		Engine:   "ollama",
		Families: []catalogDetailFamily{f},
	})
	if strings.Contains(out, svMagServingWarning) {
		t.Errorf("a working host must not grow a warning block:\n%s", out)
	}
}

func TestModelPickerRow_DegradedActiveRow(t *testing.T) {
	got := modelPickerRow(catalogDetailHost{}, degradedActiveRow())
	if !strings.Contains(got, "warning") {
		t.Errorf("picker row = %q, want it to carry the same verdict the FIT column does", got)
	}
}
