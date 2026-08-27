package router

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// hostFamilyFixture returns synthetic manifests covering the family
// shapes the catalog endpoint cares about: ollama-only, vllm-only,
// dual-engine with multiple tiers.
func hostFamilyFixture() (ollamaOnly, vllmOnly, dual catalog.Manifest) {
	ollamaOnly = catalog.Manifest{
		ModelID:     "qwen3-4b-instruct",
		DisplayName: "Qwen3 4B Instruct",
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       8, QualityTier: 35,
			Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:4b-q4"},
		}},
	}
	vllmOnly = catalog.Manifest{
		ModelID:     "qwen3-32b-instruct",
		DisplayName: "Qwen3 32B Instruct",
		Variants: []catalog.Variant{{
			VariantID: "awq-int4", Format: catalog.FormatSafetensors,
			Quantization:   "AWQ-int4",
			RuntimeSupport: []string{catalog.RuntimeVLLM},
			MinVRAMMB:      24576, QualityTier: 80,
			Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-32B-AWQ"},
		}},
	}
	dual = catalog.Manifest{
		ModelID:     "qwen3-8b-instruct",
		DisplayName: "Qwen3 8B Instruct",
		Variants: []catalog.Variant{
			{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       12, QualityTier: 50,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:8b-q4"},
			},
			{
				VariantID: "awq-int4", Format: catalog.FormatSafetensors,
				Quantization:   "AWQ-int4",
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      8000, QualityTier: 60,
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-8B-AWQ"},
			},
			{
				VariantID: "fp16", Format: catalog.FormatSafetensors,
				DType:          "float16",
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      18000, QualityTier: 65,
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-8B"},
			},
		},
	}
	return
}

func TestFamilyBestFit_OllamaFits(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 32}
	got := FamilyBestFit(o, catalog.RuntimeOllama, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	if got.Variant.VariantID != "q4-gguf" {
		t.Errorf("variant: want q4-gguf, got %q", got.Variant.VariantID)
	}
	if got.DeficitLabel != "" {
		t.Errorf("deficit should be empty, got %q", got.DeficitLabel)
	}
}

func TestFamilyBestFit_OllamaShortRAM(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 4}
	got := FamilyBestFit(o, catalog.RuntimeOllama, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	if got.DeficitLabel == "" {
		t.Errorf("expected deficit label, got empty")
	}
	want := "needs 8 GB RAM (have 4 GB)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
	// Even when no variant fits, the verdict carries the representative
	// variant so the catalog UI can still show recommended specs.
	if got.Variant.VariantID != "q4-gguf" {
		t.Errorf("no-fit representative variant: want q4-gguf, got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_VLLMPickHighestTier(t *testing.T) {
	_, _, d := hostFamilyFixture()
	// 24 GB host: both vllm variants fit (8000 MB and 18000 MB);
	// expect fp16 (tier=65) over awq-int4 (tier=60).
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24576}},
	}
	got := FamilyBestFit(d, catalog.RuntimeVLLM, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	if got.Variant.VariantID != "fp16" {
		t.Errorf("variant: want fp16 (highest tier), got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_VLLMPickLowerTierWhenFP16Doesntfit(t *testing.T) {
	_, _, d := hostFamilyFixture()
	// 12 GB host: fp16 (18000 MB) doesn't fit, awq (8000 MB) does.
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 12288}},
	}
	got := FamilyBestFit(d, catalog.RuntimeVLLM, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	if got.Variant.VariantID != "awq-int4" {
		t.Errorf("variant: want awq-int4 (only fitting variant), got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_VLLMShortVRAM(t *testing.T) {
	_, v, _ := hostFamilyFixture()
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 8192}},
	}
	got := FamilyBestFit(v, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	want := "needs 24 GB VRAM (have 8 GB)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
	if got.Variant.VariantID != "awq-int4" {
		t.Errorf("no-fit representative variant: want awq-int4, got %q", got.Variant.VariantID)
	}
}

// #678: FamilyBestFit budgets the TP aggregate on identical
// multi-NVIDIA hosts, both for the fit verdict and the deficit label.
func TestFamilyBestFit_VLLMMultiGPUAggregate(t *testing.T) {
	_, v, _ := hostFamilyFixture() // awq-int4 needs 24576 MB
	gpu := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA RTX 4080", VRAMTotalMB: 16384}

	single := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu}}
	if got := FamilyBestFit(v, catalog.RuntimeVLLM, "", single); got.Fits {
		t.Fatalf("24576 MB variant must not fit a single 16 GB GPU, got %+v", got)
	}

	// 2×16 GB: budget 2×(16384−1024) = 30720 MB ≥ 24576.
	dual := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu, gpu}}
	if got := FamilyBestFit(v, catalog.RuntimeVLLM, "", dual); !got.Fits {
		t.Errorf("24576 MB variant should fit 2×16 GB via the TP=2 budget, got deficit %q", got.DeficitLabel)
	}
}

func TestFamilyBestFit_VLLMDeficitLabelAggregated(t *testing.T) {
	_, v, _ := hostFamilyFixture() // awq-int4 needs 24576 MB
	gpu := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3070", VRAMTotalMB: 8192}
	// 2×8 GB: budget 2×(8192−1024) = 14336 MB = 14 GB — still short.
	hw := hardware.Profile{RAMTotalGB: 32, GPUs: []hardware.GPU{gpu, gpu}}
	got := FamilyBestFit(v, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	want := "needs 24 GB VRAM (have 14 GB across 2 GPUs)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
}

func TestFamilyBestFit_VLLMNoGPU(t *testing.T) {
	_, v, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 32}
	got := FamilyBestFit(v, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	want := "needs 24 GB VRAM (no GPU)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
}

func TestFamilyBestFit_EngineNotSupportedByFamily(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	// Asking for vllm on an ollama-only manifest.
	hw := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 40960}}}
	got := FamilyBestFit(o, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit (engine mismatch), got %+v", got)
	}
	if got.DeficitLabel != "no vLLM variant" {
		t.Errorf("deficit: want %q, got %q", "no vLLM variant", got.DeficitLabel)
	}
	// No engine-supported variant exists, so there is no representative
	// variant to recommend.
	if got.Variant.VariantID != "" {
		t.Errorf("representative variant should be empty when engine unsupported, got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_OllamaUnknownRAMTreatedAsFit(t *testing.T) {
	// hostFits intentionally treats RAMTotalGB == 0 as "skip the
	// fit check rather than reject all" — verify FamilyBestFit
	// inherits that lenience instead of producing a misleading
	// "needs N GB RAM (have 0 GB)" deficit.
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 0}
	got := FamilyBestFit(o, catalog.RuntimeOllama, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit when RAM detection unavailable, got %+v", got)
	}
}

// gbFigure matches a whole-GB quantity as the deficit labels write one.
// "2 GPUs" and other trailing counts are deliberately not matched: the
// contract below is about the MEMORY figures, and a device count is not
// one of them.
var gbFigure = regexp.MustCompile(`(\d+) GB`)

// assertDeficitLabelQuotesVerdict is the #625 contract, in one place
// because two tests assert it — this file's table over synthetic hosts,
// and the Apple Silicon real-host test over an actual probe.
//
// Both directions matter, and the second is the one that was broken:
//
//   - every figure the verdict decided on appears in the label, and
//   - every figure in the label is one the verdict decided on.
//
// The old UMA label satisfied neither. It read "needs ~7 GB
// GPU-resident (have 12288 MB VRAM)" beside need_mb 10455 / have_mb
// 6144 — two numbers, both absent from the decision they were
// explaining, and arranged so that 7 < 12 read as a pass on a row that
// says it does not fit.
//
// Skipped where the verdict priced nothing (an unannotated weight, a
// failed RAM probe, the engine-version floor): there is no figure to
// agree with, and the label says so in words.
func assertDeficitLabelQuotesVerdict(t *testing.T, engine, modelID string, fit FamilyFit) {
	t.Helper()
	if fit.Fits || engine != catalog.RuntimeOllama {
		return
	}
	if fit.Fit.NeedMB <= 0 || fit.Fit.HaveMB <= 0 {
		return
	}
	need, have := mbToGBCeil(fit.Fit.NeedMB), mbToGBCeil(fit.Fit.HaveMB)
	for _, want := range []int{need, have} {
		if !strings.Contains(fit.DeficitLabel, strconv.Itoa(want)+" GB") {
			t.Errorf("%s: label %q omits %d GB, which the verdict decided on (need_mb=%d have_mb=%d)",
				modelID, fit.DeficitLabel, want, fit.Fit.NeedMB, fit.Fit.HaveMB)
		}
	}
	for _, m := range gbFigure.FindAllStringSubmatch(fit.DeficitLabel, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n != need && n != have {
			t.Errorf("%s: label %q quotes %d GB, which the verdict never compared (need=%d GB have=%d GB)",
				modelID, fit.DeficitLabel, n, need, have)
		}
	}
}

// Product contract (waired-agent#625): the deficit label and the verdict
// beside it are the same decision, so the label may only quote figures
// the verdict compared.
//
// They used to be two expressions in one return statement, computed from
// different inputs, and they drifted the moment the inputs stopped
// agreeing — capacity became a total-memory computation (#497) and the
// OS deduction became a measurement (#568), while the label went on
// describing GPU residency against the raw RAM total.
//
// The table walks the shapes where the two computations can disagree,
// which is every host class the capacity gate treats differently.
func TestFamilyBestFit_DeficitLabelQuotesTheVerdict(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	weighted := catalog.Manifest{
		ModelID:       "qwen3.5-weighted",
		DisplayName:   "Weighted",
		ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport:      []string{catalog.RuntimeOllama},
			MinRAMGB:            12,
			EstimatedWeightGB:   6.6,
			KVBytesPerTokenFP16: 32768,
			ParamCount:          9_000_000_000,
			QualityTier:         52,
			Source:              catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3.5:9b-q4"},
		}},
	}
	for _, tc := range []struct {
		name string
		m    catalog.Manifest
		hw   hardware.Profile
	}{
		// The #625 host, as measured: 16 GB unified, 6 GB available at
		// install, so the gate compares against 6144 MB while the raw
		// figure the old label reached for was 12288.
		{"unified with a measured deduction", weighted, hardware.Profile{
			RAMTotalGB: 16, RAMAvailableAtInstallGB: 6,
			UnifiedMemory: true, UsableVRAMMB: 12288,
		}},
		// Apple Silicon reports NO discrete device, which is the shape
		// the fixtures elsewhere in this repo get wrong (#662): a
		// synthetic GPU entry with VRAMTotalMB set is a machine the
		// darwin detector never produces.
		{"unified, no discrete device enumerated", weighted, hardware.Profile{
			RAMTotalGB: 16, RAMAvailableAtInstallGB: 6,
			UnifiedMemory: true, UsableVRAMMB: 12288,
		}},
		// Discrete, rejected: the old label reached for RAMTotalGB here,
		// which is the figure the verdict does NOT use — the deduction
		// comes off it first and the card's memory is added after.
		{"discrete with a measured deduction", weighted, hardware.Profile{
			RAMTotalGB: 8, RAMAvailableAtInstallGB: 4,
			GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 4096, Model: "RTX 3050"}},
		}},
		{"cpu-only", weighted, hardware.Profile{RAMTotalGB: 8, RAMAvailableAtInstallGB: 6}},
		// Pooled: naming one card after judging the host on two is the
		// error #264 fixed, and the pooled budget is what "have" already
		// carries — so the label inherits the fix instead of repeating it.
		{"pooled discrete", weighted, hardware.Profile{
			RAMTotalGB: 8, RAMAvailableAtInstallGB: 4,
			GPUs: []hardware.GPU{
				{Vendor: "nvidia", VRAMTotalMB: 2048, Model: "A"},
				{Vendor: "nvidia", VRAMTotalMB: 2048, Model: "B"},
			},
		}},
		// The same Windows host as the rc8 run, where the model DOES
		// fit: a fitting row carries no label at all, and the contract
		// has to hold in that direction too.
		{"discrete, fits", weighted, hardware.Profile{
			RAMTotalGB: 32, RAMAvailableAtInstallGB: 16,
			GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 8188, Model: "RTX 4070 Laptop"}},
		}},
		// No weight annotation: the verdict falls back to min_ram_gb and
		// the label has to follow it there rather than invent a figure.
		{"unannotated weight", o, hardware.Profile{RAMTotalGB: 4}},
		{"no RAM reading at all", weighted, hardware.Profile{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FamilyBestFit(tc.m, catalog.RuntimeOllama, "", tc.hw)
			t.Logf("fits=%v label=%q reason=%q need=%d have=%d",
				got.Fits, got.DeficitLabel, got.Fit.Reason, got.Fit.NeedMB, got.Fit.HaveMB)
			if !got.Fits && got.DeficitLabel == "" {
				t.Fatalf("rejected with no label at all: %+v", got)
			}
			assertDeficitLabelQuotesVerdict(t, catalog.RuntimeOllama, tc.m.ModelID, got)
		})
	}
}

// Product contract (waired-agent#321): Fit.Runnable and Fits are the same
// answer, on every shape the catalog endpoint can produce. They are two
// fields because two generations of consumer read them; a renderer that
// greyed one while the other said "fits" would be the split-brain this
// projection exists to end.
func TestFamilyBestFit_FitAndFitsNeverDisagree(t *testing.T) {
	o, v, d := hostFamilyFixture()
	gpu := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 40960}}}
	for _, tc := range []struct {
		name   string
		m      catalog.Manifest
		engine string
		hw     hardware.Profile
	}{
		{"ollama fits", o, catalog.RuntimeOllama, hardware.Profile{RAMTotalGB: 32}},
		{"ollama short on RAM", o, catalog.RuntimeOllama, hardware.Profile{RAMTotalGB: 4}},
		{"vllm fits", v, catalog.RuntimeVLLM, gpu},
		{"vllm short on VRAM", v, catalog.RuntimeVLLM,
			hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 8192}}}},
		{"vllm with no GPU", v, catalog.RuntimeVLLM, hardware.Profile{RAMTotalGB: 64}},
		{"engine the family has no variant for", o, catalog.RuntimeVLLM, gpu},
		{"dual-engine family", d, catalog.RuntimeVLLM, gpu},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FamilyBestFit(tc.m, tc.engine, "", tc.hw)
			if got.Fit.Runnable != got.Fits {
				t.Errorf("Fit.Runnable = %v but Fits = %v (%+v)", got.Fit.Runnable, got.Fits, got)
			}
		})
	}
}

// Product contract: a family this engine cannot serve carries the machine
// code AND its quality tier. The tier is what lets a picker put the row in
// its place at the bottom of the list rather than dropping it — the F36
// half of #321, where the setup wizard hid such models and the tray greyed
// them.
func TestFamilyBestFit_NoVariantForEngineIsRenderable(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 40960}}}
	got := FamilyBestFit(o, catalog.RuntimeVLLM, "", hw)
	if got.Fit.Reason != hostfit.ReasonNoVariantForEngine {
		t.Errorf("Fit.Reason = %q, want %q", got.Fit.Reason, hostfit.ReasonNoVariantForEngine)
	}
	if got.Fit.QualityTier != 35 {
		t.Errorf("Fit.QualityTier = %d, want 35 — the tier ranks the model, not its fit",
			got.Fit.QualityTier)
	}
}

// Product contract: the size figure a picker prints is the resident
// requirement, and it comes from the shared rule rather than from
// min_ram_gb. Asserted against hostfit directly so a change to the
// overhead model moves both together or fails here.
func TestFamilyBestFit_CarriesTheResidentRequirement(t *testing.T) {
	m := catalog.Manifest{
		ModelID: "qwen3.6-27b",
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB: 32, QualityTier: 62, EstimatedWeightGB: 16.0, KVBytesPerTokenFP16: 20480,
		}},
	}
	hw := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 40960}}}
	got := FamilyBestFit(m, catalog.RuntimeOllama, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	want := hostfit.OllamaResidentMB(m.Variants[0], false)
	if got.Fit.RequiredResidentMB != want {
		t.Errorf("RequiredResidentMB = %d, want %d", got.Fit.RequiredResidentMB, want)
	}
	if got.Fit.QualityTier != 62 {
		t.Errorf("QualityTier = %d, want 62", got.Fit.QualityTier)
	}
}

// Product contract: the badge and the installer name the same model. This
// is the whole reason RecommendedFamily wraps SelectInstallModel instead
// of ranking again — a second policy is how the fit rules came to
// disagree in the first place (waired-ai/waired#942).
func TestRecommendedFamily_IsSelectInstallModelsAnswer(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	in := PickInput{
		Catalog:  manifests,
		Hardware: hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24564}}},
		Engine:   catalog.RuntimeOllama,
	}
	above, ok, err := SelectInstallModel(in)
	if err != nil || !ok || len(above) == 0 {
		t.Fatalf("fixture host must have an install pick: ok=%v err=%v", ok, err)
	}
	if got := RecommendedFamily(in); got != above[0].Manifest.ModelID {
		t.Errorf("RecommendedFamily() = %q, want SelectInstallModel's %q", got, above[0].Manifest.ModelID)
	}
}

// A record of today's behaviour, not a contract: with no engine there is
// nothing to rank against, and the caller gets no mark rather than an
// arbitrary one.
func TestRecommendedFamily_EmptyWithoutAnEngine(t *testing.T) {
	if got := RecommendedFamily(PickInput{Hardware: hardware.Profile{RAMTotalGB: 64}}); got != "" {
		t.Errorf("RecommendedFamily(no engine) = %q, want empty", got)
	}
}

// TestFamilyBestFit_VLLMWindowClampDemotesTheRow pins the agent half of
// waired-agent#1061: the family row a surface renders carries the verdict
// about THIS host, not only about the manifest.
//
// PRODUCT CONTRACT (waired-agent#1061). Before, hostfit.VLLMRecommendModel
// could not see the device list, so a model whose native window clears the
// coding target was offered with a clean row on a card that would clamp it
// — while the tuning warning on the same machine said the window had been
// clamped to well under the target.
func TestFamilyBestFit_VLLMWindowClampDemotesTheRow(t *testing.T) {
	m := catalog.Manifest{
		ModelID:       "wide-window",
		ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID: "awq-int4", RuntimeSupport: []string{catalog.RuntimeVLLM},
			QualityTier: 80, MinVRAMMB: 20480,
			// 14 GB weights x1.15 + 73728 B/tok: ~64k tokens on one 24 GB
			// card at the default utilization, ~346k across two.
			EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 73728,
		}},
	}
	card := hardware.GPU{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24576}

	one := FamilyBestFit(m, catalog.RuntimeVLLM, "", hardware.Profile{
		RAMTotalGB: 64, GPUs: []hardware.GPU{card},
	})
	if !one.Fits {
		t.Fatalf("one card: got %+v, want a fitting row — capacity is not what this measures", one)
	}
	if !one.Fit.NotRecommended || one.Fit.NotRecommendedReason != hostfit.ReasonWindowExceedsMemory {
		t.Fatalf("one card: got NotRecommended=%v reason=%q, want a window_exceeds_memory annotation",
			one.Fit.NotRecommended, one.Fit.NotRecommendedReason)
	}

	two := FamilyBestFit(m, catalog.RuntimeVLLM, "", hardware.Profile{
		RAMTotalGB: 64, GPUs: []hardware.GPU{card, card},
	})
	if two.Fit.NotRecommended {
		t.Fatalf("two cards (TP=2): got reason=%q, want no annotation — the window fits sharded",
			two.Fit.NotRecommendedReason)
	}
}
