package router

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
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
	hw := hardware.Profile{RAMTotalGB: 64}
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
		if want := "needs ollama ≥ 0.30.0 (running 0.24.0)"; got.DeficitLabel != want {
			t.Errorf("DeficitLabel = %q, want %q", got.DeficitLabel, want)
		}
	})

	t.Run("unknown version reports unknown in the deficit", func(t *testing.T) {
		m := m
		m.Variants = m.Variants[:1]
		got := FamilyBestFit(m, catalog.RuntimeOllama, "", hw)
		if want := "needs ollama ≥ 0.30.0 (running unknown version)"; got.DeficitLabel != want {
			t.Errorf("DeficitLabel = %q, want %q", got.DeficitLabel, want)
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
