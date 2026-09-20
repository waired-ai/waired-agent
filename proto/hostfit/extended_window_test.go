package hostfit_test

import (
	"reflect"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

func TestDeclarableExtendedWindow(t *testing.T) {
	yarn := func(factor float64, orig int) *catalog.RopeScaling {
		return &catalog.RopeScaling{Type: catalog.RopeScalingYaRN, Factor: factor, OriginalContextLength: orig}
	}
	for _, tc := range []struct {
		name string
		m    catalog.Manifest
		want int
	}{
		{"no scaling", catalog.Manifest{ContextLength: 262144}, 0},
		{"the Qwen shape", catalog.Manifest{ContextLength: 262144, RopeScaling: yarn(4, 262144)}, hostfit.ServingWindow1M},
		// Short of the rung, this returns 0 and NOT ServingWindow200k: the
		// 200k rung is DeclarableNativeWindow's answer, and crediting the
		// scaling for it would double-count a window the model already had.
		{"scaling that stops short of the rung", catalog.Manifest{ContextLength: 262144, RopeScaling: yarn(2, 262144)}, 0},
		{"a 1M-native model needs no scaling", catalog.Manifest{ContextLength: 1048576}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.DeclarableExtendedWindow(tc.m); got != tc.want {
				t.Errorf("DeclarableExtendedWindow = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOllamaServedWindowsWith_OnlyWhenAsked is the load-bearing test of
// waired-ai/waired#1456: the longer window appears in the ladder ONLY for a
// caller that names it. Static rope scaling is applied to every prompt the
// engine sees, so a model promoted to 1M without being asked would be a
// different model for the person who never asked.
func TestOllamaServedWindowsWith_OnlyWhenAsked(t *testing.T) {
	scaled := catalog.Manifest{ModelID: "m", ContextLength: 262144,
		RopeScaling: &catalog.RopeScaling{Type: catalog.RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144}}
	plain := catalog.Manifest{ModelID: "m", ContextLength: 262144}

	for _, tc := range []struct {
		name   string
		m      catalog.Manifest
		chosen int
		want   []int
	}{
		{"nobody asked", scaled, 0, []int{hostfit.ServingWindow200k}},
		{"asked for the coding window", scaled, hostfit.ServingWindow200k, []int{hostfit.ServingWindow200k}},
		{"asked for 1M", scaled, hostfit.ServingWindow1M, []int{hostfit.ServingWindow1M, hostfit.ServingWindow200k}},
		{"asked for 1M of a model with no scaling", plain, hostfit.ServingWindow1M, []int{hostfit.ServingWindow200k}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.OllamaServedWindowsWith(tc.m, tc.chosen); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("OllamaServedWindowsWith(%d) = %v, want %v", tc.chosen, got, tc.want)
			}
		})
	}
}

// TestOllamaServedWindows_UnchangedForEveryShippedModel is the stake in the
// ground. Every rule in this package and six callers outside it read the
// ladder; if adding rope scaling to nine manifests widened it, hosts would
// start planning a 1M window nobody asked for. The expected values are
// written out rather than computed so that a change to the ladder cannot
// quietly agree with itself.
func TestOllamaServedWindows_UnchangedForEveryShippedModel(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int{
		"granite4-350m":      {32768}, // internal_only, serves its own window
		"qwen3.5-0.8b":       {200704},
		"qwen3.5-2b":         {200704},
		"qwen3.5-4b":         {200704},
		"qwen3.5-9b":         {200704},
		"qwen3.5-27b":        {200704},
		"qwen3.5-35b-a3b":    {200704},
		"qwen3.5-122b-a10b":  {200704},
		"qwen3.6-27b":        {200704},
		"qwen3.6-35b-a3b":    {200704},
		"qwen3.8-27b":        {200704},
		"qwen3.8-flash-next": {200704},
	}
	if len(manifests) != len(want) {
		t.Errorf("the bundled catalog has %d models and this table has %d: add the new one, and say what ladder it gets",
			len(manifests), len(want))
	}
	for _, m := range manifests {
		w, ok := want[m.ModelID]
		if !ok {
			t.Errorf("%s is not in this table", m.ModelID)
			continue
		}
		if got := hostfit.OllamaServedWindows(m); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: OllamaServedWindows = %v, want %v", m.ModelID, got, w)
		}
	}
}

// TestDeclarableExtendedWindow_Catalog is what retired the "no shipped model
// declares 1M" note in serving_window_test.go: after waired-ai/waired#1456
// something we ship reaches the rung again, through scaling rather than
// through its own trained window.
func TestDeclarableExtendedWindow_Catalog(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	reach := 0
	for _, m := range manifests {
		got := hostfit.DeclarableExtendedWindow(m)
		if got != 0 && got != hostfit.ServingWindow1M {
			t.Errorf("%s: DeclarableExtendedWindow = %d — the two-rung contract has no value between", m.ModelID, got)
		}
		if got == hostfit.ServingWindow1M {
			reach++
		}
		// A publisher's stated ceiling is prose, and Qwen's lands between
		// the rungs (1,010,000). That is allowed precisely because nothing
		// serves it: only ExtendedContextLength feeds the ladder.
		if p := catalog.PublisherContextLimit(m); p != 0 && hostfit.DeclarableExtendedWindow(m) == 0 {
			t.Errorf("%s: states a publisher ceiling of %d but reaches no rung", m.ModelID, p)
		}
	}
	if reach == 0 {
		t.Error("no shipped model reaches the 1M rung; the rung is unreachable again (waired-ai/waired#1456)")
	}
}

// TestProjectModelFrom_PricesTheChosenWindow is how a surface reprices a
// whole catalog for the long window without a second wire field: the
// control plane's device catalog projects every row through this one
// function (waired-ai/waired#1359, waired-ai/waired#1456).
func TestProjectModelFrom_PricesTheChosenWindow(t *testing.T) {
	host := hostFromWire(t, wireRTX5080_16)
	scaled := catalog.Manifest{ModelID: "scaled", ContextLength: 262144,
		RopeScaling: &catalog.RopeScaling{Type: catalog.RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144},
		Variants:    []catalog.Variant{presMoE}}
	plain := catalog.Manifest{ModelID: "plain", ContextLength: 262144, Variants: []catalog.Variant{presMoE}}

	at := func(m catalog.Manifest, window int) hostfit.Presentation {
		return hostfit.ProjectModelFrom(hostfit.ModelProjection{
			Manifest: m, Variant: presMoE, Engine: catalog.RuntimeOllama, Host: host, Window: window,
		})
	}

	coding := at(scaled, 0)
	if coding.PricedWindow != hostfit.ServingWindow200k {
		t.Errorf("nobody asked: PricedWindow = %d, want %d", coding.PricedWindow, hostfit.ServingWindow200k)
	}

	long := at(scaled, hostfit.ServingWindow1M)
	if long.PricedWindow != hostfit.ServingWindow1M {
		t.Fatalf("asked for 1M: PricedWindow = %d, want %d", long.PricedWindow, hostfit.ServingWindow1M)
	}
	// The KV cache is the whole point of repricing: it is what the longer
	// window costs, and the weights do not move.
	if long.KVCacheMB <= coding.KVCacheMB {
		t.Errorf("KVCacheMB did not grow with the window: %d at 1M vs %d at 200k", long.KVCacheMB, coding.KVCacheMB)
	}
	if long.WeightsResidentMB != coding.WeightsResidentMB {
		t.Errorf("WeightsResidentMB moved with the window: %d vs %d", long.WeightsResidentMB, coding.WeightsResidentMB)
	}
	if want := hostfit.ServingWindowKVMB(presMoE, hostfit.ServingWindow1M); long.KVCacheMB < want {
		t.Errorf("KVCacheMB = %d, want at least the window's own cache %d", long.KVCacheMB, want)
	}

	// A model that cannot reach the window is priced at the coding window,
	// not refused: a row is a description, and refusing belongs to
	// OllamaDeclaresWindow.
	if got := at(plain, hostfit.ServingWindow1M); got.PricedWindow != hostfit.ServingWindow200k {
		t.Errorf("model with no scaling, asked for 1M: PricedWindow = %d, want %d", got.PricedWindow, hostfit.ServingWindow200k)
	}
}

// TestReachesWindow is the grey-out predicate. The owner's ruling of
// 2026-09-20 on waired-ai/waired#1359 is that picking the long window must
// leave the models that cannot serve it visible but unselectable, with the
// reason — so every surface has to grey the same rows, which means one
// predicate and not four.
func TestReachesWindow(t *testing.T) {
	scaled := catalog.Manifest{ContextLength: 262144,
		RopeScaling: &catalog.RopeScaling{Type: catalog.RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144}}
	plain := catalog.Manifest{ContextLength: 262144}
	small := catalog.Manifest{ContextLength: 32768}

	for _, tc := range []struct {
		name   string
		m      catalog.Manifest
		window int
		want   bool
	}{
		{"no window asked", plain, 0, true},
		{"coding window, plain model", plain, hostfit.ServingWindow200k, true},
		{"coding window, scaled model", scaled, hostfit.ServingWindow200k, true},
		{"long window, plain model", plain, hostfit.ServingWindow1M, false},
		{"long window, scaled model", scaled, hostfit.ServingWindow1M, true},
		{"coding window, internal 32k model", small, hostfit.ServingWindow200k, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.ReachesWindow(tc.m, tc.window); got != tc.want {
				t.Errorf("ReachesWindow(%d) = %v, want %v", tc.window, got, tc.want)
			}
		})
	}
}

// TestReachesWindow_Catalog counts what a person sees when they pick the long
// window: the shipped models divide, and that division is the point of the
// ruling. Nine reach it through their publishers' scaling; the two small Qwens
// and the CI-only model do not.
func TestReachesWindow_Catalog(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	var reach, greyed []string
	for _, m := range manifests {
		if hostfit.ReachesWindow(m, hostfit.ServingWindow1M) {
			reach = append(reach, m.ModelID)
		} else {
			greyed = append(greyed, m.ModelID)
		}
		// Every shipped model still reaches the coding window; that is the
		// catalog admission rule (#1400) and the long window must not have
		// disturbed it.
		if !hostfit.ReachesWindow(m, hostfit.ServingWindow200k) && m.InternalOnly == "" {
			t.Errorf("%s no longer reaches the coding window", m.ModelID)
		}
	}
	if len(reach) != 9 {
		t.Errorf("%d models reach the long window, want 9: %v", len(reach), reach)
	}
	if len(greyed) != 3 {
		t.Errorf("%d models are greyed at the long window, want 3: %v", len(greyed), greyed)
	}
}

// TestOllamaEstimate_MatchesTheEngineAtBothWindows pins the estimate to what
// llama.cpp actually allocated on the reference host for the default 35B-A3B
// build (qwen3.6-35b-a3b/mtp-q4-gguf, q4_0 KV, flash attention on), read from
// its own log lines:
//
//	200,704    llama_kv_cache: size = 1102.50 MiB   draft 392.00 MiB
//	1,048,576  llama_kv_cache: size = 5760.00 MiB   draft 2048.00 MiB
//
// The 1M column had never been checked against a running engine before
// waired-ai/waired#1456, and checking it is how a wrong reading of the catalog
// got caught: the ollama path does NOT read Variant.MTPKVBytesPerTokenFP16 —
// that field belongs to the vLLM sizing — and derives the draft head's cache
// from the full-attention layer count instead. An empty field there is not a
// missing term, and this test fails if anyone "fixes" it by adding one.
//
// Driven through OllamaEstimateMemory and the shipped manifest rather than
// recomputed here, so it bites on the arithmetic and on the catalog figures
// alike.
func TestOllamaEstimate_MatchesTheEngineAtBothWindows(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	var m catalog.Manifest
	var v catalog.Variant
	for _, cand := range manifests {
		if cand.ModelID != "qwen3.6-35b-a3b" {
			continue
		}
		for _, cv := range cand.Variants {
			if cv.VariantID == "mtp-q4-gguf" {
				m, v = cand, cv
			}
		}
	}
	if v.VariantID == "" {
		t.Skip("qwen3.6-35b-a3b/mtp-q4-gguf is no longer shipped; re-measure before re-pinning")
	}
	_ = m

	// The reference host: unified memory, room for either window.
	host := hostfit.Host{RAMTotalGB: 128, UnifiedMemory: true, GPUCount: 1, UsableVRAMMB: 98304}

	// The estimate reports whole MiB and rounds up, so where the engine
	// reported a half it reads one higher — 1102.50 becomes 1103. Rounding
	// up is the safe direction for a budget and is not a discrepancy.
	for _, tc := range []struct {
		window, wantKV, wantDraft int
		engineKV, engineDraft     string
	}{
		{hostfit.ServingWindow200k, 1103, 392, "1102.50", "392.00"},
		{hostfit.ServingWindow1M, 5760, 2048, "5760.00", "2048.00"},
	} {
		got := hostfit.OllamaEstimateMemory(v, host, catalog.KVCacheQ4_0, tc.window, 1)
		if got.KVCacheMB != tc.wantKV {
			t.Errorf("window %d: KVCacheMB = %d, want %d (the engine allocated %s MiB)",
				tc.window, got.KVCacheMB, tc.wantKV, tc.engineKV)
		}
		if got.DraftKVCacheMB != tc.wantDraft {
			t.Errorf("window %d: DraftKVCacheMB = %d, want %d (the engine allocated %s MiB)",
				tc.window, got.DraftKVCacheMB, tc.wantDraft, tc.engineDraft)
		}
	}
}
