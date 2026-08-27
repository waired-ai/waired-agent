package tray

import (
	"fmt"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
	if len(got.CatalogEntries) != 0 {
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
	if got.CatalogEntries[1].UnfitReason != "" {
		t.Errorf("preferred row carries a warning it has no reason for: %q",
			got.CatalogEntries[1].UnfitReason)
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

// Product contract (waired-agent#831, the 2026-08-08 warn-and-ask ruling
// recorded as waired-ai/waired#1067): a model this computer cannot hold
// is SELECTABLE. It states the shortfall and carries it as the warning
// the click asks with. This test used to assert the opposite — the row
// was greyed and the click was a silent no-op — which was the behaviour
// the ruling withdrew.
func TestUpdate_CatalogOverCapacityWarnsAndStaysSelectable(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-32b-instruct", DisplayName: "Qwen3 32B Instruct", Fits: false,
				DeficitLabel: "needs 24 GB VRAM (have 8 GB)"},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if got.CatalogEntries[0].UnfitReason != "needs 24 GB VRAM (have 8 GB)" {
		t.Errorf("over-capacity row must carry the shortfall as its warning, got %+v",
			got.CatalogEntries[0])
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
	if got.CatalogEntries[0].UnfitReason != "" {
		t.Errorf("not-downloaded row carries a warning it has no reason for: %q",
			got.CatalogEntries[0].UnfitReason)
	}
}

// The catalog's active entry is rendered by the top-level Engine row, and
// only when there is an engine running it. A catalog that names no active
// model used to produce the row "Active: (none)", which told a reader
// nothing they could act on — the useful fact on such a host is what the
// engine is doing, which is what the row says now (waired-agent#1032).
func TestUpdate_CatalogNoActive_SaysNothingAboutAModel(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Families: []management.CatalogFamily{
			{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B", Fits: true, Downloaded: true},
		},
	}
	snap := connectedSnapshotWithCatalog(c)
	snap.Inference = &management.InferenceStatus{SubsystemState: "awaiting_model"}
	got := Update(snap)
	if got.StatusEngineLabel != "◐ Engine: awaiting model" {
		t.Errorf("StatusEngineLabel: %q", got.StatusEngineLabel)
	}
}

// synthCatalogFamilies builds n placeholder rows in manifest order.
func synthCatalogFamilies(n int) []management.CatalogFamily {
	families := make([]management.CatalogFamily, n)
	for i := range families {
		id := fmt.Sprintf("model-%02d", i)
		families[i] = management.CatalogFamily{
			ModelID:     id,
			DisplayName: "Model " + id,
			Fits:        true,
			Downloaded:  true,
		}
	}
	return families
}

func TestUpdate_CatalogTrimsToMaxEntries(t *testing.T) {
	c := &management.ModelCatalogResponse{Families: synthCatalogFamilies(MaxCatalogEntries + 5)}
	got := Update(connectedSnapshotWithCatalog(c))
	if len(got.CatalogEntries) != MaxCatalogEntries {
		t.Errorf("entries: want %d (trimmed), got %d", MaxCatalogEntries, len(got.CatalogEntries))
	}
}

// Product contract (waired-agent#319): the menu may run out of rows, but it
// may never drop the row describing what this machine is running. Families
// render alphabetically, so on a host serving the last-sorting family the
// old tail-truncation hid the active model entirely — the submenu claimed a
// running model did not exist.
func TestUpdate_CatalogKeepsActiveAndPreferredPastTheCap(t *testing.T) {
	families := synthCatalogFamilies(MaxCatalogEntries + 5)
	active := len(families) - 1 // sorts last — the shape that broke
	preferred := len(families) - 2
	families[active].Active = true
	families[preferred].Preferred = true

	c := &management.ModelCatalogResponse{Families: families}
	got := Update(connectedSnapshotWithCatalog(c))

	if len(got.CatalogEntries) != MaxCatalogEntries {
		t.Fatalf("entries: want %d, got %d", MaxCatalogEntries, len(got.CatalogEntries))
	}
	present := map[string]bool{}
	for _, e := range got.CatalogEntries {
		present[e.ModelID] = true
	}
	if !present[families[active].ModelID] {
		t.Errorf("active family %q was dropped by the cap", families[active].ModelID)
	}
	if !present[families[preferred].ModelID] {
		t.Errorf("preferred family %q was dropped by the cap", families[preferred].ModelID)
	}
}

// The cap must always exceed what the agent can actually serve. This is the
// guard that fails the build instead of the menu: the bundled catalog has
// silently outgrown MaxCatalogEntries twice (at 12, and at 20 — #319).
func TestCatalogCapCoversBundledManifests(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if len(manifests) > MaxCatalogEntries {
		t.Fatalf("bundled manifests (%d) exceed MaxCatalogEntries (%d): raise the cap, "+
			"or the alphabetical tail silently falls off the Models submenu",
			len(manifests), MaxCatalogEntries)
	}
}

func TestUpdate_CatalogRecommendedSpec_OllamaShowsRAM(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct",
				ModelSize: "small",
				Fits:      true, Downloaded: true,
				Recommended: &management.CatalogSpec{MinRAMGB: 8, QualityTier: 50, ParamCount: 7_610_000_000},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	row := got.CatalogEntries[0]
	// The unit is spelled out and the size class joins the label
	// (waired-agent#321): the review that asked for one presentation across
	// the pickers asked for a quality value on each of them, and a bare
	// "· 8 GB" left the operator to infer which memory it meant.
	if row.Label != "Qwen3 8B Instruct · 8 GB RAM · small" {
		t.Errorf("ollama spec label: %q", row.Label)
	}
	for _, want := range []string{"needs 8 GB RAM", "small — fits an 8 GB GPU", "7.6B params"} {
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
				ModelSize: "small",
				Fits:      true, Downloaded: true, Active: true,
				Recommended: &management.CatalogSpec{MinVRAMMB: 8000, QualityTier: 60, ParamCount: 7_610_000_000},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	row := got.CatalogEntries[0]
	// 8000 MB rounds to 8 GB; active row keeps its bullet plus the suffix.
	if row.Label != "● Qwen3 8B Instruct · 8 GB VRAM · small" {
		t.Errorf("vllm active spec label: %q", row.Label)
	}
	if !strings.Contains(row.Tooltip, "needs 8 GB VRAM") {
		t.Errorf("tooltip should report VRAM on vllm: %q", row.Tooltip)
	}
	if strings.Contains(row.Tooltip, "RAM") && !strings.Contains(row.Tooltip, "VRAM") {
		t.Errorf("tooltip should not report RAM on vllm: %q", row.Tooltip)
	}
}

// Product contract: one requirement, one number. A VRAM figure that is not a
// whole GB rounds UP everywhere it is shown — the tray suffix, the tray
// tooltip, `waired models ls --detail`, and the deficit label. 24000 MB used
// to render "23 GB" here and "24 GB" in the other two (waired-agent#319).
func TestUpdate_CatalogRecommendedSpec_VLLMRoundsUp(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "vllm",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3.6-27b", DisplayName: "Qwen3.6 27B",
				ModelSize: "medium",
				Fits:      true, Downloaded: true,
				Recommended: &management.CatalogSpec{MinVRAMMB: 24000, QualityTier: 72},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	row := got.CatalogEntries[0]
	if row.Label != "Qwen3.6 27B · 24 GB VRAM · medium" {
		t.Errorf("24000 MB must round up to 24 GB, got label %q", row.Label)
	}
	if !strings.Contains(row.Tooltip, "needs 24 GB VRAM") {
		t.Errorf("tooltip must round up too: %q", row.Tooltip)
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

// Product contract (waired-agent#321): the figure a picker prints is the
// RESIDENT requirement — weights + the reserved KV budget + engine
// overhead, which is what the fit rule compares — not min_ram_gb, a
// threshold authored for a host that loads into system RAM. The two
// differ here on purpose so a regression to the old field is visible.
func TestUpdate_CatalogPrefersTheResidentRequirement(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3.6-27b", DisplayName: "Qwen3.6 27B", Fits: true, Downloaded: true,
				ModelSize:   "medium",
				Recommended: &management.CatalogSpec{MinRAMGB: 32, QualityTier: 62},
				Fit:         &hostfit.Presentation{Runnable: true, RequiredResidentMB: 18 * 1024, QualityTier: 62},
			},
		},
	}
	row := Update(connectedSnapshotWithCatalog(c)).CatalogEntries[0]
	if row.Label != "Qwen3.6 27B · 18 GB VRAM · medium" {
		t.Errorf("label should carry the resident figure in graphics memory: %q", row.Label)
	}
}

// Product contract: exactly one row is marked as this computer's own
// pick, and the mark is short — the full sentence lives in the tooltip
// because a menu the operator scans has room for a mark, not a paragraph.
func TestUpdate_CatalogRecommendedPickIsMarked(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B", Fits: true, Downloaded: true,
				ModelSize:       "small",
				RecommendedPick: true,
				Fit:             &hostfit.Presentation{Runnable: true, RequiredResidentMB: 9 * 1024, QualityTier: 55},
			},
			{
				ModelID: "qwen3.5-2b", DisplayName: "Qwen3.5 2B", Fits: true, Downloaded: true,
				Fit: &hostfit.Presentation{Runnable: true, RequiredResidentMB: 3 * 1024, QualityTier: 27},
			},
		},
	}
	rows := Update(connectedSnapshotWithCatalog(c)).CatalogEntries
	if rows[0].Label != "Qwen3.5 9B · 9 GB VRAM · small — recommended" {
		t.Errorf("recommended label: %q", rows[0].Label)
	}
	if !strings.Contains(rows[0].Tooltip, "Chosen from this computer’s RAM + VRAM combined.") {
		t.Errorf("recommended tooltip: %q", rows[0].Tooltip)
	}
	if strings.Contains(rows[1].Label, "recommended") {
		t.Errorf("only the host's own pick may be marked, got %q", rows[1].Label)
	}
}

// PRODUCT CONTRACT (waired-agent#784): a row this computer has run
// reports what it got, in the tooltip where the sentences live. The mark
// stays a mark.
//
// It is the only thing on this menu that explains a "recommended" mark
// that has moved: everything else in the tooltip is what the rules
// predict for this hardware, and this is what the hardware did.
func TestUpdate_CatalogMeasuredRateIsInTheTooltip(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B", Fits: true, Downloaded: true,
				ModelSize:     "small",
				MeasuredTokps: 11,
				Fit:           &hostfit.Presentation{Runnable: true, RequiredResidentMB: 9 * 1024, QualityTier: 55},
			},
			{
				ModelID: "qwen3.5-2b", DisplayName: "Qwen3.5 2B", Fits: true, Downloaded: true,
				RecommendedPick: true,
				Fit:             &hostfit.Presentation{Runnable: true, RequiredResidentMB: 3 * 1024, QualityTier: 27},
			},
		},
	}
	rows := Update(connectedSnapshotWithCatalog(c)).CatalogEntries

	if !strings.Contains(rows[0].Tooltip, "Measured 11 tok/s on this computer.") {
		t.Errorf("the measured row does not report its figure: %q", rows[0].Tooltip)
	}
	if strings.Contains(rows[0].Label, "tok/s") {
		t.Errorf("the figure belongs in the tooltip, not the label: %q", rows[0].Label)
	}
	if strings.Contains(rows[0].Label, "recommended") {
		t.Errorf("the measured-slow row still carries the mark: %q", rows[0].Label)
	}
	if !strings.Contains(rows[1].Label, "recommended") {
		t.Errorf("the badge did not land on the next row: %q", rows[1].Label)
	}
	if strings.Contains(rows[1].Tooltip, "Measured") {
		t.Errorf("a row nobody ran reports a measurement: %q", rows[1].Tooltip)
	}
}

// Product contract, and the one waired-agent#229 exists for: a model that
// RUNS but is not the right choice here stays SELECTABLE. It is annotated,
// never hidden and never disabled — hiding it is the bug #229 removed, and
// until now nothing on this surface said anything at all (waired#988
// shipped the rule and the tray drew such a row as ordinary).
func TestUpdate_CatalogNotRecommendedIsOfferedNotHidden(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3.6-35b-a3b", DisplayName: "Qwen3.6 35B A3B", Fits: true, Downloaded: true,
				ModelSize: "medium",
				Fit: &hostfit.Presentation{
					Runnable: true, RequiredResidentMB: 24 * 1024, QualityTier: 65,
					NotRecommended: true, NotRecommendedReason: hostfit.ReasonWeightsSpill,
				},
			},
		},
	}
	row := Update(connectedSnapshotWithCatalog(c)).CatalogEntries[0]
	if row.UnfitReason != "" {
		t.Errorf("a runnable model must not be warned over however strongly it is discouraged: %q",
			row.UnfitReason)
	}
	if row.Label != "Qwen3.6 35B A3B · 24 GB VRAM · medium — not recommended here" {
		t.Errorf("not-recommended label: %q", row.Label)
	}
	if !strings.Contains(row.Tooltip, "not entirely in VRAM") {
		t.Errorf("the tooltip has to say WHY, not just that: %q", row.Tooltip)
	}
}

// Product contract: the blocked reason is written for a person who has
// never heard of an engine or a variant. The wire's own sentence for this
// case is "no vLLM variant" — two words of ours and an engine
// name — which is why this row is the one the machine code overrides.
// PRODUCT CONTRACT (waired-agent#850): the row carries WHICH KIND of
// wall it hit, so the click's dialog is worded from the verdict rather
// than by matching the sentence back. The engine-version case is the one
// that matters: hostfit does not price it, so the row arrives with a
// DeficitLabel and no Fit at all (internal/router/family_picker.go), and
// a classification of "not no-build, therefore memory" put a sentence
// about this computer's memory on it.
func TestUpdate_CatalogUnfitKindComesFromTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		fam  management.CatalogFamily
		want UnfitKind
	}{
		{
			name: "runs here",
			fam:  management.CatalogFamily{ModelID: "a", DisplayName: "A", Fits: true},
			want: UnfitNone,
		},
		{
			name: "capacity refusal",
			fam: management.CatalogFamily{
				ModelID: "b", DisplayName: "B", DeficitLabel: "needs 68 GB — 60 GB allocatable",
				Fit: &hostfit.Presentation{Reason: hostfit.ReasonInsufficientMemory},
			},
			want: UnfitMemory,
		},
		{
			name: "no build for this way of running AI",
			fam: management.CatalogFamily{
				ModelID: "c", DisplayName: "C", DeficitLabel: "no Ollama variant",
				Fit: &hostfit.Presentation{Reason: hostfit.ReasonNoVariantForEngine},
			},
			want: UnfitNoBuild,
		},
		{
			// The shape family_picker.go's engine-version branch emits.
			name: "engine version floor, no verdict to read",
			fam: management.CatalogFamily{
				ModelID: "d", DisplayName: "D",
				DeficitLabel: "needs ollama ≥ 0.32.13 (running unknown version)",
			},
			want: UnfitOther,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &management.ModelCatalogResponse{Families: []management.CatalogFamily{tc.fam}}
			got := Update(connectedSnapshotWithCatalog(c)).CatalogEntries[0]
			if got.UnfitKind != tc.want {
				t.Errorf("UnfitKind = %q, want %q (row %+v)", got.UnfitKind, tc.want, got)
			}
		})
	}
}

func TestUpdate_CatalogNoVariantForEngineSaysSoInPlainWords(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Families: []management.CatalogFamily{
			{
				ModelID: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash", Fits: false,
				ModelSize:    "large",
				DeficitLabel: "no Ollama variant",
				Fit:          &hostfit.Presentation{Reason: hostfit.ReasonNoVariantForEngine, QualityTier: 80},
			},
		},
	}
	row := Update(connectedSnapshotWithCatalog(c)).CatalogEntries[0]
	// The row is the router's own label: it names the engine the catalog
	// has no variant for (waired-ai/waired#1272 — copy names the engine
	// where the fact is engine-specific). Inverted from the pre-#1272
	// "never show an engine name" pin.
	if row.UnfitReason != "no Ollama variant" {
		t.Errorf("a model this engine cannot serve must warn in the router's words: %q",
			row.UnfitReason)
	}
	if row.Label != "DeepSeek V4 Flash — no Ollama variant" {
		t.Errorf("blocked label: %q", row.Label)
	}
}

// Product contract (waired-agent#632): a row can be fitting AND
// recommended and still leave gigabytes of a coding session in system
// RAM, and the tray says how much on the row where selecting it starts
// the download.
//
// The figures are the rc8 Windows host's own (sv-xps15, RTX 4070 Laptop,
// budget 8188 MB): qwen3.5-9b needs 10719 MB to serve the coding window,
// qwen3.5-4b needs 7539 and fits on the card. The 9B was marked
// recommended, downloaded at 6.6 GB, and then measured 5 tok/s.
//
// The mark is a QUANTITY, not a speed. Excluding on a predicted rate is
// what docs/decisions/20260804/1937-… decision 4 removed, so the row
// stays recommended and stays selectable.
func TestUpdate_CatalogRowSaysHowMuchContextCacheSpills(t *testing.T) {
	c := &management.ModelCatalogResponse{
		Engine: "ollama",
		Host: management.CatalogHost{
			RAMTotalGB: 32, VRAMTotalMB: 8188, GPUBudgetMB: 8188,
			GPUModel: "NVIDIA GeForce RTX 4070 Laptop GPU", OSReservedGB: 16,
		},
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B",
				Fits: true, Downloaded: true, RecommendedPick: true,
				Fit: &hostfit.Presentation{Runnable: true, RequiredWindowResidentMB: 10719},
			},
			{
				ModelID: "qwen3.5-4b", DisplayName: "Qwen3.5 4B",
				Fits: true, Downloaded: true,
				Fit: &hostfit.Presentation{Runnable: true, RequiredWindowResidentMB: 7539},
			},
		},
	}
	got := Update(connectedSnapshotWithCatalog(c))
	if len(got.CatalogEntries) != 2 {
		t.Fatalf("entries: want 2, got %d", len(got.CatalogEntries))
	}
	nine, four := got.CatalogEntries[0], got.CatalogEntries[1]

	if !strings.Contains(nine.Label, "2.5 GB of KV cache in system RAM") {
		t.Errorf("spilling row label does not say how much: %q", nine.Label)
	}
	if !strings.Contains(nine.Label, "recommended") {
		t.Errorf("the mark demoted a recommended row: %q", nine.Label)
	}
	if nine.UnfitReason != "" {
		t.Errorf("the mark warned over a fitting row: %+v", nine)
	}
	if !strings.Contains(nine.Tooltip, "read from system RAM, which is slower") {
		t.Errorf("tooltip does not explain the mark: %q", nine.Tooltip)
	}

	// It fits on the card. Saying "0 GB spills" would be true and useless.
	if strings.Contains(four.Label, "system RAM") {
		t.Errorf("resident row claims a shortfall: %q", four.Label)
	}
	if strings.Contains(four.Tooltip, "system RAM") {
		t.Errorf("resident row's tooltip claims a shortfall: %q", four.Tooltip)
	}
}

// An agent too old to send gpu_budget_mb, and a host with no card at
// all, are both "no figure" rather than "nothing spills". Neither may
// produce a half-written sentence.
func TestUpdate_CatalogSpillSilentWithoutABudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		host management.CatalogHost
	}{
		{"agent too old to report a budget", management.CatalogHost{RAMTotalGB: 32}},
		{"cpu-only host", management.CatalogHost{RAMTotalGB: 32, OSReservedGB: 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &management.ModelCatalogResponse{
				Engine: "ollama",
				Host:   tc.host,
				Families: []management.CatalogFamily{{
					ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B",
					Fits: true, Downloaded: true,
					Fit: &hostfit.Presentation{Runnable: true, RequiredWindowResidentMB: 10719},
				}},
			}
			got := Update(connectedSnapshotWithCatalog(c))
			if len(got.CatalogEntries) != 1 {
				t.Fatalf("entries: want 1, got %d", len(got.CatalogEntries))
			}
			if strings.Contains(got.CatalogEntries[0].Label, "system RAM") {
				t.Errorf("label claims a shortfall with no budget to compare: %q", got.CatalogEntries[0].Label)
			}
		})
	}
}
