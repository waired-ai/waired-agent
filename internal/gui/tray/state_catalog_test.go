package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func connectedSnapshotWithCatalog(c *management.ModelCatalogResponse) Snapshot {
	return Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: true, AccountEmail: "u@example.com", DeviceID: "dev-1"},
		Status:   &management.Status{Phase: "active"},
		Catalog:  c,
	}
}

func TestUpdate_CatalogHidden_WhenSnapshotNil(t *testing.T) {
	got := Update(connectedSnapshotWithCatalog(nil))
	if got.ShowCatalog {
		t.Errorf("ShowCatalog should be false when Snapshot.Catalog is nil")
	}
	if got.CatalogActiveLabel != "" || len(got.CatalogEntries) != 0 {
		t.Errorf("catalog fields should be empty: %+v", got)
	}
}

func TestUpdate_CatalogHidden_WhenNetworkTransitioning(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B", Fits: true, Downloaded: true},
		},
	}
	snap := Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: true},
		Status:   &management.Status{Phase: "starting"},
		Catalog:  c,
	}
	got := Update(snap)
	if got.ShowCatalog {
		t.Errorf("ShowCatalog should be false during a transition phase")
	}
}

func TestUpdate_CatalogActiveRowGetsBullet(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Active: &management.CatalogActive{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct"},
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B", Fits: true, Downloaded: true},
			{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct", Fits: true, Downloaded: true, Active: true},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if !got.ShowCatalog {
		t.Fatalf("ShowCatalog should be true")
	}
	if got.CatalogActiveLabel != "Active: Qwen3 8B Instruct" {
		t.Errorf("CatalogActiveLabel: %q", got.CatalogActiveLabel)
	}
	if len(got.CatalogEntries) != 2 {
		t.Fatalf("entries: want 2, got %d", len(got.CatalogEntries))
	}
	if got.CatalogEntries[1].Label != "● Qwen3 8B Instruct" {
		t.Errorf("active row label: %q", got.CatalogEntries[1].Label)
	}
	if got.CatalogEntries[0].Label != "Qwen3 4B" {
		t.Errorf("plain row label: %q", got.CatalogEntries[0].Label)
	}
}

func TestUpdate_CatalogPreferredButNotActive(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Active:           &management.CatalogActive{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B"},
		PreferredModelID: "qwen3-8b-instruct",
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B", Fits: true, Downloaded: true, Active: true},
			{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct", Fits: true, Downloaded: true, Preferred: true},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if got.CatalogEntries[1].Label != "Qwen3 8B Instruct (switching…)" {
		t.Errorf("preferred row label: %q", got.CatalogEntries[1].Label)
	}
	if got.CatalogEntries[1].Disabled {
		t.Errorf("preferred row should remain clickable")
	}
}

func TestUpdate_CatalogDownloading(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-14b-instruct", DisplayName: "Qwen3 14B", Fits: true, Downloading: true},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if got.CatalogEntries[0].Label != "Qwen3 14B (downloading…)" {
		t.Errorf("downloading label: %q", got.CatalogEntries[0].Label)
	}
}

func TestUpdate_CatalogOverCapacityIsDisabled(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-32b-instruct", DisplayName: "Qwen3 32B Instruct", Fits: false,
				DeficitLabel: "needs 24 GB VRAM (have 8 GB)"},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if !got.CatalogEntries[0].Disabled {
		t.Errorf("over-capacity row should be Disabled, got %+v", got.CatalogEntries[0])
	}
	if got.CatalogEntries[0].Label != "Qwen3 32B Instruct — needs 24 GB VRAM (have 8 GB)" {
		t.Errorf("over-capacity label: %q", got.CatalogEntries[0].Label)
	}
}

func TestUpdate_CatalogNotDownloadedFitButMissingPullsOnSelect(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-1.7b-instruct", DisplayName: "Qwen3 1.7B", Fits: true, Downloaded: false},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if got.CatalogEntries[0].Label != "Qwen3 1.7B (downloads on select)" {
		t.Errorf("not-downloaded label: %q", got.CatalogEntries[0].Label)
	}
	if got.CatalogEntries[0].Disabled {
		t.Errorf("not-downloaded row should be clickable (click triggers pull)")
	}
}

func TestUpdate_CatalogNoActive_LabelDisplaysNone(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B", Fits: true, Downloaded: true},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if got.CatalogActiveLabel != "Active: (none)" {
		t.Errorf("active label when nil: %q", got.CatalogActiveLabel)
	}
}

func TestUpdate_CatalogTrimsToMaxEntries(t *testing.T) {
	families := make([]management.CatalogFamily, MaxCatalogEntries+5)
	for i := range families {
		families[i] = management.CatalogFamily{
			ModelID:     "model-" + string(rune('a'+i)),
			DisplayName: "Model " + string(rune('a'+i)),
			Fits:        true,
			Downloaded:  true,
		}
	}
	c := &management.ModelCatalogResponse{Families: families}
	got := Update(connectedSnapshotWithCatalog(c))
	if len(got.CatalogEntries) != MaxCatalogEntries {
		t.Errorf("entries: want %d (trimmed), got %d", MaxCatalogEntries, len(got.CatalogEntries))
	}
}

func TestUpdate_CatalogRecommendedSpec_OllamaShowsRAM(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct",
				Fits: true, Downloaded: true,
				Recommended: &management.CatalogSpec{MinRAMGB: 8, QualityTier: 50, ParamCount: 7_610_000_000},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	row := got.CatalogEntries[0]
	if row.Label != "Qwen3 8B Instruct · 8 GB" {
		t.Errorf("ollama spec label: %q", row.Label)
	}
	for _, want := range []string{"min 8 GB RAM", "quality tier 50", "7.6B params"} {
		if !strings.Contains(row.Tooltip, want) {
			t.Errorf("tooltip %q missing %q", row.Tooltip, want)
		}
	}
}

func TestUpdate_CatalogRecommendedSpec_VLLMShowsVRAM(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "vllm",
		Active: &management.CatalogActive{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct"},
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct",
				Fits: true, Downloaded: true, Active: true,
				Recommended: &management.CatalogSpec{MinVRAMMB: 8000, QualityTier: 60, ParamCount: 7_610_000_000},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	row := got.CatalogEntries[0]
	// 8000 MB rounds to 8 GB; active row keeps its bullet plus the suffix.
	if row.Label != "● Qwen3 8B Instruct · 8 GB" {
		t.Errorf("vllm active spec label: %q", row.Label)
	}
	if !strings.Contains(row.Tooltip, "min 8 GB VRAM") {
		t.Errorf("tooltip should report VRAM on vllm: %q", row.Tooltip)
	}
	if strings.Contains(row.Tooltip, "RAM") && !strings.Contains(row.Tooltip, "VRAM") {
		t.Errorf("tooltip should not report RAM on vllm: %q", row.Tooltip)
	}
}

func TestUpdate_CatalogOverCapacity_NoSpecSuffix(t *testing.T) {
	// Over-capacity rows spell out the requirement in the deficit label,
	// so the compact "· N GB" suffix must not be appended (would be
	// redundant with "needs 24 GB VRAM").
	c := &management.ModelCatalogResponse{
		Engine: "vllm",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3-32b-instruct", DisplayName: "Qwen3 32B Instruct",
				Fits: false, DeficitLabel: "needs 24 GB VRAM (have 8 GB)",
				Recommended: &management.CatalogSpec{MinVRAMMB: 24576, QualityTier: 80},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	row := got.CatalogEntries[0]
	if row.Label != "Qwen3 32B Instruct — needs 24 GB VRAM (have 8 GB)" {
		t.Errorf("over-capacity label should carry only the deficit: %q", row.Label)
	}
	if strings.Contains(row.Label, " · ") {
		t.Errorf("over-capacity label should not get a spec suffix: %q", row.Label)
	}
}

func TestUpdate_CatalogMoEParamsInTooltip(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "vllm",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3-coder-30b-a3b-instruct", DisplayName: "Qwen3 Coder 30B A3B", Fits: true, Downloaded: true,
				Recommended: &management.CatalogSpec{MinVRAMMB: 24000, QualityTier: 68, ParamCount: 30_000_000_000, ActiveParams: 3_300_000_000},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if !strings.Contains(got.CatalogEntries[0].Tooltip, "30B (3.3B active) params") {
		t.Errorf("MoE params tooltip: %q", got.CatalogEntries[0].Tooltip)
	}
}

func TestUpdate_CatalogDisplayNameFallsBackToModelID(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-30b-a3b-instruct", DisplayName: "", Fits: true, Downloaded: true},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if got.CatalogEntries[0].Label != "qwen3-30b-a3b-instruct" {
		t.Errorf("fallback label: %q", got.CatalogEntries[0].Label)
	}
}
