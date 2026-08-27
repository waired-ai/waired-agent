package hostfit_test

import (
	"encoding/json"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
	if !got.NotRecommended || got.NotRecommendedReason != hostfit.ReasonWindowTooSmall {
		t.Errorf("vLLM row for a 131072-native model: NotRecommended=%v reason=%q, want true / %q",
			got.NotRecommended, got.NotRecommendedReason, hostfit.ReasonWindowTooSmall)
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
// It is one clause today by design — the two the ollama rule adds are its
// own arithmetic, and vLLM's equivalents live in proto/modelrank, which
// imports this package.
func TestVLLMRecommendModel(t *testing.T) {
	host := hostFromWire(t, wireRTX4090)
	for _, tc := range []struct {
		name       string
		ctxLen     int
		wantFits   bool
		wantReason string
	}{
		{"a 131072 model cannot hold a coding session on any machine", 131072, false, hostfit.ReasonWindowTooSmall},
		{"the coding window exactly", 200704, true, ""},
		{"a 1M model", 1048576, true, ""},
		{"a manifest with no window declares nothing to check against", 0, false, hostfit.ReasonWindowTooSmall},
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
