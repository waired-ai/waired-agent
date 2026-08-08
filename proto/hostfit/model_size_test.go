package hostfit_test

import (
	"math"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestVariantSize_BoundaryIsTheReferenceCard pins that the class flips
// exactly where OllamaWeightsResidentMB crosses the reference card, and
// that the boundary is inclusive.
//
// The weights are hand-solved against the overhead model
// (ceil(GB·1e9/MiB) + 1024 + 40·GB, hostfit.go), and the resident figure
// each produces is asserted alongside the class on purpose: if the
// overhead calibration moves, this fails with the new figure in the
// message rather than silently reclassifying half the catalog.
func TestVariantSize_BoundaryIsTheReferenceCard(t *testing.T) {
	cases := []struct {
		name     string
		weightGB float64
		wantMB   int
		want     string
	}{
		{"just inside the 8 GB card", 7.21, 8188, hostfit.ModelSizeSmall},
		// Exactly on the line stays in the smaller class: a model that
		// fills the card is a model the card runs.
		{"exactly the 8 GB card", 7.21316, 8192, hostfit.ModelSizeSmall},
		{"just over the 8 GB card", 7.22, 8198, hostfit.ModelSizeMedium},
		{"just inside the 32 GB card", 31.94, 32762, hostfit.ModelSizeMedium},
		{"exactly the 32 GB card", 31.94592, 32768, hostfit.ModelSizeMedium},
		{"just over the 32 GB card", 31.95, 32772, hostfit.ModelSizeLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := catalog.Variant{EstimatedWeightGB: tc.weightGB}
			if got := hostfit.OllamaWeightsResidentMB(v, false); got != tc.wantMB {
				t.Fatalf("OllamaWeightsResidentMB(%g GB) = %d MiB, want %d — the overhead "+
					"model moved, so re-solve the boundary weights before touching the class",
					tc.weightGB, got, tc.wantMB)
			}
			if got := hostfit.VariantSize(v); got != tc.want {
				t.Errorf("VariantSize(%g GB = %d MiB) = %q, want %q (lines: %d / %d MiB)",
					tc.weightGB, tc.wantMB, got, tc.want,
					hostfit.ModelSizeSmallCardMB, hostfit.ModelSizeMediumCardMB)
			}
		})
	}
}

// TestVariantSize_UnannotatedIsUnknownNotSmall guards the direction that
// costs something. A variant with no weight annotation prices at 0, and
// reading 0 as "fits anything" would let an unpriceable model through
// every floor — the mirror of the bug waired-agent#364 was, where a zero
// estimate was read as a confirmed verdict.
func TestVariantSize_UnannotatedIsUnknownNotSmall(t *testing.T) {
	if got := hostfit.VariantSize(catalog.Variant{VariantID: "no-weight"}); got != "" {
		t.Errorf("VariantSize(unannotated) = %q, want %q", got, "")
	}
	if got := hostfit.SizeRank(""); got != 0 {
		t.Errorf("SizeRank(unknown) = %d, want 0 so a floor excludes it", got)
	}
}

// TestSizeRank_OrdersTheVocabulary pins the comparison consumers use so
// a gate can be written as SizeRank(got) >= SizeRank(floor).
func TestSizeRank_OrdersTheVocabulary(t *testing.T) {
	small := hostfit.SizeRank(hostfit.ModelSizeSmall)
	medium := hostfit.SizeRank(hostfit.ModelSizeMedium)
	large := hostfit.SizeRank(hostfit.ModelSizeLarge)
	if !(0 < small && small < medium && medium < large) {
		t.Errorf("SizeRank: unknown=0 small=%d medium=%d large=%d, want strictly increasing above 0",
			small, medium, large)
	}
	if got := hostfit.SizeRank("enormous"); got != 0 {
		t.Errorf("SizeRank(unrecognised) = %d, want 0", got)
	}
}

// TestModelSize_TakesTheLightestVariant is the family-vs-variant rule: a
// row is a family and the build behind it is ours to pick, so a model
// shipping both a 4-bit and a 16-bit build is reachable on whatever card
// the 4-bit one needs.
func TestModelSize_TakesTheLightestVariant(t *testing.T) {
	m := catalog.Manifest{
		ModelID: "two-builds",
		Variants: []catalog.Variant{
			{VariantID: "fp16", EstimatedWeightGB: 160}, // large
			{VariantID: "q4", EstimatedWeightGB: 18},    // medium
		},
	}
	if got := hostfit.ModelSize(m); got != hostfit.ModelSizeMedium {
		t.Errorf("ModelSize(fp16+q4) = %q, want %q — the lightest build decides the row",
			got, hostfit.ModelSizeMedium)
	}
	// An unannotated variant is skipped rather than winning as "unknown".
	withGap := catalog.Manifest{ModelID: "gap", Variants: []catalog.Variant{
		{VariantID: "no-weight"},
		{VariantID: "q4", EstimatedWeightGB: 18},
	}}
	if got := hostfit.ModelSize(withGap); got != hostfit.ModelSizeMedium {
		t.Errorf("ModelSize(unannotated+q4) = %q, want %q", got, hostfit.ModelSizeMedium)
	}
	if got := hostfit.ModelSize(catalog.Manifest{ModelID: "none"}); got != "" {
		t.Errorf("ModelSize(no variants) = %q, want %q", got, "")
	}
}

// shippedSizes is the class of every model this build ships, as of
// 2026-08-08. It is a record of today's catalog, not a contract: #522
// retires the 2025 generation and will move rows out of it.
//
// It is pinned because the classification is the thing users read now.
// A catalog change that silently moves a model between classes is a
// change to what the product tells people about their hardware, and
// this is where that shows up as a diff.
var shippedSizes = map[string]string{
	"deepseek-v4-flash":                 hostfit.ModelSizeLarge,
	"glm-4.5-air-106b-a12b":             hostfit.ModelSizeLarge,
	"glm-5.2":                           hostfit.ModelSizeLarge,
	"gpt-oss-120b":                      hostfit.ModelSizeLarge,
	"qwen3-coder-480b-a35b-instruct":    hostfit.ModelSizeLarge,
	"qwen3-coder-next-80b-a3b-instruct": hostfit.ModelSizeLarge,
	"qwen3.5-122b-a10b":                 hostfit.ModelSizeLarge,

	"gpt-oss-20b":                  hostfit.ModelSizeMedium,
	"qwen2.5-coder-14b-instruct":   hostfit.ModelSizeMedium,
	"qwen3-coder-30b-a3b-instruct": hostfit.ModelSizeMedium,
	"qwen3.5-27b":                  hostfit.ModelSizeMedium,
	"qwen3.5-35b-a3b":              hostfit.ModelSizeMedium,
	"qwen3.6-27b":                  hostfit.ModelSizeMedium,
	"qwen3.6-35b-a3b":              hostfit.ModelSizeMedium,

	"granite4-350m":             hostfit.ModelSizeSmall,
	"qwen2.5-coder-3b-instruct": hostfit.ModelSizeSmall,
	"qwen2.5-coder-7b-instruct": hostfit.ModelSizeSmall,
	"qwen3.5-0.8b":              hostfit.ModelSizeSmall,
	"qwen3.5-2b":                hostfit.ModelSizeSmall,
	"qwen3.5-4b":                hostfit.ModelSizeSmall,
	"qwen3.5-9b":                hostfit.ModelSizeSmall,
}

func TestModelSize_ShippedCatalog(t *testing.T) {
	ms, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("load bundled catalog: %v", err)
	}
	if len(ms) != len(shippedSizes) {
		t.Errorf("catalog has %d models, table has %d — add or drop the row and check its class",
			len(ms), len(shippedSizes))
	}
	for _, m := range ms {
		want, ok := shippedSizes[m.ModelID]
		if !ok {
			t.Errorf("%s: new model, no class recorded — add it to shippedSizes (got %q)",
				m.ModelID, hostfit.ModelSize(m))
			continue
		}
		if got := hostfit.ModelSize(m); got != want {
			t.Errorf("%s: ModelSize = %q, want %q — a manifest edit moved a model between "+
				"classes, which changes what every picker tells a user", m.ModelID, got, want)
		}
	}
}

// TestModelSize_ShippedCatalogClearsBothLines is why the boundaries are
// defensible rather than merely chosen: no shipped build sits near
// either one. The closest approaches as of 2026-08-08 are 7.4 %
// (qwen3.5-9b, 7,583 MiB under the 8 GB line) and 24.1 %
// (qwen3.5-35b-a3b, 24,873 MiB under the 32 GB line) — there is nothing
// at all between 24,873 and 48,721 MiB.
//
// A future variant landing inside the margin is not automatically wrong,
// but it is a model whose class turns on the overhead calibration rather
// than on the hardware, and somebody should look at it.
func TestModelSize_ShippedCatalogClearsBothLines(t *testing.T) {
	const margin = 0.05
	ms, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("load bundled catalog: %v", err)
	}
	for _, line := range []int{hostfit.ModelSizeSmallCardMB, hostfit.ModelSizeMediumCardMB} {
		for _, m := range ms {
			for _, v := range m.Variants {
				mb := hostfit.OllamaWeightsResidentMB(v, false)
				if mb <= 0 {
					continue
				}
				off := math.Abs(float64(mb)-float64(line)) / float64(line)
				if off < margin {
					t.Errorf("%s/%s prices at %d MiB, within %.1f%% of the %d MiB line — its class "+
						"turns on the overhead calibration rather than on the card",
						m.ModelID, v.VariantID, mb, off*100, line)
				}
			}
		}
	}
}

// bestSizeCatalog is a two-model set whose builds straddle the classes,
// so the tests below can tell a family answer from a variant answer.
var bestSizeCatalog = []catalog.Manifest{{
	ModelID: "twobuilds",
	Variants: []catalog.Variant{{
		VariantID: "q4-gguf", RuntimeSupport: []string{catalog.RuntimeOllama},
		EstimatedWeightGB: 4.7, // small
		Source:            catalog.VariantSource{Type: catalog.SourceOllama, Tag: "twobuilds:q4"},
	}, {
		VariantID: "fp16-safetensors", RuntimeSupport: []string{catalog.RuntimeVLLM},
		EstimatedWeightGB: 160, // large
		Source:            catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "org/twobuilds-fp16"},
	}},
}, {
	ModelID: "midsize",
	Variants: []catalog.Variant{{
		VariantID: "q4-gguf", RuntimeSupport: []string{catalog.RuntimeOllama},
		EstimatedWeightGB: 18, // medium
		Source:            catalog.VariantSource{Type: catalog.SourceOllama, Tag: "midsize:q4"},
	}},
}}

func TestBestSizeIn(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		models []string
		want   string
	}{
		{"one match", catalog.RuntimeOllama, []string{"twobuilds:q4"}, hostfit.ModelSizeSmall},
		{"largest of several", catalog.RuntimeOllama,
			[]string{"twobuilds:q4", "midsize:q4"}, hostfit.ModelSizeMedium},
		// The variant, not the family: this peer serves the fp16 build,
		// and answering "small" because the family also ships a q4 would
		// describe a machine that is not the one being asked about.
		{"the served build decides", catalog.RuntimeVLLM,
			[]string{"org/twobuilds-fp16"}, hostfit.ModelSizeLarge},
		// Names are matched per engine, the same convention BestTier uses.
		{"ollama name under vllm resolves nothing", catalog.RuntimeVLLM,
			[]string{"twobuilds:q4"}, ""},
		{"unknown name", catalog.RuntimeOllama, []string{"nothing:here"}, ""},
		{"no models", catalog.RuntimeOllama, nil, ""},
		{"unknown engine", "tensorrt", []string{"twobuilds:q4"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.BestSizeIn(bestSizeCatalog, tc.engine, tc.models); got != tc.want {
				t.Errorf("BestSizeIn(%s, %v) = %q, want %q", tc.engine, tc.models, got, tc.want)
			}
		})
	}
}

// TestBestSizeIn_RespectsRuntimeSupport: a name that matches a variant
// the engine cannot serve is not a match. Without this the fp16 build's
// repo id would answer for an ollama peer.
func TestBestSizeIn_RespectsRuntimeSupport(t *testing.T) {
	ms := []catalog.Manifest{{
		ModelID: "vllmonly",
		Variants: []catalog.Variant{{
			VariantID: "fp16", RuntimeSupport: []string{catalog.RuntimeVLLM},
			EstimatedWeightGB: 160,
			Source:            catalog.VariantSource{Type: catalog.SourceOllama, Tag: "vllmonly:fp16"},
		}},
	}}
	if got := hostfit.BestSizeIn(ms, catalog.RuntimeOllama, []string{"vllmonly:fp16"}); got != "" {
		t.Errorf("BestSizeIn matched a variant this engine cannot serve: got %q, want %q", got, "")
	}
}

// TestBestSize_UsesTheShippedCatalog covers the no-argument form the
// control plane calls, which resolves against the embedded catalog
// including internal models — a device already serving one must stay
// attributable.
func TestBestSize_UsesTheShippedCatalog(t *testing.T) {
	ms, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("load bundled catalog: %v", err)
	}
	var tag, wantSize string
	for _, m := range ms {
		if m.ModelID != "granite4-350m" {
			continue
		}
		for _, v := range m.Variants {
			if v.Source.Tag != "" {
				tag, wantSize = v.Source.Tag, hostfit.VariantSize(v)
				break
			}
		}
	}
	if tag == "" {
		t.Skip("no ollama-tagged internal variant in the shipped catalog")
	}
	if got := hostfit.BestSize(catalog.RuntimeOllama, []string{tag}); got != wantSize {
		t.Errorf("BestSize(%q) = %q, want %q", tag, got, wantSize)
	}
}

// TestProjectModel_CarriesTheSizeClass: the field has to be filled by
// the entry point every in-tree surface uses, or the pickers get an
// empty badge and fall back to nothing.
func TestProjectModel_CarriesTheSizeClass(t *testing.T) {
	m := catalog.Manifest{
		ModelID:       "sized",
		ContextLength: 262144,
		Variants:      []catalog.Variant{presSmall},
	}
	got := hostfit.ProjectModel(m, presSmall, catalog.RuntimeOllama, hostfit.Host{
		RAMTotalGB: 32, GPUCount: 1, VRAM0MB: 24576,
	}, 0)
	if got.ModelSize != hostfit.ModelSizeSmall {
		t.Errorf("ProjectModel().ModelSize = %q, want %q", got.ModelSize, hostfit.ModelSizeSmall)
	}
	if row := hostfit.NoVariantForEngineModel(m, 40); row.ModelSize != hostfit.ModelSizeSmall {
		t.Errorf("NoVariantForEngineModel().ModelSize = %q, want %q — a row that cannot run "+
			"here still tells the reader what the model is", row.ModelSize, hostfit.ModelSizeSmall)
	}
}
