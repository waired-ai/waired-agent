package hostfit_test

import (
	"encoding/json"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The three variants every table below draws from. Annotated the way the
// shipped manifests are, because the projection reads the annotations
// rather than a size class.
var (
	// A 7B-class q4 gguf: fits everything in the fixtures.
	presSmall = catalog.Variant{
		VariantID: "q4-gguf", RuntimeSupport: []string{"ollama"}, QualityTier: 40,
		MinRAMGB: 8, EstimatedWeightGB: 4.7, KVBytesPerTokenFP16: 28672,
	}
	// The waired-ai/waired#986 model: a 35B mixture of experts with 3.3B
	// active. Runs on a 16 GB card by spilling two thirds of its weights,
	// and the roofline cannot see that because only the ACTIVE weights are
	// read per token. This is the pair (runnable, not recommended) the
	// whole projection has to be able to express.
	presMoE = catalog.Variant{
		VariantID: "mtp-q4-gguf", RuntimeSupport: []string{"ollama"}, QualityTier: 65,
		MinRAMGB: 32, EstimatedWeightGB: 22.6, KVBytesPerTokenFP16: 20480,
		ParamCount: 35_000_000_000, ActiveParams: 3_300_000_000,
	}
	// A dense 62B: every byte of it is read per token, so it is the one
	// the speed term genuinely excludes.
	presDense = catalog.Variant{
		VariantID: "mxfp4-gguf", RuntimeSupport: []string{"ollama"}, QualityTier: 70,
		MinRAMGB: 96, EstimatedWeightGB: 62.0, KVBytesPerTokenFP16: 98304,
	}
	// vLLM-only, the shape that has no ollama row at all.
	presVLLM = catalog.Variant{
		VariantID: "awq", RuntimeSupport: []string{"vllm"}, QualityTier: 80,
		MinVRAMMB: 40960,
	}
)

// TestSpeedCode is a PRODUCT CONTRACT: the empty string is "no claim",
// and it is what a no-claim Estimate must produce. Reading absence as a
// positive answer in either direction is the class of defect
// waired-agent#364 was — there, the zero Estimate's false MeetsSpeedFloor
// was read as a confirmed-slow verdict on every vLLM row of an H100.
func TestSpeedCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    hostfit.Estimate
		want string
	}{
		{
			"the no-claim estimate makes no claim",
			hostfit.Estimate{MeetsSpeedFloor: true},
			"",
		},
		{
			"a bounded estimate below the floor is a fact about the computer",
			hostfit.Estimate{TokpsEstimate: 3, UpperBound: true},
			hostfit.SpeedSlow,
		},
		{
			"an unbounded estimate below the floor may only annotate",
			hostfit.Estimate{TokpsEstimate: 3},
			hostfit.SpeedMayBeSlow,
		},
		{
			// The zero value reaches here only from a producer that has not
			// been taught to spell "no claim". It must NOT collapse to "" —
			// that would hide the producer bug rather than surface it.
			"the zero value is not silently treated as no claim",
			hostfit.Estimate{},
			hostfit.SpeedMayBeSlow,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.SpeedCode(tc.e); got != tc.want {
				t.Errorf("SpeedCode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProjectOllama(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    catalog.Variant
		host hostfit.Host
		want hostfit.Presentation
	}{
		{
			// The ordinary answer: it runs, it is the right choice, and the
			// size figure is the one a picker can print beside the name.
			"a small model on a 24 GB card runs and is recommended",
			presSmall, hostFromWire(t, wireRTX4090),
			hostfit.Presentation{
				Runnable:           true,
				QualityTier:        40,
				RequiredResidentMB: hostfit.OllamaResidentMB(presSmall, false),
				WeightsResidentMB:  hostfit.OllamaWeightsResidentMB(presSmall, false),
			},
		},
		{
			// waired-ai/waired#986 verbatim. Runnable stays TRUE — narrowing
			// it here is what waired-agent#229 removed — and the demotion
			// travels beside it. Speed is empty because the roofline really
			// does clear the floor for a mixture of experts: the
			// recommendation gate is the only term that catches this one,
			// which is why the two are separate fields.
			//
			// The tok/s figure still travels: an empty Speed is "no concern
			// to report", not "no data", and ~81 tok/s is exactly the number
			// that made this model look like the right default.
			"the 35B MoE on a 16 GB card runs but is not recommended",
			presMoE, hostFromWire(t, wireRTX5080_16),
			hostfit.Presentation{
				Runnable:             true,
				QualityTier:          65,
				RequiredResidentMB:   hostfit.OllamaResidentMB(presMoE, false),
				WeightsResidentMB:    hostfit.OllamaWeightsResidentMB(presMoE, false),
				NotRecommended:       true,
				NotRecommendedReason: hostfit.ReasonWeightsSpill,
				EstimatedTokps:       hostfit.EstimateOllamaDecode(presMoE, hostFromWire(t, wireRTX5080_16)).TokpsEstimate,
			},
		},
		{
			// Dense, spilled, and the bound is structural (the card's own
			// reads are priced at zero), so this is a claim about the
			// computer rather than about a constant.
			"a dense 62B spilling off a 24 GB card is confirmed slow",
			presDense, hostFromWire(t, wireBigRAMSmallGPU),
			hostfit.Presentation{
				Runnable:             true,
				QualityTier:          70,
				RequiredResidentMB:   hostfit.OllamaResidentMB(presDense, false),
				WeightsResidentMB:    hostfit.OllamaWeightsResidentMB(presDense, false),
				NotRecommended:       true,
				NotRecommendedReason: hostfit.ReasonWeightsSpill,
				Speed:                hostfit.SpeedSlow,
				EstimatedTokps:       hostfit.EstimateOllamaDecode(presDense, hostFromWire(t, wireBigRAMSmallGPU)).TokpsEstimate,
			},
		},
		{
			// No GPU-addressable memory: the required-resident figure is not
			// unknown, it is meaningless, and a surface must print the RAM
			// threshold instead of calling this one "graphics memory".
			// Nothing is demoted either — there is no VRAM term to demote
			// against, and the roofline here rests on a constant with no
			// margin behind it, so it may annotate and never exclude.
			"a CPU-only host reports no resident requirement and no demotion",
			presDense, hostFromWire(t, wireCPUOnly),
			hostfit.Presentation{
				Runnable:          true,
				QualityTier:       70,
				WeightsResidentMB: hostfit.OllamaWeightsResidentMB(presDense, false),
				Speed:             hostfit.SpeedMayBeSlow,
				EstimatedTokps:    hostfit.EstimateOllamaDecode(presDense, hostFromWire(t, wireCPUOnly)).TokpsEstimate,
			},
		},
		{
			// Unified memory: residency IS the capacity rule, so this one
			// does not run at all. The shortfall figures are the ones the
			// gate compared: the window-inclusive requirement against the
			// machine's total memory, not a residency comparison against
			// the wired limit (waired-ai/waired#1056 decision 1).
			"a 16 GB Mac cannot hold the dense 62B",
			presDense, hostFromWire(t, wireMac16),
			hostfit.Presentation{
				Runnable:           false,
				Reason:             hostfit.ReasonInsufficientMemory,
				NeedMB:             hostfit.OllamaWindowResidentMB(presDense, hostfit.ServingWindow200k, true),
				HaveMB:             hostFromWire(t, wireMac16).TotalMemoryMB(),
				QualityTier:        70,
				RequiredResidentMB: hostfit.OllamaResidentMB(presDense, true),
				WeightsResidentMB:  hostfit.OllamaWeightsResidentMB(presDense, true),
				Speed:              hostfit.SpeedMayBeSlow,
				EstimatedTokps:     hostfit.EstimateOllamaDecode(presDense, hostFromWire(t, wireMac16)).TokpsEstimate,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.Project(tc.v, catalog.RuntimeOllama, tc.host, 0)
			if got != tc.want {
				t.Errorf("Project() =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func TestProjectVLLM(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budgetMB int
		want     hostfit.Presentation
	}{
		{
			// The H100 case from waired-agent#364. Every vLLM row used to
			// come back "may be slow" here because the zero Estimate was
			// read as a verdict; the projection must carry the absence.
			"a budget that clears the minimum runs with no speed claim",
			81559,
			hostfit.Presentation{Runnable: true, QualityTier: 80, RequiredResidentMB: 40960},
		},
		{
			"a short budget reports the shortfall it fell short of",
			16303,
			hostfit.Presentation{
				Reason: hostfit.ReasonInsufficientVRAM, NeedMB: 40960, HaveMB: 16303,
				QualityTier: 80, RequiredResidentMB: 40960,
			},
		},
		{
			// No card at all is a different answer from "not enough": there
			// is no figure to compare against, and 0 GB would read as a
			// measurement.
			"no budget at all is reported as having no card",
			0,
			hostfit.Presentation{
				Reason: hostfit.ReasonNoGPU, NeedMB: 40960,
				QualityTier: 80, RequiredResidentMB: 40960,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.Project(presVLLM, catalog.RuntimeVLLM, hostFromWire(t, wireRTX4090), tc.budgetMB)
			if got != tc.want {
				t.Errorf("Project() =\n  %+v\nwant\n  %+v", got, tc.want)
			}
			// Project holds a VARIANT and no manifest, and every clause
			// of the vLLM recommendation is a manifest fact (the model's
			// own window). So this entry point still carries no vLLM
			// verdict, and an absent rule is not a demotion. ProjectModel
			// does carry one — see TestProjectModelVLLMWindowVerdict
			// (waired-agent#1029).
			if got.NotRecommended {
				t.Error("Project() carried a vLLM recommendation verdict; " +
					"it has no manifest to build one from")
			}
		})
	}
}

// TestProjectRunnableIsExactlyCapacity is a PRODUCT CONTRACT and the
// reason Runnable, Speed and NotRecommended are three fields rather than
// one. Narrowing Runnable with either of the other two would re-break the
// monotonicity invariant waired-agent#229 restored: adding a graphics
// card would once again REMOVE models from a host that served them.
func TestProjectRunnableIsExactlyCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    catalog.Variant
		wire string
	}{
		{"spilled MoE", presMoE, wireRTX5080_16},
		{"confirmed-slow dense", presDense, wireBigRAMSmallGPU},
		{"CPU-only dense", presDense, wireCPUOnly},
		{"unified rejection", presDense, wireMac16},
		{"small everywhere", presSmall, wireRTX4090},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := hostFromWire(t, tc.wire)
			want := hostfit.OllamaFit(tc.v, h).Fits
			if got := hostfit.Project(tc.v, catalog.RuntimeOllama, h, 0).Runnable; got != want {
				t.Errorf("Project().Runnable = %v, want OllamaFit().Fits = %v", got, want)
			}
		})
	}
}

// TestProjectRequiredResidentMatchesTheGate pins that the figure a picker
// prints is the one the fit rule actually compared. A separate derivation
// would be free to drift, which is the failure this whole package exists
// to end.
func TestProjectRequiredResidentMatchesTheGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
		want int
	}{
		{"discrete", wireRTX4090, hostfit.OllamaResidentMB(presMoE, false)},
		{"unified", wireMac16, hostfit.OllamaResidentMB(presMoE, true)},
		{"cpu-only carries no resident requirement", wireCPUOnly, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.Project(presMoE, catalog.RuntimeOllama, hostFromWire(t, tc.wire), 0)
			if got.RequiredResidentMB != tc.want {
				t.Errorf("RequiredResidentMB = %d, want %d", got.RequiredResidentMB, tc.want)
			}
		})
	}
}

// TestProjectUnknownEngine records TODAY'S BEHAVIOUR, matching what the
// control plane's projection already did: a not-runnable row with no
// reason. Deliberately not ReasonNoVariantForEngine — that code says the
// catalog has nothing for this engine, and an unrecognised engine name is
// a statement about the caller instead.
func TestProjectUnknownEngine(t *testing.T) {
	got := hostfit.Project(presSmall, "tensorrt", hostFromWire(t, wireRTX4090), 0)
	want := hostfit.Presentation{QualityTier: 40}
	if got != want {
		t.Errorf("Project(unknown engine) = %+v, want %+v", got, want)
	}
}

// TestNoVariantForEngine pins the row the pickers grey rather than drop.
// The tier rides along because it ranks the MODEL, not its fit, and the
// lists sort by it even at the bottom.
func TestNoVariantForEngine(t *testing.T) {
	got := hostfit.NoVariantForEngine(65)
	if got.Runnable {
		t.Error("a model this engine cannot serve is not runnable")
	}
	if got.Reason != hostfit.ReasonNoVariantForEngine {
		t.Errorf("Reason = %q, want %q", got.Reason, hostfit.ReasonNoVariantForEngine)
	}
	if got.QualityTier != 65 {
		t.Errorf("QualityTier = %d, want 65 — the tier ranks the model, not its fit", got.QualityTier)
	}
}

// TestProjectModelWeightsResident pins the itemisation waired-ai/waired#1174
// prints under every picker row: the weights figure is
// OllamaWeightsResidentMB, it is present with or without a GPU (the
// model's own size is a fact on any host), and subtracting it from the
// window figure leaves exactly the session KV cache — no third hidden
// term, or the two-line breakdown would not reconcile with the total.
func TestProjectModelWeightsResident(t *testing.T) {
	m := catalog.Manifest{
		ModelID:       "weighed",
		ContextLength: 262144,
		Variants:      []catalog.Variant{presMoE},
	}
	host := hostFromWire(t, wireRTX5080_16)
	got := hostfit.ProjectModel(m, presMoE, catalog.RuntimeOllama, host, 0)
	wantW := hostfit.OllamaWeightsResidentMB(presMoE, host.UnifiedMemory)
	if got.WeightsResidentMB != wantW {
		t.Errorf("WeightsResidentMB = %d, want %d", got.WeightsResidentMB, wantW)
	}
	kv := hostfit.ServingWindowKVMB(presMoE, hostfit.OllamaEffectiveContextFloor(m))
	if got.RequiredWindowResidentMB-got.WeightsResidentMB != kv {
		t.Errorf("window − weights = %d, want the session KV cache %d",
			got.RequiredWindowResidentMB-got.WeightsResidentMB, kv)
	}
	noGPU := hostfit.ProjectModel(m, presMoE, catalog.RuntimeOllama,
		hostfit.Host{RAMTotalGB: 64}, 0)
	if noGPU.WeightsResidentMB != hostfit.OllamaWeightsResidentMB(presMoE, false) {
		t.Errorf("no-GPU WeightsResidentMB = %d, want %d — the CPU-only picker "+
			"prints the model's own size there",
			noGPU.WeightsResidentMB, hostfit.OllamaWeightsResidentMB(presMoE, false))
	}
	if vll := hostfit.Project(presVLLM, catalog.RuntimeVLLM, host, 16384); vll.WeightsResidentMB != 0 {
		t.Errorf("vLLM WeightsResidentMB = %d, want 0 — that path prices its "+
			"budget as min_vram_mb in RequiredResidentMB", vll.WeightsResidentMB)
	}
}

// TestPresentationCanonicalJSON pins the wire bytes, per the proto
// module's additive-only rule. Two shapes matter and for opposite
// reasons: the zero value must emit ONLY runnable (every other field is
// omitempty, so the common case stays small), and the full value must
// emit these names in this order — the control plane's existing wire
// names, chosen so adopting this type there adds required_resident_mb and
// changes nothing else.
func TestPresentationCanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    hostfit.Presentation
		want string
	}{
		{"zero", hostfit.Presentation{}, `{"runnable":false}`},
		{
			// waired-ai/waired-agent#1346: the build, its quantization, the
			// KV-cache type the row was priced with, and the layer counts.
			"variant and placement",
			hostfit.Presentation{
				Runnable: true, VariantID: "q3-gguf", Quantization: "UD-Q3_K_XL",
				KVCacheType: "q8_0", GPULayers: 63, TotalLayers: 66,
			},
			`{"runnable":true,"variant_id":"q3-gguf","quantization":"UD-Q3_K_XL",` +
				`"kv_cache_type":"q8_0","gpu_layers":63,"total_layers":66}`,
		},
		{
			"full",
			hostfit.Presentation{
				Runnable: true, Reason: "insufficient_vram", NeedMB: 23482, HaveMB: 16303,
				RequiredResidentMB: 23802, RequiredWindowResidentMB: 26333,
				WeightsResidentMB: 20480, QualityTier: 65,
				NotRecommended: true, NotRecommendedReason: hostfit.ReasonWeightsSpill,
				Speed: hostfit.SpeedMayBeSlow, EstimatedTokps: 80.9,
			},
			`{"runnable":true,"reason":"insufficient_vram","need_mb":23482,"have_mb":16303,` +
				`"required_resident_mb":23802,"required_window_resident_mb":26333,` +
				`"weights_resident_mb":20480,"quality_tier":65,"not_recommended":true,` +
				`"not_recommended_reason":"weights_spill","speed":"may_be_slow","estimated_tokps":80.9}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("marshalled to\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// TestProjectModelVLLMWindowVerdict is the waired-agent#1029 contract: the
// two engine tabs answer the window question the same way, because it is a
// question about the MODEL.
//
// Before this, ProjectModel filled NotRecommended only for ollama. A
// 131072-native model was annotated "not recommended on any computer" on
// the ollama tab and carried nothing at all on the vLLM tab — where it was
// also the preselected row, on a host whose engine then clamped it to
// 124928. The absence did not read as "no rule applies"; it read as
// "nothing is wrong with this row".
//
// Since waired-ai/waired-agent#1400 neither tab asks about the model's own
// window: the catalog admits only builds whose window reaches the coding
// window (decisions 3 and 4 of docs/decisions/20260916/0340). The parity
// is what stays — the two tabs still give one answer for one model.
func TestProjectModelVLLMWindowVerdict(t *testing.T) {
	// vLLM-only variant, big enough card to run it, so Runnable is never
	// the thing under test.
	host := hostFromWire(t, wireRTX4090)
	// The H100-sized budget TestProjectVLLM uses: capacity must never be
	// what this test is measuring.
	const budgetMB = 81559
	short := catalog.Manifest{
		ModelID:       "short-window",
		ContextLength: 131072,
		Variants:      []catalog.Variant{presVLLM},
	}
	long := catalog.Manifest{
		ModelID:       "coding-window",
		ContextLength: 262144,
		Variants:      []catalog.Variant{presVLLM},
	}

	got := hostfit.ProjectModel(short, presVLLM, catalog.RuntimeVLLM, host, budgetMB)
	if !got.Runnable {
		t.Fatal("the short-window model stopped being runnable — capacity is the only rule allowed to refuse")
	}
	if got.NotRecommendedReason == hostfit.ReasonWindowTooSmall {
		t.Errorf("vLLM row for a 131072-native model still says %q; nothing produces it since #1400",
			got.NotRecommendedReason)
	}

	// Parity is the point: the ollama tab has said this for a while.
	ollamaVariant := presVLLM
	ollamaVariant.RuntimeSupport = []string{"ollama"}
	ollamaVariant.MinRAMGB = 8
	ollamaVariant.EstimatedWeightGB = 4.7
	shortOllama := short
	shortOllama.Variants = []catalog.Variant{ollamaVariant}
	gotOllama := hostfit.ProjectModel(shortOllama, ollamaVariant, catalog.RuntimeOllama, host, 0)
	if gotOllama.NotRecommendedReason != got.NotRecommendedReason {
		t.Errorf("the two engine tabs disagree about the same model's window: ollama %q, vllm %q",
			gotOllama.NotRecommendedReason, got.NotRecommendedReason)
	}

	// A model whose own window reaches the coding window keeps the row
	// clean: the clause is about the manifest, not about vLLM.
	if got := hostfit.ProjectModel(long, presVLLM, catalog.RuntimeVLLM, host, budgetMB); got.NotRecommended {
		t.Errorf("a 262144-native model was demoted on vLLM: reason=%q", got.NotRecommendedReason)
	}
}

// TestVLLMRecommendModel pins what the vLLM rule does and does NOT ask.
// It carried one clause, the model's own window, until
// waired-ai/waired-agent#1400 removed it (decisions 3 and 4 of
// docs/decisions/20260916/0340); the host clauses live in
// VLLMRecommendModelOnHost and proto/modelrank.
func TestVLLMRecommendModel(t *testing.T) {
	host := hostFromWire(t, wireRTX4090)
	for _, tc := range []struct {
		name       string
		ctxLen     int
		wantFits   bool
		wantReason string
	}{
		{"a 131072 model is no longer judged on its window", 131072, true, ""},
		{"the coding window exactly", 200704, true, ""},
		{"a 1M model", 1048576, true, ""},
		{"a manifest with no window", 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := catalog.Manifest{ModelID: "m", ContextLength: tc.ctxLen}
			got := hostfit.VLLMRecommendModel(m, presVLLM, host)
			if got.Fits != tc.wantFits || got.Reason != tc.wantReason {
				t.Errorf("VLLMRecommendModel() = {Fits:%v Reason:%q}, want {Fits:%v Reason:%q}",
					got.Fits, got.Reason, tc.wantFits, tc.wantReason)
			}
		})
	}

	// A card too small to run it is capacity's answer, not the
	// recommendation's — the monotonicity invariant TestProjectRunnableIsExactlyCapacity
	// guards depends on these staying separate questions.
	small := hostFromWire(t, wireRTX5080_16)
	m := catalog.Manifest{ModelID: "m", ContextLength: 262144}
	if got := hostfit.VLLMRecommendModel(m, presVLLM, small); !got.Fits {
		t.Errorf("a short VRAM budget produced a recommendation verdict %q; capacity refuses, this does not", got.Reason)
	}
}

// TestProjectModelFromNamesTheBuildAndCache pins the identity half of the
// waired-ai/waired-agent#1346 wire: every projected row says which build
// it judged and which KV-cache type it priced, so a console offering both
// as choices can tell its rows apart. Both engines resolve the type
// through ResolveKVCacheType: the one the caller asked for when the host
// and the build allow it, the default otherwise.
func TestProjectModelFromNamesTheBuildAndCache(t *testing.T) {
	m := catalog.Manifest{ModelID: "m", ContextLength: 262144}
	v := catalog.Variant{
		VariantID: "q3-gguf", Quantization: "UD-Q3_K_XL",
		EstimatedWeightGB: 13.15, KVBytesPerTokenFP16: 65536, QualityTier: 66,
		KVCacheTypes: []string{catalog.KVCacheQ4_0, catalog.KVCacheQ8_0, catalog.KVCacheF16},
	}
	h := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24467}

	got := hostfit.ProjectModelFrom(hostfit.ModelProjection{Manifest: m, Variant: v, Engine: catalog.RuntimeOllama, Host: h})
	if got.VariantID != "q3-gguf" || got.Quantization != "UD-Q3_K_XL" {
		t.Errorf("ollama row names %q / %q, want q3-gguf / UD-Q3_K_XL", got.VariantID, got.Quantization)
	}
	if got.KVCacheType != catalog.KVCacheQ4_0 {
		t.Errorf("ollama row priced %q, want the q4_0 default", got.KVCacheType)
	}

	got = hostfit.ProjectModelFrom(hostfit.ModelProjection{
		Manifest: m, Variant: v, Engine: catalog.RuntimeOllama, Host: h, KVCacheType: catalog.KVCacheF16,
	})
	if got.KVCacheType != catalog.KVCacheF16 {
		t.Errorf("ollama row asked for f16 priced %q", got.KVCacheType)
	}

	// A build that does not list q4_0 is priced at the next rung, even
	// when q4_0 is asked for: the row names what would actually be served.
	unlisted := v
	unlisted.KVCacheTypes = nil
	got = hostfit.ProjectModelFrom(hostfit.ModelProjection{
		Manifest: m, Variant: unlisted, Engine: catalog.RuntimeOllama, Host: h, KVCacheType: catalog.KVCacheQ4_0,
	})
	if got.KVCacheType != catalog.KVCacheQ8_0 {
		t.Errorf("ollama row for a build without q4_0, asked for q4_0, priced %q; want q8_0", got.KVCacheType)
	}

	vl := catalog.Variant{VariantID: "fp8", Quantization: "FP8", MinVRAMMB: 20000, QualityTier: 70}
	ada := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.9"}}
	got = hostfit.ProjectModelFrom(hostfit.ModelProjection{Manifest: m, Variant: vl, Engine: catalog.RuntimeVLLM, Host: h, BudgetMB: 24564, GPUs: ada})
	if got.VariantID != "fp8" || got.KVCacheType != catalog.KVCacheFP8 {
		t.Errorf("vLLM row on Ada = %q / %q, want fp8 / fp8", got.VariantID, got.KVCacheType)
	}
	got = hostfit.ProjectModelFrom(hostfit.ModelProjection{Manifest: m, Variant: vl, Engine: catalog.RuntimeVLLM, Host: h, BudgetMB: 24564, GPUs: ada, KVCacheType: catalog.KVCacheFP16})
	if got.KVCacheType != catalog.KVCacheFP16 {
		t.Errorf("vLLM row on Ada asked for fp16 = %q, want fp16", got.KVCacheType)
	}
	ampere := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.6"}}
	got = hostfit.ProjectModelFrom(hostfit.ModelProjection{Manifest: m, Variant: vl, Engine: catalog.RuntimeVLLM, Host: h, BudgetMB: 24564, GPUs: ampere, KVCacheType: catalog.KVCacheFP8})
	if got.KVCacheType != catalog.KVCacheFP16 {
		t.Errorf("vLLM row before Ada asked for fp8 = %q, want fp16", got.KVCacheType)
	}
	got = hostfit.ProjectModelFrom(hostfit.ModelProjection{Manifest: m, Variant: vl, Engine: "mlx", Host: h})
	if got.VariantID != "" || got.KVCacheType != "" {
		t.Errorf("an unknown engine's row carries %q / %q, want the zero identity it always had", got.VariantID, got.KVCacheType)
	}
}

// A vLLM row asked for a KV-cache type is projected — and judged — at the
// type the engine would serve: fp16 when fp16 is chosen, and fp16 when fp8
// is asked of a card that cannot run it (waired-ai/waired-agent#1347).
func TestProjectModelFromVLLMHonoursTheChosenCacheType(t *testing.T) {
	m := catalog.Manifest{ModelID: "m", ContextLength: 262144}
	v := catalog.Variant{VariantID: "fp8", MinVRAMMB: 16000, EstimatedWeightGB: 14, KVBytesPerTokenFP16: 32768, QualityTier: 70}
	h := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24564}
	ada := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.9"}}
	ampere := []signer.HardwareGPUSummary{{Vendor: "nvidia", VRAMTotalMB: 24564, ComputeCap: "8.6"}}
	project := func(gpus []signer.HardwareGPUSummary, kv string) hostfit.Presentation {
		return hostfit.ProjectModelFrom(hostfit.ModelProjection{
			Manifest: m, Variant: v, Engine: catalog.RuntimeVLLM, Host: h, BudgetMB: 24564, GPUs: gpus, KVCacheType: kv,
		})
	}
	if got := project(ada, catalog.KVCacheFP16).KVCacheType; got != catalog.KVCacheFP16 {
		t.Errorf("fp16 chosen on Ada projected %q", got)
	}
	if got := project(ampere, catalog.KVCacheFP8).KVCacheType; got != catalog.KVCacheFP16 {
		t.Errorf("fp8 asked of an Ampere card projected %q, want the fp16 it would serve", got)
	}
	// The same window costs twice the KV at fp16, which is what the verdict
	// has to see: 200k fits beside 14 GB of weights at fp8 and not at fp16.
	if fp8, fp16 := project(ada, ""), project(ada, catalog.KVCacheFP16); fp8.NotRecommended || !fp16.NotRecommended {
		t.Errorf("fp8 NotRecommended=%v, fp16 NotRecommended=%v; want the window judged at the chosen type", fp8.NotRecommended, fp16.NotRecommended)
	}
}

// The ollama row itemises its window figure so a surface can print the
// weights, the KV cache and the engine's overhead separately, and none of
// the overhead reads as KV cache: the three add up to
// RequiredWindowResidentMB, and the MTP draft head's cache counts as KV
// cache (waired-ai/waired-agent#1337).
func TestProjectModelFromItemisesTheWindowFigure(t *testing.T) {
	m := catalog.Manifest{ModelID: "qwen3.8-27b", ContextLength: 262144}
	var v catalog.Variant
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	for _, bm := range ms {
		for _, bv := range bm.Variants {
			if bm.ModelID == "qwen3.8-27b" && bv.VariantID == "mtp-q4-gguf" {
				v = bv
			}
		}
	}
	if v.GGUF == nil {
		t.Fatal("the bundled catalog has no qwen3.8-27b/mtp-q4-gguf with a GGUF layout")
	}
	h := hostfit.Host{RAMTotalGB: 121, GPUCount: 1, VRAM0MB: 24467, GPUVendor: "nvidia"}
	got := hostfit.ProjectModelFrom(hostfit.ModelProjection{
		Manifest: m, Variant: v, Engine: catalog.RuntimeOllama, Host: h, KVCacheType: catalog.KVCacheQ8_0,
	})
	e := hostfit.OllamaEstimateMemory(v, h, catalog.KVCacheQ8_0, hostfit.ServingWindow200k, 1)
	// 6,664 MiB of q8_0 cache for 200,704 cells plus the draft head's
	// f16 layer, 784 MiB.
	if got.KVCacheMB != 6664+784 || e.DraftKVCacheMB != 784 {
		t.Errorf("KVCacheMB = %d (draft %d), want 6664 + 784", got.KVCacheMB, e.DraftKVCacheMB)
	}
	if got.DeviceWeightsMB != e.DeviceWeightsMB || got.DeviceWeightsMB <= 0 {
		t.Errorf("DeviceWeightsMB = %d, want the estimate's %d", got.DeviceWeightsMB, e.DeviceWeightsMB)
	}
	if overhead := got.RequiredWindowResidentMB - got.DeviceWeightsMB - got.KVCacheMB; overhead != e.RecurrentStateMB+e.ComputeMB+e.DraftMB-e.DraftKVCacheMB+e.FixedMB+e.FitTargetMB {
		t.Errorf("overhead = %d MiB, want the estimate's non-weight, non-KV terms", overhead)
	}

	vl := catalog.Variant{VariantID: "fp8", MinVRAMMB: 16000, EstimatedWeightGB: 14, KVBytesPerTokenFP16: 32768}
	if got := hostfit.ProjectModelFrom(hostfit.ModelProjection{Manifest: m, Variant: vl, Engine: catalog.RuntimeVLLM, Host: h, BudgetMB: 24467}); got.DeviceWeightsMB != 0 || got.KVCacheMB != 0 {
		t.Errorf("vLLM row itemised %d / %d, want both absent", got.DeviceWeightsMB, got.KVCacheMB)
	}
}
