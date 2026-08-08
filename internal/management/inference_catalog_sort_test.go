package management

import (
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The catalog used to arrive in manifest-filename order — alphabetical,
// and a statement about nothing. That was survivable only while every
// row printed a quality number a reader could sort by eye; #537 took the
// number off, so the order has to carry what the number did.
//
// The rule is the control plane's setup wizard's (modelsForEngine): what
// runs first, best first, with the rows that cannot run kept and sunk.
func TestSortCatalogFamilies(t *testing.T) {
	runs := func(id string, tier int) CatalogFamily {
		return CatalogFamily{ModelID: id, Fits: true,
			Fit: &hostfit.Presentation{Runnable: true, QualityTier: tier}}
	}
	slow := func(id string, tier int) CatalogFamily {
		f := runs(id, tier)
		f.Fit.Speed = hostfit.SpeedSlow
		return f
	}
	demoted := func(id string, tier int) CatalogFamily {
		f := runs(id, tier)
		f.Fit.NotRecommended = true
		return f
	}
	tooBig := func(id string, tier int) CatalogFamily {
		return CatalogFamily{ModelID: id, Fits: false,
			Fit: &hostfit.Presentation{QualityTier: tier}}
	}
	noBuild := func(id string, tier int) CatalogFamily {
		return CatalogFamily{ModelID: id, Fits: false,
			Fit: &hostfit.Presentation{Reason: hostfit.ReasonNoVariantForEngine, QualityTier: tier}}
	}

	// Deliberately in the alphabetical order the endpoint used to emit,
	// so a sort that did nothing would fail rather than pass.
	fams := []CatalogFamily{
		noBuild("a-no-build", 99),
		tooBig("b-too-big", 95),
		demoted("c-demoted", 90),
		slow("d-slow", 85),
		runs("e-runs-low", 40),
		runs("f-runs-high", 80),
	}
	sortCatalogFamilies(fams)

	got := make([]string, len(fams))
	for i, f := range fams {
		got[i] = f.ModelID
	}
	want := []string{
		"f-runs-high", // runs, best first
		"e-runs-low",
		"c-demoted", // runs but is not the right choice here
		"d-slow",    // runs but we already know it is too slow
		"b-too-big", // does not run: at least the shortfall names hardware
		"a-no-build",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// A row with no Fit at all — the engine-version floor leaves it at its
// zero value on purpose — must still sort, using the recommended-spec
// tier. Nothing may panic on the nil.
func TestSortCatalogFamilies_ToleratesAMissingFit(t *testing.T) {
	fams := []CatalogFamily{
		{ModelID: "no-fit-low", Fits: true, Recommended: &CatalogSpec{QualityTier: 10}},
		{ModelID: "no-fit-high", Fits: true, Recommended: &CatalogSpec{QualityTier: 90}},
		{ModelID: "nothing-at-all", Fits: true},
	}
	sortCatalogFamilies(fams)
	if fams[0].ModelID != "no-fit-high" {
		t.Errorf("order = %s..., want the higher-tier row first", fams[0].ModelID)
	}
	if fams[2].ModelID != "nothing-at-all" {
		t.Errorf("a row with no ranking at all must sink, got %s in last place", fams[2].ModelID)
	}
}

// Ties keep catalog order rather than being shuffled: the sort is stable
// so a catalog change is the only thing that can move a row.
func TestSortCatalogFamilies_TiesKeepCatalogOrder(t *testing.T) {
	tie := func(id string) CatalogFamily {
		return CatalogFamily{ModelID: id, Fits: true,
			Fit: &hostfit.Presentation{Runnable: true, QualityTier: 50}}
	}
	fams := []CatalogFamily{tie("first"), tie("second"), tie("third")}
	sortCatalogFamilies(fams)
	for i, want := range []string{"first", "second", "third"} {
		if fams[i].ModelID != want {
			t.Fatalf("tie order = %v, want the input order preserved", fams)
		}
	}
}

// TestInferenceCatalog_RowsCarryTheSizeClass wires the REAL producer.
//
// Every other assertion about the size class in this repo reads a
// CatalogFamily somebody built in a test — which pins how a row renders
// and leaves "does the endpoint ever fill it" untested. Blanking the
// assignment in handleInferenceCatalog passed the whole suite until this
// existed.
func TestInferenceCatalog_RowsCarryTheSizeClass(t *testing.T) {
	// Weight annotations kept out of catalogFixture(): they feed the
	// capacity math too, and this test is about the classification, not
	// about re-deciding what fits on the shared fixture's host.
	sized := []catalog.Manifest{
		{
			ModelID: "little", DisplayName: "Little",
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport:    []string{catalog.RuntimeOllama},
				MinRAMGB:          8,
				QualityTier:       35,
				EstimatedWeightGB: 3.4, // 4,403 MiB
				Source:            catalog.VariantSource{Type: catalog.SourceOllama, Tag: "little:q4"},
			}},
		},
		{
			ModelID: "middling", DisplayName: "Middling",
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport:    []string{catalog.RuntimeOllama},
				MinRAMGB:          32,
				QualityTier:       70,
				EstimatedWeightGB: 18, // 19,308 MiB
				Source:            catalog.VariantSource{Type: catalog.SourceOllama, Tag: "middling:q4"},
			}},
		},
		{
			// No weight annotation: unpriceable, and the row must say so
			// with an empty class rather than guessing a small one.
			ModelID: "unpriceable", DisplayName: "Unpriceable",
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       8, QualityTier: 20,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "unpriceable:q4"},
			}},
		},
	}
	inf := &fakeInference{hwProfile: hardware.Profile{RAMTotalGB: 64}}
	cfg := &CatalogConfig{
		PreferencePath: filepath.Join(t.TempDir(), "preferred-model.json"),
		ManifestsFn:    func() ([]catalog.Manifest, error) { return sized, nil },
	}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)
	_, got := doGet(t, s, "/waired/v1/inference/catalog")

	want := map[string]string{
		"little":      hostfit.ModelSizeSmall,
		"middling":    hostfit.ModelSizeMedium,
		"unpriceable": "",
	}
	if len(got.Families) != len(want) {
		t.Fatalf("families = %d, want %d", len(got.Families), len(want))
	}
	for _, f := range got.Families {
		if f.ModelSize != want[f.ModelID] {
			t.Errorf("%s: model_size = %q, want %q — the endpoint is the only writer of this "+
				"field, so a row that reaches a picker without it renders no class at all",
				f.ModelID, f.ModelSize, want[f.ModelID])
		}
	}
}
