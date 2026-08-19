package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// mtpFamilyFixture mirrors the qwen3.6-27b shape that motivated the
// gate: a higher-tier mtp variant floored at engine 0.30.0 ahead of an
// unfloored plain variant.
func mtpFamilyFixture() catalog.Manifest {
	return catalog.Manifest{
		ModelID:      "dense-mtp",
		DisplayName:  "Dense MTP",
		Capabilities: []string{"chat"},
		Variants: []catalog.Variant{
			{
				VariantID: "mtp-q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       24, QualityTier: 71,
				MinEngineVersion: "0.30.0",
				Source:           catalog.VariantSource{Type: catalog.SourceOllama, Tag: "dense:mtp-q4"},
			},
			{
				VariantID: "q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       24, QualityTier: 70,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "dense:q4"},
			},
		},
	}
}

func TestRankModels_MinEngineVersion(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 64}
	in := PickInput{Catalog: []catalog.Manifest{mtpFamilyFixture()}, Hardware: hw, Engine: catalog.RuntimeOllama}

	t.Run("new engine ranks the floored variant first", func(t *testing.T) {
		in := in
		in.EngineVersion = "0.30.7"
		picks, err := RankModels(in)
		if err != nil {
			t.Fatalf("RankModels: %v", err)
		}
		if picks[0].Variant.VariantID != "mtp-q4" {
			t.Errorf("top pick = %s, want mtp-q4", picks[0].Variant.VariantID)
		}
	})

	t.Run("old engine excludes the floored variant", func(t *testing.T) {
		in := in
		in.EngineVersion = "0.24.0"
		picks, err := RankModels(in)
		if err != nil {
			t.Fatalf("RankModels: %v", err)
		}
		for _, p := range picks {
			if p.Variant.VariantID == "mtp-q4" {
				t.Errorf("mtp-q4 must be excluded on a 0.24.0 engine")
			}
		}
		if picks[0].Variant.VariantID != "q4" {
			t.Errorf("top pick = %s, want q4", picks[0].Variant.VariantID)
		}
	})

	t.Run("exactly at the floor passes", func(t *testing.T) {
		in := in
		in.EngineVersion = "0.30.0"
		picks, err := RankModels(in)
		if err != nil {
			t.Fatalf("RankModels: %v", err)
		}
		if picks[0].Variant.VariantID != "mtp-q4" {
			t.Errorf("top pick = %s, want mtp-q4 at exact floor", picks[0].Variant.VariantID)
		}
	})

	t.Run("unknown engine version fails closed", func(t *testing.T) {
		picks, err := RankModels(in) // EngineVersion ""
		if err != nil {
			t.Fatalf("RankModels: %v", err)
		}
		for _, p := range picks {
			if p.Variant.VariantID == "mtp-q4" {
				t.Errorf("mtp-q4 must be excluded when the engine version is unknown")
			}
		}
	})

	t.Run("all variants floored and engine too old yields hardware-insufficient", func(t *testing.T) {
		m := mtpFamilyFixture()
		m.Variants = m.Variants[:1] // mtp only
		in := PickInput{Catalog: []catalog.Manifest{m}, Hardware: hw,
			Engine: catalog.RuntimeOllama, EngineVersion: "0.24.0"}
		if _, err := RankModels(in); !errors.Is(err, ErrHardwareInsufficient) {
			t.Errorf("RankModels = %v, want ErrHardwareInsufficient", err)
		}
	})
}

func TestFamilyBestFit_EngineVersionGate(t *testing.T) {
	// An engine IS on this host. Three of the four label arms below are
	// about a host that has one, and the fourth says so explicitly —
	// leaving the profile at its zero value would have every case
	// silently take the no-engine arm (#852).
	hw := hardware.Profile{RAMTotalGB: 64}
	hw.Engines.Ollama = hardware.EngineInfo{Installed: true, Version: "0.24.0"}
	m := mtpFamilyFixture()

	t.Run("old engine falls back to the unfloored variant", func(t *testing.T) {
		got := FamilyBestFit(m, catalog.RuntimeOllama, "0.24.0", hw)
		if !got.Fits || got.Variant.VariantID != "q4" {
			t.Errorf("FamilyBestFit = %+v, want fit on q4", got)
		}
	})

	t.Run("whole family floored reports the version deficit", func(t *testing.T) {
		m := m
		m.Variants = m.Variants[:1] // mtp only
		got := FamilyBestFit(m, catalog.RuntimeOllama, "0.24.0", hw)
		if got.Fits {
			t.Fatalf("FamilyBestFit = %+v, want no fit", got)
		}
		if want := "needs AI engine 0.30.0 (this computer has 0.24.0)"; got.DeficitLabel != want {
			t.Errorf("DeficitLabel = %q, want %q", got.DeficitLabel, want)
		}
	})

	t.Run("unknown version says so rather than naming a version", func(t *testing.T) {
		m := m
		m.Variants = m.Variants[:1]
		got := FamilyBestFit(m, catalog.RuntimeOllama, "", hw)
		want := "needs AI engine 0.30.0 (this computer's version could not be read)"
		if got.DeficitLabel != want {
			t.Errorf("DeficitLabel = %q, want %q", got.DeficitLabel, want)
		}
	})

	// Observed on pc-dell-premium (#852): the version was unreadable
	// because there was no engine, and the row said "could not be read"
	// under a header that said there was no AI engine on the computer.
	// Both empties, one cause, two different true sentences.
	t.Run("no engine at all says that, not that the version is unreadable", func(t *testing.T) {
		m := m
		m.Variants = m.Variants[:1]
		noEngine := hardware.Profile{RAMTotalGB: 64}
		got := FamilyBestFit(m, catalog.RuntimeOllama, "", noEngine)
		want := "needs AI engine 0.30.0 (no AI engine on this computer)"
		if got.DeficitLabel != want {
			t.Errorf("DeficitLabel = %q, want %q", got.DeficitLabel, want)
		}
		// The verdict is unchanged: the floor is still why THIS row is
		// out while the unfloored ones are not, so the code and the
		// versions stay what they were.
		if got.Fit.Reason != hostfit.ReasonEngineTooOld {
			t.Errorf("Reason = %q, want %q", got.Fit.Reason, hostfit.ReasonEngineTooOld)
		}
		if got.Fit.NeedEngineVersion != "0.30.0" || got.Fit.HaveEngineVersion != "" {
			t.Errorf("need/have = %q/%q, want 0.30.0/\"\"",
				got.Fit.NeedEngineVersion, got.Fit.HaveEngineVersion)
		}
	})

	// An engine kind this rule does not model must not gain an assertion
	// about absence that nothing checked; it keeps the version wording.
	t.Run("an unmodelled engine keeps the version wording", func(t *testing.T) {
		m := m
		m.Variants = m.Variants[:1]
		m.Variants[0].RuntimeSupport = []string{"lan-gpu"}
		m.Variants[0].MinEngineVersion = "0.30.0"
		got := FamilyBestFit(m, "lan-gpu", "0.24.0", hardware.Profile{RAMTotalGB: 64})
		if want := "needs AI engine 0.30.0 (this computer has 0.24.0)"; got.DeficitLabel != want {
			t.Errorf("DeficitLabel = %q, want %q", got.DeficitLabel, want)
		}
	})

	// The machine-readable half (#836). It used to be a zero-value Fit,
	// which every surface read as "unfit, cause unknown" and then guessed
	// a cause for — #850 is that guess reaching a user on a 63 GB host.
	t.Run("the verdict carries a code rather than a zero value", func(t *testing.T) {
		m := mtpFamilyFixture()
		m.Variants = m.Variants[:1]
		// The size class is derived from the weight annotation, so the
		// fixture has to carry one for "the row keeps its size class" to
		// be an assertion rather than a tautology on two empty strings.
		m.Variants[0].EstimatedWeightGB = 18.0
		wantSize := hostfit.ModelSize(m)
		if wantSize == "" {
			t.Fatalf("fixture has no size class; the assertion below would pass on nothing")
		}
		for _, have := range []string{"0.24.0", ""} {
			got := FamilyBestFit(m, catalog.RuntimeOllama, have, hw)
			fit := got.Fit
			if fit.Reason != hostfit.ReasonEngineTooOld {
				t.Errorf("have %q: Fit.Reason = %q, want %q",
					have, fit.Reason, hostfit.ReasonEngineTooOld)
			}
			if fit.Runnable != got.Fits {
				t.Errorf("have %q: Fit.Runnable = %v but Fits = %v — the two answers "+
					"may never disagree", have, fit.Runnable, got.Fits)
			}
			if fit.NeedEngineVersion != "0.30.0" || fit.HaveEngineVersion != have {
				t.Errorf("have %q: need/have = %q/%q, want %q/%q",
					have, fit.NeedEngineVersion, fit.HaveEngineVersion, "0.30.0", have)
			}
			// The wall is the engine, so no memory figure may ride along:
			// a surface that renders NeedMB here sends the operator to buy
			// hardware that changes nothing.
			if fit.NeedMB != 0 || fit.HaveMB != 0 || fit.RequiredResidentMB != 0 {
				t.Errorf("have %q: Fit carries memory figures (%d/%d, resident %d) "+
					"beside a refusal the memory has nothing to do with",
					have, fit.NeedMB, fit.HaveMB, fit.RequiredResidentMB)
			}
			// Ranking data rides along so the row keeps its place, the way
			// NoVariantForEngineModel does.
			if fit.QualityTier != got.Variant.QualityTier || fit.QualityTier == 0 {
				t.Errorf("have %q: Fit.QualityTier = %d, want the representative "+
					"variant's %d", have, fit.QualityTier, got.Variant.QualityTier)
			}
			if fit.ModelSize != wantSize {
				t.Errorf("have %q: Fit.ModelSize = %q, want %q — the row loses its size class",
					have, fit.ModelSize, wantSize)
			}
		}
	})

	// The engine's internal name is not a user-facing word (#836, found
	// on a real host by #850). Asserted on the LABEL rather than on the
	// format string so it keeps holding if the wording is rewritten.
	t.Run("the label never names the engine", func(t *testing.T) {
		for _, engine := range []string{catalog.RuntimeOllama, catalog.RuntimeVLLM} {
			m := mtpFamilyFixture()
			m.Variants = m.Variants[:1]
			m.Variants[0].RuntimeSupport = []string{engine}
			for _, have := range []string{"0.24.0", ""} {
				got := FamilyBestFit(m, engine, have, hw)
				if got.Fits {
					t.Fatalf("engine %s, have %q: FamilyBestFit = %+v, want no fit", engine, have, got)
				}
				if strings.Contains(got.DeficitLabel, engine) {
					t.Errorf("engine %s, have %q: DeficitLabel = %q names the engine; "+
						"user-facing copy says \"AI engine\"", engine, have, got.DeficitLabel)
				}
			}
		}
	})
}

func TestFirstPullableVariant(t *testing.T) {
	m := mtpFamilyFixture()

	if v, ok := FirstPullableVariant(m, catalog.RuntimeOllama, "0.30.7"); !ok || v.VariantID != "mtp-q4" {
		t.Errorf("new engine: got (%v, %v), want mtp-q4", v.VariantID, ok)
	}
	if v, ok := FirstPullableVariant(m, catalog.RuntimeOllama, "0.24.0"); !ok || v.VariantID != "q4" {
		t.Errorf("old engine: got (%v, %v), want q4 (skip the floored mtp)", v.VariantID, ok)
	}
	if v, ok := FirstPullableVariant(m, catalog.RuntimeOllama, ""); !ok || v.VariantID != "q4" {
		t.Errorf("unknown version: got (%v, %v), want q4", v.VariantID, ok)
	}

	mtpOnly := m
	mtpOnly.Variants = m.Variants[:1]
	if _, ok := FirstPullableVariant(mtpOnly, catalog.RuntimeOllama, "0.24.0"); ok {
		t.Error("mtp-only family on an old engine must yield ok=false")
	}
}
