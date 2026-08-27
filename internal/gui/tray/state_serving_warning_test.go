package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// waired-agent#1038: the tray reads the same two fields the CLI does, so
// the two surfaces cannot say different things about the same host.
func degradedTrayFamily() management.CatalogFamily {
	return management.CatalogFamily{
		ModelID:         "qwen3.8-27b",
		DisplayName:     "Qwen3.8 27B",
		Fits:            true,
		Active:          true,
		Downloaded:      true,
		MeasuredTokps:   21,
		ServingWarning:  "loaded with only 491 MB of GPU memory left free",
		ServingDegraded: true,
	}
}

func TestFormatCatalogEntry_DegradedActiveRow(t *testing.T) {
	e := formatCatalogEntry(degradedTrayFamily(), "ollama", management.CatalogHost{})
	if !strings.Contains(e.Label, "warning") {
		t.Errorf("label = %q, want it to say the model is running with a warning", e.Label)
	}
	if strings.Contains(e.Label, "recommended") {
		t.Errorf("label = %q — a degraded row must not also carry a recommendation", e.Label)
	}
	if !strings.Contains(e.Tooltip, "491 MB") {
		t.Errorf("tooltip = %q, want the engine's own sentence", e.Tooltip)
	}
}

func TestFormatCatalogEntry_PlannedSpillIsNotDegraded(t *testing.T) {
	f := degradedTrayFamily()
	f.ServingDegraded = false
	f.RecommendedPick = true
	e := formatCatalogEntry(f, "ollama", management.CatalogHost{})
	if strings.Contains(e.Label, "warning") {
		t.Errorf("label = %q — a working host keeps its badge", e.Label)
	}
	if !strings.Contains(e.Label, "recommended") {
		t.Errorf("label = %q, want the recommendation kept", e.Label)
	}
}
